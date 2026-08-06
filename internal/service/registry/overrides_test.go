package registry

import (
	"context"
	"slices"
	"testing"
)

func TestOverridesNormalizeStages(t *testing.T) {
	over := Overrides{
		SkipStages: []string{" CODE_INDEX ", "graph", "code_index", "nonsense", ""},
	}.Normalize()

	// Case and padding are noise; duplicates and typos must not survive, since
	// a stored "nonsence" would look configured while disabling nothing.
	want := []string{StageCodeIndex, StageGraph}
	if !slices.Equal(over.SkipStages, want) {
		t.Fatalf("SkipStages = %v, want %v", over.SkipStages, want)
	}
}

func TestOverridesNormalizeDropsUnknownStagesEntirely(t *testing.T) {
	over := Overrides{SkipStages: []string{"nope"}}.Normalize()

	if over.SkipStages != nil {
		t.Fatalf("SkipStages = %v, want nil", over.SkipStages)
	}
	if !over.Empty() {
		t.Fatal("an override left with nothing valid must report empty")
	}
}

func TestOverridesSkipsStage(t *testing.T) {
	over := Overrides{SkipStages: []string{StageGraph}}

	if !over.SkipsStage(StageGraph) {
		t.Error("graph should be reported as skipped")
	}
	if over.SkipsStage(StageDocs) {
		t.Error("docs was never skipped")
	}
	if (Overrides{}).SkipsStage(StageGraph) {
		t.Error("an empty override skips nothing")
	}
}

// Skipping the graph stage changes what the graph build must do, so it has to
// invalidate the graph exactly like a GraphExclude change does; otherwise a
// repo that just opted out would keep a stale graph forever.
func TestOverridesGraphChangedOnSkipToggle(t *testing.T) {
	base := Overrides{}

	if !base.GraphChanged(Overrides{SkipStages: []string{StageGraph}}) {
		t.Fatal("newly skipping the graph must invalidate it")
	}
	if base.GraphChanged(Overrides{SkipStages: []string{StageCodeIndex}}) {
		t.Fatal("skipping an unrelated stage must not invalidate the graph")
	}
}

func TestOverridesEmptyAccountsForNewFields(t *testing.T) {
	if (Overrides{DocsMaxSourceBytes: 1}).Empty() {
		t.Error("a docs budget alone must count as an override")
	}
	if (Overrides{SkipStages: []string{StageDocs}}).Empty() {
		t.Error("a skip list alone must count as an override")
	}
}

func TestSetOverridesPersistsBudgetsAndSkips(t *testing.T) {
	reg := newTestRegistry(t)
	ctx := context.Background()

	if err := reg.Upsert(ctx, &Repo{ID: "acme/deploy", URL: "https://git/acme/deploy"}); err != nil {
		t.Fatal(err)
	}

	over := Overrides{
		DocsMaxSourceBytes: 512 * 1024,
		SkipStages:         []string{StageCodeIndex, StageGraph},
	}

	repo, prev, err := reg.SetOverrides(ctx, "acme/deploy", over)
	if err != nil {
		t.Fatal(err)
	}
	if !prev.Changed(repo.Overrides) {
		t.Fatal("first write must report a change")
	}
	if repo.Overrides.DocsMaxSourceBytes != 512*1024 {
		t.Fatalf("budget not stored: %+v", repo.Overrides)
	}
	if !repo.Overrides.SkipsStage(StageGraph) {
		t.Fatalf("skip list not stored: %+v", repo.Overrides)
	}

	// The same request in a different order must normalize to the same record,
	// or every save would look like a change and trigger a rebuild.
	_, prev, err = reg.SetOverrides(ctx, "acme/deploy", Overrides{
		DocsMaxSourceBytes: 512 * 1024,
		SkipStages:         []string{StageGraph, StageCodeIndex},
	})
	if err != nil {
		t.Fatal(err)
	}
	if prev.Changed(over.Normalize()) {
		t.Fatal("reordered skip stages must not count as a change")
	}
}
