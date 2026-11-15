package apicatalog

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"GET /v1/invoices/{id}": "get-v1-invoices-id",
		"  Hello,   World!  ":   "hello-world",
		"already-a-slug":        "already-a-slug",
		"CamelCaseName":         "camelcasename",
		"ünïcödé only":          "n-c-d-only",
		"":                      "op",
		"!!!":                   "op",
		"---":                   "op",
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlugifyIsBoundedAndClean(t *testing.T) {
	// 79 letters then a separator lands the cut exactly on a dash.
	got := Slugify(strings.Repeat("a", 79) + "/" + strings.Repeat("b", 50))

	if len(got) > 80 {
		t.Errorf("slug is %d characters, want at most 80", len(got))
	}
	if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
		t.Errorf("slug %q starts or ends with a dash", got)
	}
}

// Whatever a spec author writes, the slug it produces must be storable: it
// becomes a file name and half of a record id.
func TestSlugifyOutputIsAlwaysAValidSlug(t *testing.T) {
	for _, in := range []string{
		"../../etc/passwd", "..", ".", "/", "a/../b", `C:\Windows\system32`, "name.md",
		"-leading", "trailing-", strings.Repeat("x/", 400), "\x00null", "",
	} {
		if got := Slugify(in); !ValidSlug(got) {
			t.Errorf("Slugify(%q) = %q, which is not a valid slug", in, got)
		}
	}
}

func TestOpSlug(t *testing.T) {
	a := OpSlug("GET", "/v1/invoices/{id}")

	if a != OpSlug("GET", "/v1/invoices/{id}") {
		t.Fatal("OpSlug is not stable for the same operation")
	}
	if !strings.HasPrefix(a, "get-v1-invoices-id-") {
		t.Errorf("OpSlug = %q, want it to read as the method and path", a)
	}
	if !ValidSlug(a) {
		t.Errorf("OpSlug = %q, which is not a valid slug", a)
	}

	// Method case and padding are presentation, not identity.
	if b := OpSlug(" get ", "/v1/invoices/{id}"); b != a {
		t.Errorf("OpSlug differs by method spelling: %q vs %q", a, b)
	}
	if OpSlug("POST", "/v1/invoices/{id}") == a {
		t.Error("two methods on one path share a slug")
	}
}

// "/v1/a-b" and "/v1/a/b" slugify to the same text; without the hash one
// operation's document would overwrite the other's.
func TestOpSlugDisambiguatesPathsThatSlugifyAlike(t *testing.T) {
	a, b := OpSlug("GET", "/v1/a-b"), OpSlug("GET", "/v1/a/b")

	if a == b {
		t.Fatalf("distinct paths produced the same slug %q", a)
	}
	if a[:len(a)-8] != b[:len(b)-8] {
		t.Fatalf("expected the two to differ only in the hash: %q vs %q", a, b)
	}
}

func TestOpSlugStaysBoundedForLongPaths(t *testing.T) {
	long := "/" + strings.Repeat("segment/", 100)

	got := OpSlug("GET", long)
	if len(got) > 60+1+8 {
		t.Errorf("slug is %d characters for a long path", len(got))
	}
	if !ValidSlug(got) {
		t.Errorf("OpSlug = %q, which is not a valid slug", got)
	}
	if got == OpSlug("GET", long+"more") {
		t.Error("two long paths sharing a prefix collided")
	}
}

func TestOpSlugWithoutAMethod(t *testing.T) {
	if got := OpSlug("", "/billing.v1.Invoices/Create"); !strings.HasPrefix(got, "op-billing-v1-invoices-create-") {
		t.Errorf("OpSlug = %q", got)
	}
}

func TestValidSlug(t *testing.T) {
	for slug, want := range map[string]bool{
		"get-v1-invoices-id-1a2b3c4d": true,
		"a":                           true,
		"with.dot_and-dash":           true,
		"":                            false,
		".":                           false,
		"..":                          false,
		"../escape":                   false,
		"a/b":                         false,
		`a\b`:                         false,
		"-leading-dash":               false,
		".hidden":                     false,
		"Upper":                       false,
		"with space":                  false,
		"null\x00byte":                false,
		strings.Repeat("a", 200):      true,
		strings.Repeat("a", 201):      false,
	} {
		if got := ValidSlug(slug); got != want {
			t.Errorf("ValidSlug(%q) = %v, want %v", slug, got, want)
		}
	}
}

// operationId is free text from an untrusted document. A slug that escaped the
// service directory would turn deleting an operation into deleting any .md file
// the process can reach.
func TestOperationFileRefusesToLeaveItsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "apis", "billing")

	got, err := OperationFile(dir, "get-v1-invoices-1a2b3c4d")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "get-v1-invoices-1a2b3c4d.md"); got != want {
		t.Errorf("OperationFile = %q, want %q", got, want)
	}

	for _, slug := range []string{"..", "../../secrets", "a/b", "/etc/passwd", "", ".", `..\..\x`} {
		path, err := OperationFile(dir, slug)
		if err == nil {
			t.Errorf("OperationFile(%q) = %q, want it refused", slug, path)
		}
		if path != "" {
			t.Errorf("OperationFile(%q) returned a path %q alongside its error", slug, path)
		}
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"billing":                true,
		"billing-v2":             true,
		"team.billing_api":       true,
		"0leading-digit":         true,
		"":                       false,
		"Billing":                false,
		"has space":              false,
		"-leading-dash":          false,
		".leading-dot":           false,
		"a/b":                    false,
		"..":                     false,
		strings.Repeat("a", 100): true,
		strings.Repeat("a", 101): false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNormalizeGroup(t *testing.T) {
	for in, want := range map[string]string{
		"Billing":   "billing",
		"  ops  ":   "ops",
		"default":   "",
		" DEFAULT ": "",
		"":          "",
		"defaults":  "defaults",
	} {
		if got := NormalizeGroup(in); got != want {
			t.Errorf("NormalizeGroup(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScopeKeyRoundTrip(t *testing.T) {
	key := ScopeKey("billing")

	if key != "api:billing" {
		t.Errorf("ScopeKey = %q, want api:billing", key)
	}
	if got := ServiceName(key); got != "billing" {
		t.Errorf("ServiceName(%q) = %q", key, got)
	}
	// A repo id or a web-source key is not an API service.
	for _, other := range []string{"github.com/acme/api", "web:wiki", "billing", ""} {
		if got := ServiceName(other); got != "" {
			t.Errorf("ServiceName(%q) = %q, want empty", other, got)
		}
	}
}

func TestHashIsStableAndContentSensitive(t *testing.T) {
	a := Hash("# GET /v1/invoices")

	if a != Hash("# GET /v1/invoices") {
		t.Error("Hash is not stable")
	}
	if a == Hash("# GET /v1/invoice") {
		t.Error("different content hashed alike")
	}
	if len(a) != 32 {
		t.Errorf("Hash is %d hex characters, want 32", len(a))
	}
}
