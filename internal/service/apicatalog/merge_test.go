package apicatalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func decodeUpdate(t *testing.T, body string) ServiceUpdate {
	t.Helper()

	var u ServiceUpdate
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}

	return u
}

func storedService() *Service {
	return &Service{
		Name:            "billing",
		Group:           "finance",
		Description:     "Invoices and payments",
		BaseURL:         "https://billing.internal",
		SpecPatch:       json.RawMessage(`{"info":{"title":"Billing"}}`),
		Operations:      map[string]OperationOverride{"listInvoices": {Summary: "List"}},
		RefreshInterval: 24 * time.Hour,
		Specs:           []string{"0 3 * * *"},
	}
}

// A request that changes one field must not disturb the others: that is the
// whole reason the update distinguishes "absent" from "null".
func TestApplyLeavesUnmentionedFieldsAlone(t *testing.T) {
	svc := storedService()

	rerender, err := decodeUpdate(t, `{"description":"Invoices, payments and dunning"}`).Apply(svc)
	if err != nil {
		t.Fatal(err)
	}
	if rerender {
		t.Error("a description change asked for a re-render")
	}

	want := storedService()
	want.Description = "Invoices, payments and dunning"

	got, _ := json.Marshal(svc)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) || svc.RefreshInterval != want.RefreshInterval {
		t.Errorf("service after a description-only update:\n got %s\nwant %s", got, exp)
	}
}

func TestApplyNullClears(t *testing.T) {
	svc := storedService()

	rerender, err := decodeUpdate(t, `{
		"group": null, "description": null, "base_url": null,
		"spec_patch": null, "operations": null, "refresh_interval": null, "specs": null
	}`).Apply(svc)
	if err != nil {
		t.Fatal(err)
	}
	if !rerender {
		t.Error("clearing the base URL, patch and overrides did not ask for a re-render")
	}

	if svc.Group != "" || svc.Description != "" || svc.BaseURL != "" {
		t.Errorf("text fields not cleared: %+v", svc)
	}
	if svc.SpecPatch != nil || svc.Operations != nil {
		t.Errorf("patch or overrides not cleared: %s %v", svc.SpecPatch, svc.Operations)
	}
	if svc.RefreshInterval != 0 || len(svc.Specs) != 0 {
		t.Errorf("schedule not cleared: %v %v", svc.RefreshInterval, svc.Specs)
	}
	if svc.Name != "billing" {
		t.Errorf("name changed to %q; it identifies the service and is not updatable", svc.Name)
	}
}

// What an operation renders to depends on the base URL, the patch and the
// overrides. Changing any of them without re-rendering leaves stale documents
// in the index, because the source document itself did not move.
func TestApplyReportsWhenOperationsNeedReRendering(t *testing.T) {
	for name, tt := range map[string]struct {
		body     string
		rerender bool
	}{
		"description only":              {`{"description":"x"}`, false},
		"group only":                    {`{"group":"ops"}`, false},
		"schedule only":                 {`{"refresh_interval":"1h","specs":["@daily"]}`, false},
		"same base url":                 {`{"base_url":"https://billing.internal"}`, false},
		"same base url, trailing slash": {`{"base_url":"https://billing.internal/"}`, false},
		"new base url":                  {`{"base_url":"https://billing.staging"}`, true},
		"same patch, other whitespace":  {`{"spec_patch":{ "info" : {"title":"Billing"} }}`, false},
		"new patch":                     {`{"spec_patch":{"info":{"title":"Other"}}}`, true},
		"patch cleared":                 {`{"spec_patch":{}}`, true},
		"overrides replaced":            {`{"operations":{"listInvoices":{"hidden":true}}}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := decodeUpdate(t, tt.body).Apply(storedService())
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.rerender {
				t.Errorf("rerender = %v, want %v", got, tt.rerender)
			}
		})
	}
}

func TestApplyNormalizesValues(t *testing.T) {
	svc := storedService()

	if _, err := decodeUpdate(t, `{
		"group": "  Platform ",
		"description": "  trimmed  ",
		"base_url": " https://api.example.com/v2/ ",
		"specs": [" 0 3 * * * ", "", "  "]
	}`).Apply(svc); err != nil {
		t.Fatal(err)
	}

	if svc.Group != "platform" {
		t.Errorf("group = %q, want it lowercased and trimmed", svc.Group)
	}
	if svc.Description != "trimmed" {
		t.Errorf("description = %q", svc.Description)
	}
	if svc.BaseURL != "https://api.example.com/v2" {
		t.Errorf("base_url = %q, want it trimmed without the trailing slash", svc.BaseURL)
	}
	if len(svc.Specs) != 1 || svc.Specs[0] != "0 3 * * *" {
		t.Errorf("specs = %q, want blanks dropped", svc.Specs)
	}
}

// "default" is how the unset group is displayed; storing it would give the
// same bucket two spellings.
func TestApplyFoldsDefaultGroupToUnset(t *testing.T) {
	svc := storedService()

	if _, err := decodeUpdate(t, `{"group":"Default"}`).Apply(svc); err != nil {
		t.Fatal(err)
	}
	if svc.Group != "" || svc.EffectiveGroup() != GroupDefault {
		t.Errorf("group stored as %q, shown as %q", svc.Group, svc.EffectiveGroup())
	}
}

func TestApplyRefreshInterval(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"90m":    90 * time.Minute,
		" 24h ":  24 * time.Hour,
		"":       0,
		"manual": 0,
	} {
		svc := storedService()
		body, _ := json.Marshal(map[string]string{"refresh_interval": raw})

		if _, err := decodeUpdate(t, string(body)).Apply(svc); err != nil {
			t.Errorf("refresh_interval %q: %v", raw, err)

			continue
		}
		if svc.RefreshInterval != want {
			t.Errorf("refresh_interval %q stored as %v, want %v", raw, svc.RefreshInterval, want)
		}
	}
}

func TestApplyRejectsInvalidValues(t *testing.T) {
	for name, tt := range map[string]struct{ body, wantErr string }{
		"group with a slash":      {`{"group":"a/b"}`, "invalid group name"},
		"group with a space":      {`{"group":"two words"}`, "invalid group name"},
		"relative base url":       {`{"base_url":"/api"}`, "http(s) URL"},
		"non-http base url":       {`{"base_url":"file:///etc/passwd"}`, "http(s) URL"},
		"scalar patch":            {`{"spec_patch":"replace everything"}`, "must be a JSON object"},
		"array patch":             {`{"spec_patch":[1,2]}`, "must be a JSON object"},
		"unparseable interval":    {`{"refresh_interval":"soon"}`, "refresh_interval"},
		"override with empty key": {`{"operations":{"  ":{"hidden":true}}}`, "empty operation id"},
	} {
		t.Run(name, func(t *testing.T) {
			svc := storedService()
			before, _ := json.Marshal(svc)

			_, err := decodeUpdate(t, tt.body).Apply(svc)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}

			// The rejected field must not have been written.
			if after, _ := json.Marshal(svc); string(after) != string(before) {
				t.Errorf("a rejected update changed the service:\nbefore %s\n after %s", before, after)
			}
		})
	}
}

func TestSpecPatchSizeLimit(t *testing.T) {
	big := `{"x":"` + strings.Repeat("a", maxSpecPatchBytes) + `"}`

	if _, err := normalizeSpecPatch(json.RawMessage(big)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("an oversized patch was accepted: %v", err)
	}
}

// Every way of writing "no patch" stores the same thing, so comparing against
// the previous patch cannot mistake one empty form for a change.
func TestSpecPatchEmptyForms(t *testing.T) {
	for _, raw := range []string{"", "  ", "null", "{}", " {} "} {
		got, err := normalizeSpecPatch(json.RawMessage(raw))
		if err != nil || got != nil {
			t.Errorf("normalizeSpecPatch(%q) = %s, %v; want nil", raw, got, err)
		}
	}
}

func TestOverridesAreTrimmedAndPruned(t *testing.T) {
	got, err := normalizeOverrides(map[string]OperationOverride{
		" listInvoices ": {Summary: "  List invoices ", Tags: []string{" billing ", "", "  "}},
		"noop":           {Summary: "  ", Description: "", Tags: []string{" "}},
		"hidden":         {Hidden: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("kept %d overrides, want the two that do something: %v", len(got), got)
	}
	if _, ok := got["noop"]; ok {
		t.Error("an override with no effect was stored")
	}

	list, ok := got["listInvoices"]
	if !ok {
		t.Fatalf("override key was not trimmed: %v", got)
	}
	if list.Summary != "List invoices" || len(list.Tags) != 1 || list.Tags[0] != "billing" {
		t.Errorf("override not trimmed: %+v", list)
	}
	if !got["hidden"].Hidden {
		t.Error("a hide-only override was dropped")
	}
}

func TestOverridesThatDoNothingStoreNothing(t *testing.T) {
	for name, in := range map[string]map[string]OperationOverride{
		"nil":       nil,
		"empty":     {},
		"all no-op": {"a": {}, "b": {Summary: " "}},
	} {
		got, err := normalizeOverrides(in)
		if err != nil || got != nil {
			t.Errorf("%s: normalizeOverrides = %v, %v; want nil", name, got, err)
		}
	}
}

func TestOverrideCountLimit(t *testing.T) {
	in := make(map[string]OperationOverride, maxOperationOverrides+1)
	for i := range maxOperationOverrides + 1 {
		in["op"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+strings.Repeat("y", i/26)] = OperationOverride{Hidden: true}
	}
	if len(in) != maxOperationOverrides+1 {
		t.Fatalf("fixture built %d distinct keys", len(in))
	}

	if _, err := normalizeOverrides(in); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("an oversized override map was accepted: %v", err)
	}
}

func TestOverrideLookup(t *testing.T) {
	svc := storedService()

	if ov, ok := svc.Override("listInvoices"); !ok || ov.Summary != "List" {
		t.Errorf("Override(listInvoices) = %+v, %v", ov, ok)
	}
	if _, ok := svc.Override("missing"); ok {
		t.Error("Override reported an entry that does not exist")
	}
	if _, ok := (Service{}).Override("anything"); ok {
		t.Error("Override on a service with no overrides reported one")
	}
}

func TestEffectiveDescription(t *testing.T) {
	if got := (Service{Description: "human", SpecSummary: "spec"}).EffectiveDescription(); got != "human" {
		t.Errorf("got %q, want the human-written description to win", got)
	}
	if got := (Service{SpecSummary: "spec"}).EffectiveDescription(); got != "spec" {
		t.Errorf("got %q, want the spec summary as the fallback", got)
	}
}

func TestEffectiveSpecs(t *testing.T) {
	for name, tt := range map[string]struct {
		svc  Service
		want []string
	}{
		"cron wins over the interval": {
			Service{Specs: []string{" 0 3 * * * "}, RefreshInterval: time.Hour}, []string{"0 3 * * *"},
		},
		"interval when there is no cron": {
			Service{RefreshInterval: 90 * time.Minute}, []string{"@every 1h30m0s"},
		},
		"blank cron entries fall back to the interval": {
			Service{Specs: []string{"", "  "}, RefreshInterval: time.Hour}, []string{"@every 1h0m0s"},
		},
		"manual only": {Service{}, nil},
	} {
		got := tt.svc.EffectiveSpecs()
		if strings.Join(got, "|") != strings.Join(tt.want, "|") || (tt.want == nil) != (got == nil) {
			t.Errorf("%s: EffectiveSpecs() = %q, want %q", name, got, tt.want)
		}
	}
}
