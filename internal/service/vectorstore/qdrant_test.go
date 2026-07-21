package vectorstore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/config"
)

func TestQdrantVectorSize(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"result":{"config":{"params":{"vectors":{"size":1024,"distance":"Cosine"}}}}}`)
	if got := qdrantVectorSize(raw); got != 1024 {
		t.Fatalf("got %d, want 1024", got)
	}

	if got := qdrantVectorSize([]byte(`{"result":{}}`)); got != 0 {
		t.Fatalf("unknown response: got %d, want 0", got)
	}
}

func TestQdrantDimensionMismatchIsNonDestructive(t *testing.T) {
	t.Parallel()

	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"config": map[string]any{
					"params": map[string]any{"vectors": map[string]any{"size": 768}},
				},
			},
		})
	}))
	defer server.Close()

	_, err := newQdrant(config.Qdrant{URL: server.URL, Collection: "shared"}, 1024)
	if err == nil || !strings.Contains(err.Error(), "dimension 768") {
		t.Fatalf("got error %v, want dimension mismatch", err)
	}

	for _, method := range methods {
		if method == http.MethodDelete || method == http.MethodPut {
			t.Fatalf("dimension mismatch issued destructive method %s", method)
		}
	}
}
