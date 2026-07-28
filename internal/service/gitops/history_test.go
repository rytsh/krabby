package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo builds a small repository with two tagged releases so the history
// helpers can be exercised against real git output rather than a fixture that
// can drift from what git actually prints.
func testRepo(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()

		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@example.com",
			"GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}

	write := func(name, content string) {
		t.Helper()

		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q", "-b", "main")

	write("app.go", "package app\n")
	run("add", "app.go")
	run("commit", "-q", "-m", "initial commit")
	run("tag", "v1.0.0")

	write("app.go", "package app\n\nfunc Run() {}\n")
	write("README.md", "# app\n")
	run("add", ".")
	run("commit", "-q", "-m", "feat: add Run\n\nThe scheduler needs an entry point it can call\nwithout importing the whole service.")

	write("old.go", "package app\n")
	run("add", "old.go")
	run("commit", "-q", "-m", "chore: add old.go")
	run("rm", "-q", "old.go")
	run("commit", "-q", "-m", "chore: drop old.go")
	// An annotated tag: its ref points at a tag object, not a commit.
	run("tag", "-a", "v1.1.0", "-m", "release 1.1.0")

	return dir
}

func TestRefsDereferencesAnnotatedTags(t *testing.T) {
	dir := testRepo(t)
	g := New("")
	ctx := context.Background()

	refs, err := g.Refs(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]Ref{}
	for _, r := range refs {
		byName[r.Name] = r
	}

	head, err := g.Head(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	annotated, ok := byName["v1.1.0"]
	if !ok {
		t.Fatalf("v1.1.0 missing from %+v", refs)
	}
	if annotated.Kind != "annotated" {
		t.Errorf("v1.1.0 kind = %q, want annotated", annotated.Kind)
	}
	// The point of dereferencing: the sha must be comparable with a commit sha
	// from blame or the repo record, not the tag object's own id.
	if annotated.Commit != head {
		t.Errorf("annotated tag commit = %s, want the commit it points at (%s)", annotated.Commit, head)
	}

	if lightweight, ok := byName["v1.0.0"]; !ok {
		t.Error("v1.0.0 missing")
	} else if lightweight.Kind != "lightweight" {
		t.Errorf("v1.0.0 kind = %q, want lightweight", lightweight.Kind)
	}

	if main, ok := byName["main"]; !ok || main.Type != "branch" {
		t.Errorf("main branch missing or mistyped: %+v", byName["main"])
	}
}

func TestLogRangeBetweenTags(t *testing.T) {
	dir := testRepo(t)
	g := New("")

	commits, err := g.Log(context.Background(), dir, LogOptions{From: "v1.0.0", To: "v1.1.0"})
	if err != nil {
		t.Fatal(err)
	}

	// from is exclusive: the tagged v1.0.0 commit itself is not in the range.
	if len(commits) != 3 {
		t.Fatalf("got %d commits, want the 3 that landed after v1.0.0: %+v", len(commits), commits)
	}

	var feat *Commit
	for i := range commits {
		if strings.HasPrefix(commits[i].Subject, "feat:") {
			feat = &commits[i]
		}
	}
	if feat == nil {
		t.Fatalf("feat commit missing from %+v", commits)
	}

	// The body is the whole point: a subject line and a diff together still do
	// not say why the change was made.
	if !strings.Contains(feat.Body, "scheduler needs an entry point") {
		t.Errorf("commit body not captured: %q", feat.Body)
	}
	if feat.Author != "Alice" || feat.Email != "alice@example.com" || feat.Time == 0 {
		t.Errorf("author metadata incomplete: %+v", feat)
	}

	files := strings.Join(feat.Files, ",")
	if !strings.Contains(files, "app.go") || !strings.Contains(files, "README.md") {
		t.Errorf("touched files = %v, want both app.go and README.md", feat.Files)
	}
}

func TestLogPathFiltersHistory(t *testing.T) {
	dir := testRepo(t)
	g := New("")

	commits, err := g.Log(context.Background(), dir, LogOptions{Path: "README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || !strings.HasPrefix(commits[0].Subject, "feat:") {
		t.Fatalf("path-filtered log = %+v, want only the commit that added README.md", commits)
	}
}

func TestLogPaginates(t *testing.T) {
	dir := testRepo(t)
	g := New("")
	ctx := context.Background()

	first, err := g.Log(ctx, dir, LogOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := g.Log(ctx, dir, LogOptions{Limit: 2, Skip: 2})
	if err != nil {
		t.Fatal(err)
	}

	if len(first) != 2 {
		t.Fatalf("first page = %d commits, want 2", len(first))
	}
	if len(second) == 0 || second[0].Commit == first[0].Commit {
		t.Fatalf("skip did not advance the page: %+v vs %+v", first, second)
	}
}

func TestDiffSingleCommitExplainsABlameSha(t *testing.T) {
	dir := testRepo(t)
	g := New("")
	ctx := context.Background()

	commits, err := g.Log(ctx, dir, LogOptions{Path: "README.md"})
	if err != nil {
		t.Fatal(err)
	}
	sha := commits[0].Commit

	// from empty: "what did this one commit do", which is the follow-up to a
	// blame result.
	res, err := g.Diff(ctx, dir, "", sha, "", true)
	if err != nil {
		t.Fatal(err)
	}

	status := map[string]string{}
	for _, f := range res.Files {
		status[f.Path] = f.Status
	}
	if status["README.md"] != "A" || status["app.go"] != "M" {
		t.Fatalf("file statuses = %+v, want README.md added and app.go modified", res.Files)
	}
	if !strings.Contains(res.Patch, "func Run()") {
		t.Errorf("patch missing the added code: %q", res.Patch)
	}
}

func TestDiffRangeReportsDeletes(t *testing.T) {
	dir := testRepo(t)
	g := New("")

	res, err := g.Diff(context.Background(), dir, "v1.0.0", "v1.1.0", "", false)
	if err != nil {
		t.Fatal(err)
	}

	// old.go was added and removed inside the range, so the range diff must not
	// mention it at all; README.md was added and survives.
	for _, f := range res.Files {
		if f.Path == "old.go" {
			t.Errorf("a file added and deleted within the range should not appear: %+v", res.Files)
		}
	}
	if res.Patch != "" {
		t.Error("patch must be omitted unless requested")
	}

	found := false
	for _, f := range res.Files {
		if f.Path == "README.md" && f.Status == "A" {
			found = true
		}
	}
	if !found {
		t.Errorf("README.md not reported as added: %+v", res.Files)
	}
}

func TestDiffRootCommit(t *testing.T) {
	dir := testRepo(t)
	g := New("")
	ctx := context.Background()

	root, err := g.run(ctx, dir, nil, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// A root commit has no parent, so a "to^..to" implementation would fail
	// here. It is the first thing anyone asks about when reading a new repo.
	res, err := g.Diff(ctx, dir, "", strings.TrimSpace(root), "", false)
	if err != nil {
		t.Fatalf("root commit must be describable: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "app.go" {
		t.Fatalf("root diff = %+v, want app.go added", res.Files)
	}
}

// Revisions reach a command line and arrive from a model, so anything that
// could be read as an option must be rejected before it gets there.
func TestCheckRevisionRejectsOptionLikeInput(t *testing.T) {
	for _, bad := range []string{
		"", "   ", "--upload-pack=touch /tmp/x", "-n1", "--all",
		"v1.0.0; rm -rf /", "v1.0.0 --output=/tmp/x", "a`b`", "$(id)", "a|b",
		strings.Repeat("v", 300),
	} {
		if err := CheckRevision(bad); err == nil {
			t.Errorf("CheckRevision(%q) accepted an unsafe revision", bad)
		}
	}

	for _, ok := range []string{
		"HEAD", "main", "origin/main", "v1.0.0", "release/1.2", "HEAD~3", "abc123^", "v1.0.0-rc1",
	} {
		if err := CheckRevision(ok); err != nil {
			t.Errorf("CheckRevision(%q) rejected a valid revision: %v", ok, err)
		}
	}
}

func TestRevRange(t *testing.T) {
	tests := []struct {
		from, to, want string
	}{
		{"v1", "v2", "v1..v2"},
		{"", "v2", "v2"},
		{"v1", "", "v1..HEAD"},
		{"", "", ""},
	}

	for _, tt := range tests {
		got, err := revRange(tt.from, tt.to)
		if err != nil {
			t.Fatalf("revRange(%q,%q): %v", tt.from, tt.to, err)
		}
		if got != tt.want {
			t.Errorf("revRange(%q,%q) = %q, want %q", tt.from, tt.to, got, tt.want)
		}
	}

	if _, err := revRange("--all", "v2"); err == nil {
		t.Error("revRange must validate both endpoints")
	}
}
