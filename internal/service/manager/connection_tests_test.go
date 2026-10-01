package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/rytsh/krabby/internal/storage"
)

func TestEmbedderConnectionsSelectConfigAndKeepStoredSettings(t *testing.T) {
	for _, tc := range []struct {
		name, path, model, key string
		code                   bool
		patchKey               string
		dedicated              bool
	}{
		{name: "docs stored key", path: "/docs/embeddings", model: "docs-preview", key: "stored-docs"},
		{name: "docs typed key", path: "/docs/embeddings", model: "docs-preview", key: "typed-docs", patchKey: "typed-docs"},
		{name: "code stored key", code: true, dedicated: true, path: "/code/embeddings", model: "code-preview", key: "stored-code"},
		{name: "code typed key", code: true, dedicated: true, path: "/code/embeddings", model: "code-preview", key: "typed-code", patchKey: "typed-code"},
		{name: "code docs fallback", code: true, path: "/docs/embeddings", model: "docs-preview", key: "stored-docs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type request struct{ path, model, auth string }
			requests := make(chan request, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- request{r.URL.Path, body.Model, r.Header.Get("Authorization")}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
			}))
			defer server.Close()

			db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			store, err := settings.New(db, settings.Settings{
				EmbedBaseURL: "https://stored.invalid/docs", EmbedAPIKey: "stored-docs", EmbedModel: "stored-docs-model",
				CodeEmbedBaseURL: "https://stored.invalid/code", CodeEmbedAPIKey: "stored-code", CodeEmbedModel: "stored-code-model",
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			before, err := store.Get(ctx)
			if err != nil {
				t.Fatal(err)
			}
			m := &Manager{settings: store}
			patch := settings.Settings{EmbedBaseURL: server.URL + "/docs", EmbedModel: "docs-preview"}
			if tc.dedicated {
				patch.CodeEmbedBaseURL = server.URL + "/code"
				patch.CodeEmbedModel = "code-preview"
			}
			if tc.code {
				patch.CodeEmbedAPIKey = tc.patchKey
			} else {
				patch.EmbedAPIKey = tc.patchKey
			}

			probe := m.TestEmbedder
			if tc.code {
				probe = m.TestCodeEmbedder
			}
			result := probe(ctx, patch)
			if !result.OK || result.Error != "" || result.Model != tc.model || result.Dim != 3 {
				t.Fatalf("connection result = %+v", result)
			}
			select {
			case got := <-requests:
				if got.path != tc.path || got.model != tc.model || got.auth != "Bearer "+tc.key {
					t.Fatalf("request = %+v", got)
				}
			default:
				t.Fatal("connection test sent no request")
			}
			after, err := store.Get(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("connection test persisted preview settings")
			}
		})
	}
}

func TestEmbedderConnectionsReportFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid credentials"}}`))
	}))
	defer server.Close()
	m := &Manager{}
	for name, probe := range map[string]func(context.Context, settings.Settings) TestResult{
		"docs": m.TestEmbedder,
		"code": m.TestCodeEmbedder,
	} {
		t.Run(name, func(t *testing.T) {
			missing := probe(context.Background(), settings.Settings{})
			if missing.OK || missing.Error != embedder.ErrNotConfigured.Error() {
				t.Fatalf("unconfigured result = %+v", missing)
			}
			failed := probe(context.Background(), settings.Settings{EmbedBaseURL: server.URL, EmbedModel: "preview"})
			if failed.OK || failed.Model != "preview" || failed.Dim != 0 || !strings.Contains(failed.Error, "401") {
				t.Fatalf("rejected credentials result = %+v", failed)
			}
		})
	}
}
