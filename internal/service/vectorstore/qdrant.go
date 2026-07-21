package vectorstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rytsh/krabby/internal/config"
)

// qdrant is an HTTP-backed vector store for the Qdrant engine. Opt-in via
// rag.store.kind = "qdrant" for larger corpora / shared deployments.
//
// Qdrant point IDs must be UUIDs or integers, so the stable Item.ID is hashed
// into a deterministic UUID (same item -> same point -> upsert overwrites) and
// the original ID is kept in the payload.
type qdrant struct {
	url        string
	apiKey     string
	collection string
	dim        int
	http       *http.Client

	ensureMu sync.Mutex
	ensured  bool
}

// upsertBatch bounds how many points are sent per upsert request.
const upsertBatch = 256

func newQdrant(cfg config.Qdrant, dim int) (*qdrant, error) {
	if cfg.URL == "" {
		return nil, errors.New("qdrant url is required")
	}

	collection := cfg.Collection
	if collection == "" {
		collection = "krabby"
	}

	q := &qdrant{
		url:        strings.TrimRight(cfg.URL, "/"),
		apiKey:     cfg.APIKey,
		collection: collection,
		dim:        dim,
		http:       &http.Client{Timeout: 30 * time.Second},
	}

	// The embedding dimension may still be unknown here (embedder infers it
	// from the first response). Collection creation is deferred to the first
	// Upsert, where the vector length is authoritative.
	if dim > 0 {
		if err := q.ensureCollection(context.Background(), dim); err != nil {
			return nil, err
		}
	}

	return q, nil
}

// ensureCollection creates the collection with cosine distance when missing.
func (q *qdrant) ensureCollection(ctx context.Context, dim int) error {
	q.ensureMu.Lock()
	defer q.ensureMu.Unlock()

	if q.ensured {
		return nil
	}

	status, _, err := q.do(ctx, http.MethodGet, "/collections/"+q.collection, nil)
	if err != nil {
		return fmt.Errorf("qdrant get collection; %w", err)
	}

	if status == http.StatusOK {
		q.ensured = true

		return nil
	}

	if status != http.StatusNotFound {
		return fmt.Errorf("qdrant get collection: http %d", status)
	}

	body := map[string]any{
		"vectors": map[string]any{
			"size":     dim,
			"distance": "Cosine",
		},
	}

	status, raw, err := q.do(ctx, http.MethodPut, "/collections/"+q.collection, body)
	if err != nil {
		return fmt.Errorf("qdrant create collection; %w", err)
	}

	if status < 200 || status >= 300 {
		return fmt.Errorf("qdrant create collection: http %d; %s", status, trimBody(raw))
	}

	q.dim = dim
	q.ensured = true

	return nil
}

type qdrantPoint struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

func (q *qdrant) Upsert(ctx context.Context, items []Item) error {
	if len(items) == 0 {
		return nil
	}

	if err := q.ensureCollection(ctx, len(items[0].Vector)); err != nil {
		return err
	}

	for start := 0; start < len(items); start += upsertBatch {
		end := min(start+upsertBatch, len(items))

		points := make([]qdrantPoint, 0, end-start)
		for _, it := range items[start:end] {
			points = append(points, qdrantPoint{
				ID:     pointID(it.ID),
				Vector: it.Vector,
				Payload: map[string]any{
					"item_id":  it.ID,
					"repo":     it.Payload.Repo,
					"doc_path": it.Payload.DocPath,
					"title":    it.Payload.Title,
					"chunk":    it.Payload.Chunk,
				},
			})
		}

		status, raw, err := q.do(ctx, http.MethodPut,
			"/collections/"+q.collection+"/points?wait=true",
			map[string]any{"points": points},
		)
		if err != nil {
			return fmt.Errorf("qdrant upsert; %w", err)
		}

		if status < 200 || status >= 300 {
			return fmt.Errorf("qdrant upsert: http %d; %s", status, trimBody(raw))
		}
	}

	return nil
}

func (q *qdrant) Search(ctx context.Context, repo string, vec []float32, topK int) ([]Match, error) {
	if topK <= 0 {
		return nil, nil
	}

	body := map[string]any{
		"vector":       vec,
		"limit":        topK,
		"with_payload": true,
	}

	if repo != "" {
		body["filter"] = repoFilter(repo)
	}

	status, raw, err := q.do(ctx, http.MethodPost,
		"/collections/"+q.collection+"/points/search", body)
	if err != nil {
		return nil, fmt.Errorf("qdrant search; %w", err)
	}

	if status == http.StatusNotFound {
		return nil, nil // collection not created yet -> nothing indexed
	}

	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("qdrant search: http %d; %s", status, trimBody(raw))
	}

	var out struct {
		Result []struct {
			Score   float32 `json:"score"`
			Payload struct {
				Repo    string `json:"repo"`
				DocPath string `json:"doc_path"`
				Title   string `json:"title"`
				Chunk   string `json:"chunk"`
			} `json:"payload"`
		} `json:"result"`
	}

	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("qdrant search: decode response; %w", err)
	}

	matches := make([]Match, 0, len(out.Result))
	for _, r := range out.Result {
		matches = append(matches, Match{
			Score: r.Score,
			Payload: Payload{
				Repo:    r.Payload.Repo,
				DocPath: r.Payload.DocPath,
				Title:   r.Payload.Title,
				Chunk:   r.Payload.Chunk,
			},
		})
	}

	return matches, nil
}

func (q *qdrant) DeleteRepo(ctx context.Context, repo string) error {
	status, raw, err := q.do(ctx, http.MethodPost,
		"/collections/"+q.collection+"/points/delete?wait=true",
		map[string]any{"filter": repoFilter(repo)},
	)
	if err != nil {
		return fmt.Errorf("qdrant delete repo; %w", err)
	}

	if status == http.StatusNotFound {
		return nil // nothing indexed yet
	}

	if status < 200 || status >= 300 {
		return fmt.Errorf("qdrant delete repo: http %d; %s", status, trimBody(raw))
	}

	return nil
}

func (q *qdrant) Close() error { return nil }

// do sends one JSON request and returns the status code and raw body.
func (q *qdrant) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}

		rd = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, q.url+path, rd)
	if err != nil {
		return 0, nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	if q.apiKey != "" {
		req.Header.Set("api-key", q.apiKey)
	}

	resp, err := q.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	return resp.StatusCode, raw, nil
}

// repoFilter is the Qdrant payload filter matching one repo.
func repoFilter(repo string) map[string]any {
	return map[string]any{
		"must": []map[string]any{
			{"key": "repo", "match": map[string]any{"value": repo}},
		},
	}
}

// pointID derives a deterministic UUID-shaped id from the stable item ID, since
// Qdrant only accepts UUIDs or unsigned integers as point ids.
func pointID(id string) string {
	sum := sha256.Sum256([]byte(id))
	h := hex.EncodeToString(sum[:16])

	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// trimBody compacts an HTTP error body for log/error messages.
func trimBody(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "..."
	}

	return s
}
