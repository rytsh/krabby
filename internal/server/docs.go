package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/rakunlabs/ada"

	"github.com/rytsh/krabby/internal/service/coderag"
)

func listRepoFiles(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		subdir := c.Request.URL.Query().Get("subdir")
		snapshot := c.Request.URL.Query().Get("snapshot")
		recursive := c.Request.URL.Query().Get("recursive") == "true"

		entries, token, err := mgr.ListRepoFilesAt(c.Request.Context(), repoID(c.Request), subdir, snapshot, recursive)
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}
		c.Response.Header().Set("X-Krabby-Snapshot", token)

		return c.SendJSON(entries)
	}
}

func globRepoFiles(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		params := c.Request.URL.Query()
		pattern := strings.TrimSpace(params.Get("pattern"))
		if pattern == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "pattern query param is required"})
		}

		page, err := mgr.GlobRepoFiles(c.Request.Context(), repoID(c.Request), params.Get("snapshot"), pattern, queryInt(params.Get("limit"), 0))
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}
		c.Response.Header().Set("X-Krabby-Snapshot", page.Snapshot)

		return c.SendJSON(page)
	}
}

func readRepoFile(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		path := c.Request.URL.Query().Get("path")
		if path == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "path query param is required"})
		}

		var offset int64
		if v := c.Request.URL.Query().Get("offset"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				offset = n
			}
		}

		var maxBytes int
		if v := c.Request.URL.Query().Get("max_bytes"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				maxBytes = n
			}
		}

		fc, err := mgr.ReadRepoFileAt(c.Request.Context(), repoID(c.Request), path, c.Request.URL.Query().Get("snapshot"), offset, maxBytes)
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(fc)
	}
}

func listDocs(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		docs, err := mgr.ListDocs(c.Request.Context(), repoID(c.Request))
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(docs)
	}
}

func getDoc(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		path := c.Request.URL.Query().Get("path")
		if path == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "path query param is required"})
		}

		doc, err := mgr.GetDoc(c.Request.Context(), repoID(c.Request), path, 0, 0)
		if err != nil {
			return c.SetStatus(http.StatusNotFound).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(doc)
	}
}

func searchDocs(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		q := c.Request.URL.Query().Get("q")
		if q == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "q query param is required"})
		}

		// repo may be a repository id, a web-source key ("web:<name>") or an
		// API-catalog key ("api:<name>") and wins over scope; scope selects
		// all/repos/sources/apis when repo is empty.
		repo := c.Request.URL.Query().Get("repo")
		scope := c.Request.URL.Query().Get("scope")
		namespace := namespaceParam(c.Request)
		mode := c.Request.URL.Query().Get("mode")

		var top int
		if v := c.Request.URL.Query().Get("top"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				top = n
			}
		}

		docs, err := mgr.SearchDocs(c.Request.Context(), scope, repo, namespace, mode, q, top)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(docs)
	}
}

func searchCode(mgr docsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		q := strings.TrimSpace(c.Request.URL.Query().Get("q"))
		if q == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "q query param is required"})
		}

		params := c.Request.URL.Query()
		repo := params.Get("repo") // "" = all repos
		namespace := namespaceParam(c.Request)
		mode := params.Get("mode")
		if mode == "" {
			mode = "normal"
		}

		switch mode {
		case "normal":
			result, err := mgr.SearchCodeText(c.Request.Context(), repo, namespace, q, coderag.TextSearchOptions{
				Page:    queryInt(params.Get("page"), 1),
				PerPage: queryInt(params.Get("per_page"), 20),
				Path:    params.Get("path"),
			})
			if err != nil {
				return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
			}

			return c.SendJSON(result)
		case "regex":
			opts := coderag.RegexOptions{
				CaseSensitive: params.Get("case_sensitive") == "true",
				Path:          params.Get("path"),
				Page:          queryInt(params.Get("page"), 1),
				PerPage:       queryInt(params.Get("per_page"), 20),
			}
			if n, err := strconv.Atoi(params.Get("context_lines")); err == nil {
				opts.ContextLines = n
			}
			if n, err := strconv.Atoi(params.Get("max_matches")); err == nil {
				opts.MaxMatches = n
			}

			result, err := mgr.SearchCodeRegex(c.Request.Context(), repo, namespace, q, opts)
			if err != nil {
				return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
			}

			return c.SendJSON(result)
		case "semantic":
			page, err := mgr.SearchCode(c.Request.Context(), repo, namespace, q, coderag.SemanticOptions{
				TopK: queryInt(params.Get("top"), 0),
				Path: params.Get("path"),
			})
			if err != nil {
				return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
			}

			return c.SendJSON(page)
		default:
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{
				"error": "mode must be normal, regex or semantic",
			})
		}
	}
}

func mergedGraph(mgr docsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := mgr.MergedPath()
		if path == "" {
			http.Error(w, "merged graph not built yet", http.StatusNotFound)

			return
		}

		http.ServeFile(w, r, path)
	}
}
