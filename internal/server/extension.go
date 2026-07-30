package server

import (
	"net/http"
	"strconv"

	"github.com/rakunlabs/ada"

	browserextension "github.com/rytsh/krabby/browser-extension"
	"github.com/rytsh/krabby/internal/config"
)

func downloadBrowserExtension() ada.HandlerFunc {
	return func(c *ada.Context) error {
		archive, err := browserextension.Archive(config.Version)
		if err != nil {
			return c.SetStatus(http.StatusInternalServerError).Err(err)
		}

		c.Response.Header().Set("Content-Type", "application/zip")
		c.Response.Header().Set("Content-Disposition", `attachment; filename="`+browserextension.Filename(config.Version)+`"`)
		c.Response.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		c.Response.Header().Set("Cache-Control", "no-store")
		_, err = c.Response.Write(archive)

		return err
	}
}
