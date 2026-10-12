package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rytsh/krabby/internal/service/memory"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/storage"
)

func newMemoryTestManager(t *testing.T) (*Manager, *registry.Repo, string) {
	t.Helper()
	dataDir := t.TempDir()
	clone := filepath.Join(dataDir, "clone")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	snapshotTestWrite(t, filepath.Join(clone, "queue", "retry.go"), "package queue\n\nfunc Retry() int {\n\treturn 3\n}\n")
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
	return &Manager{reg: reg, memories: store}, repo, clone
}

func TestMemoryEvidenceLifecycle(t *testing.T) {
	m, repo, clone := newMemoryTestManager(t)
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
	snapshotTestWrite(t, filepath.Join(clone, "queue", "retry.go"), "package queue\n\n// Retry caps attempts.\nfunc Retry() int {\n\treturn 3\n}\n")
	if err := m.CheckRepoMemories(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Memory(ctx, id)
	if got.Status != memory.StatusActive || got.Evidence[0].StartLine != 4 || got.History[len(got.History)-1].Action != memory.ActionMoved {
		t.Fatalf("moved: %+v", got)
	}

	snapshotTestWrite(t, filepath.Join(clone, "queue", "retry.go"), "package queue\n\n// Retry caps attempts.\nfunc Retry() int {\n\treturn 5\n}\n")
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
	summary, err := m.RepoMemorySummary(ctx, repo.ID)
	if err != nil || summary.Total != 1 || summary.Orphaned != 1 {
		t.Fatalf("summary: %+v %v", summary, err)
	}

	recall, err := m.RecallMemories(ctx, "retries capped", "semantic", memory.ListOptions{}, 5)
	if err != nil || recall.Mode != MemorySearchLexical || recall.Note == "" || len(recall.Results) != 1 {
		t.Fatalf("recall fallback: %+v %v", recall, err)
	}
	if _, err := m.RecallMemories(ctx, "x", "fuzzy", memory.ListOptions{}, 5); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("bad mode: %v", err)
	}
}

func TestOrphanRepoMemories(t *testing.T) {
	m, repo, _ := newMemoryTestManager(t)
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
