package manager

import (
	"context"
	"testing"
	"time"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

// codeSearchFixture builds a manager over an in-memory database holding one
// repository's chunks, which is everything the code-search entry points touch.
func codeSearchFixture(t *testing.T, repoID string, items []vectorstore.Item) (*Manager, context.Context) {
	t.Helper()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(db)
	if err != nil {
		t.Fatal(err)
	}
	text, err := coderag.NewTextStore(db)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// The repository is tracked as well as indexed: a search names an id, and
	// an id that is not in the registry is refused rather than searched.
	if err := reg.Upsert(ctx, &registry.Repo{ID: repoID, URL: "https://example.com/" + repoID}); err != nil {
		t.Fatal(err)
	}
	if err := text.ReplaceRepo(ctx, repoID, items); err != nil {
		t.Fatal(err)
	}

	return &Manager{reg: reg, codeText: text}, ctx
}

func codeChunk(repo, path, symbol string, startLine int, text string) vectorstore.Item {
	return vectorstore.Item{
		ID: repo + "/" + path + "#0",
		Payload: vectorstore.Payload{
			Repo:      repo,
			DocPath:   path,
			Symbol:    symbol,
			StartLine: startLine,
			EndLine:   startLine + 20,
			Chunk:     text,
		},
	}
}

// TestSearchCodeTextAnswersAQuestion is the defect this rewrite fixes: bw ANDs
// a bare query's terms, so a natural-language question required every one of
// its words inside a single chunk and reliably returned nothing.
func TestSearchCodeTextAnswersAQuestion(t *testing.T) {
	t.Parallel()

	m, ctx := codeSearchFixture(t, "acme/api", []vectorstore.Item{
		codeChunk("acme/api", "auth.go", "Middleware", 1,
			"// Middleware authenticates a request before the handler runs.\nfunc Middleware(next http.Handler) http.Handler {\n\treturn next\n}\n"),
		codeChunk("acme/api", "unrelated.go", "Sum", 1, "func Sum(a, b int) int { return a + b }\n"),
	})

	page, err := m.SearchCodeText(ctx, "acme/api", "", "how does the auth middleware work", coderag.TextSearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total == 0 {
		t.Fatal("a question over an indexed corpus returned nothing")
	}
	if got := page.Results[0].Path; got != "auth.go" {
		t.Errorf("best hit = %q, want auth.go", got)
	}
}

// TestSearchCodeTextKeepsIdentifiersRequired checks the loosening does not cost
// precision: a dotted identifier stays a required clause, so a chunk that
// carries only the surrounding prose does not outrank the real match.
func TestSearchCodeTextKeepsIdentifiersRequired(t *testing.T) {
	t.Parallel()

	m, ctx := codeSearchFixture(t, "acme/api", []vectorstore.Item{
		codeChunk("acme/api", "store.go", "Get", 1, "if errors.Is(err, bw.ErrNotFound) {\n\treturn nil, nil\n}\n"),
		codeChunk("acme/api", "prose.go", "Doc", 1, "// handle the error and return nil when the record is missing\n"),
	})

	page, err := m.SearchCodeText(ctx, "acme/api", "", "handle bw.ErrNotFound return", coderag.TextSearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Fatalf("total = %d, want only the chunk carrying the identifier", page.Total)
	}
	if got := page.Results[0].Path; got != "store.go" {
		t.Errorf("hit = %q, want store.go", got)
	}
}

// TestSearchCodeTextScopesByPath checks the path glob narrows a search without
// the caller having to encode it into the query string.
func TestSearchCodeTextScopesByPath(t *testing.T) {
	t.Parallel()

	m, ctx := codeSearchFixture(t, "acme/api", []vectorstore.Item{
		codeChunk("acme/api", "internal/auth/token.go", "Token", 1, "func Token() string { return \"needle\" }\n"),
		codeChunk("acme/api", "cmd/main.go", "main", 1, "func main() { print(\"needle\") }\n"),
	})

	all, err := m.SearchCodeText(ctx, "acme/api", "", "needle", coderag.TextSearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Total != 2 {
		t.Fatalf("unscoped total = %d, want 2", all.Total)
	}

	scoped, err := m.SearchCodeText(ctx, "acme/api", "", "needle", coderag.TextSearchOptions{Path: "internal/**"})
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Total != 1 || scoped.Results[0].Path != "internal/auth/token.go" {
		t.Fatalf("path-scoped page = %+v", scoped)
	}
}

// TestSearchCodeTextRanksAcrossRepositories is the second defect fixed here:
// a namespace search used to fan out per repository and merge the pages, so
// the reported total was a merged candidate count rather than a count. One
// pass with a key filter makes it exact, and bw's corpus-wide statistics make
// the scores comparable across repositories in the first place.
func TestSearchCodeTextRanksAcrossRepositories(t *testing.T) {
	t.Parallel()

	m, ctx := codeSearchFixture(t, "acme/api", []vectorstore.Item{
		codeChunk("acme/api", "a.go", "A", 1, "needle here\n"),
	})
	if err := m.codeText.ReplaceRepo(ctx, "acme/web", []vectorstore.Item{
		codeChunk("acme/web", "b.go", "B", 1, "needle here\n"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.codeText.ReplaceRepo(ctx, "other/lib", []vectorstore.Item{
		codeChunk("other/lib", "c.go", "C", 1, "needle here\n"),
	}); err != nil {
		t.Fatal(err)
	}

	page, err := m.SearchCodeText(ctx, "", registry.NamespaceAll, "needle", coderag.TextSearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 {
		t.Fatalf("cross-repository total = %d, want 3", page.Total)
	}
}

// TestSearchCodeTextReportsIndexFreshness checks a page says how current the
// index behind it is. Without it a hit from a three-commit-old index is
// indistinguishable from one indexed a second ago.
func TestSearchCodeTextReportsIndexFreshness(t *testing.T) {
	t.Parallel()

	m, ctx := codeSearchFixture(t, "acme/api", []vectorstore.Item{
		codeChunk("acme/api", "a.go", "A", 1, "needle here\n"),
	})

	indexedAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	repo := &registry.Repo{ID: "acme/api", URL: "https://example.com/acme/api", LastCommit: "newsha"}
	repo.Stages.CodeIndex = registry.StageState{Status: registry.StageOK, Commit: "oldsha", FinishedAt: indexedAt}
	if err := m.reg.Upsert(ctx, repo); err != nil {
		t.Fatal(err)
	}

	page, err := m.SearchCodeText(ctx, "acme/api", "", "needle", coderag.TextSearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Indexed) != 1 {
		t.Fatalf("indexed = %+v, want one entry", page.Indexed)
	}
	got := page.Indexed[0]
	if got.Repo != "acme/api" || got.Commit != "oldsha" || !got.IndexedAt.Equal(indexedAt) {
		t.Errorf("index state = %+v", got)
	}
	if !got.Stale {
		t.Error("index built at an older commit than the clone was not reported stale")
	}
}

// TestSearchCodeTextReportsFreshnessOnAnEmptyResult checks "no matches" can be
// told apart from "nothing indexed yet", which is the case where the metadata
// matters most.
func TestSearchCodeTextReportsFreshnessOnAnEmptyResult(t *testing.T) {
	t.Parallel()

	m, ctx := codeSearchFixture(t, "acme/api", nil)

	page, err := m.SearchCodeText(ctx, "acme/api", "", "nothingmatchesthis", coderag.TextSearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Fatalf("total = %d, want 0", page.Total)
	}
	if len(page.Indexed) != 1 || page.Indexed[0].Repo != "acme/api" {
		t.Fatalf("indexed = %+v, want the searched repository", page.Indexed)
	}
	if !page.Indexed[0].IndexedAt.IsZero() {
		t.Error("a repository that never indexed reported an index time")
	}
}

// An id that names no tracked repository is a mistake, and every mode must say
// so. Scoping the search to it instead produces an empty page, and an empty
// page from a code search reads as "this code does not exist" - a wrong answer
// dressed as a confident one.
func TestCodeSearchRejectsUntrackedRepo(t *testing.T) {
	t.Parallel()

	m, ctx := codeSearchFixture(t, "acme/api", []vectorstore.Item{
		codeChunk("acme/api", "auth.go", "Middleware", 1, "func Middleware() {}\n"),
	})

	if _, err := m.SearchCodeText(ctx, "acme/apy", "", "Middleware", coderag.TextSearchOptions{}); err == nil {
		t.Error("normal mode searched an untracked repository")
	}
	if _, err := m.SearchCodeRegex(ctx, "acme/apy", "", "Middleware", coderag.RegexOptions{}); err == nil {
		t.Error("regex mode searched an untracked repository")
	}

	// The tracked id still works, so the check is a check and not a block.
	page, err := m.SearchCodeText(ctx, "acme/api", "", "Middleware", coderag.TextSearchOptions{})
	if err != nil || page.Total == 0 {
		t.Fatalf("tracked repository: total = %d, err = %v", page.Total, err)
	}
}
