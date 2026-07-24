// Package jira implements the JIRA web-source fetcher: it lists issues of a
// project (or any JQL query) through the JIRA REST API, filters by labels and
// renders each ticket to markdown so the shared docs RAG index can retrieve
// tickets and return them as top items with their original data and a browse
// link back to JIRA.
//
// Auth follows the Atlassian conventions (identical to the Confluence
// fetcher): User (email) + APIToken does basic auth (JIRA Cloud API tokens),
// APIToken alone is sent as a Bearer token (Data Center personal access
// tokens).
//
// JIRA is a discovery source: the JQL result set is the source of truth, so
// tickets that no longer match the query are pruned on the next sync, and
// tickets whose rendered content changed are re-embedded automatically via the
// generic hash-based change detection in the manager.
package jira

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

	"github.com/worldline-go/types"

	"github.com/rytsh/krabby/internal/service/websource"
)

const (
	// pageLimit is the REST page size for the issue search.
	pageLimit = 50
	// maxIssuesDefault caps a sync so a runaway query cannot grind the system.
	// It can be lowered per collection via Config.MaxIssues.
	maxIssuesDefault = 5000
)

// Fetcher syncs one JIRA project / JQL query per collection.
type Fetcher struct {
	client *http.Client
}

// Config is owned entirely by the JIRA provider. Auth follows the Atlassian
// conventions: User+APIToken does basic auth (Cloud API tokens), APIToken
// alone is sent as a Bearer token (Data Center PATs).
//
// Every field is a types.Null so a partial update merges precisely onto the
// stored config (see MergeConfig): absent = keep, null = clear, value =
// override. Use resolve() for plain values in the sync logic.
type Config struct {
	BaseURL  types.Null[string] `json:"base_url"`
	User     types.Null[string] `json:"user,omitempty"`
	APIToken types.Null[string] `json:"api_token,omitempty"`

	// Project selects all issues of a project key (e.g. "PROJ"). Ignored when
	// JQL is set.
	Project types.Null[string] `json:"project,omitempty"`
	// JQL is a raw JIRA query. When set it takes precedence over Project and
	// gives full control over which tickets are indexed.
	JQL types.Null[string] `json:"jql,omitempty"`

	// IncludeLabels, when non-empty, keeps only tickets carrying at least one
	// of these labels. ExcludeLabels (the "skip labels") drop any ticket
	// carrying one of them; excludes win over includes.
	IncludeLabels types.Null[[]string] `json:"include_labels,omitempty"`
	ExcludeLabels types.Null[[]string] `json:"exclude_labels,omitempty"`

	// TeamFields are the JIRA field ids that hold team/squad ownership. These
	// are instance-specific custom fields (e.g. "customfield_104705" for a
	// "Squad" field). Their values are extracted per ticket, written into the
	// indexed markdown (so team names are searchable) and stored on the ticket
	// record so tickets can be listed and filtered by team. The standard
	// "components" field id is also accepted.
	TeamFields types.Null[[]string] `json:"team_fields,omitempty"`

	// MaxIssues caps how many tickets a single sync ingests (0 = default).
	MaxIssues types.Null[int] `json:"max_issues,omitempty"`

	// FullResyncEvery is a Go duration ("24h") controlling how often a full,
	// non-incremental pass runs to reconcile remotely-deleted tickets. Empty or
	// invalid uses the default (24h).
	FullResyncEvery types.Null[string] `json:"full_resync_every,omitempty"`
}

// resolvedConfig is the plain, validated view of a Config used by the sync
// logic (strings trimmed, base URL de-slashed).
type resolvedConfig struct {
	BaseURL         string
	User            string
	APIToken        string
	Project         string
	JQL             string
	IncludeLabels   []string
	ExcludeLabels   []string
	TeamFields      []string
	MaxIssues       int
	FullResyncEvery string
}

// resolve flattens the nullable config into plain values.
func (c Config) resolve() resolvedConfig {
	return resolvedConfig{
		BaseURL:         strings.TrimRight(strings.TrimSpace(c.BaseURL.ValueOrZero()), "/"),
		User:            strings.TrimSpace(c.User.ValueOrZero()),
		APIToken:        c.APIToken.ValueOrZero(),
		Project:         strings.TrimSpace(c.Project.ValueOrZero()),
		JQL:             strings.TrimSpace(c.JQL.ValueOrZero()),
		IncludeLabels:   c.IncludeLabels.ValueOrZero(),
		ExcludeLabels:   c.ExcludeLabels.ValueOrZero(),
		TeamFields:      c.TeamFields.ValueOrZero(),
		MaxIssues:       c.MaxIssues.ValueOrZero(),
		FullResyncEvery: strings.TrimSpace(c.FullResyncEvery.ValueOrZero()),
	}
}

type configView struct {
	BaseURL         string   `json:"base_url"`
	User            string   `json:"user,omitempty"`
	APITokenSet     bool     `json:"api_token_set"`
	Project         string   `json:"project,omitempty"`
	JQL             string   `json:"jql,omitempty"`
	IncludeLabels   []string `json:"include_labels,omitempty"`
	ExcludeLabels   []string `json:"exclude_labels,omitempty"`
	TeamFields      []string `json:"team_fields,omitempty"`
	MaxIssues       int      `json:"max_issues,omitempty"`
	FullResyncEvery string   `json:"full_resync_every,omitempty"`
}

// fullResyncEvery parses the configured interval, falling back to the default.
func (c resolvedConfig) fullResyncEvery() time.Duration {
	if c.FullResyncEvery == "" {
		return websource.DefaultFullResyncEvery
	}
	d, err := time.ParseDuration(c.FullResyncEvery)
	if err != nil || d <= 0 {
		return websource.DefaultFullResyncEvery
	}

	return d
}

// New creates the fetcher.
func New() *Fetcher {
	return &Fetcher{client: &http.Client{Timeout: 60 * time.Second}}
}

// decodeConfig unmarshals the raw config and returns its resolved (plain) form.
func decodeConfig(raw json.RawMessage) (resolvedConfig, error) {
	cfg, err := decodeRawConfig(raw)
	if err != nil {
		return resolvedConfig{}, err
	}

	return cfg.resolve(), nil
}

// decodeRawConfig unmarshals the raw config keeping the nullable fields, so
// MergeConfig can tell set fields from absent ones.
func decodeRawConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode jira config; %w", err)
		}
	}

	return cfg, nil
}

// mergeNull returns the update value when the field was present in the update
// JSON (a value OR explicit null), otherwise the stored value.
func mergeNull[T any](update, stored types.Null[T]) types.Null[T] {
	if update.Valid || update.ParsedNull {
		return update
	}

	return stored
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return err
	}
	if cfg.BaseURL == "" || (!strings.HasPrefix(cfg.BaseURL, "https://") && !strings.HasPrefix(cfg.BaseURL, "http://")) {
		return fmt.Errorf("jira base_url must be an http(s) URL")
	}
	if cfg.Project == "" && cfg.JQL == "" {
		return fmt.Errorf("jira requires a project key or a jql query")
	}

	return nil
}

// MergeConfig merges an update onto the stored config using types.Null
// semantics: a field absent from the update keeps the stored value, an explicit
// null clears it, and a value overrides it. A blank api_token keeps the stored
// secret (tokens are write-only).
func (f *Fetcher) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	next, err := decodeRawConfig(update)
	if err != nil {
		return nil, err
	}

	if len(current) != 0 {
		prev, err := decodeRawConfig(current)
		if err != nil {
			return nil, err
		}

		next.BaseURL = mergeNull(next.BaseURL, prev.BaseURL)
		next.User = mergeNull(next.User, prev.User)
		next.Project = mergeNull(next.Project, prev.Project)
		next.JQL = mergeNull(next.JQL, prev.JQL)
		next.IncludeLabels = mergeNull(next.IncludeLabels, prev.IncludeLabels)
		next.ExcludeLabels = mergeNull(next.ExcludeLabels, prev.ExcludeLabels)
		next.TeamFields = mergeNull(next.TeamFields, prev.TeamFields)
		next.MaxIssues = mergeNull(next.MaxIssues, prev.MaxIssues)
		next.FullResyncEvery = mergeNull(next.FullResyncEvery, prev.FullResyncEvery)

		if next.APIToken.ValueOrZero() == "" {
			next.APIToken = prev.APIToken
		}
	}

	raw, err := json.Marshal(next)
	if err != nil {
		return nil, fmt.Errorf("encode jira config; %w", err)
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
		BaseURL: cfg.BaseURL, User: cfg.User, APITokenSet: cfg.APIToken != "",
		Project: cfg.Project, JQL: cfg.JQL,
		IncludeLabels: cfg.IncludeLabels, ExcludeLabels: cfg.ExcludeLabels,
		TeamFields:      cfg.TeamFields,
		MaxIssues:       cfg.MaxIssues,
		FullResyncEvery: cfg.FullResyncEvery,
	}
}

// issue mirrors the fields we consume from the JIRA search API. The typed
// Fields cover the standard fields; rawFields keeps every field as raw JSON so
// instance-specific team custom fields can be extracted by id.
type issue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Labels      []string        `json:"labels"`
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		IssueType struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Priority struct {
			Name string `json:"name"`
		} `json:"priority"`
		Assignee struct {
			DisplayName string `json:"displayName"`
		} `json:"assignee"`
		Reporter struct {
			DisplayName string `json:"displayName"`
		} `json:"reporter"`
		Created string `json:"created"`
		Updated string `json:"updated"`
	} `json:"fields"`

	// rawFields holds the "fields" object verbatim for custom-field lookups.
	rawFields map[string]json.RawMessage
}

// UnmarshalJSON decodes the typed fields and separately keeps the raw fields
// map so configurable team custom fields can be read by id.
func (i *issue) UnmarshalJSON(data []byte) error {
	type alias issue
	if err := json.Unmarshal(data, (*alias)(i)); err != nil {
		return err
	}

	var envelope struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	i.rawFields = envelope.Fields

	return nil
}

type searchResult struct {
	Issues     []issue `json:"issues"`
	StartAt    int     `json:"startAt"`
	MaxResults int     `json:"maxResults"`
	Total      int     `json:"total"`
}

// jiraTimeLayout is JIRA's JQL datetime format ("2024-01-02 15:04"). JIRA's
// updated field is minute-granular for JQL, so the watermark uses minutes.
const jiraTimeLayout = "2006-01-02 15:04"

// jiraTimestampLayout parses the full timestamp JIRA returns in the updated
// field (ISO 8601 with milliseconds and a numeric zone, e.g.
// "2024-01-02T15:04:05.000+0000").
const jiraTimestampLayout = "2006-01-02T15:04:05.000-0700"

// syncState is the opaque provider watermark persisted between syncs. Watermark
// is the highest issue "updated" time ingested so far, in JIRA JQL format; the
// next incremental fetch asks only for issues updated at or after it. FullAt is
// the time of the last full (non-incremental) pass, used to schedule periodic
// full sweeps that reconcile remotely-deleted tickets.
type syncState struct {
	Watermark string    `json:"watermark,omitempty"`
	FullAt    time.Time `json:"full_at,omitzero"`
}

// Fetch runs the configured JQL, applies label filters and renders each ticket
// to markdown. It is incremental: after the first full sync it fetches only
// issues updated since the stored watermark and returns an advanced watermark,
// so a large project is not re-pulled or re-embedded every cycle. The persisted
// page records are ignored: JIRA is a discovery source, the query is the truth.
func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, rawState json.RawMessage) (*websource.FetchResult, error) {
	cfg, err := decodeConfig(col.Config)
	if err != nil {
		return nil, err
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("jira base_url is required")
	}

	maxIssues := cfg.MaxIssues
	if maxIssues <= 0 {
		maxIssues = maxIssuesDefault
	}

	var state syncState
	if len(rawState) != 0 {
		_ = json.Unmarshal(rawState, &state)
	}

	// Periodically force a full pass so tickets deleted remotely (which an
	// incremental "updated >=" query never returns) are reconciled.
	full := websource.FullResyncDue(state.FullAt, cfg.fullResyncEvery())
	watermark := state.Watermark
	if full {
		watermark = ""
	}
	incremental := watermark != ""
	jql := buildJQL(cfg, watermark)

	var (
		out     []websource.RemotePage
		maxSeen time.Time
	)

	for start := 0; start < maxIssues; start += pageLimit {
		res, err := f.search(ctx, cfg, jql, start)
		if err != nil {
			return nil, err
		}

		for _, iss := range res.Issues {
			if u := parseJiraTime(iss.Fields.Updated); u.After(maxSeen) {
				maxSeen = u
			}

			if !labelSelected(iss.Fields.Labels, cfg.IncludeLabels, cfg.ExcludeLabels) {
				continue
			}

			title := iss.Key
			if iss.Fields.Summary != "" {
				title = iss.Key + ": " + iss.Fields.Summary
			}

			teams := extractTeams(iss, cfg.TeamFields)

			out = append(out, websource.RemotePage{
				// Slug is derived from the immutable issue key so ticket
				// updates map to the same markdown file across syncs.
				Slug:      strings.ToLower(iss.Key),
				Title:     title,
				URL:       cfg.BaseURL + "/browse/" + iss.Key,
				Teams:     teams,
				UpdatedAt: parseJiraTime(iss.Fields.Updated),
				Markdown:  renderIssue(iss, teams),
			})
		}

		if len(res.Issues) == 0 || res.StartAt+len(res.Issues) >= res.Total {
			break
		}
	}

	// Advance the watermark to the newest issue seen. Step back one minute so
	// issues updated within the same minute as the boundary are not skipped by
	// the next run's ">=" (the hash check makes the small re-fetch a no-op).
	nextWatermark := state.Watermark
	if !maxSeen.IsZero() {
		nextWatermark = maxSeen.Add(-time.Minute).Format(jiraTimeLayout)
	}

	fullAt := state.FullAt
	if full {
		fullAt = time.Now()
	}

	nextState, err := json.Marshal(syncState{Watermark: nextWatermark, FullAt: fullAt})
	if err != nil {
		return nil, fmt.Errorf("encode jira sync state; %w", err)
	}

	return &websource.FetchResult{
		Pages:       out,
		Incremental: incremental,
		State:       nextState,
	}, nil
}

// buildJQL returns the effective query. A raw JQL wins (a "updated >="
// watermark clause is AND-ed on for incremental runs); otherwise a project
// filter is built. Results are ordered by updated ascending so a watermark can
// advance monotonically even when the run is capped by MaxIssues.
func buildJQL(cfg resolvedConfig, watermark string) string {
	base := cfg.JQL
	if base == "" {
		base = fmt.Sprintf("project = %q", cfg.Project)
	}

	if watermark != "" {
		base = fmt.Sprintf("(%s) AND updated >= %q", stripOrderBy(base), watermark)
	}

	return base + " ORDER BY updated ASC"
}

// stripOrderBy removes a trailing "ORDER BY ..." clause so we can re-append our
// own ordering after AND-ing the watermark clause.
func stripOrderBy(jql string) string {
	upper := strings.ToUpper(jql)
	if i := strings.LastIndex(upper, "ORDER BY"); i >= 0 {
		return strings.TrimSpace(jql[:i])
	}

	return strings.TrimSpace(jql)
}

// parseJiraTime parses an issue's updated timestamp, tolerating minor format
// differences. A zero time is returned when it cannot be parsed.
func parseJiraTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(jiraTimestampLayout, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}

	return time.Time{}
}

// search fetches one page of the issue search.
func (f *Fetcher) search(ctx context.Context, cfg resolvedConfig, jql string, start int) (*searchResult, error) {
	fields := []string{
		"summary", "description", "labels", "status", "issuetype",
		"priority", "assignee", "reporter", "created", "updated",
	}
	fields = append(fields, cfg.TeamFields...)

	params := url.Values{}
	params.Set("jql", jql)
	params.Set("startAt", strconv.Itoa(start))
	params.Set("maxResults", strconv.Itoa(pageLimit))
	params.Set("fields", strings.Join(fields, ","))

	endpoint := cfg.BaseURL + "/rest/api/2/search?" + params.Encode()

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
		return nil, fmt.Errorf("search jira issues; %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read jira response; %w", err)
	}

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search jira issues: status %s: %s", res.Status, truncate(string(body), 300))
	}

	var result searchResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode jira response; %w", err)
	}

	return &result, nil
}

// renderIssue builds the markdown document for one ticket: a metadata block
// (so status/assignee/labels are searchable) followed by the description. The
// original ticket link is carried on RemotePage.URL, not here.
func renderIssue(iss issue, teams []string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s: %s\n\n", iss.Key, strings.TrimSpace(iss.Fields.Summary))

	meta := [][2]string{
		{"Key", iss.Key},
		{"Type", iss.Fields.IssueType.Name},
		{"Status", iss.Fields.Status.Name},
		{"Priority", iss.Fields.Priority.Name},
		{"Assignee", iss.Fields.Assignee.DisplayName},
		{"Reporter", iss.Fields.Reporter.DisplayName},
		{"Created", iss.Fields.Created},
		{"Updated", iss.Fields.Updated},
	}
	for _, kv := range meta {
		if strings.TrimSpace(kv[1]) != "" {
			fmt.Fprintf(&b, "- **%s:** %s\n", kv[0], kv[1])
		}
	}

	if len(iss.Fields.Labels) > 0 {
		fmt.Fprintf(&b, "- **Labels:** %s\n", strings.Join(iss.Fields.Labels, ", "))
	}

	// Teams are written into the body so team names are searchable by RAG.
	if len(teams) > 0 {
		fmt.Fprintf(&b, "- **Teams:** %s\n", strings.Join(teams, ", "))
	}

	if desc := renderDescription(iss.Fields.Description); desc != "" {
		b.WriteString("\n## Description\n\n")
		b.WriteString(desc)
		b.WriteString("\n")
	}

	return b.String()
}

// renderDescription normalises the JIRA description, which is a plain string on
// Data Center / REST v2 but an Atlassian Document Format (ADF) object on Cloud.
func renderDescription(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}

	// REST v2 plain-string (or wiki-markup) description.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}

	// Cloud Atlassian Document Format: flatten its text nodes.
	var doc adfNode
	if err := json.Unmarshal(raw, &doc); err == nil {
		return strings.TrimSpace(adfText(doc))
	}

	return ""
}

// adfNode is a minimal Atlassian Document Format node used to extract plain
// text from a Cloud description without pulling in a full ADF renderer.
type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text"`
	Content []adfNode `json:"content"`
}

// adfText flattens an ADF tree to text, inserting blank lines between the
// block-level nodes (paragraphs, list items, headings) so the result reads as
// coherent markdown-ish prose.
func adfText(n adfNode) string {
	if n.Type == "text" {
		return n.Text
	}

	var parts []string
	for _, c := range n.Content {
		parts = append(parts, adfText(c))
	}

	switch n.Type {
	case "paragraph", "heading", "listItem", "blockquote", "codeBlock":
		return strings.Join(parts, "") + "\n\n"
	default:
		return strings.Join(parts, "")
	}
}

// labelSelected applies the include/exclude ("skip") label filters to one
// ticket. Excludes win over includes; an empty include list keeps everything
// not excluded.
func labelSelected(ticketLabels, include, exclude []string) bool {
	labels := make(map[string]bool, len(ticketLabels))
	for _, l := range ticketLabels {
		labels[strings.ToLower(strings.TrimSpace(l))] = true
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

// extractTeams reads the configured team custom fields off one issue and
// returns their de-duplicated, human-readable values. JIRA custom-field values
// vary in shape (option object, user object, component, an array of those, or
// a plain string) so each is normalised to text; blank/placeholder values are
// dropped.
func extractTeams(iss issue, fieldIDs []string) []string {
	if len(fieldIDs) == 0 || iss.rawFields == nil {
		return nil
	}

	var teams []string
	seen := map[string]bool{}

	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		key := strings.ToLower(v)
		if seen[key] {
			return
		}
		seen[key] = true
		teams = append(teams, v)
	}

	for _, id := range fieldIDs {
		id = strings.TrimSpace(id)
		raw, ok := iss.rawFields[id]
		if !ok || len(raw) == 0 {
			continue
		}
		for _, v := range fieldValues(raw) {
			add(v)
		}
	}

	return teams
}

// fieldValues normalises one JIRA field value (which may be a scalar, an
// option/user/component object, or an array of them) into display strings.
func fieldValues(raw json.RawMessage) []string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}

	// Plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}
	}

	// Array of values.
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		var out []string
		for _, el := range arr {
			out = append(out, fieldValues(el)...)
		}

		return out
	}

	// Object: options use "value", users "displayName"/"name", components
	// "name".
	var obj struct {
		Value       string `json:"value"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		switch {
		case obj.Value != "":
			return []string{obj.Value}
		case obj.DisplayName != "":
			return []string{obj.DisplayName}
		case obj.Name != "":
			return []string{obj.Name}
		}
	}

	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "…"
}
