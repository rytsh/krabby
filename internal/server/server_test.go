package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestVerifyGitWebhook(t *testing.T) {
	secret := "topsecret"
	body := []byte(`{"repository":{"full_name":"rytsh/krabby"}}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	valid := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	githubHeader := http.Header{"X-Hub-Signature-256": []string{valid}}
	if !verifyGitWebhook(secret, body, githubHeader) {
		t.Error("valid signature rejected")
	}

	if verifyGitWebhook(secret, body, http.Header{"X-Gitea-Signature": []string{"deadbeef"}}) {
		t.Error("invalid signature accepted")
	}

	if verifyGitWebhook(secret, body, http.Header{}) {
		t.Error("missing signature accepted")
	}

	if !verifyGitWebhook(secret, body, http.Header{"X-Gitlab-Token": []string{secret}}) {
		t.Error("valid gitlab token rejected")
	}
	if !verifyGitWebhook(secret, body, http.Header{"Authorization": []string{"Bearer " + secret}}) {
		t.Error("provider-neutral bearer token rejected")
	}
}

func TestGitEventRepoRef(t *testing.T) {
	var event gitPushEvent
	event.Project.GitHTTPURL = "https://gitlab.example.com/group/sub/project.git"
	if got := gitEventRepoRef(event); got != "gitlab.example.com/group/sub/project" {
		t.Fatalf("gitEventRepoRef() = %q", got)
	}

	event.Project.GitHTTPURL = ""
	event.Project.PathWithNamespace = "group/sub/project"
	if got := gitEventRepoRef(event); got != "group/sub/project" {
		t.Fatalf("gitEventRepoRef() fallback = %q", got)
	}
}

func TestAPIKeyMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	key := "secret-key"
	handler := apiKeyMiddleware(func() string { return key })(next)

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
	open := apiKeyMiddleware(func() string { return "" })(next)
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("empty api key should disable auth, got %d", rec.Code)
	}

	// The key is resolved per request: a runtime change applies immediately.
	key = "rotated"
	req = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-Api-Key", "secret-key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("old key should be rejected after rotation, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-Api-Key", "rotated")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("rotated key should be accepted, got %d", rec.Code)
	}
}

func TestMCPServerForRequest(t *testing.T) {
	standard := mcp.NewServer(&mcp.Implementation{Name: "standard", Version: "test"}, nil)
	full := mcp.NewServer(&mcp.Implementation{Name: "full", Version: "test"}, nil)

	tests := []struct {
		name   string
		header string
		want   *mcp.Server
	}{
		{name: "default", want: standard},
		{name: "standard", header: "standard", want: standard},
		{name: "unknown", header: "other", want: standard},
		{name: "full", header: "full", want: full},
		{name: "full case insensitive", header: " FULL ", want: full},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req.Header.Set(MCPToolProfileHeader, tt.header)
			if got := mcpServerForRequest(req, standard, full); got != tt.want {
				t.Fatalf("selected server %p, want %p", got, tt.want)
			}
		})
	}
}
