// Package forge implements the "forge" web-source type: issues, pull/merge
// requests and their discussion threads from a code-hosting provider
// (GitHub, GitLab, Gitea/Forgejo, Bitbucket Cloud). Combined with the indexed
// repository it answers "why is this code like this?" with the PR description
// and review conversation. Syncs are incremental by update time.
package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/progress"
	"github.com/rytsh/krabby/internal/service/websource"
)

// Type is the collection type served by this fetcher.
const Type = "forge"

// Providers.
const (
	GitHub    = "github"
	GitLab    = "gitlab"
	Gitea     = "gitea"
	Bitbucket = "bitbucket"
)

const maxCommentsPerItem = 200

// Config is the provider config.
type Config struct {
	Provider           string   `json:"provider"`
	BaseURL            string   `json:"base_url,omitempty"`
	Repo               string   `json:"repo"`
	Token              string   `json:"token,omitempty"`
	User               string   `json:"user,omitempty"`
	Issues             *bool    `json:"issues,omitempty"`
	Pulls              *bool    `json:"pulls,omitempty"`
	Comments           *bool    `json:"comments,omitempty"`
	State              string   `json:"state,omitempty"`
	IncludeLabels      []string `json:"include_labels,omitempty"`
	ExcludeLabels      []string `json:"exclude_labels,omitempty"`
	MaxItems           int      `json:"max_items,omitempty"`
	FullResyncSchedule string   `json:"full_resync_schedule,omitempty"`
}

func on(b *bool) bool { return b == nil || *b }

func (c Config) normalize() Config {
	c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	c.Repo = strings.Trim(strings.TrimSpace(c.Repo), "/")
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.State == "" {
		c.State = "all"
	}
	c.FullResyncSchedule = websource.FullResyncSchedule(c.FullResyncSchedule, "")

	return c
}

// apiBase returns the REST API root for the provider.
func (c Config) apiBase() string { //nolint:staticcheck // ST1016: local receiver names do not affect provider behavior.
	switch c.Provider {
	case GitHub:
		if c.BaseURL == "" || c.BaseURL == "https://github.com" {
			return "https://api.github.com"
		}
		if strings.Contains(c.BaseURL, "/api/") {
			return c.BaseURL
		}
		return c.BaseURL + "/api/v3"
	case GitLab:
		if c.BaseURL == "" {
			return "https://gitlab.com/api/v4"
		}
		if strings.HasSuffix(c.BaseURL, "/api/v4") {
			return c.BaseURL
		}
		return c.BaseURL + "/api/v4"
	case Gitea:
		if strings.HasSuffix(c.BaseURL, "/api/v1") {
			return c.BaseURL
		}
		return c.BaseURL + "/api/v1"
	case Bitbucket:
		if c.BaseURL == "" {
			return "https://api.bitbucket.org/2.0"
		}
		return c.BaseURL
	}

	return c.BaseURL
}

// Item is one normalized issue or pull request.
type Item struct {
	Kind     string // "issue" or "pull"
	Number   string
	Title    string
	Body     string
	State    string
	Author   string
	Labels   []string
	URL      string
	Created  time.Time
	Updated  time.Time
	Branch   string
	Comments []Comment
	// commentsURL / commentsKey is provider-specific, used to load comments.
	commentsURL string
}

// Comment is one discussion entry.
type Comment struct {
	Author  string
	Body    string
	Created time.Time
}

type adapter interface {
	// list streams items updated at or after since (zero = all).
	list(ctx context.Context, since time.Time, fn func(Item) (bool, error)) error
	comments(ctx context.Context, it Item) ([]Comment, error)
}

// Fetcher syncs a forge repository.
type Fetcher struct {
	websource.Schema
	client *http.Client
}

// New creates the fetcher.
func New() *Fetcher {
	return &Fetcher{client: websource.DefaultHTTPClient(), Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:  Type,
		Label: "Git forge issues & PRs",
		Description: "Issues and pull/merge requests with their discussion from GitHub, GitLab, Gitea/Forgejo or Bitbucket Cloud, " +
			"incremental by update time. Pairs with indexed repositories to explain why code changed.",
		Fields: []websource.Field{
			{Key: "provider", Label: "Provider", Kind: websource.FieldSelect, Required: true, Options: []string{GitHub, GitLab, Gitea, Bitbucket}, Default: GitHub},
			{Key: "base_url", Label: "Server URL (self-hosted)", Kind: websource.FieldString, Placeholder: "https://git.example.com", Help: "Empty for github.com, gitlab.com, bitbucket.org"},
			{Key: "repo", Label: "Repository", Kind: websource.FieldString, Required: true, Placeholder: "owner/name (GitLab: group/sub/project)"},
			{Key: "user", Label: "User (Bitbucket app password user)", Kind: websource.FieldString},
			{Key: "token", Label: "Token", Kind: websource.FieldSecret},
			{Key: "issues", Label: "Index issues", Kind: websource.FieldBool, Default: true},
			{Key: "pulls", Label: "Index pull / merge requests", Kind: websource.FieldBool, Default: true},
			{Key: "comments", Label: "Include comments", Kind: websource.FieldBool, Default: true},
			{Key: "state", Label: "State", Kind: websource.FieldSelect, Options: []string{"all", "open", "closed"}, Default: "all"},
			{Key: "include_labels", Label: "Include labels", Kind: websource.FieldList},
			{Key: "exclude_labels", Label: "Exclude labels", Kind: websource.FieldList, Placeholder: "wontfix, duplicate"},
			{Key: "max_items", Label: "Max items per sync (0 = no limit)", Kind: websource.FieldNumber},
			{Key: "full_resync_schedule", Label: "Full re-sync schedule (cron)", Kind: websource.FieldString, Placeholder: websource.DefaultFullResyncSchedule},
		},
	}}}
}

func decode(raw json.RawMessage) (Config, error) {
	cfg, err := websource.DecodeConfig[Config](raw, Type)

	return cfg.normalize(), err
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	switch cfg.Provider {
	case GitHub, GitLab, Bitbucket:
	case Gitea:
		if cfg.BaseURL == "" {
			return fmt.Errorf("gitea needs base_url")
		}
	default:
		return fmt.Errorf("forge provider must be github, gitlab, gitea or bitbucket")
	}
	if cfg.BaseURL != "" {
		if err := websource.ValidHTTPURL(cfg.BaseURL); err != nil {
			return fmt.Errorf("base_url %w", err)
		}
	}
	if cfg.Repo == "" || (cfg.Provider != GitLab && strings.Count(cfg.Repo, "/") != 1) {
		return fmt.Errorf("forge repo must be owner/name")
	}
	if !on(cfg.Issues) && !on(cfg.Pulls) {
		return fmt.Errorf("enable issues, pulls or both")
	}
	switch cfg.State {
	case "all", "open", "closed":
	default:
		return fmt.Errorf("state must be all, open or closed")
	}

	return websource.ValidateFullResyncSchedule(cfg.FullResyncSchedule)
}

func (f *Fetcher) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	merged, err := f.MergeObjects(current, update)
	if err != nil {
		return nil, err
	}

	return merged, f.Validate(merged)
}

func (f *Fetcher) ConfigView(raw json.RawMessage) any { return f.View(raw) }

func (f *Fetcher) FullResyncSpec(raw json.RawMessage) (string, error) {
	cfg, err := decode(raw)

	return cfg.FullResyncSchedule, err
}

func (f *Fetcher) adapter(cfg Config) adapter {
	h := &httpClient{client: f.client, cfg: cfg}
	switch cfg.Provider {
	case GitLab:
		return &gitlab{h}
	case Gitea:
		return &gitea{h}
	case Bitbucket:
		return &bitbucket{h}
	default:
		return &github{h}
	}
}

func labelSelected(labels, include, exclude []string) bool {
	has := func(set []string) bool {
		for _, l := range labels {
			for _, s := range set {
				if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(s)) {
					return true
				}
			}
		}
		return false
	}
	if has(exclude) {
		return false
	}

	return len(include) == 0 || has(include)
}

func (cfg Config) wants(it Item) bool {
	if it.Kind == "pull" && !on(cfg.Pulls) || it.Kind == "issue" && !on(cfg.Issues) {
		return false
	}

	return labelSelected(it.Labels, cfg.IncludeLabels, cfg.ExcludeLabels)
}

func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	var out websource.PreviewResult
	if err := f.Validate(raw); err != nil {
		return out, err
	}
	cfg, _ := decode(raw)
	limit := cfg.MaxItems
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	out.Limit = limit
	err := f.adapter(cfg).list(ctx, time.Time{}, func(it Item) (bool, error) {
		out.Scanned++
		if cfg.wants(it) {
			out.ItemCount++
		}
		if out.Scanned >= limit {
			out.Truncated = true
			return true, nil
		}
		return false, nil
	})

	return out, err
}

type syncState struct {
	Since  time.Time `json:"since,omitzero"`
	FullAt time.Time `json:"full_at,omitzero"`
	Filter string    `json:"filter,omitempty"`
}

func filterSignature(cfg Config) string {
	return websource.Hash(fmt.Sprint(cfg.Provider, cfg.apiBase(), cfg.Repo, on(cfg.Issues), on(cfg.Pulls), on(cfg.Comments),
		cfg.State, cfg.IncludeLabels, cfg.ExcludeLabels))
}

func slugFor(it Item) string { return it.Kind + "-" + websource.Slugify(it.Number) }

func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, rawState json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	var state syncState
	if len(rawState) != 0 {
		_ = json.Unmarshal(rawState, &state)
	}
	filter := filterSignature(cfg)
	full := filter != state.Filter || websource.FullResyncDue(state.FullAt, cfg.FullResyncSchedule, time.Now())
	since := state.Since
	if full {
		since = time.Time{}
	}

	ad := f.adapter(cfg)
	maxSeen := since
	count := 0
	truncated := false
	var removed []string

	err = ad.list(ctx, since, func(it Item) (bool, error) {
		if it.Updated.After(maxSeen) {
			maxSeen = it.Updated
		}
		if cfg.MaxItems > 0 && count >= cfg.MaxItems {
			truncated = true
			return true, nil
		}
		count++
		progress.Report(ctx, count, 0)
		if !cfg.wants(it) {
			// An incremental run may see an item whose labels changed out of
			// the selection: drop it now rather than at the next full sweep.
			if !full {
				removed = append(removed, slugFor(it))
			}
			return false, nil
		}
		if on(cfg.Comments) {
			comments, err := ad.comments(ctx, it)
			if err != nil {
				return false, emit(websource.RemotePage{Slug: slugFor(it), Title: it.Title, URL: it.URL, Err: err})
			}
			it.Comments = comments
		}

		return false, emit(render(cfg, it))
	})
	if err != nil {
		return nil, err
	}

	next := syncState{Since: maxSeen, FullAt: state.FullAt, Filter: filter}
	if !maxSeen.IsZero() {
		next.Since = maxSeen.Add(-time.Second)
	}
	if full {
		next.FullAt = time.Now()
	}
	raw, _ := json.Marshal(next)

	return &websource.FetchResult{Complete: full && !truncated, Removed: removed, State: raw}, nil
}

func render(cfg Config, it Item) websource.RemotePage {
	kind := "Issue"
	if it.Kind == "pull" {
		kind = "Pull request"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s #%s: %s\n\n", kind, it.Number, strings.TrimSpace(it.Title))
	for _, kv := range [][2]string{
		{"Repository", cfg.Repo}, {"Type", kind}, {"State", it.State}, {"Author", it.Author},
		{"Labels", strings.Join(it.Labels, ", ")}, {"Branch", it.Branch},
		{"Created", fmtTime(it.Created)}, {"Updated", fmtTime(it.Updated)},
	} {
		if strings.TrimSpace(kv[1]) != "" {
			fmt.Fprintf(&b, "- **%s**: %s\n", kv[0], kv[1])
		}
	}
	if body := strings.TrimSpace(it.Body); body != "" {
		b.WriteString("\n## Description\n\n" + body + "\n")
	}
	if len(it.Comments) > 0 {
		b.WriteString("\n## Discussion\n")
		for _, c := range it.Comments {
			body := strings.TrimSpace(c.Body)
			if body == "" {
				continue
			}
			fmt.Fprintf(&b, "\n### %s (%s)\n\n%s\n", firstNonEmpty(c.Author, "unknown"), fmtTime(c.Created), body)
		}
	}

	return websource.RemotePage{
		Slug: slugFor(it), Title: fmt.Sprintf("#%s: %s", it.Number, it.Title),
		URL: it.URL, UpdatedAt: it.Updated, Teams: it.Labels, Markdown: b.String(),
	}
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.UTC().Format("2006-01-02 15:04")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}

	return ""
}

// httpClient issues authenticated JSON GETs against the provider API.
type httpClient struct {
	client *http.Client
	cfg    Config
}

func (h *httpClient) get(ctx context.Context, endpoint string, out any) (*websource.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	switch h.cfg.Provider {
	case GitHub:
		req.Header.Set("Accept", "application/vnd.github+json")
		if h.cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+h.cfg.Token)
		}
	case GitLab:
		if h.cfg.Token != "" {
			req.Header.Set("PRIVATE-TOKEN", h.cfg.Token)
		}
	case Gitea:
		if h.cfg.Token != "" {
			req.Header.Set("Authorization", "token "+h.cfg.Token)
		}
	default:
		websource.ApplyAuth(req, h.cfg.User, h.cfg.Token)
	}
	res, err := websource.Do(h.client, req, 32<<20)
	if err != nil {
		return nil, err
	}
	if out != nil {
		if err := json.Unmarshal(res.Body, out); err != nil {
			return nil, fmt.Errorf("decode %s response; %w", h.cfg.Provider, err)
		}
	}

	return res, nil
}

// pageLinks walks a Link-header paginated listing.
func (h *httpClient) pageLinks(ctx context.Context, first string, decode func([]byte) (bool, error)) error {
	next := first
	for page := 0; next != "" && page < 10_000; page++ {
		res, err := h.get(ctx, next, nil)
		if err != nil {
			return err
		}
		stop, err := decode(res.Body)
		if err != nil || stop {
			return err
		}
		next = websource.NextLink(res.Header)
		if next != "" && !websource.SameOrigin(first, next) {
			return fmt.Errorf("pagination link changed origin")
		}
	}

	return nil
}

func repoPath(repo string) string {
	parts := strings.Split(repo, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}

	return strings.Join(parts, "/")
}
