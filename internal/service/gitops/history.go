package gitops

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// History output is fed to an LLM, so every read is bounded. A busy repository
// can hold hundreds of thousands of commits and a single merge can carry a
// megabyte of patch; an unbounded answer would blow the caller's context long
// before it became useful.
const (
	// MaxLogCommits caps one page of commits.
	MaxLogCommits = 200
	// MaxPatchBytes caps how much patch text one diff returns.
	MaxPatchBytes = 256 * 1024
	// MaxDiffFiles caps the per-file status list of one diff.
	MaxDiffFiles = 1000
)

// Field and record separators for the --format parsing below. Commit messages
// are free text and routinely contain newlines, tabs, quotes and pipes, so the
// separators are ASCII control characters that cannot occur in practice.
const (
	fieldSep  = "\x1f"
	recordSep = "\x1e"
)

// revRe restricts revisions to what a ref, sha or range endpoint can look like.
//
// The values reach a command line, and they arrive from an LLM: without this a
// revision of "--upload-pack=..." would be read by git as an option rather than
// a ref. Argument order alone is not a defence, since git accepts options after
// positional arguments.
var revRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@^~+-]*$`)

// CheckRevision validates a user-supplied revision.
func CheckRevision(rev string) error {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return fmt.Errorf("revision is required")
	}
	if len(rev) > 255 {
		return fmt.Errorf("revision is too long")
	}
	if !revRe.MatchString(rev) {
		return fmt.Errorf("invalid revision %q", rev)
	}

	return nil
}

// Ref is one tag or branch in a clone.
type Ref struct {
	Name    string `json:"name"`
	Type    string `json:"type"`           // "tag" or "branch"
	Kind    string `json:"kind,omitempty"` // tags only: "annotated" or "lightweight"
	Commit  string `json:"commit"`
	Time    int64  `json:"time,omitempty"`    // creation time, unix seconds
	Subject string `json:"subject,omitempty"` // tag message or commit subject
}

// Refs lists the clone's tags and branches, newest first.
//
// Tags are dereferenced to the commit they point at: an annotated tag is its
// own object, and returning that sha would hand callers something that git log
// and git diff still accept but that never matches a commit sha from blame or
// the repo record — a mismatch that is invisible until someone compares them.
func (g *Git) Refs(ctx context.Context, dir string) ([]Ref, error) {
	format := strings.Join([]string{
		"%(refname)", "%(objecttype)", "%(objectname)", "%(*objectname)",
		"%(creatordate:unix)", "%(contents:subject)",
	}, fieldSep)

	out, err := g.run(ctx, dir, nil,
		"for-each-ref", "--sort=-creatordate", "--format="+format,
		"refs/tags", "refs/heads", "refs/remotes/origin",
	)
	if err != nil {
		return nil, err
	}

	var refs []Ref
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		parts := strings.Split(line, fieldSep)
		if len(parts) < 6 {
			continue
		}

		full, objType, objName, derefName, created, subject := parts[0], parts[1], parts[2], parts[3], parts[4], parts[5]

		ref := Ref{Commit: objName, Subject: subject}
		if derefName != "" {
			ref.Commit = derefName
		}

		switch {
		case strings.HasPrefix(full, "refs/tags/"):
			ref.Name = strings.TrimPrefix(full, "refs/tags/")
			ref.Type = "tag"
			ref.Kind = "lightweight"
			if objType == "tag" {
				ref.Kind = "annotated"
			}
		case strings.HasPrefix(full, "refs/heads/"):
			ref.Name = strings.TrimPrefix(full, "refs/heads/")
			ref.Type = "branch"
		case strings.HasPrefix(full, "refs/remotes/origin/"):
			name := strings.TrimPrefix(full, "refs/remotes/origin/")
			if name == "HEAD" {
				continue // a symbolic alias, not a branch of its own
			}
			ref.Name = "origin/" + name
			ref.Type = "branch"
		default:
			continue
		}

		ref.Time, _ = strconv.ParseInt(created, 10, 64)
		refs = append(refs, ref)
	}

	return refs, nil
}

// Commit is one entry of the history.
type Commit struct {
	Commit  string `json:"commit"`
	Author  string `json:"author"`
	Email   string `json:"email,omitempty"`
	Time    int64  `json:"time,omitempty"` // author time, unix seconds
	Subject string `json:"subject"`
	// Body is the message below the subject: the "why" that a subject line and
	// a diff cannot carry between them.
	Body string `json:"body,omitempty"`
	// Files are the paths this commit touched. Empty on a merge commit, which
	// git reports no changes for without an explicit strategy.
	Files []string `json:"files,omitempty"`
}

// LogOptions selects the slice of history to read.
type LogOptions struct {
	// From and To bound the range. Both set reads From..To (commits reachable
	// from To but not From) — the "what landed between two releases" question.
	// Only To set reads history ending at To; neither reads from HEAD.
	From string
	To   string
	// Path restricts history to one file or directory.
	Path string
	// Skip and Limit paginate. Limit is clamped to MaxLogCommits.
	Skip  int
	Limit int
}

// Log reads commit metadata. It never returns patch content: a range spanning a
// release is routinely thousands of commits, and callers that want the changes
// ask Diff for the range or for one commit.
func (g *Git) Log(ctx context.Context, dir string, opts LogOptions) ([]Commit, error) {
	rev, err := revRange(opts.From, opts.To)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 || limit > MaxLogCommits {
		limit = MaxLogCommits
	}

	format := recordSep + strings.Join([]string{"%H", "%an", "%ae", "%at", "%s", "%b"}, fieldSep) + fieldSep

	args := []string{
		"log", "--no-color", "--no-merges", "--name-only",
		"--format=" + format,
		"-n", strconv.Itoa(limit),
	}
	if opts.Skip > 0 {
		args = append(args, "--skip", strconv.Itoa(opts.Skip))
	}
	if rev != "" {
		args = append(args, rev)
	}
	// The separator keeps a path that looks like a revision from being read as
	// one, which is exactly how a file named "HEAD" or a branch-shaped
	// directory would otherwise be mis-resolved.
	if opts.Path != "" {
		args = append(args, "--", opts.Path)
	}

	out, err := g.run(ctx, dir, nil, args...)
	if err != nil {
		return nil, err
	}

	return parseLog(out), nil
}

// parseLog splits the record-separated log output. Anything after the last
// field separator of a record is the --name-only file list.
func parseLog(out string) []Commit {
	var commits []Commit

	for _, record := range strings.Split(out, recordSep) {
		if strings.TrimSpace(record) == "" {
			continue
		}

		parts := strings.SplitN(record, fieldSep, 7)
		if len(parts) < 7 {
			continue
		}

		c := Commit{
			Commit:  strings.TrimSpace(parts[0]),
			Author:  parts[1],
			Email:   parts[2],
			Subject: parts[4],
			Body:    strings.TrimSpace(parts[5]),
		}
		c.Time, _ = strconv.ParseInt(parts[3], 10, 64)

		for _, f := range strings.Split(parts[6], "\n") {
			if f = strings.TrimSpace(f); f != "" {
				c.Files = append(c.Files, f)
			}
		}

		commits = append(commits, c)
	}

	return commits
}

// DiffFile is one path changed by a diff, with its git status letter
// (A added, M modified, D deleted, ...).
type DiffFile struct {
	Status string `json:"status"`
	Path   string `json:"path"`
}

// DiffResult is a change set: always the per-file status, and the patch text
// only when asked for.
type DiffResult struct {
	From string `json:"from,omitempty"`
	To   string `json:"to"`

	Files []DiffFile `json:"files"`
	// FilesTruncated reports that the change set is larger than MaxDiffFiles.
	FilesTruncated bool `json:"files_truncated,omitempty"`

	Patch string `json:"patch,omitempty"`
	// PatchTruncated reports that the patch was cut at MaxPatchBytes. Narrow
	// the diff with a path rather than expecting the rest.
	PatchTruncated bool `json:"patch_truncated,omitempty"`
	PatchBytes     int  `json:"patch_bytes,omitempty"`
}

// Diff reports what changed between two revisions. With from empty it reports
// what a single commit changed, which is what turns a sha from blame into an
// explanation.
func (g *Git) Diff(ctx context.Context, dir, from, to, path string, patch bool) (*DiffResult, error) {
	if err := CheckRevision(to); err != nil {
		return nil, err
	}
	if from != "" {
		if err := CheckRevision(from); err != nil {
			return nil, err
		}
	}

	res := &DiffResult{From: from, To: to}

	files, err := g.diffStatus(ctx, dir, from, to, path)
	if err != nil {
		return nil, err
	}
	if len(files) > MaxDiffFiles {
		files = files[:MaxDiffFiles]
		res.FilesTruncated = true
	}
	res.Files = files

	if !patch {
		return res, nil
	}

	body, err := g.run(ctx, dir, nil, g.diffArgs(from, to, path, true)...)
	if err != nil {
		return nil, err
	}

	res.PatchBytes = len(body)
	if len(body) > MaxPatchBytes {
		body = body[:MaxPatchBytes]
		res.PatchTruncated = true
	}
	res.Patch = body

	return res, nil
}

func (g *Git) diffStatus(ctx context.Context, dir, from, to, path string) ([]DiffFile, error) {
	out, err := g.run(ctx, dir, nil, g.diffArgs(from, to, path, false)...)
	if err != nil {
		return nil, err
	}

	var files []DiffFile
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		status, p, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}

		files = append(files, DiffFile{Status: strings.TrimSpace(status), Path: strings.TrimSpace(p)})
	}

	return files, nil
}

// diffArgs builds the git invocation. A single commit goes through `git show`
// rather than `to^..to` so the first commit of a repository — which has no
// parent — is describable like any other.
func (g *Git) diffArgs(from, to, path string, patch bool) []string {
	var args []string
	if from == "" {
		args = []string{"show", "--format=", to}
	} else {
		args = []string{"diff", from, to}
	}

	if patch {
		args = append(args, "--no-color", "-U3")
	} else {
		args = append(args, "--name-status", "--no-renames")
	}

	if path != "" {
		args = append(args, "--", path)
	}

	return args
}

// revRange turns the two endpoints into a git revision argument.
func revRange(from, to string) (string, error) {
	for _, rev := range []string{from, to} {
		if rev == "" {
			continue
		}
		if err := CheckRevision(rev); err != nil {
			return "", err
		}
	}

	switch {
	case from != "" && to != "":
		return from + ".." + to, nil
	case to != "":
		return to, nil
	case from != "":
		// "everything since From" on the current branch.
		return from + "..HEAD", nil
	default:
		return "", nil
	}
}
