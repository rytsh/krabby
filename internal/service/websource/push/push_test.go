package push_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/push"
)

// A push collection is filled by its clients. If Fetch emitted or removed
// anything, the sync that follows it would overwrite or prune what they pushed.
func TestFetchLeavesPushedItemsAlone(t *testing.T) {
	f := push.New()

	stored := []*websource.Page{{Slug: "release-notes"}, {Slug: "runbook"}}
	emitted := 0

	res, err := f.Fetch(context.Background(), &websource.Collection{Name: "ci", Type: push.Type}, stored, nil,
		func(websource.RemotePage) error {
			emitted++

			return nil
		})
	if err != nil {
		t.Fatal(err)
	}

	if emitted != 0 {
		t.Errorf("Fetch emitted %d pages, want none", emitted)
	}
	if res == nil {
		t.Fatal("Fetch returned no result")
	}
	// Complete would tell the manager the emitted set is the whole collection,
	// and with nothing emitted that prunes every stored item.
	if res.Complete {
		t.Error("Fetch reported a complete sweep, which would prune every pushed item")
	}
	if len(res.Removed) != 0 {
		t.Errorf("Fetch reported removals %v, want none", res.Removed)
	}
}

func TestDescriptorMarksItemsAsClientManaged(t *testing.T) {
	d := push.New().Describe()

	if d.Type != websource.TypePush {
		t.Errorf("type = %q, want %q", d.Type, websource.TypePush)
	}
	if !d.ManualItems {
		t.Error("push type does not declare its items as client-managed")
	}
	if d.Preview {
		t.Error("push type advertises a preview it cannot produce")
	}
	if len(d.Fields) != 0 {
		t.Errorf("push type declares config fields %v, want none", d.Fields)
	}
}

// The type takes no configuration, so anything a client sends is a mistake
// worth reporting rather than storing.
func TestConfigAcceptsNothing(t *testing.T) {
	f := push.New()

	for _, ok := range []json.RawMessage{nil, json.RawMessage(`{}`)} {
		if err := f.Validate(ok); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", ok, err)
		}
	}

	if err := f.Validate(json.RawMessage(`{"url":"https://example.com"}`)); err == nil {
		t.Error("an unknown config field was accepted")
	}
	if _, err := f.MergeConfig(nil, json.RawMessage(`{"token":"x"}`)); err == nil {
		t.Error("MergeConfig stored an unknown field")
	}

	merged, err := f.MergeConfig(json.RawMessage(`{}`), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(merged, &obj); err != nil || len(obj) != 0 {
		t.Errorf("merged empty config = %s (%v), want {}", merged, err)
	}
}
