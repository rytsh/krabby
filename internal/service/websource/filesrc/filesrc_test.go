package filesrc_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/convert"
	"github.com/rytsh/krabby/internal/service/websource/filesrc"
)

func TestMaxBytes(t *testing.T) {
	for name, tt := range map[string]struct {
		configured, want int64
	}{
		"unset uses the default":                 {0, filesrc.DefaultMaxFileBytes},
		"negative uses the default":              {-1, filesrc.DefaultMaxFileBytes},
		"a smaller limit is honoured":            {1024, 1024},
		"cannot exceed what the converter takes": {convert.MaxBytes + 1, min(int64(filesrc.DefaultMaxFileBytes), convert.MaxBytes)},
	} {
		if got := (filesrc.Common{MaxFileBytes: tt.configured}).MaxBytes(); got != tt.want {
			t.Errorf("%s: MaxBytes() = %d, want %d", name, got, tt.want)
		}
	}
}

func TestSelect(t *testing.T) {
	cfg := filesrc.Common{
		Include:      []string{"docs/**"},
		Exclude:      []string{"**/archive/**"},
		MaxFileBytes: 1000,
	}

	for name, tt := range map[string]struct {
		key, contentType string
		size             int64
		want             bool
	}{
		"included markdown":                     {"docs/guide.md", "", 10, true},
		"leading slash is ignored":              {"/docs/guide.md", "", 10, true},
		"outside include":                       {"src/guide.md", "", 10, false},
		"excluded wins over included":           {"docs/archive/old.md", "", 10, false},
		"too large":                             {"docs/guide.md", "", 1001, false},
		"exactly at the limit":                  {"docs/guide.md", "", 1000, true},
		"unknown size is not a reason to skip":  {"docs/guide.md", "", 0, true},
		"extension the converter cannot read":   {"docs/logo.png", "", 10, false},
		"no extension but a convertible type":   {"docs/README", "text/markdown", 10, true},
		"no extension and an unconvertible one": {"docs/blob", "application/octet-stream", 10, false},
	} {
		if got := cfg.Select(tt.key, tt.contentType, tt.size); got != tt.want {
			t.Errorf("%s: Select(%q, %q, %d) = %v, want %v", name, tt.key, tt.contentType, tt.size, got, tt.want)
		}
	}

	if !(filesrc.Common{}).Select("anywhere/notes.md", "", 10) {
		t.Error("an empty include list did not select everything convertible")
	}
}

// ListingSync re-emits everything when the signature changes, so every setting
// that changes which files are selected has to change it.
func TestSignatureTracksSelection(t *testing.T) {
	base := filesrc.Common{Include: []string{"docs/**"}, MaxFiles: 10}
	sig := base.Signature("bucket")

	if again := (filesrc.Common{Include: []string{"docs/**"}, MaxFiles: 10}).Signature("bucket"); again != sig {
		t.Fatal("the same selection produced two signatures")
	}

	for name, other := range map[string]string{
		"include":    filesrc.Common{Include: []string{"**/*.md"}, MaxFiles: 10}.Signature("bucket"),
		"exclude":    filesrc.Common{Include: []string{"docs/**"}, Exclude: []string{"x/**"}, MaxFiles: 10}.Signature("bucket"),
		"max bytes":  filesrc.Common{Include: []string{"docs/**"}, MaxFileBytes: 5, MaxFiles: 10}.Signature("bucket"),
		"max files":  filesrc.Common{Include: []string{"docs/**"}, MaxFiles: 11}.Signature("bucket"),
		"the source": base.Signature("other-bucket"),
	} {
		if other == sig {
			t.Errorf("changing %s left the signature unchanged", name)
		}
	}
}

func TestCheckEnforcesMaxFiles(t *testing.T) {
	items := make([]websource.ListedItem, 3)

	if err := (filesrc.Common{}).Check(items); err != nil {
		t.Errorf("no limit: %v", err)
	}
	if err := (filesrc.Common{MaxFiles: 3}).Check(items); err != nil {
		t.Errorf("at the limit: %v", err)
	}

	err := filesrc.Common{MaxFiles: 2}.Check(items)
	if !errors.Is(err, filesrc.ErrTooMany) {
		t.Fatalf("over the limit: err = %v, want ErrTooMany", err)
	}
	if !strings.Contains(err.Error(), "3 > 2") {
		t.Errorf("error %q does not say how far over the limit the listing is", err)
	}
}

func TestPreview(t *testing.T) {
	items := make([]websource.ListedItem, 5)

	if got := filesrc.Preview(items, 40, 0); got.ItemCount != 5 || got.Scanned != 40 || got.Truncated {
		t.Errorf("no limit: %+v", got)
	}
	if got := filesrc.Preview(items, 40, 5); got.Truncated {
		t.Errorf("at the limit reported truncated: %+v", got)
	}
	if got := filesrc.Preview(items, 40, 4); !got.Truncated || got.Limit != 4 {
		t.Errorf("over the limit: %+v", got)
	}
}

func syncOne(t *testing.T, item websource.ListedItem, data, contentType string, maxBytes int64) websource.RemotePage {
	t.Helper()

	var pages []websource.RemotePage
	_, err := filesrc.Sync(context.Background(), nil, "sig", []websource.ListedItem{item},
		func(context.Context, websource.ListedItem) ([]byte, string, error) {
			return []byte(data), contentType, nil
		}, maxBytes, func(p websource.RemotePage) error {
			pages = append(pages, p)

			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 {
		t.Fatalf("emitted %d pages, want 1", len(pages))
	}

	return pages[0]
}

// The path line goes under an existing heading: the manager adds a title
// heading when a document does not start with one, and a path line above the
// heading would hide it and produce two titles.
func TestSyncPlacesPathUnderTheHeading(t *testing.T) {
	page := syncOne(t, websource.ListedItem{Key: "docs/guide.md"}, "# Guide\n\nBody text.\n", "", 1<<20)

	if page.Err != nil {
		t.Fatal(page.Err)
	}
	if !strings.HasPrefix(page.Markdown, "# Guide\n\n_Path: `docs/guide.md`_\n\nBody text.") {
		t.Errorf("markdown = %q", page.Markdown)
	}
	if page.Title != "Guide" {
		t.Errorf("title = %q, want the document's own heading", page.Title)
	}
}

func TestSyncPrefixesPathWhenThereIsNoHeading(t *testing.T) {
	page := syncOne(t, websource.ListedItem{Key: "notes/todo.txt"}, "buy milk\n", "", 1<<20)

	if page.Err != nil {
		t.Fatal(page.Err)
	}
	if !strings.HasPrefix(page.Markdown, "_Path: `notes/todo.txt`_\n\n") || !strings.Contains(page.Markdown, "buy milk") {
		t.Errorf("markdown = %q", page.Markdown)
	}
}

func TestSyncTitlePrecedence(t *testing.T) {
	// The listing's title beats the document's.
	page := syncOne(t, websource.ListedItem{Key: "a.md", Title: "From listing"}, "# From document\n", "", 1<<20)
	if page.Title != "From listing" {
		t.Errorf("title = %q, want the listing's", page.Title)
	}

	// With neither, the file name is all there is; the converter drops the
	// extension, which reads as a title rather than as a path.
	page = syncOne(t, websource.ListedItem{Key: "notes/todo.txt"}, "buy milk\n", "", 1<<20)
	if page.Title != "todo" {
		t.Errorf("title = %q, want the file name without its extension", page.Title)
	}
}

// Providers keyed by an opaque id (Drive, SharePoint) carry the readable
// location in Path; that is what a reader should see, not the id.
func TestSyncShowsPathRatherThanOpaqueKey(t *testing.T) {
	page := syncOne(t, websource.ListedItem{Key: "1AbC_opaque", Path: "Team/Plans/q3.md"}, "plan\n", "text/markdown", 1<<20)

	if page.Err != nil {
		t.Fatal(page.Err)
	}
	if !strings.Contains(page.Markdown, "_Path: `Team/Plans/q3.md`_") || strings.Contains(page.Markdown, "1AbC_opaque") {
		t.Errorf("markdown = %q", page.Markdown)
	}
	if page.Title != "q3" {
		t.Errorf("title = %q, want it taken from the readable path, not the id", page.Title)
	}
}

// A backtick in a file name would close the code span around the path.
func TestSyncEscapesBackticksInPath(t *testing.T) {
	page := syncOne(t, websource.ListedItem{Key: "odd`name.md"}, "text\n", "", 1<<20)

	if !strings.Contains(page.Markdown, "_Path: `odd'name.md`_") {
		t.Errorf("markdown = %q", page.Markdown)
	}
}

// A file larger than the listing claimed must not be indexed: the size check
// in Select only sees what the listing reported.
func TestSyncRejectsOversizedDownload(t *testing.T) {
	page := syncOne(t, websource.ListedItem{Key: "big.md"}, strings.Repeat("x", 200), "", 100)

	if page.Err == nil {
		t.Fatal("a download over the limit was converted")
	}
	if !strings.Contains(page.Err.Error(), "big.md") || !strings.Contains(page.Err.Error(), "100") {
		t.Errorf("error %q does not name the file and the limit", page.Err)
	}
	if page.Markdown != "" {
		t.Errorf("an oversized file still produced markdown: %q", page.Markdown)
	}
}

// One file failing to download is recorded on that page; the run carries on.
func TestSyncRecordsDownloadFailurePerFile(t *testing.T) {
	boom := errors.New("403 forbidden")
	items := []websource.ListedItem{{Key: "a.md", Version: "1"}, {Key: "b.md", Version: "1"}}

	var pages []websource.RemotePage
	res, err := filesrc.Sync(context.Background(), nil, "sig", items,
		func(_ context.Context, item websource.ListedItem) ([]byte, string, error) {
			if item.Key == "a.md" {
				return nil, "", boom
			}

			return []byte("# B\n"), "", nil
		}, 1<<20, func(p websource.RemotePage) error {
			pages = append(pages, p)

			return nil
		})
	if err != nil {
		t.Fatalf("one failing file failed the whole sync: %v", err)
	}
	if res == nil || len(pages) != 2 {
		t.Fatalf("emitted %d pages, want both", len(pages))
	}
	if !errors.Is(pages[0].Err, boom) {
		t.Errorf("first page error = %v, want the download error", pages[0].Err)
	}
	if pages[1].Err != nil || pages[1].Title != "B" {
		t.Errorf("second page = %+v, want it converted", pages[1])
	}
}

// Unchanged files cost no download on the next run.
func TestSyncSkipsUnchangedFiles(t *testing.T) {
	items := []websource.ListedItem{{Key: "a.md", Version: "v1"}, {Key: "b.md", Version: "v1"}}
	downloads := map[string]int{}
	download := func(_ context.Context, item websource.ListedItem) ([]byte, string, error) {
		downloads[item.Key]++

		return []byte("# " + item.Key + "\n"), "", nil
	}
	discard := func(websource.RemotePage) error { return nil }

	first, err := filesrc.Sync(context.Background(), nil, "sig", items, download, 1<<20, discard)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Complete {
		t.Error("a first run that emitted every file did not report a complete sweep")
	}

	items[1].Version = "v2"
	second, err := filesrc.Sync(context.Background(), first.State, "sig", items, download, 1<<20, discard)
	if err != nil {
		t.Fatal(err)
	}

	if downloads["a.md"] != 1 || downloads["b.md"] != 2 {
		t.Errorf("downloads = %v, want only the changed file fetched again", downloads)
	}
	if second.Complete {
		t.Error("an incremental run reported a complete sweep, which would prune the skipped file")
	}
}

// Only an http(s) item URL may become the base for relative links: a file
// path or an s3:// URI there would produce links that go nowhere.
func TestSyncResolvesRelativeLinksAgainstHTTPURLOnly(t *testing.T) {
	const html = `<html><body><h1>T</h1><p><a href="other.html">other</a></p></body></html>`

	web := syncOne(t, websource.ListedItem{Key: "p.html", URL: "https://docs.example.com/guide/p.html"}, html, "text/html", 1<<20)
	if !strings.Contains(web.Markdown, "https://docs.example.com/guide/other.html") {
		t.Errorf("relative link not resolved against the http URL: %q", web.Markdown)
	}

	object := syncOne(t, websource.ListedItem{Key: "p.html", URL: "s3://bucket/guide/p.html"}, html, "text/html", 1<<20)
	if strings.Contains(object.Markdown, "s3://") {
		t.Errorf("relative link resolved against a non-http URL: %q", object.Markdown)
	}
}
