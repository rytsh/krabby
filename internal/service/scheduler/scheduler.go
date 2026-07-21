// Package scheduler periodically refreshes tracked repositories.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/rytsh/krabby/internal/service/manager"
)

// Run polls every interval and triggers a refresh for each tracked repo.
// It blocks until ctx is cancelled; interval <= 0 disables polling.
func Run(ctx context.Context, mgr *manager.Manager, interval time.Duration) {
	if interval <= 0 {
		slog.Info("scheduler disabled (poll_interval <= 0)")

		return
	}

	slog.Info("scheduler started", "interval", interval.String())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			repos, err := mgr.Registry().List(ctx)
			if err != nil {
				slog.Error("scheduler list repos", "error", err)

				continue
			}

			for _, repo := range repos {
				mgr.TriggerRefresh(repo.ID)
			}
		}
	}
}
