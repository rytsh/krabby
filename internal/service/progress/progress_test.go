package progress_test

import (
	"context"
	"testing"

	"github.com/rytsh/krabby/internal/service/progress"
)

func TestReportReachesTheReporterInContext(t *testing.T) {
	var done, total, calls int
	ctx := progress.With(context.Background(), func(d, tot int) {
		done, total = d, tot
		calls++
	})

	progress.Report(ctx, 3, 10)
	progress.Report(ctx, 4, 10)

	if calls != 2 || done != 4 || total != 10 {
		t.Fatalf("reporter saw %d calls, last %d/%d; want 2 calls ending 4/10", calls, done, total)
	}
}

// A reporter travels with derived contexts, which is how it crosses a plugin
// boundary whose signature knows nothing about progress.
func TestReporterSurvivesDerivedContexts(t *testing.T) {
	var got int
	ctx := progress.With(context.Background(), func(d, _ int) { got = d })

	child, cancel := context.WithCancel(ctx)
	defer cancel()

	type other struct{}
	progress.Report(context.WithValue(child, other{}, "x"), 7, 0)

	if got != 7 {
		t.Fatalf("derived context reported %d, want 7", got)
	}
}

// Reporting is unconditional at the call site, so every way of having no
// reporter has to be a silent no-op rather than a panic.
func TestReportWithoutAReporterIsANoOp(t *testing.T) {
	progress.Report(context.Background(), 1, 2)
	progress.Report(nil, 1, 2) //nolint:staticcheck // a nil context is tolerated on purpose

	var nilFn progress.Func
	progress.Report(progress.With(context.Background(), nilFn), 1, 2)
}

// With(nil) must not shadow a reporter installed further up.
func TestNilReporterLeavesTheContextAlone(t *testing.T) {
	var got int
	outer := progress.With(context.Background(), func(d, _ int) { got = d })

	inner := progress.With(outer, nil)
	if inner != outer {
		t.Fatal("With(ctx, nil) returned a new context")
	}

	progress.Report(inner, 5, 5)
	if got != 5 {
		t.Fatalf("outer reporter saw %d, want 5", got)
	}
}

func TestInnerReporterReplacesOuter(t *testing.T) {
	var outer, inner int
	ctx := progress.With(context.Background(), func(d, _ int) { outer = d })
	ctx = progress.With(ctx, func(d, _ int) { inner = d })

	progress.Report(ctx, 9, 9)

	if inner != 9 || outer != 0 {
		t.Fatalf("outer=%d inner=%d; want only the inner reporter called", outer, inner)
	}
}
