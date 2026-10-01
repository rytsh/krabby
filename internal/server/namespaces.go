package server

import (
	"context"
	"net/http"

	"github.com/rakunlabs/ada"
	"github.com/worldline-go/types"
)

// listRepoNamespaces returns the namespace groups (namespace + count +
// description). Untagged repos are reported under "default".
func listRepoNamespaces(mgr repoReader) ada.HandlerFunc {
	return func(c *ada.Context) error {
		namespaces, err := mgr.RepoNamespaces(c.Request.Context())
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(namespaces)
	}
}

type upsertNamespaceRequest struct {
	Name string `json:"name"`
	// Description is nullable: omit it to keep the stored one, send null to
	// clear it, send a value to replace it. The record is written whole, so
	// without this an update that only meant to touch something else erased it.
	Description types.Null[string] `json:"description"`
}

// upsertNamespace creates or updates a namespace's description record.
func upsertNamespace(mgr repoAdmin) ada.HandlerFunc {
	return func(c *ada.Context) error {
		var req upsertNamespaceRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		rec, err := mgr.UpsertNamespace(context.WithoutCancel(c.Request.Context()), req.Name, req.Description)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(rec)
	}
}

// deleteNamespace removes a namespace's description record (repos keep the tag).
func deleteNamespace(mgr repoAdmin) ada.HandlerFunc {
	return func(c *ada.Context) error {
		name := c.Request.PathValue("name")
		if err := mgr.DeleteNamespace(context.WithoutCancel(c.Request.Context()), name); err != nil {
			return c.Err(err)
		}

		return c.SetStatus(http.StatusNoContent).SendJSON(nil)
	}
}

type setRepoNamespaceRequest struct {
	Namespace string `json:"namespace"`
}

// setRepoNamespace moves a repo into a namespace ("" / "default" returns it to
// the default bucket). It only re-tags the record; no rebuild is triggered.
func setRepoNamespace(mgr repoAdmin) ada.HandlerFunc {
	return func(c *ada.Context) error {
		id := repoID(c.Request)

		var req setRepoNamespaceRequest
		if err := c.Bind(&req); err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(err)
		}

		repo, err := mgr.SetRepoNamespace(context.WithoutCancel(c.Request.Context()), id, req.Namespace)
		if err != nil {
			return c.Err(err)
		}

		return c.SendJSON(repo)
	}
}
