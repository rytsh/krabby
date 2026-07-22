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

var repoIDRe = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)

// ParseRepoID extracts "owner/name" from common git URL forms:
// git@host:owner/name.git, ssh://git@host/owner/name.git, https://host/owner/name(.git).
func ParseRepoID(url string) (string, error) {
	s := strings.TrimSuffix(strings.TrimSpace(url), "/")
	s = strings.TrimSuffix(s, ".git")

	var path string

	switch {
	case strings.Contains(s, "://"): // https:// or ssh://
		_, rest, ok := strings.Cut(s, "://")
		if !ok {
			return "", fmt.Errorf("invalid git url: %s", url)
		}

		_, p, ok := strings.Cut(rest, "/")
		if !ok {
			return "", fmt.Errorf("invalid git url: %s", url)
		}

		path = p
	case strings.Contains(s, ":"): // scp-like git@host:owner/name
		_, p, _ := strings.Cut(s, ":")
		path = p
	default:
		path = s
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("cannot derive owner/name from url: %s", url)
	}

	id := parts[len(parts)-2] + "/" + parts[len(parts)-1]
	if !repoIDRe.MatchString(id) {
		return "", fmt.Errorf("cannot derive owner/name from url: %s", url)
	}

	return id, nil
}
