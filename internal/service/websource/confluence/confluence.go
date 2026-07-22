// Package confluence implements the Confluence web-source fetcher: it crawls
// every page of a space through the Confluence REST API, filters by labels
// and converts the storage-format HTML to markdown.
//
// Auth follows the Atlassian conventions: User (email) + APIToken does basic
// auth (Confluence Cloud API tokens), APIToken alone is sent as a Bearer
// token (Data Center personal access tokens).
package confluence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/websource"
)

const (
	// pageLimit is the REST page size for content listing.
	pageLimit = 50
	// maxPages caps a space sync so a runaway space cannot grind the system.
	maxPages = 5000
)

// Fetcher syncs one Confluence space per collection.
type Fetcher struct {
	client *http.Client
}

// Config is owned entirely by the Confluence provider. Auth follows the
// Atlassian conventions: User+APIToken does basic auth (Cloud API tokens),
// APIToken alone is sent as a Bearer token (Data Center PATs).
type Config struct {
	BaseURL       string   `json:"base_url"`
	Space         string   `json:"space"`
	User          string   `json:"user,omitempty"`
	APIToken      string   `json:"api_token,omitempty"`
	IncludeLabels []string `json:"include_labels,omitempty"`
	ExcludeLabels []string `json:"exclude_labels,omitempty"`
}

type configView struct {
	BaseURL       string   `json:"base_url"`
	Space         string   `json:"space"`
	User          string   `json:"user,omitempty"`
	APITokenSet   bool     `json:"api_token_set"`
	IncludeLabels []string `json:"include_labels,omitempty"`
	ExcludeLabels []string `json:"exclude_labels,omitempty"`
}

// New creates the fetcher.
func New() *Fetcher {
	return &Fetcher{client: &http.Client{Timeout: 60 * time.Second}}
}

func decodeConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode confluence config; %w", err)
		}
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.Space = strings.TrimSpace(cfg.Space)
	cfg.User = strings.TrimSpace(cfg.User)
	return cfg, nil
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return err
	}
	if cfg.BaseURL == "" || (!strings.HasPrefix(cfg.BaseURL, "https://") && !strings.HasPrefix(cfg.BaseURL, "http://")) {
		return fmt.Errorf("confluence base_url must be an http(s) URL")
	}
	if cfg.Space == "" {
		return fmt.Errorf("confluence space is required")
	}
	return nil
}

func (f *Fetcher) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	next, err := decodeConfig(update)
	if err != nil {
		return nil, err
	}
	if next.APIToken == "" && len(current) != 0 {
		prev, err := decodeConfig(current)
		if err != nil {
			return nil, err
		}
		next.APIToken = prev.APIToken
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return nil, fmt.Errorf("encode confluence config; %w", err)
	}
	if err := f.Validate(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (f *Fetcher) ConfigView(raw json.RawMessage) any {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return nil
	}
	return configView{
		BaseURL: cfg.BaseURL, Space: cfg.Space, User: cfg.User,
		APITokenSet: cfg.APIToken != "", IncludeLabels: cfg.IncludeLabels,
		ExcludeLabels: cfg.ExcludeLabels,
	}
}

// contentPage mirrors the fields we consume from the Confluence content API.
type contentPage struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
	Metadata struct {
		Labels struct {
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		} `json:"labels"`
	} `json:"metadata"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

type contentList struct {
	Results []contentPage `json:"results"`
	Size    int           `json:"size"`
}

// Fetch lists all current pages of the configured space, applies the label
// filters and converts each page to markdown. The persisted page records are
// ignored: Confluence is a discovery source, the space is the truth.
func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page) ([]websource.RemotePage, error) {
	cfg, err := decodeConfig(col.Config)
	if err != nil {
		return nil, err
	}

	base := cfg.BaseURL
	if base == "" {
		return nil, fmt.Errorf("confluence base_url is required")
	}

	if strings.TrimSpace(cfg.Space) == "" {
		return nil, fmt.Errorf("confluence space is required")
	}

	var out []websource.RemotePage

	for start := 0; start < maxPages; start += pageLimit {
		list, err := f.listContent(ctx, cfg, base, start)
		if err != nil {
			return nil, err
		}

		for _, page := range list.Results {
			if !labelSelected(page, cfg.IncludeLabels, cfg.ExcludeLabels) {
				continue
			}

			remote := websource.RemotePage{
				Slug:  page.ID + "-" + websource.Slugify(page.Title),
				Title: page.Title,
				URL:   base + page.Links.WebUI,
			}

			md, err := websource.MarkdownFromHTML(page.Body.Storage.Value)
			if err != nil {
				remote.Err = fmt.Errorf("convert page %s (%s); %w", page.ID, page.Title, err)
			} else {
				remote.Markdown = md
			}

			out = append(out, remote)
		}

		if list.Size < pageLimit {
			break
		}
	}

	return out, nil
}

// listContent fetches one page of the space content listing.
func (f *Fetcher) listContent(ctx context.Context, cfg Config, base string, start int) (*contentList, error) {
	params := url.Values{}
	params.Set("spaceKey", cfg.Space)
	params.Set("type", "page")
	params.Set("status", "current")
	params.Set("limit", strconv.Itoa(pageLimit))
	params.Set("start", strconv.Itoa(start))
	params.Set("expand", "body.storage,metadata.labels")

	endpoint := base + "/rest/api/content?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request; %w", err)
	}

	req.Header.Set("Accept", "application/json")

	switch {
	case cfg.User != "":
		req.SetBasicAuth(cfg.User, cfg.APIToken)
	case cfg.APIToken != "":
		req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	}

	res, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list confluence content; %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read confluence response; %w", err)
	}

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list confluence content: status %s: %s", res.Status, truncate(string(body), 300))
	}

	var list contentList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode confluence response; %w", err)
	}

	return &list, nil
}

// labelSelected applies the include/exclude label filters to one page.
func labelSelected(page contentPage, include, exclude []string) bool {
	labels := map[string]bool{}
	for _, l := range page.Metadata.Labels.Results {
		labels[strings.ToLower(l.Name)] = true
	}

	for _, l := range exclude {
		if labels[strings.ToLower(strings.TrimSpace(l))] {
			return false
		}
	}

	if len(include) == 0 {
		return true
	}

	for _, l := range include {
		if labels[strings.ToLower(strings.TrimSpace(l))] {
			return true
		}
	}

	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "…"
}
