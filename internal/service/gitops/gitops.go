// Package gitops shells out to git for clone/fetch/pull with per-repo auth
// (SSH key or HTTPS token) and an optional global SSH key fallback.
package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

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

	if keyPath != "" {
		env = append(env, fmt.Sprintf(
			"GIT_SSH_COMMAND=ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new",
			keyPath,
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

func (g *Git) run(ctx context.Context, dir string, auth *credentials.Auth, args ...string) (string, error) {
	full := args
	if auth != nil && auth.Token != "" {
		// Reset any system credential helpers, then install ours.
		full = append([]string{"-c", "credential.helper=", "-c", "credential.helper=" + tokenHelper}, args...)
	}

	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = g.env(auth)

	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s; %w; %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}

	return strings.TrimSpace(out.String()), nil
}

// Clone clones url into dest. Branch may be empty for the default branch.
func (g *Git) Clone(ctx context.Context, url, branch, dest string, auth *credentials.Auth) error {
	args := []string{"clone", "--single-branch"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, url, dest)

	if _, err := g.run(ctx, "", auth, args...); err != nil {
		return err
	}

	return nil
}

// SetRemoteURL points an existing clone at url. Snapshot preparation first
// clones the active local version for speed, then restores the real origin
// before fetching updates.
func (g *Git) SetRemoteURL(ctx context.Context, dir, url string) error {
	_, err := g.run(ctx, dir, nil, "remote", "set-url", "origin", url)

	return err
}

// Fetch updates remote refs.
func (g *Git) Fetch(ctx context.Context, dir string, auth *credentials.Auth) error {
	_, err := g.run(ctx, dir, auth, "fetch", "--prune", "origin")

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

// Pull fast-forwards the working tree to the remote branch.
func (g *Git) Pull(ctx context.Context, dir string, auth *credentials.Auth) error {
	_, err := g.run(ctx, dir, auth, "pull", "--ff-only")

	return err
}

// DiffNames returns the repo-relative paths that differ between two commits
// (added, modified and deleted). Renames are reported as delete+add so callers
// can treat every returned path uniformly: drop old data, index what exists.
func (g *Git) DiffNames(ctx context.Context, dir, from, to string) ([]string, error) {
	out, err := g.run(ctx, dir, nil, "diff", "--name-only", "--no-renames", from, to)
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

	out, err := g.run(ctx, dir, nil, args...)
	if err != nil {
		return nil, err
	}

	return parseBlamePorcelain(out), nil
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
			// Header line: "<40-hex-sha> <orig> <final> [group-size]".
			fields := strings.Fields(ln)
			if len(fields) >= 3 && len(fields[0]) == 40 {
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

	return id, nil
}
