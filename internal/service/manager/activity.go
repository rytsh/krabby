package manager

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/progress"
)

// Pipeline step names, reported by Activity and consumed by the UI. They are
// part of the UI contract, so they live beside the task kinds rather than being
// spelled out at each of the two dozen call sites that used to repeat them.
const (
	stepSync      = "sync"
	stepFetch     = "fetch"
	stepGraph     = "graph"
	stepDocs      = "docs"
	stepDocsIndex = "docs_index"
	stepCodeIndex = "code_index"
)

// Progress phase names, reported alongside Done/Total counters.
const (
	phaseFetch = "fetch"
	phaseIndex = "index"
)

func (m *Manager) setActivity(id, step string) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	steps := m.activity[id]
	if steps == nil {
		steps = map[string]struct{}{}
		m.activity[id] = steps
	}

	steps[step] = struct{}{}
}

func (m *Manager) clearActivity(id, step string) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	delete(m.activity[id], step)

	if len(m.activity[id]) == 0 {
		delete(m.activity, id)
	}
}

// clearRepoActivity removes all running-step markers for a repo. Used as a
// safety net when a whole pipeline run ends.
func (m *Manager) clearRepoActivity(id string) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	delete(m.activity, id)
}

// Progress is a live view of a long-running step's counters, so the UI can show
// a determinate progress bar. Phase names the current work ("fetch",
// "index"); Done/Total are its item counters (e.g. embedded chunks). All fields
// are transient and reset when the step ends.
type Progress struct {
	Phase string `json:"phase"`
	Done  int    `json:"done"`
	Total int    `json:"total"`

	// StartedAt is when the current phase began. It is kept across updates
	// within a phase so the observed rate is measured over the whole phase
	// rather than between two samples.
	StartedAt time.Time `json:"started_at,omitzero"`

	// ETASeconds estimates the time left in this phase, computed on read from
	// the rate achieved so far. Zero means "not predictable yet": an
	// indeterminate phase, one that has not produced a usable sample, or one
	// that is already finished.
	ETASeconds int `json:"eta_seconds,omitempty"`
}

// etaWarmup is how much of a phase must be observed before its remaining time
// is estimated. An estimate drawn from the first few items of a long run swings
// wildly (the first embedding batch pays connection setup, the first pages of a
// sync are cached), and a number that jumps from "3 hours" to "2 minutes" is
// worse than no number at all.
const (
	etaWarmupDuration = 3 * time.Second
	etaWarmupItems    = 5
)

// eta estimates the seconds remaining in the phase from the throughput observed
// since it started. It returns 0 when there is nothing meaningful to say.
func (p Progress) eta(now time.Time) int {
	if p.Total <= 0 || p.Done <= 0 || p.Done >= p.Total || p.StartedAt.IsZero() {
		return 0
	}

	elapsed := now.Sub(p.StartedAt)
	if elapsed < etaWarmupDuration || p.Done < etaWarmupItems {
		return 0
	}

	perItem := elapsed.Seconds() / float64(p.Done)
	remaining := perItem * float64(p.Total-p.Done)
	if remaining < 1 {
		return 1
	}

	return int(math.Round(remaining))
}

// setProgress records the live counters for one phase of id's work, replacing
// any prior value for that phase. A zero Total means "indeterminate" (the UI
// shows a spinner).
//
// The phase start time is preserved while the phase's counters are updated, so
// callers publish counters without tracking timing themselves and the rate is
// measured over the whole phase.
func (m *Manager) setProgress(id string, p Progress) {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()

	phases := m.progress[id]
	if phases == nil {
		phases = map[string]Progress{}
		m.progress[id] = phases
	}

	if prev, ok := phases[p.Phase]; ok && !prev.StartedAt.IsZero() {
		p.StartedAt = prev.StartedAt
	} else if p.StartedAt.IsZero() {
		p.StartedAt = time.Now()
	}

	phases[p.Phase] = p
}

// clearProgress removes one phase's counters (that step finished or aborted).
func (m *Manager) clearProgress(id, phase string) {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()

	phases := m.progress[id]
	if phases == nil {
		return
	}

	delete(phases, phase)
	if len(phases) == 0 {
		delete(m.progress, id)
	}
}

// progressReporter returns a callback that publishes counters for one phase of
// id. It is the adapter between the services' (done, total) callbacks and the
// per-phase progress map, so a caller only names the phase once.
func (m *Manager) progressReporter(id, phase string) progress.Func {
	return func(done, total int) {
		m.setProgress(id, Progress{Phase: phase, Done: done, Total: total})
	}
}

// clearAllProgress drops every phase recorded for id. Used on the paths that
// own the whole job and must not leak counters if a phase returns early.
func (m *Manager) clearAllProgress(id string) {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()

	delete(m.progress, id)
}

// Progress returns the live counters for every phase id is running, ordered by
// phase name so the UI does not reshuffle between polls. The estimates are
// computed on read, so they reflect the time since the last counter update
// rather than the moment it was published.
func (m *Manager) Progress(id string) ([]Progress, bool) {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()

	phases := m.progress[id]
	if len(phases) == 0 {
		return nil, false
	}

	now := time.Now()
	out := make([]Progress, 0, len(phases))
	for _, p := range phases {
		p.ETASeconds = p.eta(now)
		out = append(out, p)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Phase < out[j].Phase })

	return out, true
}

// Activity returns the pipeline steps currently running for a repo ("sync",
// "graph", "docs", "docs_index", "code_index"), comma-joined when several run
// in parallel, or "" when idle.
func (m *Manager) Activity(id string) string {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	steps := make([]string, 0, len(m.activity[id]))
	for step := range m.activity[id] {
		steps = append(steps, step)
	}

	sort.Strings(steps)

	return strings.Join(steps, ",")
}

// ActiveRepos returns the repos that currently have running pipeline steps,
// mapping each repo id to its comma-joined steps. This lets the UI show live
// jobs without scanning every tracked repo.
func (m *Manager) ActiveRepos() map[string]string {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()

	out := make(map[string]string, len(m.activity))
	for id, stepSet := range m.activity {
		steps := make([]string, 0, len(stepSet))
		for step := range stepSet {
			steps = append(steps, step)
		}
		sort.Strings(steps)
		out[id] = strings.Join(steps, ",")
	}

	return out
}

// job is the cancellation handle of one running refresh/generate run.
type job struct {
	cancel context.CancelFunc
}

// registerJob derives a cancellable context for a repo's running job and
// registers its handle so CancelJob can abort it. The returned cleanup must be
// called when the job finishes; it releases the context and unregisters the
// handle (unless a newer job already replaced it). Per-repo jobs serialize on
// the repo lock, so at most one handle exists per id.
func (m *Manager) registerJob(ctx context.Context, id string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	j := &job{cancel: cancel}

	m.jobMu.Lock()
	m.jobs[id] = j
	m.jobMu.Unlock()

	return ctx, func() {
		m.jobMu.Lock()
		if m.jobs[id] == j {
			delete(m.jobs, id)
		}
		m.jobMu.Unlock()

		cancel()
	}
}

// CancelJob aborts the refresh/generate job currently running for a repo by
// cancelling its context; the in-flight git/graph build is canceled and
// the outcome is recorded as cancelled. It reports whether a job was running.
func (m *Manager) CancelJob(id string) bool {
	// Cancel top-level queue work first. The job handle remains as a fallback for
	// synchronous callers that did not enter through the central queue.
	canceled := m.CancelTasks(id)

	m.jobMu.Lock()
	j := m.jobs[id]
	m.jobMu.Unlock()

	if j == nil {
		return canceled > 0
	}

	slog.Info("cancelling running job", "repo", id)
	j.cancel()

	return true
}

// ErrCancelled is recorded as the repo's LastError when a running job is
// aborted via CancelJob.
var ErrCancelled = errors.New("cancelled by user")
