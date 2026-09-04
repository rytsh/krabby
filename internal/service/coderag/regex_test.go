package coderag

import (
	"context"
	"strings"
	"testing"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

func newRegexStore(t *testing.T) (*TextStore, context.Context) {
	t.Helper()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	return store, context.Background()
}

// managerSource is a chunk whose text looks like the code a regex search is
// actually aimed at: a receiver signature, punctuation and mixed case, none of
// which a term index can express.
const managerSource = "func (m *Manager) SearchCodeText(\n" +
	"\tctx context.Context,\n" +
	") (coderag.SearchPage, error) {\n" +
	"\tif errors.Is(err, ErrCodeRAGDisabled) {\n" +
	"\t\treturn coderag.SearchPage{}, err\n" +
	"\t}\n" +
	"}\n"

// TestSearchRegexLocatesSignature is the contract: an RE2 pattern resolves to
// the file, the line and the column, not to the chunk that contains it.
func TestSearchRegexLocatesSignature(t *testing.T) {
	t.Parallel()

	store, ctx := newRegexStore(t)
	if err := store.ReplaceRepo(ctx, "acme/api", []vectorstore.Item{
		textItem("acme/api", "docs.go", "SearchCodeText", 1996, managerSource),
		textItem("acme/api", "other.go", "Unrelated", 1, "func plain() {}\n"),
	}); err != nil {
		t.Fatal(err)
	}

	page, err := store.SearchRegex(ctx, scopeFilter("acme/api"), `func \(m \*Manager\)`, RegexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Results) != 1 {
		t.Fatalf("total = %d results = %d", page.Total, len(page.Results))
	}
	if !page.Exhaustive {
		t.Error("a two-chunk corpus reported itself as non-exhaustive")
	}

	hit := page.Results[0]
	if hit.Repo != "acme/api" || hit.Path != "docs.go" {
		t.Fatalf("hit = %+v", hit)
	}
	if len(hit.Matches) != 1 {
		t.Fatalf("matches = %+v", hit.Matches)
	}
	// StartLine 1996 is the chunk's first file line and the match is on the
	// chunk's first line, so the file line is 1996 exactly.
	if got := hit.Matches[0]; got.Line != 1996 || got.Column != 1 {
		t.Errorf("location = %d:%d, want 1996:1", got.Line, got.Column)
	}
	if got := hit.Matches[0].Text; got != "func (m *Manager) SearchCodeText(" {
		t.Errorf("text = %q", got)
	}
}

// TestSearchRegexColumnAndContext checks the column is a byte offset into the
// line and that context comes from the surrounding source.
func TestSearchRegexColumnAndContext(t *testing.T) {
	t.Parallel()

	store, ctx := newRegexStore(t)
	if err := store.ReplaceRepo(ctx, "acme/api", []vectorstore.Item{
		textItem("acme/api", "docs.go", "SearchCodeText", 10, managerSource),
	}); err != nil {
		t.Fatal(err)
	}

	page, err := store.SearchRegex(ctx, nil, `errors\.Is\(`, RegexOptions{ContextLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Results) != 1 || len(page.Results[0].Matches) != 1 {
		t.Fatalf("page = %+v", page)
	}

	m := page.Results[0].Matches[0]
	// "\tif errors.Is(" — the match starts at byte 4 of the line, so column 5.
	if m.Line != 13 || m.Column != 5 {
		t.Errorf("location = %d:%d, want 13:5", m.Line, m.Column)
	}
	if len(m.Before) != 1 || !strings.Contains(m.Before[0], "coderag.SearchPage, error") {
		t.Errorf("before = %q", m.Before)
	}
	if len(m.After) != 1 || !strings.Contains(m.After[0], "return coderag.SearchPage{}") {
		t.Errorf("after = %q", m.After)
	}
}

// TestSearchRegexCaseSensitivity checks the index (which is case-folded) does
// not decide case: the regexp does.
func TestSearchRegexCaseSensitivity(t *testing.T) {
	t.Parallel()

	store, ctx := newRegexStore(t)
	if err := store.ReplaceRepo(ctx, "acme/api", []vectorstore.Item{
		textItem("acme/api", "a.go", "A", 1, "value := Manager{}\n"),
		textItem("acme/api", "b.go", "B", 1, "value := manager{}\n"),
	}); err != nil {
		t.Fatal(err)
	}

	sensitive, err := store.SearchRegex(ctx, nil, `Manager`, RegexOptions{CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if sensitive.Total != 1 || sensitive.Results[0].Path != "a.go" {
		t.Fatalf("case-sensitive page = %+v", sensitive)
	}

	insensitive, err := store.SearchRegex(ctx, nil, `Manager`, RegexOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if insensitive.Total != 2 {
		t.Fatalf("case-insensitive total = %d, want 2", insensitive.Total)
	}
}

// TestSearchRegexAnchorsAreLineAnchors pins the multi-line default: `^` and
// `$` must anchor to source lines, not to the chunk the line happens to sit
// in. A chunk boundary is an artefact of indexing and would make anchored
// patterns silently return nothing.
func TestSearchRegexAnchorsAreLineAnchors(t *testing.T) {
	t.Parallel()

	store, ctx := newRegexStore(t)
	if err := store.ReplaceRepo(ctx, "acme/api", []vectorstore.Item{
		textItem("acme/api", "docs.go", "SearchCodeText", 10, managerSource),
	}); err != nil {
		t.Fatal(err)
	}

	page, err := store.SearchRegex(ctx, nil, `^}$`, RegexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Results[0].Matches) != 1 {
		t.Fatalf("page = %+v", page)
	}
	if got := page.Results[0].Matches[0].Line; got != 16 {
		t.Errorf("closing brace line = %d, want 16", got)
	}
}

// TestSearchRegexGroupsChunksPerFile checks a file split across chunks is one
// result with its matches ordered by line, not one result per chunk.
func TestSearchRegexGroupsChunksPerFile(t *testing.T) {
	t.Parallel()

	store, ctx := newRegexStore(t)
	// Chunk ids sort "#10" before "#2", so the second chunk's match is
	// discovered after the third's: ordering has to be restored by line.
	items := []vectorstore.Item{
		chunkItem("acme/api", "big.go", "First", 1, 0, "func Alpha() {}\n"),
		chunkItem("acme/api", "big.go", "Second", 100, 2, "func Beta() {}\n"),
		chunkItem("acme/api", "big.go", "Third", 50, 10, "func Gamma() {}\n"),
	}
	if err := store.ReplaceRepo(ctx, "acme/api", items); err != nil {
		t.Fatal(err)
	}

	page, err := store.SearchRegex(ctx, nil, `^func `, RegexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Results) != 1 {
		t.Fatalf("total = %d results = %d, want one file", page.Total, len(page.Results))
	}
	got := page.Results[0].Matches
	if len(got) != 3 {
		t.Fatalf("matches = %+v", got)
	}
	if got[0].Line != 1 || got[1].Line != 50 || got[2].Line != 100 {
		t.Errorf("lines = %d,%d,%d, want 1,50,100", got[0].Line, got[1].Line, got[2].Line)
	}
}

// TestSearchRegexDedupesOverlappingChunks checks the line that two overlapping
// chunks share is reported once. Overlap happens whenever a symbol was too
// large to fit a single chunk.
func TestSearchRegexDedupesOverlappingChunks(t *testing.T) {
	t.Parallel()

	store, ctx := newRegexStore(t)
	if err := store.ReplaceRepo(ctx, "acme/api", []vectorstore.Item{
		chunkItem("acme/api", "big.go", "Huge", 1, 0, "a\nneedle here\nb\n"),
		// Second window starts one line earlier than the first ended, so
		// file line 2 appears in both chunks.
		chunkItem("acme/api", "big.go", "Huge", 2, 1, "needle here\nb\nc\n"),
	}); err != nil {
		t.Fatal(err)
	}

	page, err := store.SearchRegex(ctx, nil, `needle`, RegexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Results) != 1 || len(page.Results[0].Matches) != 1 {
		t.Fatalf("matches = %+v", page.Results)
	}
	if page.Results[0].Matches[0].Line != 2 {
		t.Errorf("line = %d, want 2", page.Results[0].Matches[0].Line)
	}
}

// Several matches on one line are several matches. The overlap dedup used to
// key on the line alone, which collapsed them into one and reported no
// truncation - a silent undercount, and the shape a grep user is least likely
// to check.
func TestSearchRegexKeepsRepeatedMatchesOnOneLine(t *testing.T) {
	t.Parallel()

	store, ctx := newRegexStore(t)
	if err := store.ReplaceRepo(ctx, "acme/api", []vectorstore.Item{
		textItem("acme/api", "wire.go", "Wire", 1, "handler(a), handler(b), handler(c)\nother\n"),
	}); err != nil {
		t.Fatal(err)
	}

	page, err := store.SearchRegex(ctx, nil, `handler\(`, RegexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Results) != 1 {
		t.Fatalf("results = %+v", page.Results)
	}

	matches := page.Results[0].Matches
	if len(matches) != 3 {
		t.Fatalf("matches = %+v, want the three occurrences on line 1", matches)
	}
	if page.Results[0].Truncated {
		t.Error("a complete match list reported itself truncated")
	}

	// Ordered by column within the line, so the list reads left to right.
	for i, want := range []int{1, 13, 25} {
		if matches[i].Line != 1 || matches[i].Column != want {
			t.Errorf("match %d = line %d col %d, want line 1 col %d", i, matches[i].Line, matches[i].Column, want)
		}
	}
}

// TestSearchRegexScopeAndPaging checks the key filter scopes by repository and
// that the file total stays exact while paging.
func TestSearchRegexScopeAndPaging(t *testing.T) {
	t.Parallel()

	store, ctx := newRegexStore(t)
	for _, repo := range []string{"acme/api", "acme/web", "other/lib"} {
		items := []vectorstore.Item{
			textItem(repo, "a.go", "A", 1, "call handler(ctx)\n"),
			textItem(repo, "b.go", "B", 1, "call handler(ctx)\n"),
		}
		if err := store.ReplaceRepo(ctx, repo, items); err != nil {
			t.Fatal(err)
		}
	}

	scoped, err := store.SearchRegex(ctx, scopeFilter("acme/api", "acme/web"), `handler\(`, RegexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Total != 4 {
		t.Fatalf("namespace total = %d, want 4", scoped.Total)
	}
	for _, hit := range scoped.Results {
		if hit.Repo == "other/lib" {
			t.Fatalf("scope leaked %q", hit.Repo)
		}
	}

	second, err := store.SearchRegex(ctx, scopeFilter("acme/api", "acme/web"), `handler\(`, RegexOptions{Page: 2, PerPage: 3})
	if err != nil {
		t.Fatal(err)
	}
	if second.Total != 4 || len(second.Results) != 1 {
		t.Fatalf("page 2 = %+v", second)
	}
}

// TestSearchRegexMaxMatchesReportsTruncation checks the per-file cap says so
// rather than quietly dropping occurrences.
func TestSearchRegexMaxMatchesReportsTruncation(t *testing.T) {
	t.Parallel()

	store, ctx := newRegexStore(t)
	if err := store.ReplaceRepo(ctx, "acme/api", []vectorstore.Item{
		textItem("acme/api", "a.go", "A", 1, strings.Repeat("needle\n", 10)),
	}); err != nil {
		t.Fatal(err)
	}

	page, err := store.SearchRegex(ctx, nil, `needle`, RegexOptions{MaxMatches: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Results) != 1 {
		t.Fatalf("results = %d", len(page.Results))
	}
	if len(page.Results[0].Matches) != 3 || !page.Results[0].Truncated {
		t.Fatalf("matches = %d truncated = %v", len(page.Results[0].Matches), page.Results[0].Truncated)
	}
}

// TestSearchRegexInvalidPattern checks a bad pattern is a named error, not an
// empty result an agent would read as "no matches".
func TestSearchRegexInvalidPattern(t *testing.T) {
	t.Parallel()

	store, ctx := newRegexStore(t)
	if _, err := store.SearchRegex(ctx, nil, `func (`, RegexOptions{}); err == nil {
		t.Fatal("expected a parse error")
	}
}

// TestSearchRegexBackfillsExistingChunks is the migration contract: chunks
// indexed before the trigram field existed must become searchable by regex
// without re-reading the clone. v1 records are written through a bucket that
// has no trigram tag, then the store is re-registered with the current schema,
// which is exactly what an upgrade does.
func TestSearchRegexBackfillsExistingChunks(t *testing.T) {
	t.Parallel()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	type textRecordV1 struct {
		ID        string `bw:"id,pk"`
		Repo      string `bw:"repo,index"`
		Path      string `bw:"path,fts"`
		Symbol    string `bw:"symbol,fts"`
		StartLine int    `bw:"start_line"`
		EndLine   int    `bw:"end_line"`
		Snippet   string `bw:"snippet,fts"`
	}

	ctx := context.Background()
	old, err := bw.RegisterBucket[textRecordV1](db, textBucketName, bw.WithVersion[textRecordV1](1))
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Insert(ctx, &textRecordV1{
		ID: "acme/api/docs.go#0", Repo: "acme/api", Path: "docs.go",
		Symbol: "SearchCodeText", StartLine: 1996, EndLine: 2003, Snippet: managerSource,
	}); err != nil {
		t.Fatal(err)
	}

	store, err := NewTextStore(db)
	if err != nil {
		t.Fatalf("re-register at v2: %v", err)
	}

	page, err := store.SearchRegex(ctx, nil, `func \(m \*Manager\)`, RegexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Results) != 1 || page.Results[0].Matches[0].Line != 1996 {
		t.Fatalf("v1 chunk was not backfilled: %+v", page)
	}
}

// chunkItem is textItem with an explicit chunk index, for files that occupy
// more than one chunk.
func chunkItem(repo, path, symbol string, line, chunk int, snippet string) vectorstore.Item {
	item := textItem(repo, path, symbol, line, snippet)
	item.ID = repo + "/" + path + "#" + itoa(chunk)

	return item
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	return string(buf[i:])
}

// scopeFilter builds the chunk-id filter for a set of repositories, the way
// the manager builds it from a request's repo/namespace arguments.
func scopeFilter(repos ...string) func(string) bool {
	filter, err := Scope{Repos: repos}.KeyFilter()
	if err != nil {
		panic(err)
	}

	return filter
}
