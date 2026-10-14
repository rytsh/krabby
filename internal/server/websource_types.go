package server

import (
	"net/http"
	"strings"

	"github.com/rakunlabs/ada"

	"github.com/rytsh/krabby/internal/service/manager"
	"github.com/rytsh/krabby/internal/service/websource"
)

// teamTaggedTypes are the source types whose items carry team tags.
var teamTaggedTypes = map[string]bool{
	websource.TypeJira: true, websource.TypePush: true,
	"http": true, "sql": true, "forge": true, "chat": true, "feed": true,
}

func listSourceTypes(mgr sourceService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		return c.SendJSON(mgr.WebSourceDescriptors())
	}
}

type pushItemsRequest struct {
	Items []manager.WebPageImport `json:"items"`
}

// pushSourceItems upserts client-pushed items into a push source. Items are
// identified by their "id"; binary documents are sent with encoding "base64".
func pushSourceItems(mgr sourceService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req pushItemsRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		result, err := mgr.ImportWebPages(c.Request.Context(), c.Request.PathValue("name"), req.Items)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(result)
	}
}

type pruneItemsRequest struct {
	Keep []string `json:"keep"`
}

func prunePushItems(mgr sourceService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req pruneItemsRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		n, err := mgr.PrunePushItems(c.Request.Context(), c.Request.PathValue("name"), req.Keep)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendJSON(map[string]int{"removed": n})
	}
}

func deletePushItem(mgr sourceService) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id := strings.TrimSpace(c.Request.URL.Query().Get("id"))
		if id == "" {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": "id query param is required"})
		}

		if err := mgr.DeleteWebPage(c.Request.Context(), c.Request.PathValue("name"), manager.PushSlug(id)); err != nil {
			return c.SetStatus(http.StatusBadRequest).SendJSON(map[string]string{"error": err.Error()})
		}

		return c.SendNoContent()
	}
}
