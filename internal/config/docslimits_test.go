package config

import "testing"

func TestDocsLimitsResolveFillsUnsetFields(t *testing.T) {
	got := DocsLimits{MaxSourceBytes: 512 * 1024}.Resolve()

	if got.MaxSourceBytes != 512*1024 {
		t.Errorf("MaxSourceBytes = %d, want the explicit value kept", got.MaxSourceBytes)
	}
	if got.MaxGroupBytes != DefaultDocsMaxGroupBytes {
		t.Errorf("MaxGroupBytes = %d, want default %d", got.MaxGroupBytes, DefaultDocsMaxGroupBytes)
	}
	if got.MaxSynthesisBytes != DefaultDocsMaxSynthesisBytes {
		t.Errorf("MaxSynthesisBytes = %d, want default %d", got.MaxSynthesisBytes, DefaultDocsMaxSynthesisBytes)
	}
}

func TestDocsLimitsResolveTreatsNegativeAsUnset(t *testing.T) {
	got := DocsLimits{MaxSourceBytes: -1}.Resolve()

	if got.MaxSourceBytes != DefaultDocsMaxSourceBytes {
		t.Errorf("MaxSourceBytes = %d, want default %d", got.MaxSourceBytes, DefaultDocsMaxSourceBytes)
	}
}

func TestDocsLimitsMergeIsPerField(t *testing.T) {
	global := DocsLimits{
		MaxSourceBytes:    100,
		MaxGroupBytes:     200,
		MaxSynthesisBytes: 300,
	}

	// A repo raising only one budget must not reset the other two to defaults:
	// that is the whole point of a partial override.
	got := global.Merge(DocsLimits{MaxSourceBytes: 999})

	if got.MaxSourceBytes != 999 {
		t.Errorf("MaxSourceBytes = %d, want the override 999", got.MaxSourceBytes)
	}
	if got.MaxGroupBytes != 200 || got.MaxSynthesisBytes != 300 {
		t.Errorf("untouched budgets changed: %+v", got)
	}
}

func TestDocsLimitsMergeDoesNotMutateReceiver(t *testing.T) {
	global := DocsLimits{MaxSourceBytes: 100}
	_ = global.Merge(DocsLimits{MaxSourceBytes: 999})

	if global.MaxSourceBytes != 100 {
		t.Errorf("receiver mutated: %+v", global)
	}
}

func TestDocsLimitsEmpty(t *testing.T) {
	if !(DocsLimits{}).Empty() {
		t.Error("zero DocsLimits should be empty")
	}
	if (DocsLimits{MaxGroupBytes: 1}).Empty() {
		t.Error("a set field should make it non-empty")
	}
}

func TestDocsOverrideEmptyAccountsForLimits(t *testing.T) {
	if (DocsOverride{Limits: DocsLimits{MaxSourceBytes: 1}}).Empty() {
		t.Error("an override carrying only a limit must not report empty, or it would be dropped")
	}
}
