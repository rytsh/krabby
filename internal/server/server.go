// Package server wires the ada HTTP server: REST API, GitHub webhook and the
// MCP endpoint.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rakunlabs/ada"
	mcors "github.com/rakunlabs/ada/middleware/cors"
	mlog "github.com/rakunlabs/ada/middleware/log"
	mrecover "github.com/rakunlabs/ada/middleware/recover"
	mrequestid "github.com/rakunlabs/ada/middleware/requestid"
	mserver "github.com/rakunlabs/ada/middleware/server"
	mtelemetry "github.com/rakunlabs/ada/middleware/telemetry"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/credentials"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/lease"
	"github.com/rytsh/krabby/internal/service/manager"
)

// Start runs the HTTP server until ctx is cancelled.
func Start(ctx context.Context, cfg *config.Config, mgr *manager.Manager, mcpServer *mcp.Server) error {
	server := ada.New()
	server.Use(
		mrecover.Middleware(),
		mserver.Middleware(config.ServiceName+":"+config.Version),
		mcors.Middleware(),
		mrequestid.Middleware(),
		mlog.Middleware(),
		mtelemetry.Middleware(),
	)

	server.GET("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// MCP endpoint (streamable HTTP). POST/GET/DELETE share the same path.
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{},
	)
	server.Handle(cfg.MCP.Path, mcpHandler, apiKeyMiddleware(cfg.MCP.APIKey))

	api := server.Group("/api/v1")
	api.GET("/repos", server.Wrap(listRepos(mgr)))
	api.POST("/repos", server.Wrap(addRepo(mgr)))
	api.GET("/repos/{owner}/{name}", server.Wrap(getRepo(mgr)))
	api.DELETE("/repos/{owner}/{name}", server.Wrap(deleteRepo(mgr)))
	api.POST("/repos/{owner}/{name}/refresh", server.Wrap(refreshRepo(mgr)))
	api.POST("/repos/{owner}/{name}/lock", server.Wrap(lockRepo(mgr)))
	api.GET("/repos/{owner}/{name}/lock", server.Wrap(lockStatus(mgr)))
	api.DELETE("/repos/{owner}/{name}/lock", server.Wrap(unlockRepo(mgr)))
	api.GET("/repos/{owner}/{name}/graph", repoArtifact(mgr, graphify.GraphPath))
	api.GET("/repos/{owner}/{name}/report", repoArtifact(mgr, graphify.ReportPath))
	api.GET("/repos/{owner}/{name}/html", repoArtifact(mgr, graphify.HTMLPath))
	api.GET("/graph", mergedGraph(mgr))
	api.GET("/credentials", server.Wrap(listCredentials(mgr)))
	api.PUT("/credentials", server.Wrap(setCredential(mgr)))
	api.DELETE("/credentials", server.Wrap(deleteCredential(mgr)))

	server.POST("/webhook/github", githubWebhook(cfg.Webhook.GithubSecret, mgr))

	return server.StartWithContext(ctx, cfg.Server.Host+":"+cfg.Server.Port)
}

// ---- middleware -------------------------------------------------------------

func apiKeyMiddleware(apiKey string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if apiKey == "" {
			return next
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-Api-Key")
			if got == "" {
				got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}

			if subtle.ConstantTimeCompare([]byte(got), []byte(apiKey)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)

				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ---- REST handlers ----------------------------------------------------------

type addRepoRequest struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

func repoID(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("name")
}

func listRepos(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		repos, err := mgr.Registry().List(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(repos)
	}
}

func addRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req addRepoRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		if req.URL == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "url is required"})
		}

		repo, err := mgr.AddRepo(c.Request.Context(), req.URL, req.Branch)
		if err != nil {
			return c.Err(err)
		}

		return c.SetStatus(http.StatusAccepted).SendJSON(repo)
	}
}

func getRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		repo, err := mgr.Registry().Get(c.Request.Context(), repoID(c.Request))
		if err != nil {
			return c.Err(err)
		}

		if repo == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		return c.SendJSON(repo)
	}
}

func deleteRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		if err := mgr.RemoveRepo(c.Request.Context(), repoID(c.Request)); err != nil {
			return c.Err(err)
		}

		return c.SendNoContent()
	}
}

func refreshRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id := repoID(c.Request)

		repo, err := mgr.Registry().Get(c.Request.Context(), id)
		if err != nil {
			return c.Err(err)
		}

		if repo == nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": "not found"})
		}

		mgr.TriggerRefresh(id)

		return c.SetStatus(http.StatusAccepted).SendJSON(map[string]string{"status": "refresh queued", "repo": id})
	}
}

// ---- lease handlers ---------------------------------------------------------

type lockRequest struct {
	Owner string `json:"owner"`
	TTL   string `json:"ttl"` // Go duration, e.g. "5m"; empty = default
}

func lockRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req lockRequest
		if c.Request.ContentLength > 0 {
			if err := c.Bind(&req); err != nil {
				return c.SetStatus(http.StatusBadRequest).Err(err)
			}
		}

		var ttl time.Duration

		if req.TTL != "" {
			d, err := time.ParseDuration(req.TTL)
			if err != nil {
				return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "invalid ttl: " + err.Error()})
			}

			ttl = d
		}

		l, err := mgr.AcquireLease(c.Request.Context(), repoID(c.Request), req.Owner, ttl)
		if err != nil {
			if errors.Is(err, lease.ErrLeased) {
				return c.SetStatus(http.StatusConflict).SendJSON(map[string]string{"error": err.Error()})
			}

			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		// Token is included: the caller needs it to release the lock.
		return c.SendJSON(l)
	}
}

func lockStatus(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		l := mgr.LeaseInfo(repoID(c.Request))
		if l == nil {
			return c.SendJSON(map[string]any{"locked": false})
		}

		return c.SendJSON(map[string]any{"locked": true, "lease": l})
	}
}

func unlockRepo(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		token := c.Request.Header.Get("X-Lock-Token")
		if token == "" {
			token = c.Request.URL.Query().Get("token")
		}

		if token == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "X-Lock-Token header (or token query param) is required"})
		}

		if err := mgr.ReleaseLease(repoID(c.Request), token); err != nil {
			switch {
			case errors.Is(err, lease.ErrNotLeased):
				return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
			case errors.Is(err, lease.ErrBadToken):
				return c.SetStatus(http.StatusForbidden).SendJSON(map[string]string{"error": err.Error()})
			default:
				return c.Err(err)
			}
		}

		return c.SendNoContent()
	}
}

// ---- artifact handlers ------------------------------------------------------

// repoArtifact serves a graphify output file (graph.json, GRAPH_REPORT.md,
// graph.html) for a tracked repository so external tools can consume them
// without filesystem access.
func repoArtifact(mgr *manager.Manager, pathFn func(repoPath string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo, err := mgr.Registry().Get(r.Context(), repoID(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		if repo == nil {
			http.Error(w, "repo not found", http.StatusNotFound)

			return
		}

		path := pathFn(repo.Path)
		if _, err := os.Stat(path); err != nil {
			http.Error(w, "artifact not built yet (status: "+repo.Status+")", http.StatusNotFound)

			return
		}

		http.ServeFile(w, r, path)
	}
}

func mergedGraph(mgr *manager.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := mgr.MergedPath()
		if path == "" {
			http.Error(w, "merged graph not built yet", http.StatusNotFound)

			return
		}

		http.ServeFile(w, r, path)
	}
}

// ---- credential handlers ----------------------------------------------------

type setCredentialRequest struct {
	Pattern  string `json:"pattern"`
	Kind     string `json:"kind"`
	Username string `json:"username"`
	Secret   string `json:"secret"`
}

func listCredentials(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		creds, err := mgr.Credentials().List(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		// Credential.Secret carries json:"-"; secrets never leave the server.
		return c.SendJSON(creds)
	}
}

func setCredential(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req setCredentialRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		cred := &credentials.Credential{
			Pattern:  req.Pattern,
			Kind:     req.Kind,
			Username: req.Username,
			Secret:   req.Secret,
		}
		if err := mgr.Credentials().Set(c.Request.Context(), cred); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SetStatus(http.StatusCreated).SendJSON(cred)
	}
}

func deleteCredential(mgr *manager.Manager) ada.HandlerFunc {
	return func(c *ada.Context) error {
		pattern := c.Request.URL.Query().Get("pattern")
		if pattern == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "pattern query param is required"})
		}

		if err := mgr.Credentials().Delete(c.Request.Context(), pattern); err != nil {
			return c.Err(err)
		}

		return c.SendNoContent()
	}
}

// ---- GitHub webhook ---------------------------------------------------------

type githubPushEvent struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func githubWebhook(secret string, mgr *manager.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)

			return
		}

		if secret != "" {
			if !verifyGithubSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
				http.Error(w, "invalid signature", http.StatusUnauthorized)

				return
			}
		}

		var event githubPushEvent
		if err := json.Unmarshal(body, &event); err != nil || event.Repository.FullName == "" {
			http.Error(w, "invalid payload", http.StatusBadRequest)

			return
		}

		repo, err := mgr.Registry().Get(r.Context(), event.Repository.FullName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		if repo == nil {
			http.Error(w, "repo not tracked", http.StatusNotFound)

			return
		}

		mgr.TriggerRefresh(repo.ID)

		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, `{"status":"refresh queued","repo":%q}`, repo.ID)
	}
}

func verifyGithubSignature(secret string, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(sig))
}
