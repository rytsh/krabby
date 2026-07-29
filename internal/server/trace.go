package server

import (
	"fmt"
	"net/http"

	"github.com/rytsh/krabby/internal/observability/langfuse"
	"github.com/rytsh/krabby/internal/service/manager"
)

// langfuseMiddleware exports REST requests to Langfuse.
//
// This scope is off by default and should stay that way for most installs:
// these spans carry no model, no tokens and no cost, yet Langfuse bills per
// observation and krabby's UI polls. It exists for the case where a request
// has to be correlated with the LLM work it triggered.
//
// The gRPC-side telemetry middleware already traces every request
// independently; the two are separate providers and do not interfere.
func langfuseMiddleware(mgr *manager.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tracer := mgr.Tracer()
			if !tracer.On(langfuse.ScopeHTTP) {
				next.ServeHTTP(w, r)

				return
			}

			name := r.Method + " " + r.URL.Path

			ctx, endTrace := tracer.StartTrace(r.Context(), langfuse.ScopeHTTP, langfuse.TraceInfo{
				Name: name,
				Tags: []string{"krabby", "http"},
				Metadata: map[string]string{
					"method": r.Method,
					"path":   r.URL.Path,
				},
			})

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			// A 5xx marks the observation as failed so it surfaces in
			// Langfuse's error filters; a 4xx is the caller's problem and is
			// recorded without raising the level.
			var err error
			if rec.status >= http.StatusInternalServerError {
				err = fmt.Errorf("http %d", rec.status)
			}

			endTrace(map[string]any{"status": rec.status}, err)
		})
	}
}

// statusRecorder captures the response status so it can be reported without
// buffering the body.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
