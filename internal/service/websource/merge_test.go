package websource

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/worldline-go/types"
)

// TestMergeNull covers the rule every partial-update surface shares.
func TestMergeNull(t *testing.T) {
	t.Parallel()

	stored := types.NewNull("stored")

	var absent types.Null[string]
	if got := MergeNull(absent, stored); got.ValueOrZero() != "stored" {
		t.Fatalf("absent update did not keep the stored value: %#v", got)
	}

	explicitNull := types.Null[string]{ParsedNull: true}
	if got := MergeNull(explicitNull, stored); got.Valid || got.ValueOrZero() != "" {
		t.Fatalf("explicit null did not clear: %#v", got)
	}

	if got := MergeNull(types.NewNull("new"), stored); got.ValueOrZero() != "new" {
		t.Fatalf("value did not override: %#v", got)
	}
}

// TestCollectionUpdateApply is the regression test for the destructive update:
// a request that only changed the description used to wipe the schedule and
// the refresh interval, because the whole envelope was replaced.
func TestCollectionUpdateApply(t *testing.T) {
	t.Parallel()

	stored := func() *Collection {
		return &Collection{
			Name:            "wiki",
			Type:            "confluence",
			Description:     "team wiki",
			RefreshInterval: 6 * time.Hour,
			Specs:           []string{"0 2 * * *"},
		}
	}

	t.Run("omitted fields are kept", func(t *testing.T) {
		var update CollectionUpdate
		if err := json.Unmarshal([]byte(`{"description":"renamed"}`), &update); err != nil {
			t.Fatal(err)
		}

		col := stored()
		if err := update.Apply(col); err != nil {
			t.Fatal(err)
		}
		if col.Description != "renamed" {
			t.Fatalf("description = %q", col.Description)
		}
		if !slices.Equal(col.Specs, []string{"0 2 * * *"}) {
			t.Fatalf("updating the description wiped the schedule: %#v", col.Specs)
		}
		if col.RefreshInterval != 6*time.Hour {
			t.Fatalf("updating the description wiped the interval: %v", col.RefreshInterval)
		}
	})

	t.Run("explicit null clears", func(t *testing.T) {
		var update CollectionUpdate
		if err := json.Unmarshal([]byte(`{"specs":null,"description":null}`), &update); err != nil {
			t.Fatal(err)
		}

		col := stored()
		if err := update.Apply(col); err != nil {
			t.Fatal(err)
		}
		if len(col.Specs) != 0 {
			t.Fatalf("null specs did not clear: %#v", col.Specs)
		}
		if col.Description != "" {
			t.Fatalf("null description did not clear: %q", col.Description)
		}
		if col.RefreshInterval != 6*time.Hour {
			t.Fatalf("unmentioned interval changed: %v", col.RefreshInterval)
		}
	})

	t.Run("values override", func(t *testing.T) {
		var update CollectionUpdate
		if err := json.Unmarshal([]byte(`{"specs":["@every 30m"," "],"refresh_interval":"12h"}`), &update); err != nil {
			t.Fatal(err)
		}

		col := stored()
		if err := update.Apply(col); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(col.Specs, []string{"@every 30m"}) {
			t.Fatalf("specs = %#v; blanks must be dropped", col.Specs)
		}
		if col.RefreshInterval != 12*time.Hour {
			t.Fatalf("refresh interval = %v", col.RefreshInterval)
		}
	})

	t.Run("manual clears the interval", func(t *testing.T) {
		for _, body := range []string{`{"refresh_interval":"manual"}`, `{"refresh_interval":""}`, `{"refresh_interval":null}`} {
			var update CollectionUpdate
			if err := json.Unmarshal([]byte(body), &update); err != nil {
				t.Fatal(err)
			}

			col := stored()
			if err := update.Apply(col); err != nil {
				t.Fatal(err)
			}
			if col.RefreshInterval != 0 {
				t.Fatalf("%s left the interval at %v", body, col.RefreshInterval)
			}
		}
	})

	t.Run("a bad duration is rejected", func(t *testing.T) {
		var update CollectionUpdate
		if err := json.Unmarshal([]byte(`{"refresh_interval":"soon"}`), &update); err != nil {
			t.Fatal(err)
		}
		if err := update.Apply(stored()); err == nil {
			t.Fatal("invalid duration accepted")
		}
	})
}
