package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rytsh/krabby/internal/service/gitops"
)

// gitPushEvent covers the repository identity fields used by GitHub, GitLab,
// Gitea and compatible servers. Unknown fields are intentionally ignored.
type gitPushEvent struct {
	Repository struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
		SSHURL   string `json:"ssh_url"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"`
		GitHTTPURL        string `json:"git_http_url"`
		GitSSHURL         string `json:"git_ssh_url"`
		WebURL            string `json:"web_url"`
	} `json:"project"`
	RepositoryURL string `json:"repository_url"`
}

func gitWebhook(mgr webhookService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)

			return
		}

		// Resolve per request so a secret changed through UI/REST applies
		// immediately without rebuilding the HTTP routes or restarting.
		if secret := mgr.WebhookSecret(); secret != "" {
			if !verifyGitWebhook(secret, body, r.Header) {
				http.Error(w, "invalid signature", http.StatusUnauthorized)

				return
			}
		}

		var event gitPushEvent
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)

			return
		}

		ref := gitEventRepoRef(event)
		if ref == "" {
			http.Error(w, "payload has no repository identity", http.StatusBadRequest)

			return
		}

		repo, err := mgr.ResolveRepo(r.Context(), ref)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		if repo == nil {
			http.Error(w, "repo not tracked", http.StatusNotFound)

			return
		}

		if err := mgr.TriggerRefresh(repo.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, `{"status":"refresh queued","repo":%q}`, repo.ID)
	}
}

// gitEventRepoRef prefers clone/web URLs because ParseRepoID preserves the git
// server host. Provider path-only fields are a fallback and resolve by unique
// suffix when the payload does not expose a URL.
func gitEventRepoRef(event gitPushEvent) string {
	urls := []string{
		event.Repository.CloneURL,
		event.Repository.SSHURL,
		event.Project.GitHTTPURL,
		event.Project.GitSSHURL,
		event.RepositoryURL,
		event.Repository.HTMLURL,
		event.Project.WebURL,
	}
	for _, raw := range urls {
		if raw == "" {
			continue
		}
		if id, err := gitops.ParseRepoID(raw); err == nil {
			return id
		}
	}

	if event.Project.PathWithNamespace != "" {
		return event.Project.PathWithNamespace
	}

	return event.Repository.FullName
}

// verifyGitWebhook first accepts provider-neutral shared-token headers, then
// the common authentication schemes used by popular git servers. A custom git
// server can always send Authorization: Bearer <secret> or X-Webhook-Token.
func verifyGitWebhook(secret string, body []byte, header http.Header) bool {
	tokens := []string{
		header.Get("X-Webhook-Token"),
		strings.TrimPrefix(header.Get("Authorization"), "Bearer "),
		header.Get("X-Gitlab-Token"),
	}
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1 {
			return true
		}
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	for _, name := range []string{"X-Hub-Signature-256", "X-Gitea-Signature", "X-Gogs-Signature"} {
		sig := strings.TrimPrefix(header.Get(name), "sha256=")
		if sig != "" && hmac.Equal([]byte(expected), []byte(sig)) {
			return true
		}
	}

	return false
}
