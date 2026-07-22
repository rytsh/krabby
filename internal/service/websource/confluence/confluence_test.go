package confluence

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLabelSelected(t *testing.T) {
	var page contentPage
	page.Metadata.Labels.Results = append(page.Metadata.Labels.Results,
		struct {
			Name string `json:"name"`
		}{Name: "Published"},
		struct {
			Name string `json:"name"`
		}{Name: "Wine"},
	)

	tests := []struct {
		name             string
		include, exclude []string
		want             bool
	}{
		{name: "no filters", want: true},
		{name: "included", include: []string{"wine"}, want: true},
		{name: "include case insensitive", include: []string{"PUBLISHED"}, want: true},
		{name: "missing include", include: []string{"beer"}, want: false},
		{name: "excluded", exclude: []string{"wine"}, want: false},
		{name: "exclude wins", include: []string{"published"}, exclude: []string{"WINE"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := labelSelected(page, tt.include, tt.exclude); got != tt.want {
				t.Fatalf("labelSelected() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigMergeAndRedaction(t *testing.T) {
	f := New()
	current := json.RawMessage(`{"base_url":"https://wiki.example.com","space":"OLD","user":"a@example.com","api_token":"secret"}`)
	update := json.RawMessage(`{"base_url":"https://wiki.example.com/","space":"WINE","user":"a@example.com","api_token":"","include_labels":["published"]}`)

	merged, err := f.MergeConfig(current, update)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decodeConfig(merged)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIToken != "secret" || cfg.Space != "WINE" || cfg.BaseURL != "https://wiki.example.com" {
		t.Fatalf("merged config = %#v", cfg)
	}

	view, ok := f.ConfigView(merged).(configView)
	if !ok || !view.APITokenSet {
		t.Fatalf("redacted view = %#v", view)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" || strings.Contains(string(raw), "secret") {
		t.Fatalf("secret leaked in view: %s", raw)
	}
}
