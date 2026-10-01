package server

import (
	"net/http"
	"strconv"

	"github.com/rakunlabs/ada"
)

// listTasks returns the central work-queue snapshot: the concurrency limit,
// live running/queued counters and the queued/running/recent tasks. Powers the
// Activity page's task view.
func listTasks(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		return c.SendJSON(mgr.TaskSnapshot())
	}
}

// clearTaskHistory removes completed, failed and canceled tasks while leaving
// queued and running work untouched.
func clearTaskHistory(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		mgr.ClearTaskHistory()

		return c.SendJSON(mgr.TaskSnapshot())
	}
}

// cancelPendingTasks removes every queued task while running work continues.
func cancelPendingTasks(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		mgr.CancelPendingTasks()

		return c.SendJSON(mgr.TaskSnapshot())
	}
}

type taskConcurrencyRequest struct {
	Limit int `json:"limit"`
}

// setTaskConcurrency changes how many background tasks run at once, effective
// immediately. It returns the resulting snapshot so the UI reflects the new
// limit without a second fetch. The value is not persisted to settings.
func setTaskConcurrency(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req taskConcurrencyRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		mgr.SetTaskConcurrency(req.Limit)

		return c.SendJSON(mgr.TaskSnapshot())
	}
}

// bumpTask moves a queued task to the front of the backlog so it starts next.
func bumpTask(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		seq, err := strconv.ParseUint(c.Request.PathValue("seq"), 10, 64)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "invalid task seq"})
		}

		if !mgr.BumpTask(seq) {
			return c.SetStatus(http.StatusConflict).SendJSON(map[string]string{
				"error": "no queued task with that seq (it may be running or already finished)",
			})
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(mgr.TaskSnapshot())
	}
}

// cancelTask cancels a single task by seq: a queued task is dropped from the
// backlog, while a running task has its underlying job aborted.
func cancelTask(mgr queueService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		seq, err := strconv.ParseUint(c.Request.PathValue("seq"), 10, 64)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "invalid task seq"})
		}

		if !mgr.CancelTask(seq) {
			return c.SetStatus(http.StatusConflict).SendJSON(map[string]string{
				"error": "no task with that seq (it may already be finished)",
			})
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(mgr.TaskSnapshot())
	}
}
