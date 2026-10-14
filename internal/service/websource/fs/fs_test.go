package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/websource"
)

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFSIncrementalSync(t *testing.T) {
	root := t.TempDir()
	docs := filepath.Join(root, "docs")
	write(t, filepath.Join(docs, "a.md"), "# A\n\nalpha")
	write(t, filepath.Join(docs, "sub", "b.txt"), "beta")
	write(t, filepath.Join(docs, "bin.exe"), "MZ")
	write(t, filepath.Join(docs, ".hidden", "c.md"), "hidden")

	f := New([]string{root})
	if _, err := f.MergeConfig(nil, json.RawMessage(`{"path":"/etc"}`)); err == nil {
		t.Fatal("path outside roots accepted")
	}
	cfg, err := f.MergeConfig(nil, json.RawMessage(`{"path":"`+docs+`","base_url":"https://files.example.com/docs"}`))
	if err != nil {
		t.Fatal(err)
	}
	run := func(state json.RawMessage) ([]websource.RemotePage, *websource.FetchResult) {
		var pages []websource.RemotePage
		res, err := f.Fetch(context.Background(), &websource.Collection{Config: cfg}, nil, state, func(p websource.RemotePage) error {
			pages = append(pages, p)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return pages, res
	}

	pages, res := run(nil)
	if len(pages) != 2 || !res.Complete {
		t.Fatalf("first run pages=%+v", pages)
	}
	if pages[0].URL != "https://files.example.com/docs/a.md" || !strings.Contains(pages[0].Markdown, "alpha") {
		t.Fatalf("page = %+v", pages[0])
	}

	future := time.Now().Add(time.Hour)
	write(t, filepath.Join(docs, "a.md"), "# A\n\nalpha changed")
	_ = os.Chtimes(filepath.Join(docs, "a.md"), future, future)
	_ = os.Remove(filepath.Join(docs, "sub", "b.txt"))

	pages, res = run(res.State)
	if len(pages) != 1 || !strings.Contains(pages[0].Markdown, "changed") || len(res.Removed) != 1 {
		t.Fatalf("second run pages=%d removed=%v", len(pages), res.Removed)
	}

	pages, res = run(res.State)
	if len(pages) != 0 || res.Complete {
		t.Fatalf("unchanged run pages=%d complete=%v", len(pages), res.Complete)
	}
}
