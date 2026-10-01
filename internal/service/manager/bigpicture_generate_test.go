package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/llm"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/storage"
)

type pictureCompletionFunc func(context.Context, []llm.Message) (string, error)

func (f pictureCompletionFunc) Complete(ctx context.Context, messages []llm.Message) (string, error) {
	return f(ctx, messages)
}

func TestPictureGenerationPublishesAndRejectsStaleWork(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := bigpicture.New(db, filepath.Join(dir, "pictures"))
	if err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(dir, "sources")
	if err := os.MkdirAll(filepath.Join(sources, "runbooks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sources, "runbooks", "deployment.md"), []byte("Checkout publishes order-created; Payment consumes it."), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := bigpicture.Config{Name: "commerce", Title: "Commerce", Prompt: "Explain event flows", Sources: []bigpicture.Source{{Kind: "web", Ref: "runbooks"}}}
	p, err := s.Save(ctx, "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	modelText := `{"overview":"overview.md","documents":[{"path":"overview.md","title":"Overview","markdown":"# Commerce\n[Coverage](research.md)","evidence_ids":["e1"]}]}`
	web, err := websource.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := web.UpsertCollection(ctx, &websource.Collection{Name: "runbooks", Type: "pages"}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{bigPictures: s, sourcesRootDir: sources, webStore: web}
	m.docs = &docsBundle{pictureChat: pictureCompletionFunc(func(_ context.Context, messages []llm.Message) (string, error) {
		if !strings.Contains(messages[1].Content, "order-created") {
			t.Fatal("source evidence missing")
		}
		return modelText, nil
	})}
	spec := queue.Spec{Kind: pictureTaskKind, ID: "bigpicture:commerce", Params: map[string]string{"instance": p.StorageID, "version": strconv.FormatUint(p.Version, 10), "revision": ""}}
	if err := m.pictureGenerateTask(spec).Run(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(ctx, p.Name)
	if err != nil || current.CurrentRevision == "" {
		t.Fatal("generation did not publish")
	}
	if err := m.pictureGenerateTask(spec).Run(ctx); !errors.Is(err, bigpicture.ErrConflict) {
		t.Fatal("stale job ran")
	}
	spec.Params["revision"] = current.CurrentRevision
	if err := os.WriteFile(filepath.Join(sources, "runbooks", "deployment.md"), []byte("New event: payment-created"), 0600); err != nil {
		t.Fatal(err)
	}
	m.docs.pictureChat = pictureCompletionFunc(func(context.Context, []llm.Message) (string, error) { return "bad JSON", nil })
	if err := m.pictureGenerateTask(spec).Run(ctx); err == nil {
		t.Fatal("invalid output succeeded")
	}
	after, _ := s.Get(ctx, p.Name)
	if after.CurrentRevision != current.CurrentRevision {
		t.Fatal("failed generation changed current publication")
	}
	m.docs.pictureChat = pictureCompletionFunc(func(context.Context, []llm.Message) (string, error) {
		cfg.ExpectedVersion = 1
		cfg.Prompt = "New research scope"
		if _, err := s.Save(ctx, p.Name, cfg); err != nil {
			t.Fatal(err)
		}
		return `{"overview":"overview.md","change_summary":"Scope changed","upserts":[],"delete_paths":[]}`, nil
	})
	if err := m.pictureGenerateTask(spec).Run(ctx); !errors.Is(err, bigpicture.ErrConflict) {
		t.Fatal("config change during generation was overwritten")
	}
	after, _ = s.Get(ctx, p.Name)
	if after.CurrentRevision != current.CurrentRevision {
		t.Fatal("conflicted generation changed publication")
	}
	if task, ok := m.rebuildTask(spec); !ok || task.Spec.Kind != pictureTaskKind {
		t.Fatal("generation task cannot restore")
	}
	m.queue = queue.New(ctx, 1)
	m.docs.pictureChat = pictureCompletionFunc(func(context.Context, []llm.Message) (string, error) { return modelText, nil })
	t.Cleanup(m.queue.Close)
	if err := m.TriggerBigPictureGeneration(ctx, p.Name); err != nil {
		t.Fatal(err)
	}
}

func TestPictureRunsRecordOutcomesWithoutNoOpRevisions(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := bigpicture.New(db, filepath.Join(t.TempDir(), "pictures"))
	if err != nil {
		t.Fatal(err)
	}
	sources := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sources, "runbooks"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(sources, "runbooks", "deployment.md"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("Checkout publishes order-created.")
	web, err := websource.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := web.UpsertCollection(ctx, &websource.Collection{Name: "runbooks", Type: "pages"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Save(ctx, "", bigpicture.Config{Name: "commerce", Title: "Commerce", Prompt: "Explain event flows", Sources: []bigpicture.Source{{Kind: "web", Ref: "runbooks"}}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	reply := `{"overview":"overview.md","documents":[{"path":"overview.md","title":"Overview","markdown":"# Commerce","evidence_ids":["e1"]}]}`
	m := &Manager{bigPictures: s, sourcesRootDir: sources, webStore: web}
	m.docs = &docsBundle{pictureChat: pictureCompletionFunc(func(context.Context, []llm.Message) (string, error) {
		calls++
		return reply, nil
	})}
	run := func() error {
		t.Helper()
		current, err := s.Get(ctx, p.Name)
		if err != nil {
			t.Fatal(err)
		}
		spec := queue.Spec{Kind: pictureTaskKind, ID: "bigpicture:commerce", Params: map[string]string{"instance": current.StorageID, "version": strconv.FormatUint(current.Version, 10), "revision": current.CurrentRevision, "trigger": "schedule"}}
		return m.pictureGenerateTask(spec).Run(ctx)
	}
	state := func() *bigpicture.Picture {
		t.Helper()
		current, err := s.Get(ctx, p.Name)
		if err != nil {
			t.Fatal(err)
		}
		return current
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if got := state(); got.LastRun == nil || got.LastRun.Status != bigpicture.RunPublished || got.LastRun.Trigger != "schedule" || len(got.Revisions) != 1 {
		t.Fatalf("initial run: %+v", got)
	}

	// Unchanged evidence skips the model entirely.
	if err := run(); err != nil || calls != 1 || state().LastRun.Status != bigpicture.RunUnchanged || len(state().Revisions) != 1 {
		t.Fatalf("unchanged: err=%v calls=%d run=%+v", err, calls, state().LastRun)
	}

	// Changed evidence with an empty patch records a check, not a revision,
	// and the next identical run skips the model again.
	write("Checkout publishes order-created; formatting changed.")
	reply = `{"overview":"overview.md","change_summary":"Formatting only","upserts":[],"delete_paths":[]}`
	if err := run(); err != nil || calls != 2 || state().LastRun.Status != bigpicture.RunNoChanges || len(state().Revisions) != 1 {
		t.Fatalf("no changes: err=%v calls=%d run=%+v", err, calls, state().LastRun)
	}
	if err := run(); err != nil || calls != 2 || state().LastRun.Status != bigpicture.RunUnchanged {
		t.Fatalf("repeat no-op: err=%v calls=%d run=%+v", err, calls, state().LastRun)
	}

	// Failures are visible on the workspace and do not change the publication.
	write("Payment now consumes order-created.")
	reply = "not JSON"
	before := state().CurrentRevision
	if err := run(); err == nil {
		t.Fatal("invalid model output succeeded")
	}
	if got := state(); got.LastRun.Status != bigpicture.RunFailed || got.LastRun.Message == "" || got.CurrentRevision != before {
		t.Fatalf("failure not recorded: %+v", got.LastRun)
	}
	// A failed run never suppresses the retry.
	reply = `{"overview":"overview.md","change_summary":"Payment consumer","upserts":[{"path":"overview.md","title":"Overview","markdown":"# Payment consumes order-created","evidence_ids":["e1"]}],"delete_paths":[]}`
	if err := run(); err != nil || state().LastRun.Status != bigpicture.RunPublished || len(state().Revisions) != 2 || state().LastRun.Message != "Payment consumer" {
		t.Fatalf("retry after failure: err=%v run=%+v", err, state().LastRun)
	}
}

func TestPictureFileRankingExcludesCommonSecrets(t *testing.T) {
	for _, file := range []string{".env", "deploy/secrets.yaml", "config/credentials.yml", "tls/cert.pem", "private.key"} {
		if pictureFileRank(file, "repo") != 0 {
			t.Errorf("secret selected: %s", file)
		}
	}
	if pictureFileRank("deploy/payment.yaml", "repo") <= pictureFileRank("src/helper.go", "repo") {
		t.Fatal("deployment files not prioritized")
	}
}
