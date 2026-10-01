package bigpicture

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/llm"
)

func TestIncrementalUpdatePreservesUntouchedPages(t *testing.T) {
	p := &Picture{Version: 2, StorageID: "instance", CurrentRevision: "old", Sources: []Source{{Kind: "repo", Ref: "service"}}, Prompt: "Update architecture"}
	r := Research{Items: []ResearchItem{{ID: "e1", Evidence: Evidence{Source: p.Sources[0], Locator: "deploy.yaml"}, Content: "Kafka consumers changed"}}, Previous: []Document{{Path: "overview.md", Title: "Overview", Markdown: "Truncated context"}}}
	previous := &Snapshot{Revision: Revision{ID: "old", ConfigVersion: 1}, Overview: "overview.md", Documents: []DocMeta{{Path: "overview.md", Title: "Overview"}, {Path: "services/detail.md", Title: "Detail"}, {Path: "obsolete.md", Title: "Obsolete"}}}
	documents := []Document{{Path: "overview.md", Title: "Overview", Markdown: "Old overview"}, {Path: "services/detail.md", Title: "Detail", Markdown: strings.Repeat("Preserved detail 🙂\n", 5000), Evidence: []Evidence{r.Items[0].Evidence}}, {Path: "obsolete.md", Title: "Obsolete", Markdown: "Old flow"}}
	model := completionFunc(func(_ context.Context, messages []llm.Message) (string, error) {
		if !strings.Contains(messages[1].Content, "services/detail.md") {
			t.Fatal("complete structure omitted")
		}
		return `{"overview":"overview.md","change_summary":"Update Kafka consumers; retire obsolete flow","upserts":[{"path":"overview.md","title":"Overview","markdown":"# Kafka consumers\n[Detail](services/detail.md)","evidence_ids":["e1"]}],"delete_paths":["obsolete.md"]}`, nil
	})
	pub, err := Update(context.Background(), model, p, r, previous, documents)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Document{}
	for _, doc := range pub.Documents {
		byPath[doc.Path] = doc
	}
	if byPath["services/detail.md"].Markdown != documents[1].Markdown || len(byPath["services/detail.md"].Evidence) != 1 {
		t.Fatal("untouched full page or citations changed")
	}
	if _, exists := byPath["obsolete.md"]; exists {
		t.Fatal("explicit deletion ignored")
	}
	if pub.ExpectedRevision != "old" || pub.ResearchHash != Fingerprint(p, r) || pub.ChangeSummary == "" {
		t.Fatal("update metadata missing")
	}
}

func TestFingerprintIgnoresSourceRevisionAndUnchangedChecks(t *testing.T) {
	p := &Picture{Version: 1}
	item := ResearchItem{ID: "e1", Evidence: Evidence{Locator: "deploy.yaml", Revision: "commit-a"}, Content: "unchanged"}
	first := Fingerprint(p, Research{Items: []ResearchItem{item}})
	item.Evidence.Revision = "commit-b"
	if Fingerprint(p, Research{Items: []ResearchItem{item}}) != first {
		t.Fatal("unrelated commit changed the research fingerprint")
	}
	item.Content = "changed"
	if Fingerprint(p, Research{Items: []ResearchItem{item}}) == first {
		t.Fatal("content change not detected")
	}
	previous := &Snapshot{Revision: Revision{ID: "rev", ResearchHash: "published"}}
	if !Unchanged(p, previous, "published") || Unchanged(p, previous, first) {
		t.Fatal("publication fingerprint comparison wrong")
	}
	// A no-op check remembers its fingerprint only for the same publication;
	// failures never suppress a retry.
	p.LastRun = &Run{Status: RunNoChanges, Revision: "rev", ResearchHash: first}
	if !Unchanged(p, previous, first) {
		t.Fatal("no-change check not remembered")
	}
	p.LastRun.Revision = "older"
	if Unchanged(p, previous, first) {
		t.Fatal("no-change check from another publication reused")
	}
	p.LastRun = &Run{Status: RunFailed, Revision: "rev", ResearchHash: first}
	if Unchanged(p, previous, first) {
		t.Fatal("failed run suppressed retry")
	}
}

func TestEmptyIncrementalPatchDoesNotPublish(t *testing.T) {
	p := &Picture{Version: 2, CurrentRevision: "old", Sources: []Source{{Kind: "repo", Ref: "service"}}}
	r := Research{Items: []ResearchItem{{ID: "e1", Evidence: Evidence{Source: p.Sources[0], Locator: "README.md"}, Content: "evidence"}}}
	previous := &Snapshot{Overview: "overview.md"}
	docs := []Document{{Path: "overview.md", Title: "Overview", Markdown: "Keep"}}
	_, err := Update(context.Background(), completionFunc(func(context.Context, []llm.Message) (string, error) {
		return `{"overview":"overview.md","change_summary":"Nothing relevant changed","upserts":[],"delete_paths":[]}`, nil
	}), p, r, previous, docs)
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("empty patch err = %v, want ErrNoChanges", err)
	}
}

func TestModelInputDoesNotHTMLEscape(t *testing.T) {
	data, err := marshalInput(map[string]string{"x": "<a & b>"})
	if err != nil || !strings.Contains(string(data), "<a & b>") {
		t.Fatalf("input escaped: %s %v", data, err)
	}
}

func TestIncrementalPatchRejectsUnsafeChanges(t *testing.T) {
	p := &Picture{Version: 1, CurrentRevision: "old", Sources: []Source{{Kind: "repo", Ref: "service"}}}
	r := Research{Items: []ResearchItem{{ID: "e1", Evidence: Evidence{Source: p.Sources[0], Locator: "README.md"}, Content: "evidence"}}}
	previous := &Snapshot{Overview: "overview.md"}
	docs := []Document{{Path: "overview.md", Title: "Overview", Markdown: "Preserve this"}}
	for _, text := range []string{
		`{"overview":"overview.md","change_summary":"Delete missing","upserts":[],"delete_paths":["missing.md"]}`,
		`{"overview":"overview.md","change_summary":"Delete overview","upserts":[],"delete_paths":["overview.md"]}`,
		`{"overview":"overview.md","change_summary":"Bad citation","upserts":[{"path":"overview.md","title":"Overview","markdown":"Changed","evidence_ids":["invented"]}],"delete_paths":[]}`,
		`{"overview":"overview.md","change_summary":"Wrong protocol","documents":[]}`,
	} {
		if _, err := Update(context.Background(), completionFunc(func(context.Context, []llm.Message) (string, error) { return text, nil }), p, r, previous, docs); err == nil {
			t.Fatalf("unsafe patch accepted: %s", text)
		}
		if docs[0].Markdown != "Preserve this" {
			t.Fatal("input tree mutated")
		}
	}
}

func TestScheduleValidation(t *testing.T) {
	cfg := testConfig("scheduled")
	cfg.Schedule = &[]string{" @every 6h", "0 2 * * *"}
	normalized, err := NormalizeConfig(cfg)
	if err != nil || (*normalized.Schedule)[0] != "@every 6h" {
		t.Fatalf("valid cron schedule: %v", err)
	}
	cfg.Schedule = &[]string{"not a cron"}
	if _, err := NormalizeConfig(cfg); err == nil {
		t.Fatal("invalid schedule accepted")
	}
}

func TestOmittedScheduleKeepsSavedSchedule(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cfg := testConfig("scheduled")
	cfg.Schedule = &[]string{"@every 6h"}
	p, err := s.Save(ctx, "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Schedule, cfg.ExpectedVersion, cfg.Title = nil, p.Version, "Renamed"
	if p, err = s.Save(ctx, cfg.Name, cfg); err != nil || len(p.Schedule) != 1 {
		t.Fatalf("partial update dropped schedule: %+v %v", p, err)
	}
	cfg.Schedule, cfg.ExpectedVersion = &[]string{}, p.Version
	if p, err = s.Save(ctx, cfg.Name, cfg); err != nil || len(p.Schedule) != 0 {
		t.Fatalf("explicit empty schedule not applied: %+v %v", p, err)
	}
}
