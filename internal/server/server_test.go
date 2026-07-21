package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyGithubSignature(t *testing.T) {
	secret := "topsecret"
	body := []byte(`{"repository":{"full_name":"rytsh/krabby"}}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	valid := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyGithubSignature(secret, body, valid) {
		t.Error("valid signature rejected")
	}

	if verifyGithubSignature(secret, body, "sha256=deadbeef") {
		t.Error("invalid signature accepted")
	}

	if verifyGithubSignature(secret, body, "") {
		t.Error("missing signature accepted")
	}
}

func TestAPIKeyMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := apiKeyMiddleware("secret-key")(next)

	tests := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{name: "no key", header: nil, want: http.StatusUnauthorized},
		{name: "wrong key", header: map[string]string{"X-Api-Key": "nope"}, want: http.StatusUnauthorized},
		{name: "x-api-key", header: map[string]string{"X-Api-Key": "secret-key"}, want: http.StatusOK},
		{name: "bearer", header: map[string]string{"Authorization": "Bearer secret-key"}, want: http.StatusOK},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		for k, v := range tt.header {
			req.Header.Set(k, v)
		}

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != tt.want {
			t.Errorf("%s: got %d, want %d", tt.name, rec.Code, tt.want)
		}
	}

	// Empty key disables auth.
	open := apiKeyMiddleware("")(next)
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("empty api key should disable auth, got %d", rec.Code)
	}
}
