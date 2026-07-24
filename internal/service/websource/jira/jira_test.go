package jira

import (
	"encoding/json"
	"strings"
	"testing"
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

func TestValidate(t *testing.T) {
	f := New()

	tests := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "project", raw: `{"base_url":"https://j.example.com","project":"PROJ"}`, ok: true},
		{name: "jql", raw: `{"base_url":"https://j.example.com","jql":"assignee = currentUser()"}`, ok: true},
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
	// Raw JQL is preserved, with our ordering appended.
	if got := buildJQL(resolvedConfig{JQL: "status = Done"}, ""); got != "status = Done ORDER BY updated ASC" {
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
