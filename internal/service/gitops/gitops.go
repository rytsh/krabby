// Package gitops shells out to git for clone/fetch/pull with per-repo auth
// (SSH key or HTTPS token) and an optional global SSH key fallback.
package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/credentials"
)

// tokenHelper feeds https credentials to git from env vars so the secret never
// appears in argv.
const tokenHelper = `!f() { echo "username=${KRABBY_GIT_USERNAME}"; echo "password=${KRABBY_GIT_TOKEN}"; }; f`

// Git runs git commands.
type Git struct {
	sshKeyPath string // global fallback key
}

// New creates a Git runner. sshKeyPath may be empty to use the ambient ssh config/agent.
func New(sshKeyPath string) *Git {
	return &Git{sshKeyPath: sshKeyPath}
}

func (g *Git) env(auth *credentials.Auth) []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	keyPath := g.sshKeyPath
	if auth != nil && auth.SSHKeyPath != "" {
		keyPath = auth.SSHKeyPath
	}

	// git runs GIT_SSH_COMMAND through a shell, so the key path is quoted: a
	// data directory with a space in it would otherwise split the argument.
	// BatchMode keeps ssh from waiting on a prompt nobody can answer, and the
	// connect timeout keeps an unreachable host from holding a worker.
	if keyPath != "" {
		env = append(env, fmt.Sprintf(
			"GIT_SSH_COMMAND=ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=30",
			shellQuote(keyPath),
		))
	}

	if auth != nil && auth.Token != "" {
		username := auth.Username
		if username == "" {
			username = "oauth2"
		}

		env = append(env,
			"KRABBY_GIT_USERNAME="+username,
			"KRABBY_GIT_TOKEN="+auth.Token,
		)
	}

	return env
}

// run executes git and returns its output with surrounding whitespace
// trimmed, which suits single values (a sha, a ref) and line lists. Output
// whose edges are content, a patch or blame of a file ending in a blank line,
// goes through runRaw.
func (g *Git) run(ctx context.Context, dir string, auth *credentials.Auth, args ...string) (string, error) {
	out, err := g.runRaw(ctx, dir, auth, args...)

	return strings.TrimSpace(out), err
}

// runRaw executes git and returns its output as written.
//
// core.quotePath=false makes git print paths as they are: by default a name
// with non-ASCII bytes comes back C-quoted ("caf\303\251.go"), which matches
// no file, so a changed file would never be reindexed and its old chunks
// never dropped.
func (g *Git) runRaw(ctx context.Context, dir string, auth *credentials.Auth, args ...string) (string, error) {
	out, _, err := g.runCapped(ctx, dir, auth, 0, args...)

	return out, err
}

// errOutputCapped stops git once its output passed the caller's cap.
var errOutputCapped = errors.New("git output reached its cap")

// cappedBuffer keeps at most limit bytes and fails the write past them, which
// ends git with a broken pipe instead of buffering everything it would print.
type cappedBuffer struct {
	buf    bytes.Buffer
	limit  int
	capped bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:max(room, 0)])
		b.capped = true

		return 0, errOutputCapped
	}

	return b.buf.Write(p)
}

// runCapped is runRaw keeping at most limit bytes of output (0 = no limit).
// capped reports that the output was cut there; git is stopped at that point
// rather than allowed to produce the rest into memory, so a diff across a
// large range costs the cap, not the size of the diff.
func (g *Git) runCapped(ctx context.Context, dir string, auth *credentials.Auth, limit int, args ...string) (string, bool, error) {
	// A stalled HTTP transfer (under 1 KiB/s for a minute) fails instead of
	// holding the repository's worker until someone notices.
	full := append([]string{
		"-c", "core.quotePath=false",
		"-c", "http.lowSpeedLimit=1024", "-c", "http.lowSpeedTime=60",
	}, args...)
	if auth != nil && auth.Token != "" {
		// Reset any system credential helpers, then install ours.
		full = append([]string{"-c", "credential.helper=", "-c", "credential.helper=" + tokenHelper}, full...)
	}

	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = g.env(auth)
	// Cancelling kills git, but an ssh child that inherited its stdout keeps
	// the pipe open and Wait blocked. WaitDelay bounds that.
	cmd.WaitDelay = 10 * time.Second

	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	var out bytes.Buffer
	capped := &cappedBuffer{limit: limit}
	if limit > 0 {
		cmd.Stdout = capped
	} else {
		cmd.Stdout = &out
	}

	if err := cmd.Run(); err != nil {
		// A cut output is the expected end of a capped run: git died of the
		// write failing, and what was kept is the answer.
		if capped.capped {
			return capped.buf.String(), true, nil
		}

		return "", false, fmt.Errorf("git %s; %w; %s", redactArgs(args), err, redactURLs(strings.TrimSpace(errOut.String())))
	}
	if limit > 0 {
		return capped.buf.String(), capped.capped, nil
	}

	return out.String(), false, nil
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// redactArgs renders git arguments for an error message without the
// credentials a URL can carry.
func redactArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = redactURLs(a)
	}

	return strings.Join(out, " ")
}

// userinfoRe matches the user:password@ part of a URL.
var userinfoRe = regexp.MustCompile(`(://)[^/@\s]+@`)

// redactURLs removes userinfo from every URL in s. A clone URL such as
// https://user:token@host/... would otherwise put the token into the error,
// and from there into LastError, the API and the logs.
func redactURLs(s string) string {
	return userinfoRe.ReplaceAllString(s, "${1}[redacted]@")
}

// Clone clones url into dest. Branch may be empty for the default branch.
func (g *Git) Clone(ctx context.Context, url, branch, dest string, auth *credentials.Auth) error {
	args := []string{"clone", "--single-branch"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	// "--" ends the options: a URL or path beginning with "-" is data, never
	// an option such as --upload-pack, which runs a command.
	args = append(args, "--", url, dest)

	if _, err := g.run(ctx, "", auth, args...); err != nil {
		return err
	}

	return nil
}

// SetRemoteURL points an existing clone at url. Snapshot preparation first
// clones the active local version for speed, then restores the real origin
// before fetching updates.
func (g *Git) SetRemoteURL(ctx context.Context, dir, url string) error {
	_, err := g.run(ctx, dir, nil, "remote", "set-url", "--", "origin", url)

	return err
}

// Fetch updates remote refs, including tags. Remote tags are authoritative:
// moved tags replace local ones and tags deleted upstream are removed locally.
//
// --tags is explicit rather than left to git's default tag-following: the
// clones are --single-branch, so the default only picks up tags reachable from
// that one branch's new history. Release tags are how a caller names "the
// version before this one", and a tag list that silently lags the remote is
// worse than none.
func (g *Git) Fetch(ctx context.Context, dir string, auth *credentials.Auth) error {
	_, err := g.run(ctx, dir, auth, "fetch", "--force", "--prune", "--prune-tags", "--tags", "origin")

	return err
}

// Head returns the local HEAD commit.
func (g *Git) Head(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, nil, "rev-parse", "HEAD")
}

// RemoteHead returns the remote head commit for branch ("" = upstream of HEAD).
func (g *Git) RemoteHead(ctx context.Context, dir, branch string) (string, error) {
	ref := "@{upstream}"
	if branch != "" {
		ref = "origin/" + branch
	}

	return g.run(ctx, dir, nil, "rev-parse", ref)
}

// ResetToRemote moves the checked-out branch and working tree to the fetched
// remote branch ("" = the upstream of HEAD), whatever its history.
//
// It is for krabby's private staging clones, which nobody edits: there is no
// local work a reset could lose, and a fast-forward-only pull would leave a
// repository failing every refresh once its upstream was force-pushed.
func (g *Git) ResetToRemote(ctx context.Context, dir, branch string) error {
	ref := "@{upstream}"
	if branch != "" {
		ref = "origin/" + branch
	}

	_, err := g.run(ctx, dir, nil, "reset", "--hard", ref, "--")

	return err
}

// DiffNames returns the repo-relative paths that differ between two commits
// (added, modified and deleted). Renames are reported as delete+add so callers
// can treat every returned path uniformly: drop old data, index what exists.
func (g *Git) DiffNames(ctx context.Context, dir, from, to string) ([]string, error) {
	out, err := g.run(ctx, dir, nil, "diff", "--name-only", "--no-renames", from, to, "--")
	if err != nil {
		return nil, err
	}

	var files []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}

	return files, nil
}

// BlameLine is one attributed source line from `git blame`.
type BlameLine struct {
	Line    int    `json:"line"`              // 1-based line number in the current file
	Commit  string `json:"commit"`            // full commit sha that last touched the line
	Author  string `json:"author"`            // author name
	Email   string `json:"email,omitempty"`   // author email (angle brackets stripped)
	Time    int64  `json:"time,omitempty"`    // author time, unix seconds
	Summary string `json:"summary,omitempty"` // first line of the commit message
	Content string `json:"content"`           // the source line itself (no trailing newline)
}

// Blame returns structured `git blame` output for a repo-relative file. When
// start > 0 it limits the output to the line range [start, end] via -L; end <= 0
// (or end < start) blames from start to the end of the file. start <= 0 blames
// the whole file. The path is passed after "--" so it is never treated as a
// revision or option.
func (g *Git) Blame(ctx context.Context, dir, file string, start, end int) ([]BlameLine, error) {
	args := []string{"blame", "--line-porcelain"}
	if start > 0 {
		rng := fmt.Sprintf("%d,", start)
		if end >= start {
			rng = fmt.Sprintf("%d,%d", start, end)
		}
		args = append(args, "-L", rng)
	}
	args = append(args, "--", file)

	// Raw: the last source line of blame output is a tab and the line's
	// content, and trimming would drop a blank final line entirely.
	out, err := g.runRaw(ctx, dir, nil, args...)
	if err != nil {
		return nil, err
	}

	return parseBlamePorcelain(strings.TrimSuffix(out, "\n")), nil
}

// parseBlamePorcelain parses `git blame --line-porcelain` output. Each line is
// a header ("<sha> <origLine> <finalLine> [group]"), a repeated block of
// key-value metadata, and finally the source content prefixed by a tab.
func parseBlamePorcelain(out string) []BlameLine {
	var (
		lines []BlameLine
		cur   BlameLine
		have  bool
	)

	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ln, "\t"):
			// Source content: closes the current record.
			cur.Content = ln[1:]
			lines = append(lines, cur)
			cur = BlameLine{}
			have = false
		case !have:
			// Header line: "<sha> <orig> <final> [group-size]", where the
			// sha is 40 hex digits, or 64 in a SHA-256 repository.
			fields := strings.Fields(ln)
			if len(fields) >= 3 && (len(fields[0]) == 40 || len(fields[0]) == 64) {
				cur.Commit = fields[0]
				cur.Line, _ = strconv.Atoi(fields[2])
				have = true
			}
		default:
			key, val, ok := strings.Cut(ln, " ")
			if !ok {
				continue
			}
			switch key {
			case "author":
				cur.Author = val
			case "author-mail":
				cur.Email = strings.Trim(val, "<>")
			case "author-time":
				cur.Time, _ = strconv.ParseInt(val, 10, 64)
			case "summary":
				cur.Summary = val
			}
		}
	}

	return lines
}

// repoIDRe validates a full repo id: two or more "/"-separated segments of
// word characters, dots and dashes (host/group/.../name).
var repoIDRe = regexp.MustCompile(`^[\w.-]+(/[\w.-]+)+$`)

// ParseRepoID derives the full repository id "host/group/.../name" from
// common git URL forms: git@host:owner/name.git, ssh://git@host/owner/name.git,
// https://host/group/sub/name(.git). The host and the whole URL path are
// preserved (nested GitLab groups included) so repositories on different git
// servers or in different groups can never collide. A bare "a/b/c" input is
// kept as-is.
func ParseRepoID(url string) (string, error) {
	// A repository is cloned from a remote. A local path or file:// URL would
	// let anyone who can add a repository clone any git directory on this
	// host and read it back through the tools.
	if trimmed := strings.TrimSpace(url); strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, ".") ||
		strings.HasPrefix(strings.ToLower(trimmed), "file:") {
		return "", fmt.Errorf("cannot derive repo id from url %s: local paths are not remote repositories", url)
	}

	s := strings.TrimSuffix(strings.TrimSpace(url), "/")
	s = strings.TrimSuffix(s, ".git")

	var host, path string

	switch {
	case strings.Contains(s, "://"): // https:// or ssh://
		_, rest, ok := strings.Cut(s, "://")
		if !ok {
			return "", fmt.Errorf("invalid git url: %s", url)
		}

		h, p, ok := strings.Cut(rest, "/")
		if !ok {
			return "", fmt.Errorf("invalid git url: %s", url)
		}

		host, path = h, p
	case strings.Contains(s, ":"): // scp-like git@host:owner/name
		h, p, _ := strings.Cut(s, ":")
		host, path = h, p
	default:
		path = s
	}

	// Strip user@ and :port from the host, keeping the bare hostname.
	if host != "" {
		if _, h, ok := strings.Cut(host, "@"); ok {
			host = h
		}
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h
		}
	}

	id := strings.Trim(path, "/")
	if host != "" {
		id = host + "/" + id
	}

	if !repoIDRe.MatchString(id) {
		return "", fmt.Errorf("cannot derive repo id from url: %s", url)
	}
	// The id becomes a directory path under the data dir. A "." or ".."
	// segment would point that path at another repository's clones, or above
	// the repos directory altogether, and the snapshot cleanup deletes what
	// it finds there.
	// A segment beginning with "-" reads as an option to git wherever the id
	// or the URL it came from ends up on a command line.
	for seg := range strings.SplitSeq(id, "/") {
		if seg == "." || seg == ".." || strings.HasPrefix(seg, "-") {
			return "", fmt.Errorf("cannot derive repo id from url %s: path segment %q", url, seg)
		}
	}

	return id, nil
}
