package manager

import (
	"sync"
	"testing"
	"time"
)

// TestProgressETA covers the estimate itself: it must stay quiet until it has
// something to say, and otherwise extrapolate the observed rate.
func TestProgressETA(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return start.Add(d) }

	for _, tc := range []struct {
		name string
		p    Progress
		now  time.Time
		want int
	}{
		{
			name: "indeterminate phase has no estimate",
			p:    Progress{Phase: "fetch", StartedAt: start},
			now:  at(time.Minute),
		},
		{
			name: "nothing done yet",
			p:    Progress{Phase: "index", Total: 100, StartedAt: start},
			now:  at(time.Minute),
		},
		{
			name: "too early to extrapolate",
			p:    Progress{Phase: "index", Done: 10, Total: 100, StartedAt: start},
			now:  at(time.Second),
		},
		{
			name: "too few items to extrapolate",
			p:    Progress{Phase: "index", Done: 2, Total: 100, StartedAt: start},
			now:  at(time.Minute),
		},
		{
			// 10 items in 10s = 1/s, 90 left.
			name: "extrapolates the observed rate",
			p:    Progress{Phase: "index", Done: 10, Total: 100, StartedAt: start},
			now:  at(10 * time.Second),
			want: 90,
		},
		{
			// 22697 chunks, 1200 embedded in 60s = 20/s, so 21497 left ≈ 1075s.
			name: "large embedding run",
			p:    Progress{Phase: "index", Done: 1200, Total: 22697, StartedAt: start},
			now:  at(time.Minute),
			want: 1075,
		},
		{
			name: "finished phase has no estimate",
			p:    Progress{Phase: "index", Done: 100, Total: 100, StartedAt: start},
			now:  at(time.Minute),
		},
		{
			name: "missing start time is not extrapolated from the zero time",
			p:    Progress{Phase: "index", Done: 10, Total: 100},
			now:  at(time.Minute),
		},
		{
			name: "sub-second remainder is reported as one second",
			p:    Progress{Phase: "index", Done: 999, Total: 1000, StartedAt: start},
			now:  at(10 * time.Second),
			want: 1,
		},
	} {
		if got := tc.p.eta(tc.now); got != tc.want {
			t.Errorf("%s: eta = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestSetProgressKeepsPhaseStart pins the timing contract callers rely on:
// publishing counters must not restart the clock, or the rate would be measured
// between the last two samples and the estimate would be meaningless.
func TestSetProgressKeepsPhaseStart(t *testing.T) {
	t.Parallel()

	m := &Manager{progress: map[string]map[string]Progress{}, progressMu: sync.Mutex{}}

	m.setProgress("web:jira", Progress{Phase: "index", Total: 100})
	first := onlyPhase(t, m, "web:jira")
	if first.StartedAt.IsZero() {
		t.Fatalf("first progress = %#v", first)
	}

	m.setProgress("web:jira", Progress{Phase: "index", Done: 10, Total: 100})
	second := onlyPhase(t, m, "web:jira")
	if !second.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("phase start moved from %v to %v within a phase", first.StartedAt, second.StartedAt)
	}

	m.clearProgress("web:jira", "index")
	if _, ok := m.Progress("web:jira"); ok {
		t.Fatal("progress survived clearProgress")
	}
}

// TestProgressTracksConcurrentPhases is the regression test for repository
// builds: code_index runs alongside docs/docs_index, and a single slot per id
// let one overwrite the other's counters.
func TestProgressTracksConcurrentPhases(t *testing.T) {
	t.Parallel()

	m := &Manager{progress: map[string]map[string]Progress{}}
	id := "acme/payments"

	m.setProgress(id, Progress{Phase: "code_index", Done: 300, Total: 5000})
	m.setProgress(id, Progress{Phase: "docs", Done: 4, Total: 20})

	got, ok := m.Progress(id)
	if !ok || len(got) != 2 {
		t.Fatalf("progress = %#v, want both phases", got)
	}
	// Ordered by phase name so the UI does not reshuffle between polls.
	if got[0].Phase != "code_index" || got[1].Phase != "docs" {
		t.Fatalf("phases out of order: %#v", got)
	}
	if got[0].Done != 300 || got[1].Done != 4 {
		t.Fatalf("phases overwrote each other: %#v", got)
	}

	// Finishing one phase leaves the other reporting.
	m.clearProgress(id, "docs")
	got, ok = m.Progress(id)
	if !ok || len(got) != 1 || got[0].Phase != "code_index" {
		t.Fatalf("after clearing one phase: %#v", got)
	}

	m.setProgress(id, Progress{Phase: "docs_index", Done: 1, Total: 10})
	m.clearAllProgress(id)
	if _, ok := m.Progress(id); ok {
		t.Fatal("clearAllProgress left phases behind")
	}
}

// TestProgressETAIsComputedOnRead checks the estimate reflects the time since
// the last counter update, not only the moment it was published.
func TestProgressETAIsComputedOnRead(t *testing.T) {
	t.Parallel()

	m := &Manager{progress: map[string]map[string]Progress{}}
	m.setProgress("web:jira", Progress{
		Phase:     "index",
		Done:      10,
		Total:     20,
		StartedAt: time.Now().Add(-10 * time.Second),
	})

	// 10 items in ~10s, 10 to go.
	if p := onlyPhase(t, m, "web:jira"); p.ETASeconds < 9 || p.ETASeconds > 11 {
		t.Fatalf("eta_seconds = %d, want ~10", p.ETASeconds)
	}
}

// TestProgressReporterPublishesUnderItsPhase checks the adapter handed to the
// index services writes under the phase it was created for, so two services
// reporting concurrently for the same repo stay apart.
func TestProgressReporterPublishesUnderItsPhase(t *testing.T) {
	t.Parallel()

	m := &Manager{progress: map[string]map[string]Progress{}}
	id := "acme/payments"

	docs := m.progressReporter(id, "docs_index")
	code := m.progressReporter(id, "code_index")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 1; i <= 50; i++ {
			docs(i, 50)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 1; i <= 80; i++ {
			code(i, 80)
		}
	}()
	wg.Wait()

	got, ok := m.Progress(id)
	if !ok || len(got) != 2 {
		t.Fatalf("progress = %#v", got)
	}
	byPhase := map[string]Progress{}
	for _, p := range got {
		byPhase[p.Phase] = p
	}
	if p := byPhase["docs_index"]; p.Done != 50 || p.Total != 50 {
		t.Fatalf("docs_index = %d/%d, want 50/50", p.Done, p.Total)
	}
	if p := byPhase["code_index"]; p.Done != 80 || p.Total != 80 {
		t.Fatalf("code_index = %d/%d, want 80/80", p.Done, p.Total)
	}
}

// onlyPhase returns the single phase expected to be in flight for id.
func onlyPhase(t *testing.T, m *Manager, id string) Progress {
	t.Helper()

	got, ok := m.Progress(id)
	if !ok || len(got) != 1 {
		t.Fatalf("progress for %s = %#v, want exactly one phase", id, got)
	}

	return got[0]
}
