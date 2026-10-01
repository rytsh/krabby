package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/rakunlabs/ada"
	"github.com/rytsh/krabby/internal/service/mcpclient"
)

func externalMCPError(c *ada.Context, err error) error {
	status := http.StatusInternalServerError
	message := "external MCP operation failed"
	switch {
	case errors.Is(err, mcpclient.ErrInvalid):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, mcpclient.ErrNotFound):
		status, message = http.StatusNotFound, err.Error()
	case errors.Is(err, mcpclient.ErrExists):
		status, message = http.StatusConflict, err.Error()
	}
	return c.SetStatus(status).SendJSON(map[string]string{"error": message})
}

// Bound administrator inputs and never echo malformed JSON (which may contain
// secret fields) into an error response or request log.
func decodeExternalMCP(c *ada.Context, dst any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response, c.Request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return mcpclient.ErrInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return mcpclient.ErrInvalid
	}
	return nil
}

func listExternalMCPs(s externalMCPService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		items, err := s.ListExternalMCPs(c.Request.Context())
		if err != nil {
			return externalMCPError(c, err)
		}
		return c.SendJSON(items)
	}
}

func saveExternalMCP(s externalMCPService, create bool) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var cfg mcpclient.Config
		if err := decodeExternalMCP(c, &cfg); err != nil {
			return externalMCPError(c, err)
		}
		name := ""
		if !create {
			name = c.Request.PathValue("name")
		}
		view, err := s.SaveExternalMCP(c.Request.Context(), name, cfg)
		if err != nil {
			return externalMCPError(c, err)
		}
		if create {
			c.SetStatus(http.StatusCreated)
		}
		return c.SendJSON(view)
	}
}

func deleteExternalMCP(s externalMCPService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		if err := s.DeleteExternalMCP(c.Request.Context(), c.Request.PathValue("name")); err != nil {
			return externalMCPError(c, err)
		}
		return c.SetStatus(http.StatusOK).SendJSON(map[string]bool{"ok": true})
	}
}

func discoverExternalMCP(s externalMCPService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req struct {
			ExistingName string `json:"existing_name"`
			mcpclient.Config
		}
		if err := decodeExternalMCP(c, &req); err != nil {
			return externalMCPError(c, err)
		}
		result, err := s.DiscoverExternalMCP(c.Request.Context(), req.ExistingName, req.Config)
		if err != nil {
			return externalMCPError(c, err)
		}
		return c.SendJSON(result)
	}
}
