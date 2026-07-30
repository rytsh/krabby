// Package pages implements the "pages" web-source fetcher: it re-fetches the
// page URLs a user registered on the collection, extracts the readable
// content and converts it to markdown.
package pages

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/progress"
	"github.com/rytsh/krabby/internal/service/websource"
)

// maxBodyBytes caps a fetched page body (HTML pages beyond this are almost
// certainly not prose worth indexing).
const maxBodyBytes = 8 << 20 // 8 MiB

const (
	maxSitemapBytes = 50 << 20
	maxSitemapURLs  = 50_000
	maxSitemapFiles = 100
)

// CredentialFunc resolves optional auth material for a page URL. A non-empty
// username selects basic auth; a bare secret is sent as a Bearer token.
// Returning empty values fetches anonymously.
type CredentialFunc func(ctx context.Context, pageURL string) (username, secret string, err error)

// Fetcher fetches user-registered page URLs.
type Fetcher struct {
	client *http.Client
	creds  CredentialFunc
}

// New creates the fetcher. creds may be nil for anonymous fetching.
func New(creds CredentialFunc) *Fetcher {
	return &Fetcher{
		client: &http.Client{Timeout: 60 * time.Second},
		creds:  creds,
	}
}

func (f *Fetcher) Validate(_ json.RawMessage) error { return nil }

func (f *Fetcher) MergeConfig(_, _ json.RawMessage) (json.RawMessage, error) { return nil, nil }

func (f *Fetcher) ConfigView(_ json.RawMessage) any { return nil }

type sitemapDocument struct {
	XMLName  xml.Name
	URLs     []sitemapLocation `xml:"url"`
	Children []sitemapLocation `xml:"sitemap"`
}

type sitemapLocation struct {
	Location string `xml:"loc"`
}

// SitemapURLs fetches a sitemap URL and returns its page URLs. Sitemap indexes
// are followed recursively, with protocol-sized bounds to keep a bad endpoint
// from consuming unbounded memory or requests.
func (f *Fetcher) SitemapURLs(ctx context.Context, sitemapURL string) ([]string, error) {
	seenSitemaps := make(map[string]struct{})
	seenURLs := make(map[string]struct{})
	urls := make([]string, 0)

	var visit func(string) error
	visit = func(current string) error {
		current = strings.TrimSpace(current)
		if _, ok := seenSitemaps[current]; ok {
			return nil
		}
		if len(seenSitemaps) >= maxSitemapFiles {
			return fmt.Errorf("sitemap index exceeds %d files", maxSitemapFiles)
		}
		if err := validateHTTPURL(current); err != nil {
			return fmt.Errorf("invalid sitemap url %q; %w", current, err)
		}
		seenSitemaps[current] = struct{}{}

		doc, err := f.fetchSitemap(ctx, current)
		if err != nil {
			return err
		}

		switch doc.XMLName.Local {
		case "urlset":
			for _, entry := range doc.URLs {
				pageURL := strings.TrimSpace(entry.Location)
				if validateHTTPURL(pageURL) != nil {
					continue
				}
				if _, ok := seenURLs[pageURL]; ok {
					continue
				}
				if len(urls) >= maxSitemapURLs {
					return fmt.Errorf("sitemap exceeds %d page urls", maxSitemapURLs)
				}
				seenURLs[pageURL] = struct{}{}
				urls = append(urls, pageURL)
			}
		case "sitemapindex":
			for _, entry := range doc.Children {
				child := resolveURL(current, entry.Location)
				if err := visit(child); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("fetch %s; unsupported sitemap root %q", current, doc.XMLName.Local)
		}

		return nil
	}

	if err := visit(sitemapURL); err != nil {
		return nil, err
	}

	return urls, nil
}

func (f *Fetcher) fetchSitemap(ctx context.Context, sitemapURL string) (*sitemapDocument, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sitemapURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build sitemap request; %w", err)
	}
	req.Header.Set("User-Agent", "krabby-websource/1.0")
	req.Header.Set("Accept", "application/xml,text/xml,application/gzip")
	if err := f.setCredentials(ctx, req, sitemapURL); err != nil {
		return nil, err
	}

	res, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s; %w", sitemapURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s; unexpected status %s", sitemapURL, res.Status)
	}

	body, err := readSitemapBody(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s; %w", sitemapURL, err)
	}

	var doc sitemapDocument
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse %s; %w", sitemapURL, err)
	}

	return &doc, nil
}

func readSitemapBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxSitemapBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxSitemapBytes {
		return nil, fmt.Errorf("sitemap exceeds %d bytes", maxSitemapBytes)
	}
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return body, nil
	}

	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("open gzip sitemap; %w", err)
	}
	defer zr.Close()

	body, err = io.ReadAll(io.LimitReader(zr, maxSitemapBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decompress sitemap; %w", err)
	}
	if len(body) > maxSitemapBytes {
		return nil, fmt.Errorf("decompressed sitemap exceeds %d bytes", maxSitemapBytes)
	}

	return body, nil
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL")
	}

	return nil
}

func resolveURL(baseURL, ref string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return strings.TrimSpace(ref)
	}
	rel, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return strings.TrimSpace(ref)
	}

	return base.ResolveReference(rel).String()
}

// Fetch re-fetches every registered page. Per-page failures are reported via
// RemotePage.Err so one broken URL never aborts the whole collection sync.
//
// The result is never Complete: the inventory of a URL-list collection is the
// registered page set itself, maintained through the add/remove page endpoints
// rather than discovered remotely, so there is nothing for the manager to
// prune. Every registered page is emitted here (a failed one included), so the
// distinction is academic — but claiming an authority this provider does not
// have would be one refactor away from deleting the user's page list. Pages
// authored directly in Krabby have no URL and are skipped because their stored
// Markdown, rather than a remote response, is authoritative.
func (f *Fetcher) Fetch(ctx context.Context, _ *websource.Collection, pages []*websource.Page, _ json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	for i, p := range pages {
		// Pages authored directly in Krabby have no remote URL. Their stored
		// Markdown is authoritative and must survive collection refreshes.
		if strings.TrimSpace(p.URL) == "" {
			progress.Report(ctx, i+1, len(pages))

			continue
		}

		remote := websource.RemotePage{Slug: p.Slug, URL: p.URL, Title: p.Title}

		title, md, err := f.fetchOne(ctx, p.URL)
		if err != nil {
			remote.Err = err
		} else {
			remote.Markdown = md
			if title != "" {
				remote.Title = title
			}
		}

		if err := emit(remote); err != nil {
			return nil, err
		}

		progress.Report(ctx, i+1, len(pages))
	}

	return &websource.FetchResult{}, nil
}

func (f *Fetcher) fetchOne(ctx context.Context, pageURL string) (title, markdown string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("build request; %w", err)
	}

	req.Header.Set("User-Agent", "krabby-websource/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	if err := f.setCredentials(ctx, req, pageURL); err != nil {
		return "", "", err
	}

	res, err := f.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch %s; %w", pageURL, err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetch %s; unexpected status %s", pageURL, res.Status)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))
	if err != nil {
		return "", "", fmt.Errorf("read %s; %w", pageURL, err)
	}

	return websource.ExtractArticle(string(body), pageURL)
}

func (f *Fetcher) setCredentials(ctx context.Context, req *http.Request, pageURL string) error {
	if f.creds == nil {
		return nil
	}

	user, secret, err := f.creds(ctx, pageURL)
	if err != nil {
		return fmt.Errorf("resolve credentials; %w", err)
	}
	switch {
	case user != "":
		req.SetBasicAuth(user, secret)
	case secret != "":
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	return nil
}
