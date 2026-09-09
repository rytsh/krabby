package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"
)

func TestCodeSearchTimingLogsStagesAndOutcome(t *testing.T) {
	for _, tc := range []struct {
		err            error
		outcome, level string
		slow           bool
	}{
		{nil, "ok", "DEBUG", false},
		{nil, "ok", "WARN", true},
		{context.Canceled, "cancelled", "DEBUG", false},
		{context.DeadlineExceeded, "deadline", "DEBUG", false},
		{errors.New("store failed"), "error", "WARN", false},
	} {
		var out bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
		timing := newCodeSearchTiming()
		if tc.slow {
			timing.started = timing.started.Add(-time.Second)
		}
		timing.times["warm"] = 42 * time.Millisecond
		timing.retried = true
		timing.enter("index")
		timing.finish(context.Background(), logger, "text", "repo", "default", 2, 5, tc.err)
		var record map[string]any
		if err := json.Unmarshal(out.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record["outcome"] != tc.outcome || record["level"] != tc.level || record["warm_ms"] != float64(42) || record["retried"] != true || record["matched"] != float64(5) {
			t.Fatalf("log=%s", out.Bytes())
		}
		for _, stage := range []string{"scope", "warm", "prepare", "index", "retry", "metadata"} {
			if _, ok := record[stage+"_ms"]; !ok {
				t.Fatalf("missing %s: %s", stage, out.Bytes())
			}
		}
	}
}
