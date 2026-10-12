package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rakunlabs/ada"

	"github.com/rytsh/krabby/internal/service/memory"
)

func memoryError(c *ada.Context, err error) error {
	status, message := http.StatusInternalServerError, "memory operation failed"
	switch {
	case errors.Is(err, memory.ErrInvalid), errors.Is(err, memory.ErrSecret):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, memory.ErrNotFound):
		status, message = http.StatusNotFound, err.Error()
	case errors.Is(err, memory.ErrConflict):
		status, message = http.StatusConflict, err.Error()
	}
	return c.SetStatus(status).SendJSON(map[string]string{"error": message})
}

func decodeMemory(c *ada.Context, dst any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response, c.Request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return memory.ErrInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return memory.ErrInvalid
	}
	return nil
}

func memoryListOptions(c *ada.Context) memory.ListOptions {
	q := c.Request.URL.Query()
	var spaces []string
	for _, v := range q["space"] {
		spaces = append(spaces, strings.Split(v, ",")...)
	}
	return memory.ListOptions{Spaces: spaces, Kind: q.Get("kind"), Status: q.Get("status"), Repo: q.Get("repo"), Tag: q.Get("tag"),
		Query: q.Get("q"), Page: queryInt(q.Get("page"), 1), PerPage: queryInt(q.Get("per_page"), 20)}
}

func listMemories(s memoryService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		page, err := s.ListMemories(c.Request.Context(), memoryListOptions(c))
		if err != nil {
			return memoryError(c, err)
		}
		return c.SendJSON(page)
	}
}

func listMemorySpaces(s memoryService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		spaces, err := s.MemorySpaces(c.Request.Context())
		if err != nil {
			return memoryError(c, err)
		}
		return c.SendJSON(spaces)
	}
}

func searchMemories(s memoryService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		q := c.Request.URL.Query()
		res, err := s.RecallMemories(c.Request.Context(), q.Get("q"), q.Get("mode"), memoryListOptions(c), queryInt(q.Get("limit"), 20))
		if err != nil {
			return memoryError(c, err)
		}
		return c.SendJSON(res)
	}
}

func getMemory(s memoryService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		m, err := s.Memory(c.Request.Context(), c.Request.PathValue("id"))
		if err != nil {
			return memoryError(c, err)
		}
		if c.Request.URL.Query().Get("format") == "markdown" {
			c.Response.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, err := c.Response.Write([]byte(memory.Markdown(m)))
			return err
		}
		return c.SendJSON(m)
	}
}

// exportMemories downloads the filtered memories as one Markdown document.
func exportMemories(s memoryService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		opts := memoryListOptions(c)
		opts.PerPage = 100
		var all []*memory.Memory
		for opts.Page = 1; ; opts.Page++ {
			page, err := s.ListMemories(c.Request.Context(), opts)
			if err != nil {
				return memoryError(c, err)
			}
			all = append(all, page.Items...)
			if len(page.Items) == 0 || opts.Page*page.PerPage >= page.Total || len(all) >= 5000 {
				break
			}
		}
		title := "Krabby memories"
		if len(opts.Spaces) > 0 {
			title += " — " + strings.Join(opts.Spaces, ", ")
		}
		c.Response.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		c.Response.Header().Set("Content-Disposition", `attachment; filename="krabby-memories-`+time.Now().UTC().Format("20060102")+`.md"`)
		_, err := c.Response.Write([]byte(memory.MarkdownSpace(title, all)))
		return err
	}
}

func createMemory(s memoryService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var in memory.Input
		if err := decodeMemory(c, &in); err != nil {
			return memoryError(c, err)
		}
		res, err := s.Remember(c.Request.Context(), in)
		if err != nil {
			return memoryError(c, err)
		}
		if res.Created {
			c.SetStatus(http.StatusCreated)
		}
		return c.SendJSON(res)
	}
}

func updateMemory(s memoryService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var p memory.Patch
		if err := decodeMemory(c, &p); err != nil {
			return memoryError(c, err)
		}
		m, err := s.UpdateMemory(c.Request.Context(), c.Request.PathValue("id"), p)
		if err != nil {
			return memoryError(c, err)
		}
		return c.SendJSON(m)
	}
}

func deleteMemory(s memoryService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		if err := s.ForgetMemory(c.Request.Context(), c.Request.PathValue("id")); err != nil {
			return memoryError(c, err)
		}
		return c.SendJSON(map[string]bool{"ok": true})
	}
}
