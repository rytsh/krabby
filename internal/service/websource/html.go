package websource

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	readability "github.com/go-shiori/go-readability"
)

// MarkdownFromHTML converts an HTML fragment or document to markdown without
// any content extraction. Use for sources that already return content-only
// HTML (e.g. the Confluence storage format).
func MarkdownFromHTML(html string) (string, error) {
	md, err := htmltomarkdown.ConvertString(html)
	if err != nil {
		return "", fmt.Errorf("convert html to markdown; %w", err)
	}

	return strings.TrimSpace(md), nil
}

// ExtractArticle runs readability over a full HTML page (dropping navigation,
// sidebars and other chrome) and converts the main content to markdown. It
// returns the extracted title and the markdown body.
func ExtractArticle(html, pageURL string) (title, markdown string, err error) {
	u, err := url.Parse(pageURL)
	if err != nil {
		return "", "", fmt.Errorf("parse page url %s; %w", pageURL, err)
	}

	article, err := readability.FromReader(strings.NewReader(html), u)
	if err != nil {
		return "", "", fmt.Errorf("extract readable content from %s; %w", pageURL, err)
	}

	content := article.Content
	if strings.TrimSpace(content) == "" {
		// Readability found no article node (e.g. very plain pages): fall back
		// to converting the full document.
		content = html
	}

	md, err := MarkdownFromHTML(content)
	if err != nil {
		return "", "", err
	}

	return strings.TrimSpace(article.Title), md, nil
}

// Hash fingerprints markdown content for change detection.
func Hash(content string) string {
	sum := sha256.Sum256([]byte(content))

	return hex.EncodeToString(sum[:16])
}

// Slugify converts a title or URL fragment into a filesystem/URL-safe slug.
// Non-alphanumeric runs collapse to single dashes; the result is lowercase
// and capped at 80 characters. Empty inputs yield "page".
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
		return "page"
	}

	return slug
}
