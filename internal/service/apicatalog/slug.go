package apicatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Hash fingerprints rendered content for change detection.
func Hash(content string) string {
	sum := sha256.Sum256([]byte(content))

	return hex.EncodeToString(sum[:16])
}

// Slugify converts free text into a filesystem/URL-safe slug. Non-alphanumeric
// runs collapse to single dashes; the result is lowercase and capped at 80
// characters. Empty inputs yield "op".
func Slugify(s string) string {
	var b strings.Builder

	lastDash := true // avoid a leading dash
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}

	slug := strings.Trim(b.String(), "-")
	if len(slug) > 80 {
		slug = strings.Trim(slug[:80], "-")
	}

	if slug == "" {
		return "op"
	}

	return slug
}

// OpSlug builds the stable per-operation slug.
//
// It is derived from the method and path rather than from operationId on
// purpose. operationId is optional, is not guaranteed unique, and is renamed
// freely between spec revisions — deriving the identity from it means a
// cosmetic rename deletes a document and re-embeds an identical one. Method
// plus path is the operation's actual identity in both OpenAPI and gRPC, and it
// only changes when the endpoint genuinely moves.
//
// The trailing hash disambiguates paths that slugify to the same string
// ("/v1/a-b" and "/v1/a/b") and keeps the slug bounded regardless of how long
// the path is.
func OpSlug(method, path string) string {
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" {
		method = "op"
	}

	base := Slugify(method + "-" + path)
	if len(base) > 60 {
		base = strings.Trim(base[:60], "-")
	}

	return base + "-" + Hash(strings.ToUpper(method) + " " + path)[:8]
}
