package server

import (
	"context"
	"net/http"

	"github.com/rakunlabs/ada"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/settings"
)

// settingsResponse is a redacted view of the running config for the UI. Secrets
// (the webhook secret) are deliberately omitted; booleans indicate
// only whether they are configured.
type settingsResponse struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	LogLevel  string `json:"log_level"`
	DataDir   string `json:"data_dir"`

	Server struct {
		Host     string `json:"host"`
		Port     string `json:"port"`
		BasePath string `json:"base_path"`
	} `json:"server"`

	MCP struct {
		Path string `json:"path"`
	} `json:"mcp"`

	Bag struct {
		Bin          string `json:"bin"`
		Version      string `json:"version"`
		BuildTimeout string `json:"build_timeout"`
	} `json:"bag"`
}

func getSettings(cfg *config.Config, system systemInfoService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var s settingsResponse

		s.Version = config.Version
		s.Commit = config.Commit
		s.BuildDate = config.Date
		s.LogLevel = cfg.LogLevel
		s.DataDir = cfg.DataDir

		s.Server.Host = cfg.Server.Host
		s.Server.Port = cfg.Server.Port
		s.Server.BasePath = cfg.Server.BasePath

		s.MCP.Path = cfg.MCP.Path

		s.Bag.Bin = "embedded"
		s.Bag.Version = system.GraphEngineVersion()
		s.Bag.BuildTimeout = cfg.Bag.BuildTimeout.String()

		return c.SendJSON(s)
	}
}

func getDocsConfig(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		cfg, err := mgr.GetDocsConfig(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(cfg)
	}
}

func setDocsConfig(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var patch settings.Patch
		if err := c.Bind(&patch); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		cfg, err := mgr.PatchDocsConfig(c.Request.Context(), patch)
		if err != nil {
			// Settings were saved but the client rebuild failed: report the
			// error while still returning the redacted (persisted) config.
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]any{
				"error":  err.Error(),
				"config": cfg,
			})
		}

		return c.SendJSON(cfg)
	}
}

func testLLM(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var patch settings.Patch
		if c.Request.ContentLength != 0 {
			if err := c.Bind(&patch); err != nil {
				return c.SetStatus(http.StatusBadRequest).Err(err)
			}
		}

		merged, err := applySettingsPatch(c.Request.Context(), mgr, patch)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(mgr.TestLLM(c.Request.Context(), merged))
	}
}

func testEmbedder(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var patch settings.Patch
		if c.Request.ContentLength != 0 {
			if err := c.Bind(&patch); err != nil {
				return c.SetStatus(http.StatusBadRequest).Err(err)
			}
		}

		merged, err := applySettingsPatch(c.Request.Context(), mgr, patch)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(mgr.TestEmbedder(c.Request.Context(), merged))
	}
}

func testLangfuse(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var patch settings.Patch
		if c.Request.ContentLength != 0 {
			if err := c.Bind(&patch); err != nil {
				return c.SetStatus(http.StatusBadRequest).Err(err)
			}
		}

		merged, err := applySettingsPatch(c.Request.Context(), mgr, patch)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(mgr.TestLangfuse(c.Request.Context(), merged))
	}
}

func testCodeEmbedder(mgr docsSettingsService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var patch settings.Patch
		if c.Request.ContentLength != 0 {
			if err := c.Bind(&patch); err != nil {
				return c.SetStatus(http.StatusBadRequest).Err(err)
			}
		}

		merged, err := applySettingsPatch(c.Request.Context(), mgr, patch)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(mgr.TestCodeEmbedder(c.Request.Context(), merged))
	}
}

func applySettingsPatch(ctx context.Context, mgr docsSettingsService, patch settings.Patch) (settings.Settings, error) {
	current, err := mgr.GetDocsConfig(ctx)
	if err != nil {
		return settings.Settings{}, err
	}

	return patch.Apply(current.Settings), nil
}
