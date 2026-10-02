package bigpicture

import (
	"fmt"
	"path"
	"strings"

	"github.com/rytsh/krabby/internal/service/repofs"
)

// NormalizeRepoPattern validates a repository id glob such as
// "github.com/acme/**". Segments use path.Match syntax and "**" spans any
// number of segments. A trailing "/" is shorthand for the whole subtree and a
// leading URL scheme is dropped, so a pasted "https://github.com/acme/" works.
func NormalizeRepoPattern(pattern string) (string, error) {
	p := strings.TrimSpace(pattern)
	for _, scheme := range []string{"https://", "http://"} {
		if len(p) >= len(scheme) && strings.EqualFold(p[:len(scheme)], scheme) {
			p = p[len(scheme):]
		}
	}
	if strings.HasSuffix(p, "/") {
		p += "**"
	}
	p = strings.TrimPrefix(p, "/")
	if p == "" || len(p) > 512 || strings.ContainsAny(p, "\\\r\n\x00") {
		return "", fmt.Errorf("%w: invalid repository pattern", ErrInvalid)
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: invalid repository pattern %q", ErrInvalid, pattern)
		}
		if strings.Contains(segment, "**") && segment != "**" {
			return "", fmt.Errorf("%w: \"**\" must be a whole path segment in %q", ErrInvalid, pattern)
		}
		// Surface malformed character classes now instead of silently
		// matching nothing at research time.
		if _, err := path.Match(segment, ""); err != nil {
			return "", fmt.Errorf("%w: invalid repository pattern %q", ErrInvalid, pattern)
		}
	}
	return p, nil
}

// MatchRepoPattern reports whether a repository id matches a normalized
// pattern. Matching is case-insensitive because hosts and most forges treat
// owner/repository names that way.
func MatchRepoPattern(pattern, repoID string) bool {
	return repofs.MatchGlob(strings.ToLower(pattern), strings.ToLower(repoID))
}
