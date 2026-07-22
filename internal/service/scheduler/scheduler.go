// Package scheduler periodically refreshes tracked repositories and synced
// web sources.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/rytsh/krabby/internal/service/manager"
)

// checkInterval is how often persisted schedules are evaluated. The actual
// repo and per-source intervals remain independently configurable.
const checkInterval = time.Minute

// Run evaluates repo and web-source schedules until ctx is cancelled. Repo
// polling reads its interval from persisted settings on every tick, so UI/REST
// changes take effect without restarting the process.
func Run(ctx context.Context, mgr *manager.Manager) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	lastRepoPoll := time.Now()

	slog.Info("scheduler started", "check_interval", checkInterval.String())

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			mgr.RefreshDueWebSources(ctx)

			interval := mgr.PollInterval()
			if interval <= 0 || (!lastRepoPoll.IsZero() && now.Sub(lastRepoPoll) < interval) {
				continue
			}

			repos, err := mgr.Registry().List(ctx)
			if err != nil {
				slog.Error("scheduler list repos", "error", err)

				continue
			}

			for _, repo := range repos {
				mgr.TriggerRefresh(repo.ID)
			}
			lastRepoPoll = now
		}
	}
}
