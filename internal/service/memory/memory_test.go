package memory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/storage"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustNormalize(t *testing.T, in Input) Input {
	t.Helper()
	out, err := in.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNormalize(t *testing.T) {
	in := mustNormalize(t, Input{Space: " Agent:Claude ", Title: " Use bw ", Content: "Body", Tags: []string{"DB", "db", " "}})
	if in.Space != "agent:claude" || in.Kind != KindNote || in.Title != "Use bw" || len(in.Tags) != 1 || in.Tags[0] != "db" {
		t.Fatalf("normalized: %+v", in)
	}
	if in, _ := (Input{Title: "t", Content: "c"}).Normalize(); in.Space != DefaultSpace {
		t.Fatalf("default space = %q", in.Space)
	}
	for name, bad := range map[string]Input{
		"reserved space": {Space: "*", Title: "t", Content: "c"},
		"bad space":      {Space: "Has Space", Title: "t", Content: "c"},
		"kind":           {Kind: "rumour", Title: "t", Content: "c"},
		"no title":       {Content: "c"},
		"multiline":      {Title: "a\nb", Content: "c"},
		"author":         {Title: "t", Content: "c", Author: "a\nb"},
		"no content":     {Title: "t"},
		"evidence":       {Title: "t", Content: "c", Evidence: []Evidence{{Repo: "r"}}},
		"lines":          {Title: "t", Content: "c", Evidence: []Evidence{{Repo: "r", Path: "a", StartLine: 5, EndLine: 2}}},
	} {
		if _, err := bad.Normalize(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := (Input{Title: "creds", Content: "api_key = 0123456789abcdef"}).Normalize(); !errors.Is(err, ErrSecret) {
		t.Fatalf("secret accepted: %v", err)
	}
	if _, err := (Input{Title: "ok", Content: "Set `api_key: ${API_KEY}` from the vault."}).Normalize(); err != nil {
		t.Fatalf("placeholder rejected: %v", err)
	}
}

func TestStoreLifecycle(t *testing.T) {
	s, ctx := newTestStore(t), context.Background()
	in := mustNormalize(t, Input{Space: "team", Kind: KindDecision, Author: " Eray ", Title: "Queue retries", Content: "Retries use **exponential** backoff.", Tags: []string{"queue"},
		Evidence: []Evidence{{Repo: "example.com/a/svc", Path: "queue/retry.go", StartLine: 3, EndLine: 4}}})
	created, err := s.Create(ctx, in, []float32{1, 0}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 || created.Status != StatusActive || len(created.History) != 1 || created.Author != "Eray" || created.History[0].Actor != "Eray" {
		t.Fatalf("created: %+v", created)
	}
	if dup, err := s.FindDuplicate(ctx, "team", ContentHash(" queue  retries ", "retries use **EXPONENTIAL** backoff.")); err != nil || dup == nil || dup.ID != created.ID {
		t.Fatalf("duplicate not found: %v %v", dup, err)
	}
	if dup, _ := s.FindDuplicate(ctx, "other", ContentHash(in.Title, in.Content)); dup != nil {
		t.Fatal("duplicate matched across spaces")
	}

	if _, err := s.Update(ctx, created.ID, 7, func(*Memory, *[]float32, *string) (bool, error) { return true, nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	updated, err := s.Update(ctx, created.ID, 1, func(m *Memory, _ *[]float32, _ *string) (bool, error) {
		m.Content = "Retries use linear backoff."
		AddEvent(m, Event{At: time.Now(), Action: ActionUpdated, Actor: "agent-2", Note: "switched"})
		return true, nil
	})
	if err != nil || updated.Version != 2 || len(updated.History) != 2 {
		t.Fatalf("update: %+v %v", updated, err)
	}

	other := mustNormalize(t, Input{Space: "agent:bot", Kind: KindGotcha, Title: "Flaky test", Content: "TestX races.", Tags: []string{"tests"}})
	if _, err := s.Create(ctx, other, nil, ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		opts ListOptions
		want int
	}{
		{ListOptions{}, 2},
		{ListOptions{Spaces: []string{"team"}}, 1},
		{ListOptions{Spaces: []string{"team", "agent:bot"}}, 2},
		{ListOptions{Kind: KindGotcha}, 1},
		{ListOptions{Repo: "example.com/a/svc"}, 1},
		{ListOptions{Tag: "tests"}, 1},
		{ListOptions{Query: "linear"}, 1},
		{ListOptions{Query: "retry.go"}, 1},
	} {
		page, err := s.List(ctx, tc.opts)
		if err != nil || page.Total != tc.want {
			t.Errorf("list %+v: total %d, want %d (%v)", tc.opts, page.Total, tc.want, err)
		}
	}
	hits, err := s.SearchLexical(ctx, "backoff", ListOptions{}, 10)
	if err != nil || len(hits) != 1 || hits[0].ID != created.ID {
		t.Fatalf("lexical: %+v %v", hits, err)
	}
	spaces, err := s.Spaces(ctx)
	if err != nil || len(spaces) != 2 || spaces[0].Space != "agent:bot" {
		t.Fatalf("spaces: %+v %v", spaces, err)
	}

	if err := s.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted memory readable: %v", err)
	}
	if _, err := s.Get(ctx, "../etc"); !errors.Is(err, ErrNotFound) {
		t.Fatal("invalid id accepted")
	}
}

func TestExpiredHiddenByDefault(t *testing.T) {
	s, ctx := newTestStore(t), context.Background()
	in := mustNormalize(t, Input{Title: "Freeze", Content: "Release freeze this week.", ExpiresAt: time.Now().Add(-time.Hour)})
	if _, err := s.Create(ctx, in, nil, ""); err != nil {
		t.Fatal(err)
	}
	if page, _ := s.List(ctx, ListOptions{}); page.Total != 0 {
		t.Fatal("expired memory listed by default")
	}
	page, _ := s.List(ctx, ListOptions{Status: StatusExpired})
	if page.Total != 1 || page.Items[0].Status != StatusExpired {
		t.Fatalf("expired filter: %+v", page)
	}
}

func TestEvidenceCheck(t *testing.T) {
	src := "package a\n\nfunc A() {\n\treturn\n}\n"
	pinned, ok := Pin(Evidence{Repo: "r", Path: "a.go", StartLine: 3, EndLine: 5}, src)
	if !ok || pinned.Hash == "" {
		t.Fatal("pin failed")
	}
	if _, ok := Pin(Evidence{Repo: "r", Path: "a.go", StartLine: 9, EndLine: 9}, src); ok {
		t.Fatal("out of range pin accepted")
	}
	if got := Check(pinned, src+"  \n", true); got.Status != EvidenceOK {
		t.Fatalf("unchanged: %s", got.Status)
	}
	if got := Check(pinned, "package a\n\n// doc\n// more\nfunc A() {  \n\treturn\n}\n", true); got.Status != EvidenceMoved || got.StartLine != 5 || got.EndLine != 7 {
		t.Fatalf("moved: %+v", got)
	}
	if got := Check(pinned, "package a\n\nfunc A() {\n\treturn 1\n}\n", true); got.Status != EvidenceChanged {
		t.Fatalf("changed: %s", got.Status)
	}
	if got := Check(pinned, "", false); got.Status != EvidenceMissing {
		t.Fatalf("missing: %s", got.Status)
	}
	file, _ := Pin(Evidence{Repo: "r", Path: "a.go"}, src)
	if got := Check(file, "anything", true); got.Status != EvidenceOK {
		t.Fatalf("file pin: %s", got.Status)
	}
	if StatusFromEvidence([]Evidence{{Status: EvidenceOK}, {Status: EvidenceChanged}}) != StatusStale ||
		StatusFromEvidence([]Evidence{{Status: EvidenceChanged}, {Status: EvidenceMissing}}) != StatusOrphaned {
		t.Fatal("status derivation")
	}
}

func TestMarkdown(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	m := &Memory{ID: "ABC", Space: "team", Kind: KindDecision, Status: StatusStale, Title: "Use bw", Content: "We store state in **bw**.",
		Tags: []string{"db"}, Author: "a|b", Version: 2, CreatedAt: now, UpdatedAt: now,
		Evidence: []Evidence{{Repo: "r", Path: "x.go", StartLine: 1, EndLine: 2, Commit: "0123456789abcdef", Status: EvidenceChanged}},
		History:  []Event{{At: now, Action: ActionCreated, Actor: "agent"}, {At: now, Action: ActionStale, Actor: "krabby", Note: "pinned code changed"}}}
	md := Markdown(m)
	for _, want := range []string{"# Use bw", "| Space | `team` |", `a\|b`, "We store state in **bw**.", "## Evidence", "`r` — `x.go` L1-2 @ `0123456789ab` — changed", "## History", "2026-10-12 09:30 UTC — **stale** by krabby: pinned code changed"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	all := MarkdownSpace("Space team", []*Memory{m})
	if !strings.Contains(all, "## Use bw") || !strings.Contains(all, "### History") {
		t.Errorf("space export headings:\n%s", all)
	}
}
