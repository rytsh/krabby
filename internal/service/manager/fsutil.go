package manager

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))

	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)

	return err == nil
}

// withinDir reports whether path is root itself or lies under it, comparing
// whole path elements. It is the containment test to use before a destructive
// operation: the lexical filepath.HasPrefix would accept "/data/srcs-evil" as
// living under "/data/srcs".
func withinDir(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// removeContentDir deletes a generated-content directory, but only after
// confirming it really lies inside root.
//
// Entity names are ValidName-checked on create, so the guard is not the primary
// defence; it protects the os.RemoveAll against a name that predates the
// current validation, and against an unconfigured root, where the naive
// filepath.Join(root, name) collapses to a bare relative name and would delete
// an arbitrary directory next to the working directory.
//
// It exists as a shared helper because the web-source and API-catalog delete
// paths are copies of one another and the guard was already lost once in the
// copying: only the web-source side had it.
func removeContentDir(root, dir, what string) error {
	if root == "" {
		return nil
	}

	if !withinDir(root, dir) {
		slog.Warn("refusing to remove content outside its root", "kind", what, "dir", dir, "root", root)

		return nil
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s content %s; %w", what, dir, err)
	}

	return nil
}

// dirHasMarkdown reports whether dir contains at least one generated markdown
// document, used to decide whether the docs stage has already produced output.
func dirHasMarkdown(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			return true
		}
	}

	return false
}

// shortSHA trims a git commit hash to its 12-char prefix for readable logs.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}

	return sha
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmp := dst + ".tmp"

	out, err := os.Create(tmp)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		// Without this the failed attempt leaves a partial ".tmp" file next to
		// the destination, which the next run would overwrite but a snapshot
		// sweep would otherwise carry along as if it were content.
		_ = os.Remove(tmp)

		return err
	}

	// Checked, not deferred: Close is where buffered writes surface their
	// error, and renaming a short file over the destination would silently
	// corrupt it.
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)

		return err
	}

	return os.Rename(tmp, dst)
}
