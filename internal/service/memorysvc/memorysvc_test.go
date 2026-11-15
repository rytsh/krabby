package memorysvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/memory"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/storage"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// testDeps wires memory to a real registry and to files read straight from
// each repository's recorded clone path, which is all evidence pinning needs.
// There is no graph and no embedder, so recall is lexical.
func testDeps(reg *registry.Registry) Deps {
	return Deps{
		ResolveRepo: reg.Resolve,
		Repo:        reg.Get,
		ReadRepoFile: func(ctx context.Context, repoID, path string, offset int64, maxBytes int) (*repofs.FileContent, error) {
			repo, err := reg.Get(ctx, repoID)
			if err != nil || repo == nil {
				return nil, fmt.Errorf("repo %s not tracked; %v", repoID, err)
			}

			return repofs.ReadFile(repo.Path, path, offset, maxBytes)
		},
		FindDefinition: func(context.Context, string, string, string, int) (graphquery.SymbolDefs, error) {
			return graphquery.SymbolDefs{}, nil
		},
		Embed: func(context.Context, string, bool) ([]float32, string, error) { return nil, "", nil },
		Go: func(fn func(context.Context)) bool {
			go fn(context.Background())

			return true
		},
	}
}

func newMemoryTestService(t *testing.T) (*Service, *registry.Repo, string) {
	t.Helper()
	dataDir := t.TempDir()
	clone := filepath.Join(dataDir, "clone")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(clone, "queue", "retry.go"), "package queue\n\nfunc Retry() int {\n\treturn 3\n}\n")
	db, err := storage.Open(filepath.Join(dataDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}
	repo := &registry.Repo{ID: "example.com/team/svc", URL: "https://example.com/team/svc.git", Path: clone, Status: registry.StatusReady, LastCommit: "c1"}
	if err := reg.Upsert(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	store, err := memory.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return New(store, testDeps(reg)), repo, clone
}

func TestMemoryEvidenceLifecycle(t *testing.T) {
	m, repo, clone := newMemoryTestService(t)
	ctx := context.Background()
	in := memory.Input{Space: "team", Kind: memory.KindDecision, Title: "Retry count", Content: "Retries are capped at **3**.",
		Evidence: []memory.Evidence{{Repo: "svc", Path: "queue/retry.go", StartLine: 3, EndLine: 5}}}
	res, err := m.Remember(ctx, in)
	if err != nil || !res.Created {
		t.Fatalf("remember: %+v %v", res, err)
	}
	id := res.Memory.ID
	if !res.NewSpace || len(res.Spaces) != 0 || res.Note != "" {
		t.Fatalf("first space: %+v", res)
	}
	second, err := m.Remember(ctx, memory.Input{Space: "teams", Title: "Other", Content: "Something else."})
	if err != nil || !second.NewSpace || len(second.Spaces) != 1 || second.Spaces[0] != "team" || second.Note == "" {
		t.Fatalf("new space hint: %+v %v", second, err)
	}
	if third, _ := m.Remember(ctx, memory.Input{Space: "team", Title: "Third", Content: "Yet another."}); third.NewSpace {
		t.Fatal("existing space reported as new")
	}
	if ev := res.Memory.Evidence[0]; ev.Repo != repo.ID || ev.Commit != "c1" || ev.Status != memory.EvidenceOK {
		t.Fatalf("evidence not resolved: %+v", ev)
	}
	if again, err := m.Remember(ctx, in); err != nil || again.Created || !again.Duplicate || again.Memory.ID != id {
		t.Fatalf("duplicate stored: %+v %v", again, err)
	}
	bad := in
	bad.Title, bad.Evidence = "Other", []memory.Evidence{{Repo: repo.ID, Path: "queue/retry.go", StartLine: 40}}
	if _, err := m.Remember(ctx, bad); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("out-of-range evidence: %v", err)
	}

	// A comment above the function moves the pinned lines.
	writeFile(t, filepath.Join(clone, "queue", "retry.go"), "package queue\n\n// Retry caps attempts.\nfunc Retry() int {\n\treturn 3\n}\n")
	if err := m.CheckRepoMemories(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Memory(ctx, id)
	if got.Status != memory.StatusActive || got.Evidence[0].StartLine != 4 || got.History[len(got.History)-1].Action != memory.ActionMoved {
		t.Fatalf("moved: %+v", got)
	}

	writeFile(t, filepath.Join(clone, "queue", "retry.go"), "package queue\n\n// Retry caps attempts.\nfunc Retry() int {\n\treturn 5\n}\n")
	if err := m.CheckRepoMemories(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = m.Memory(ctx, id)
	if got.Status != memory.StatusStale || got.Evidence[0].Status != memory.EvidenceChanged || got.History[len(got.History)-1].Action != memory.ActionStale {
		t.Fatalf("stale: %+v", got)
	}
	page, _ := m.ListMemories(ctx, memory.ListOptions{Status: memory.StatusStale, Repo: "svc"})
	if page.Total != 1 {
		t.Fatalf("stale filter by repo suffix: %d", page.Total)
	}

	// Reviewing the memory re-pins it to today's code.
	content := "Retries are capped at **5**."
	updated, err := m.UpdateMemory(ctx, id, memory.Patch{ExpectedVersion: got.Version, Content: &content, Note: "bumped", Author: "payments-team"})
	if err != nil || updated.Status != memory.StatusActive || updated.Evidence[0].Status != memory.EvidenceOK ||
		updated.Author != "" || updated.History[len(updated.History)-1].Actor != "payments-team" {
		t.Fatalf("review: %+v %v", updated, err)
	}
	if _, err := m.UpdateMemory(ctx, id, memory.Patch{ExpectedVersion: got.Version, Content: &content}); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("stale version accepted: %v", err)
	}

	if err := os.Remove(filepath.Join(clone, "queue", "retry.go")); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRepoMemories(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = m.Memory(ctx, id); got.Status != memory.StatusOrphaned {
		t.Fatalf("orphaned: %s", got.Status)
	}
	summary, err := m.Summarize(ctx, repo.ID)
	if err != nil || summary.Total != 1 || summary.Orphaned != 1 {
		t.Fatalf("summary: %+v %v", summary, err)
	}

	recall, err := m.RecallMemories(ctx, "retries capped", "semantic", memory.ListOptions{}, 5)
	if err != nil || recall.Mode != SearchLexical || recall.Note == "" || len(recall.Results) != 1 {
		t.Fatalf("recall fallback: %+v %v", recall, err)
	}
	if _, err := m.RecallMemories(ctx, "x", "fuzzy", memory.ListOptions{}, 5); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("bad mode: %v", err)
	}
}

func TestOrphanRepoMemories(t *testing.T) {
	m, repo, _ := newMemoryTestService(t)
	ctx := context.Background()
	res, err := m.Remember(ctx, memory.Input{Title: "File", Content: "Retry lives here.", Evidence: []memory.Evidence{{Repo: repo.ID, Path: "queue/retry.go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.OrphanRepoMemories(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Memory(ctx, res.Memory.ID)
	if got.Status != memory.StatusOrphaned || got.History[len(got.History)-1].Actor != "krabby" {
		t.Fatalf("orphan: %+v", got)
	}
}

func TestCosine(t *testing.T) {
	if got := cosine([]float32{1, 0}, []float32{1, 0}); got < 0.999 {
		t.Fatalf("identical = %f", got)
	}
	if got := cosine([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Fatalf("orthogonal = %f", got)
	}
	if got := cosine([]float32{1}, []float32{1, 0}); got != 0 {
		t.Fatalf("mismatched = %f", got)
	}
}

// A manager that never configured memory holds a nil service. Requests are
// refused plainly, and the side effects of repository work do nothing.
func TestNilServiceIsUnavailable(t *testing.T) {
	var s *Service
	ctx := context.Background()

	if _, err := s.Remember(ctx, memory.Input{Title: "x", Content: "y"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Remember: %v", err)
	}
	if _, err := s.RecallMemories(ctx, "x", "", memory.ListOptions{}, 5); !errors.Is(err, ErrUnavailable) {
		t.Errorf("RecallMemories: %v", err)
	}
	if _, err := s.Memory(ctx, "id"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Memory: %v", err)
	}
	if err := s.ForgetMemory(ctx, "id"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ForgetMemory: %v", err)
	}

	if err := s.CheckRepoMemories(ctx, "repo"); err != nil {
		t.Errorf("CheckRepoMemories: %v", err)
	}
	if err := s.OrphanRepoMemories(ctx, "repo"); err != nil {
		t.Errorf("OrphanRepoMemories: %v", err)
	}
	if err := s.BackfillMemoryEmbeddings(ctx); err != nil {
		t.Errorf("BackfillMemoryEmbeddings: %v", err)
	}
	if sum, err := s.Summarize(ctx, "repo"); err != nil || sum.Total != 0 {
		t.Errorf("Summarize = %+v, %v", sum, err)
	}
}

// An embedder that fails must degrade recall to lexical, not fail it.
func TestRecallSurvivesAFailingEmbedder(t *testing.T) {
	m, _, _ := newMemoryTestService(t)
	m.deps.Embed = func(context.Context, string, bool) ([]float32, string, error) {
		return nil, "", errors.New("embedder unreachable")
	}
	ctx := context.Background()

	if _, err := m.Remember(ctx, memory.Input{Title: "Retry count", Content: "Retries are capped at 3."}); err != nil {
		t.Fatal(err)
	}

	got, err := m.RecallMemories(ctx, "retries capped", "", memory.ListOptions{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != SearchLexical || len(got.Results) != 1 {
		t.Fatalf("recall = %+v, want a lexical hit", got)
	}
}

// With an embedder, recall is hybrid by default and a near-restatement in the
// same space is returned instead of stored twice.
func TestSemanticDuplicateAndHybridRecall(t *testing.T) {
	m, _, _ := newMemoryTestService(t)
	m.deps.Embed = func(_ context.Context, text string, _ bool) ([]float32, string, error) {
		// Every text about retries points the same way.
		if strings.Contains(strings.ToLower(text), "retr") {
			return []float32{1, 0}, "test-model", nil
		}

		return []float32{0, 1}, "test-model", nil
	}
	ctx := context.Background()

	first, err := m.Remember(ctx, memory.Input{Title: "Retry count", Content: "Retries are capped at 3."})
	if err != nil || !first.Created {
		t.Fatalf("first: %+v %v", first, err)
	}

	again, err := m.Remember(ctx, memory.Input{Title: "Retry limit", Content: "We retry at most three times."})
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || !again.Duplicate || again.Memory.ID != first.Memory.ID || again.Score < memoryDuplicateScore {
		t.Fatalf("near-duplicate was stored: %+v", again)
	}

	if other, err := m.Remember(ctx, memory.Input{Space: "elsewhere", Title: "Retry limit", Content: "We retry at most three times."}); err != nil || !other.Created {
		t.Fatalf("the same note in another space must be stored: %+v %v", other, err)
	}

	got, err := m.RecallMemories(ctx, "how many retries", "", memory.ListOptions{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != SearchHybrid || len(got.Results) == 0 {
		t.Fatalf("recall = %+v, want hybrid results", got)
	}
}

// The background backfill reports refusal, and a refused start must not leave
// the guard set, or no backfill could ever run again.
func TestRefusedBackfillCanBeRetried(t *testing.T) {
	m, _, _ := newMemoryTestService(t)

	started := 0
	m.deps.Go = func(fn func(context.Context)) bool {
		started++

		return false
	}

	m.queueMemoryEmbeddings()
	m.queueMemoryEmbeddings()

	if started != 2 {
		t.Fatalf("second backfill was not attempted after the first was refused (%d attempts)", started)
	}
}
