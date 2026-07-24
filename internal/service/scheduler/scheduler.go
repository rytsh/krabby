// Package scheduler periodically refreshes tracked repositories and synced
// web sources.
//
// Repository polling is driven by per-namespace cron schedules (see
// settings.RepoSchedule) run through github.com/worldline-go/hardloop. Web
// sources keep their own per-collection refresh intervals, evaluated on a fixed
// reconcile tick. Both schedule sources live in the runtime settings, so UI/REST
// changes take effect without restarting the process: every reconcile tick the
// scheduler re-reads the schedules and, when they change, rebuilds the cron set.
package scheduler

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/settings"
	"github.com/worldline-go/hardloop"
)

// checkInterval is how often web-source schedules are evaluated and repo cron
// schedules are reconciled against the persisted settings. The cron jobs
// themselves fire independently once loaded; this tick only detects config
// changes and drives web-source refresh.
const checkInterval = time.Minute

// cronRunner is the subset of hardloop's cron job used here. It lets the
// scheduler hold the (unexported) *hardloop cron type behind an interface.
type cronRunner interface {
	Start(ctx context.Context) error
	Stop()
}

// scheduler owns the currently loaded repo cron set and the signature of the
// schedules that produced it, so reconcile can detect changes cheaply.
type scheduler struct {
	mgr  *manager.Manager
	cron cronRunner
	sig  string
}

// Run evaluates repo and web-source schedules until ctx is cancelled. Repo
// polling reads its cron schedules from persisted settings on every reconcile
// tick, so UI/REST changes take effect without restarting the process.
func Run(ctx context.Context, mgr *manager.Manager) {
	s := &scheduler{mgr: mgr}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	slog.Info("scheduler started", "check_interval", checkInterval.String())

	// Load the initial cron set before the first tick so polling starts
	// immediately rather than after one interval.
	s.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			s.stop()

			return
		case <-ticker.C:
			mgr.RefreshDueWebSources(ctx)
			s.reconcile(ctx)
		}
	}
}

// reconcile rebuilds the repo cron set when the effective schedules have
// changed since the last load. A build/parse failure leaves the previously
// loaded set running and is retried on the next tick.
func (s *scheduler) reconcile(ctx context.Context) {
	schedules := s.mgr.RepoSchedules()
	sig := scheduleSignature(schedules)
	if s.cron != nil && sig == s.sig {
		return
	}

	crons := s.buildCrons(schedules)

	job, err := hardloop.NewCron(crons...)
	if err != nil {
		slog.Error("build repo poll schedules", "error", err)

		return
	}

	if err := job.Start(ctx); err != nil {
		slog.Error("start repo poll schedules", "error", err)

		return
	}

	// Swap only after the new set has started successfully, then stop the old
	// one. A brief overlap is harmless: per-repo refresh triggers coalesce.
	s.stop()
	s.cron = job
	s.sig = sig

	slog.Info("repo poll schedules loaded", "jobs", len(crons))
}

// buildCrons turns the effective schedules into hardloop cron jobs, one per
// enabled schedule with specs. Each job's function polls its namespace.
func (s *scheduler) buildCrons(schedules []settings.RepoSchedule) []hardloop.Cron {
	crons := make([]hardloop.Cron, 0, len(schedules))

	for _, sc := range schedules {
		if sc.Disabled || len(sc.Specs) == 0 {
			continue
		}

		ns := sc.Namespace
		crons = append(crons, hardloop.Cron{
			Name:  "repo-poll:" + namespaceLabel(ns),
			Specs: sc.Specs,
			Func: func(ctx context.Context) error {
				return s.mgr.RefreshNamespace(ctx, ns)
			},
		})
	}

	return crons
}

// stop cancels the loaded cron set (if any) and waits for in-flight jobs.
func (s *scheduler) stop() {
	if s.cron != nil {
		s.cron.Stop()
		s.cron = nil
	}
}

// namespaceLabel renders a namespace for the cron job name/log line.
func namespaceLabel(ns string) string {
	switch strings.TrimSpace(ns) {
	case "":
		return "default"
	case "*":
		return "all"
	default:
		return ns
	}
}

// scheduleSignature is a stable fingerprint of the effective schedules so
// reconcile only rebuilds crons when they actually change.
func scheduleSignature(schedules []settings.RepoSchedule) string {
	var b strings.Builder

	for _, sc := range schedules {
		if sc.Disabled {
			b.WriteString("!")
		}

		b.WriteString(sc.Namespace)
		b.WriteString("=")
		b.WriteString(strings.Join(sc.Specs, ","))
		b.WriteString(";")
	}

	return b.String()
}
