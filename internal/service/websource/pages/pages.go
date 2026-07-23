// Package pages implements the "pages" web-source fetcher: it re-fetches the
// page URLs a user registered on the collection, extracts the readable
// content and converts it to markdown.
package pages

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rytsh/krabby/internal/service/websource"
)

// maxBodyBytes caps a fetched page body (HTML pages beyond this are almost
// certainly not prose worth indexing).
const maxBodyBytes = 8 << 20 // 8 MiB

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

// Fetch re-fetches every registered page. Per-page failures are reported via
// RemotePage.Err so one broken URL never aborts the whole collection sync.
// URL-list collections are always a full fetch (no incremental state).
func (f *Fetcher) Fetch(ctx context.Context, _ *websource.Collection, pages []*websource.Page, _ json.RawMessage) (*websource.FetchResult, error) {
	out := make([]websource.RemotePage, 0, len(pages))

	for _, p := range pages {
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

		out = append(out, remote)
	}

	return &websource.FetchResult{Pages: out}, nil
}

func (f *Fetcher) fetchOne(ctx context.Context, pageURL string) (title, markdown string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("build request; %w", err)
	}

	req.Header.Set("User-Agent", "krabby-websource/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	if f.creds != nil {
		user, secret, cerr := f.creds(ctx, pageURL)
		if cerr != nil {
			return "", "", fmt.Errorf("resolve credentials; %w", cerr)
		}

		switch {
		case user != "":
			req.SetBasicAuth(user, secret)
		case secret != "":
			req.Header.Set("Authorization", "Bearer "+secret)
		}
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
