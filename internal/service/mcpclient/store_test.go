package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/storage"
)

func TestStoreSecretsAndLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Name: "config", URL: "https://example.com/mcp", BearerToken: "token-secret", Headers: map[string]string{"x-api-key": "header-secret"}, AllowedTools: []string{"read_config", " read_config "}}
	view, err := s.Save(ctx, "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertRedacted := func(v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"token-secret", "header-secret"} {
			if strings.Contains(string(b), secret) {
				t.Fatalf("secret exposed in JSON: %s", b)
			}
		}
	}
	assertRedacted(view)
	if !view.BearerTokenSet || !reflect.DeepEqual(view.HeaderNames, []string{"X-Api-Key"}) || len(view.AllowedTools) != 1 || view.TimeoutSeconds != 30 {
		t.Fatalf("unexpected view: %+v", view)
	}
	if _, err := s.Save(ctx, "", cfg); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate error: %v", err)
	}
	// Reopen the DB: credentials and grants must survive process restarts.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err = New(db)
	if err != nil {
		t.Fatal(err)
	}
	cfg.BearerToken, cfg.Headers = "", map[string]string{"X-API-Key": ""}
	if _, err := s.Save(ctx, "config", cfg); err != nil {
		t.Fatal(err)
	}
	c, err := s.Get(ctx, "config")
	if err != nil {
		t.Fatal(err)
	}
	if c.BearerToken != "token-secret" || c.Headers["X-Api-Key"] != "header-secret" {
		t.Fatal("blank values did not preserve saved credentials")
	}
	items, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertRedacted(items)
	// Preview has no side effects.
	preview, err := s.Preview(ctx, "config", Config{URL: cfg.URL, ClearBearerToken: true, Headers: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if preview.BearerToken != "" || len(preview.Headers) != 0 {
		t.Fatal("explicit clearing failed")
	}
	c, _ = s.Get(ctx, "config")
	if c.BearerToken == "" {
		t.Fatal("preview changed persisted config")
	}
	// URL changes drop old credentials and grants even if the client resends grants.
	cfg.URL, cfg.Headers = "https://another.example/mcp", nil
	view, err = s.Save(ctx, "config", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if view.BearerTokenSet || len(view.HeaderNames) != 0 || len(view.AllowedTools) != 0 {
		t.Fatal("endpoint change retained credentials or grants")
	}
	if err := s.Delete(ctx, "config"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "config"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted record: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, url := range []string{"", "file:///tmp/test", "http:///missing", "https://user:secret@example.com/mcp", "https://example.com/mcp?token=secret", "https://example.com/#x"} {
		if _, err := merge(nil, Config{Name: "test", URL: url}); !errors.Is(err, ErrInvalid) {
			t.Errorf("URL %q accepted: %v", url, err)
		}
	}
	for _, name := range []string{"", "../x", "has space", "Upper", strings.Repeat("a", 129)} {
		if _, err := merge(nil, Config{Name: name, URL: "https://example.com/mcp"}); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q accepted", name)
		}
	}
	for _, headers := range []map[string]string{{"Mcp-Session-Id": "x"}, {"Content-Type": "x"}, {"Host": "x"}, {"Cookie": "x"}, {"X-Key": "secret\r\ninjected"}, {"X-Key": ""}, {"x-key": "a", "X-Key": "b"}} {
		if _, err := merge(nil, Config{Name: "test", URL: "http://localhost/mcp", Headers: headers}); !errors.Is(err, ErrInvalid) {
			t.Errorf("headers accepted: %v", headers)
		}
	}
	if _, err := merge(nil, Config{Name: "test", URL: "https://example.com", BearerToken: "token", Headers: map[string]string{"Authorization": "custom"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("ambiguous authentication accepted")
	}
	for _, timeout := range []int{-1, 121} {
		if _, err := merge(nil, Config{Name: "test", URL: "https://example.com", TimeoutSeconds: timeout}); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid timeout accepted")
		}
	}
}
