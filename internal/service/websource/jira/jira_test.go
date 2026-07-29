package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/service/websource"
)

func TestLabelSelected(t *testing.T) {
	labels := []string{"Published", "Wine"}

	tests := []struct {
		name             string
		include, exclude []string
		want             bool
	}{
		{name: "no filters", want: true},
		{name: "included", include: []string{"wine"}, want: true},
		{name: "include case insensitive", include: []string{"PUBLISHED"}, want: true},
		{name: "missing include", include: []string{"beer"}, want: false},
		{name: "skip label excluded", exclude: []string{"wine"}, want: false},
		{name: "exclude wins", include: []string{"published"}, exclude: []string{"WINE"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := labelSelected(labels, tt.include, tt.exclude); got != tt.want {
				t.Fatalf("labelSelected() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigMergeAndRedaction(t *testing.T) {
	f := New()
	current := json.RawMessage(`{"base_url":"https://jira.example.com","project":"OLD","user":"a@example.com","api_token":"secret"}`)
	update := json.RawMessage(`{"base_url":"https://jira.example.com/","project":"PROJ","user":"a@example.com","api_token":"","exclude_labels":["wontfix"]}`)

	merged, err := f.MergeConfig(current, update)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decodeConfig(merged)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIToken != "secret" || cfg.Project != "PROJ" || cfg.BaseURL != "https://jira.example.com" {
		t.Fatalf("merged config = %#v", cfg)
	}
	if len(cfg.ExcludeLabels) != 1 || cfg.ExcludeLabels[0] != "wontfix" {
		t.Fatalf("skip labels not preserved: %#v", cfg.ExcludeLabels)
	}

	view, ok := f.ConfigView(merged).(configView)
	if !ok || !view.APITokenSet {
		t.Fatalf("redacted view = %#v", view)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("secret leaked in view: %s", raw)
	}
}

func TestIncludeSubtasksMerge(t *testing.T) {
	f := New()
	base := json.RawMessage(`{"base_url":"https://j.example.com","project":"PROJ"}`)

	// Default: off. A config written before the option existed reads as off.
	cfg, err := decodeConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IncludeSubtasks {
		t.Fatal("sub-tasks must be excluded by default")
	}

	on, err := f.MergeConfig(base, json.RawMessage(`{"include_subtasks":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = decodeConfig(on); err != nil || !cfg.IncludeSubtasks {
		t.Fatalf("include_subtasks not set: %#v (%v)", cfg, err)
	}

	// An update that does not mention it keeps the stored value...
	kept, err := f.MergeConfig(on, json.RawMessage(`{"project":"OTHER"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = decodeConfig(kept); err != nil || !cfg.IncludeSubtasks {
		t.Fatalf("include_subtasks not preserved: %#v (%v)", cfg, err)
	}

	// ...and an explicit false turns it back off.
	off, err := f.MergeConfig(on, json.RawMessage(`{"include_subtasks":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = decodeConfig(off); err != nil || cfg.IncludeSubtasks {
		t.Fatalf("include_subtasks not cleared: %#v (%v)", cfg, err)
	}
}

func TestValidate(t *testing.T) {
	f := New()

	tests := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "project", raw: `{"base_url":"https://j.example.com","project":"PROJ"}`, ok: true},
		{name: "jql", raw: `{"base_url":"https://j.example.com","jql":"assignee = currentUser()"}`, ok: true},
		{name: "full cron", raw: `{"base_url":"https://j.example.com","project":"PROJ","full_resync_schedule":"0 3 * * *"}`, ok: true},
		{name: "invalid full cron", raw: `{"base_url":"https://j.example.com","project":"PROJ","full_resync_schedule":"tomorrow"}`, ok: false},
		{name: "missing base_url", raw: `{"project":"PROJ"}`, ok: false},
		{name: "missing selector", raw: `{"base_url":"https://j.example.com"}`, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := f.Validate(json.RawMessage(tt.raw))
			if (err == nil) != tt.ok {
				t.Fatalf("Validate() err = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestBuildJQL(t *testing.T) {
	// Raw JQL is preserved, with its ordering replaced by the monotonic one.
	if got := buildJQL(resolvedConfig{JQL: "status = Done ORDER BY created DESC"}, ""); got != "status = Done ORDER BY updated ASC" {
		t.Fatalf("raw jql = %q", got)
	}

	// Project builds a project filter ordered ascending (monotonic watermark).
	got := buildJQL(resolvedConfig{Project: "PROJ"}, "")
	if !strings.Contains(got, "project =") || !strings.Contains(got, "ORDER BY updated ASC") {
		t.Fatalf("project jql = %q", got)
	}

	// Incremental: the watermark clause is AND-ed on and any prior ORDER BY is
	// stripped before re-appending ours.
	inc := buildJQL(resolvedConfig{JQL: "status = Done ORDER BY created DESC"}, "2024-01-02 15:04")
	if !strings.Contains(inc, `updated >= "2024-01-02 15:04"`) {
		t.Fatalf("watermark clause missing: %q", inc)
	}
	if strings.Contains(inc, "created DESC") {
		t.Fatalf("prior ORDER BY not stripped: %q", inc)
	}
	if strings.Count(strings.ToUpper(inc), "ORDER BY") != 1 {
		t.Fatalf("expected exactly one ORDER BY: %q", inc)
	}
}

func TestParseJiraTime(t *testing.T) {
	if parseJiraTime("2024-01-02T15:04:05.000+0000").IsZero() {
		t.Fatal("failed to parse JIRA timestamp")
	}
	if !parseJiraTime("garbage").IsZero() {
		t.Fatal("garbage should parse to zero time")
	}
}

func TestRenderIssuePlainDescription(t *testing.T) {
	var iss issue
	iss.Key = "PROJ-1"
	iss.Fields.Summary = "Login fails"
	iss.Fields.Status.Name = "Open"
	iss.Fields.Labels = []string{"bug", "backend"}
	iss.Fields.Description = json.RawMessage(`"Users cannot log in after the last deploy."`)

	md := renderIssue(iss, []string{"FinOps - CUST"})
	for _, want := range []string{
		"# PROJ-1: Login fails",
		"**Status:** Open",
		"**Labels:** bug, backend",
		"**Teams:** FinOps - CUST",
		"## Description",
		"Users cannot log in",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("rendered markdown missing %q:\n%s", want, md)
		}
	}
}

func TestExtractTeams(t *testing.T) {
	// Mirrors the real jira.worldline-solutions.com shapes: Squad is a single
	// option object, Team is an array of option objects, plus a plain string.
	raw := []byte(`{
		"key":"OFS-1",
		"fields":{
			"summary":"x",
			"customfield_104705":{"value":"FinOps - CUST"},
			"customfield_110643":[{"value":"No specific Team"},{"value":"Payments"}],
			"customfield_99999":"Platform",
			"customfield_empty":null
		}
	}`)

	var iss issue
	if err := json.Unmarshal(raw, &iss); err != nil {
		t.Fatal(err)
	}

	got := extractTeams(iss, []string{"customfield_104705", "customfield_110643", "customfield_99999", "customfield_empty", "customfield_missing"})
	want := []string{"FinOps - CUST", "No specific Team", "Payments", "Platform"}

	if len(got) != len(want) {
		t.Fatalf("extractTeams() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("extractTeams()[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestExtractTeamsNoConfig(t *testing.T) {
	raw := []byte(`{"key":"OFS-1","fields":{"summary":"x","customfield_104705":{"value":"FinOps"}}}`)
	var iss issue
	if err := json.Unmarshal(raw, &iss); err != nil {
		t.Fatal(err)
	}
	if got := extractTeams(iss, nil); got != nil {
		t.Fatalf("extractTeams(nil fields) = %v, want nil", got)
	}
}

// jiraServer serves one page of search results and records the JQL it was
// asked for.
func jiraServer(t *testing.T, issues string) (*httptest.Server, *[]string) {
	t.Helper()

	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("jql"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"startAt":0,"maxResults":50,"total":2,"issues":[%s]}`, issues)
	}))
	t.Cleanup(srv.Close)

	return srv, &queries
}

// twoIssues is a story plus one sub-task of it, in the shape the search API
// returns (issuetype.subtask is JIRA's own classification).
const twoIssues = `
	{"key":"PROJ-1","fields":{"summary":"Story","updated":"2024-01-02T15:04:05.000+0000",
		"issuetype":{"name":"Story","subtask":false}}},
	{"key":"PROJ-2","fields":{"summary":"Sub-task","updated":"2024-01-02T15:04:05.000+0000",
		"issuetype":{"name":"Sub-task","subtask":true}}}`

func fetchSlugs(t *testing.T, baseURL, config string, state json.RawMessage) ([]string, *websource.FetchResult) {
	t.Helper()

	f := New()
	col := &websource.Collection{
		Name:   "tickets",
		Type:   websource.TypeJira,
		Config: json.RawMessage(fmt.Sprintf(config, baseURL)),
	}

	var slugs []string
	res, err := f.Fetch(context.Background(), col, nil, state, func(p websource.RemotePage) error {
		slugs = append(slugs, p.Slug)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return slugs, res
}

func TestFetchSkipsSubtasksByDefault(t *testing.T) {
	srv, _ := jiraServer(t, twoIssues)

	slugs, _ := fetchSlugs(t, srv.URL, `{"base_url":%q,"project":"PROJ"}`, nil)
	if len(slugs) != 1 || slugs[0] != "proj-1" {
		t.Fatalf("emitted %v, want only the parent story", slugs)
	}
}

func TestFetchIncludeSubtasks(t *testing.T) {
	srv, _ := jiraServer(t, twoIssues)

	slugs, _ := fetchSlugs(t, srv.URL, `{"base_url":%q,"project":"PROJ","include_subtasks":true}`, nil)
	if len(slugs) != 2 {
		t.Fatalf("emitted %v, want both issues", slugs)
	}
}

func TestPreviewCountsFilteredIssuesWithoutContentFields(t *testing.T) {
	t.Parallel()

	var requestedFields string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedFields = r.URL.Query().Get("fields")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"startAt":0,"maxResults":50,"total":2,"issues":[%s]}`, twoIssues)
	}))
	defer srv.Close()

	raw := json.RawMessage(fmt.Sprintf(`{"base_url":%q,"project":"PROJ"}`, srv.URL))
	got, err := New().Preview(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ItemCount != 1 || got.Scanned != 2 || got.Total != 2 || got.Truncated {
		t.Fatalf("Preview() = %+v, want 1 selected from 2", got)
	}
	if strings.Contains(requestedFields, "description") || strings.Contains(requestedFields, "summary") {
		t.Fatalf("preview downloaded content fields: %q", requestedFields)
	}
}

func TestPreviewRespectsMaxIssuesExactly(t *testing.T) {
	t.Parallel()

	srv, _ := jiraServer(t, twoIssues)
	raw := json.RawMessage(fmt.Sprintf(`{"base_url":%q,"project":"PROJ","include_subtasks":true,"max_issues":1}`, srv.URL))
	got, err := New().Preview(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ItemCount != 1 || got.Scanned != 1 || !got.Truncated {
		t.Fatalf("Preview() = %+v, want one scanned item and truncation", got)
	}
}

// A collection synced before the sub-task filter existed carries a watermark
// but no filter signature. Its next run must be a full pass so it reports
// Complete and the manager prunes the sub-tasks already in the index; an
// incremental run would leave them there until the next scheduled sweep.
func TestFetchForcesFullPassWhenFilterChanged(t *testing.T) {
	srv, queries := jiraServer(t, twoIssues)

	cfg := `{"base_url":%q,"project":"PROJ"}`
	stale, err := json.Marshal(syncState{Watermark: "2024-01-01 00:00", FullAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}

	_, res := fetchSlugs(t, srv.URL, cfg, stale)
	if !res.Complete {
		t.Fatal("changed filter must force a full, prunable pass")
	}
	if strings.Contains((*queries)[0], "updated >=") {
		t.Fatalf("full pass must not carry the watermark clause: %q", (*queries)[0])
	}

	// Same filter next time: back to incremental, no re-walk of the project.
	_, res2 := fetchSlugs(t, srv.URL, cfg, res.State)
	if res2.Complete {
		t.Fatal("unchanged filter should resume incremental syncing")
	}
	if !strings.Contains((*queries)[1], "updated >=") {
		t.Fatalf("incremental pass missing watermark clause: %q", (*queries)[1])
	}

	// Flipping include_subtasks is itself a filter change.
	_, res3 := fetchSlugs(t, srv.URL, `{"base_url":%q,"project":"PROJ","include_subtasks":true}`, res.State)
	if !res3.Complete {
		t.Fatal("toggling include_subtasks must force a full pass")
	}
}

func TestRenderDescriptionADF(t *testing.T) {
	adf := json.RawMessage(`{
		"type":"doc",
		"content":[
			{"type":"paragraph","content":[{"type":"text","text":"First line."}]},
			{"type":"paragraph","content":[{"type":"text","text":"Second line."}]}
		]
	}`)

	got := renderDescription(adf)
	if !strings.Contains(got, "First line.") || !strings.Contains(got, "Second line.") {
		t.Fatalf("ADF flatten = %q", got)
	}
}
