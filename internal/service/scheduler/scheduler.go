// Package scheduler periodically refreshes tracked repositories, synced web
// sources and API-catalog services.
//
// Repository polling is driven by per-namespace cron schedules (see
// settings.RepoSchedule) run through github.com/worldline-go/hardloop. Web
// sources and API services keep their own per-record schedules, evaluated on a
// fixed reconcile tick. Every schedule lives in persisted state, so UI/REST
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

// scheduler owns the currently loaded repo and web-source cron sets and the
// signatures of the schedules that produced them, so reconcile can detect
// changes cheaply and rebuild only the set that changed.
type scheduler struct {
	mgr *manager.Manager

	cron cronRunner
	sig  string

	webCron cronRunner
	webSig  string

	apiCron cronRunner
	apiSig  string
}

// Run evaluates repo and web-source schedules until ctx is cancelled. Repo
// polling reads its cron schedules from persisted settings on every reconcile
// tick, so UI/REST changes take effect without restarting the process.
func Run(ctx context.Context, mgr *manager.Manager) {
	s := &scheduler{mgr: mgr}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	slog.Info("scheduler started", "check_interval", checkInterval.String())

	// Load the initial cron sets before the first tick so polling starts
	// immediately rather than after one interval.
	s.reconcile(ctx)
	s.reconcileWeb(ctx)
	s.reconcileAPI(ctx)

	for {
		select {
		case <-ctx.Done():
			s.stop()

			return
		case <-ticker.C:
			// Interval-based fallback for web sources without a cron spec
			// (legacy RefreshInterval-only collections).
			mgr.RefreshDueWebSources(ctx)
			mgr.RefreshDueAPIServices(ctx)
			s.reconcile(ctx)
			s.reconcileWeb(ctx)
			s.reconcileAPI(ctx)
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

// reconcileWeb rebuilds the web-source cron set when the effective per-source
// schedules have changed since the last load. Mirrors reconcile for repos: a
// build/parse failure leaves the previously loaded set running and is retried
// on the next tick.
func (s *scheduler) reconcileWeb(ctx context.Context) {
	schedules := s.mgr.WebSourceSchedules(ctx)
	sig := webScheduleSignature(schedules)
	if s.webCron != nil && sig == s.webSig {
		return
	}

	crons := s.buildWebCrons(schedules)

	// No web schedules: tear down any existing set and remember the empty
	// signature so we do not rebuild every tick.
	if len(crons) == 0 {
		s.stopWeb()
		s.webSig = sig

		return
	}

	job, err := hardloop.NewCron(crons...)
	if err != nil {
		slog.Error("build web-source poll schedules", "error", err)

		return
	}

	if err := job.Start(ctx); err != nil {
		slog.Error("start web-source poll schedules", "error", err)

		return
	}

	s.stopWeb()
	s.webCron = job
	s.webSig = sig

	slog.Info("web-source poll schedules loaded", "jobs", len(crons))
}

// buildWebCrons turns per-source schedules into hardloop cron jobs, one per
// collection. Each job's function triggers a background refresh of that source;
// triggers coalesce and the work queue bounds concurrency.
func (s *scheduler) buildWebCrons(schedules []manager.WebSourceSchedule) []hardloop.Cron {
	crons := make([]hardloop.Cron, 0, len(schedules))

	for _, sc := range schedules {
		if len(sc.Specs) == 0 {
			continue
		}

		name := sc.Name
		crons = append(crons, hardloop.Cron{
			Name:  "websource-poll:" + name,
			Specs: sc.Specs,
			Func: func(_ context.Context) error {
				s.mgr.TriggerWebRefresh(name)

				return nil
			},
		})
	}

	return crons
}

// reconcileAPI rebuilds the API-catalog cron set when the effective per-service
// schedules have changed since the last load. Mirrors reconcileWeb: a
// build/parse failure leaves the previously loaded set running and is retried
// on the next tick.
func (s *scheduler) reconcileAPI(ctx context.Context) {
	schedules := s.mgr.APISchedules(ctx)
	sig := apiScheduleSignature(schedules)
	if s.apiCron != nil && sig == s.apiSig {
		return
	}

	crons := s.buildAPICrons(schedules)

	// No API schedules: tear down any existing set and remember the empty
	// signature so we do not rebuild every tick.
	if len(crons) == 0 {
		s.stopAPI()
		s.apiSig = sig

		return
	}

	job, err := hardloop.NewCron(crons...)
	if err != nil {
		slog.Error("build api-service poll schedules", "error", err)

		return
	}

	if err := job.Start(ctx); err != nil {
		slog.Error("start api-service poll schedules", "error", err)

		return
	}

	s.stopAPI()
	s.apiCron = job
	s.apiSig = sig

	slog.Info("api-service poll schedules loaded", "jobs", len(crons))
}

// buildAPICrons turns per-service schedules into hardloop cron jobs, one per
// service. Each job triggers a background sync; triggers coalesce and the work
// queue bounds concurrency.
func (s *scheduler) buildAPICrons(schedules []manager.APISchedule) []hardloop.Cron {
	crons := make([]hardloop.Cron, 0, len(schedules))

	for _, sc := range schedules {
		if len(sc.Specs) == 0 {
			continue
		}

		name := sc.Name
		crons = append(crons, hardloop.Cron{
			Name:  "apicatalog-poll:" + name,
			Specs: sc.Specs,
			Func: func(_ context.Context) error {
				s.mgr.TriggerAPIRefresh(name)

				return nil
			},
		})
	}

	return crons
}

// stop cancels every loaded cron set (if any) and waits for in-flight jobs.
func (s *scheduler) stop() {
	if s.cron != nil {
		s.cron.Stop()
		s.cron = nil
	}
	s.stopWeb()
	s.stopAPI()
}

// stopWeb cancels the loaded web-source cron set (if any).
func (s *scheduler) stopWeb() {
	if s.webCron != nil {
		s.webCron.Stop()
		s.webCron = nil
	}
}

// stopAPI cancels the loaded API-catalog cron set (if any).
func (s *scheduler) stopAPI() {
	if s.apiCron != nil {
		s.apiCron.Stop()
		s.apiCron = nil
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

// webScheduleSignature is a stable fingerprint of the effective web-source
// schedules so reconcileWeb only rebuilds crons when they actually change.
func webScheduleSignature(schedules []manager.WebSourceSchedule) string {
	var b strings.Builder

	for _, sc := range schedules {
		b.WriteString(sc.Name)
		b.WriteString("=")
		b.WriteString(strings.Join(sc.Specs, ","))
		b.WriteString(";")
	}

	return b.String()
}

// apiScheduleSignature is a stable fingerprint of the effective API-service
// schedules so reconcileAPI only rebuilds crons when they actually change.
func apiScheduleSignature(schedules []manager.APISchedule) string {
	var b strings.Builder

	for _, sc := range schedules {
		b.WriteString(sc.Name)
		b.WriteString("=")
		b.WriteString(strings.Join(sc.Specs, ","))
		b.WriteString(";")
	}

	return b.String()
}
