package manager

import (
	"context"
	"log/slog"

	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/websource"
)

// enrichDocSources makes every broad-search result identify its exact scope and
// source kind. Web hits additionally carry collection metadata and item links,
// and API hits the service they belong to.
func (m *Manager) enrichDocSources(ctx context.Context, docs []rag.Doc) {
	collections := map[string]*websource.Collection{}
	services := map[string]*apicatalog.Service{}
	for i := range docs {
		name, revision := bigpicture.ParseIndexKey(docs[i].Repo)
		if name == "" && docs[i].Revision != "" {
			name, revision = bigpicture.Name(docs[i].Repo), docs[i].Revision
		}
		if name != "" {
			docs[i].Repo, docs[i].ScopeKey = bigpicture.ScopeKey(name), bigpicture.ScopeKey(name)
			docs[i].Revision, docs[i].SourceKind = revision, "bigpicture"
			docs[i].Evidence = rag.DocEvidence{Kind: "architecture_snapshot"}
			if m.bigPictures != nil {
				if p, err := m.BigPicture(ctx, name); err == nil {
					docs[i].Namespace = p.Namespace
					for _, r := range p.Revisions {
						if r.ID == revision {
							docs[i].UpdatedAt = r.PublishedAt
						}
					}
				}
			}
			continue
		}
		docs[i].ScopeKey = docs[i].Repo

		if service := apicatalog.ServiceName(docs[i].Repo); service != "" {
			m.enrichAPIDoc(ctx, &docs[i], service, services)

			continue
		}

		name = websource.CollectionName(docs[i].Repo)
		if name == "" {
			docs[i].SourceKind = "repository"
			docs[i].Evidence = rag.DocEvidence{Kind: "generated_summary"}
			if m.reg != nil {
				repo, err := m.reg.Get(ctx, docs[i].Repo)
				if err == nil && repo != nil {
					docs[i].Namespace = registry.NormalizeNamespace(repo.Namespace)
					if docs[i].Namespace == "" {
						docs[i].Namespace = registry.NamespaceDefault
					}
				}
			}
			continue
		}
		docs[i].SourceKind = "web"
		docs[i].Evidence = rag.DocEvidence{Kind: "synced_snapshot"}
		docs[i].CollectionName = name
		if m.webStore == nil {
			continue
		}
		col, seen := collections[name]
		if !seen {
			var err error
			// A lookup failure degrades this hit's metadata but must not fail
			// the search, so it is logged rather than returned. The result is
			// cached either way: one broken store should not be re-queried once
			// per hit. Bounded by distinct collection names per search.
			if col, err = m.webStore.GetCollection(ctx, name); err != nil {
				slog.Warn("enrich web doc: collection lookup failed",
					"collection", name, "error", err)
			}
			collections[name] = col
		}
		if col != nil {
			docs[i].CollectionType = col.Type
			docs[i].CollectionDescription = col.Description
			docs[i].Evidence.CollectionRefreshedAt = col.LastRefreshAt
			docs[i].Evidence.SyncStatus = col.Status
		}

		slug := docSlug(docs[i].Path)
		page, err := m.webStore.GetPage(ctx, websource.PageID(name, slug))
		if err != nil {
			// Debug rather than warn: this runs once per hit, so a store
			// outage would otherwise emit a line per result.
			slog.Debug("enrich web doc: page lookup failed",
				"collection", name, "slug", slug, "error", err)

			continue
		}
		if page == nil {
			continue
		}

		docs[i].URL = page.URL
		docs[i].Teams = page.Teams
		docs[i].Evidence.ItemStatus = page.Status
		docs[i].Evidence.IndexPending = page.IndexDirty
	}
}

// enrichAPIDoc annotates one API-catalog hit with the service it belongs to.
// The cache is per search, so a result set concentrated in one service reads
// the record once rather than per endpoint.
func (m *Manager) enrichAPIDoc(ctx context.Context, doc *rag.Doc, service string, cache map[string]*apicatalog.Service) {
	doc.SourceKind = "api"
	doc.Evidence = rag.DocEvidence{Kind: "catalog_snapshot"}
	doc.ServiceName = service

	if m.apiStore == nil {
		return
	}

	svc, seen := cache[service]
	if !seen {
		var err error
		// Logged, not returned: missing service metadata degrades the hit but
		// must not fail the search. Bounded by distinct service names.
		if svc, err = m.apiStore.GetService(ctx, service); err != nil {
			slog.Warn("enrich api doc: service lookup failed",
				"service", service, "error", err)
		}
		cache[service] = svc
	}
	if svc == nil {
		return
	}

	doc.ServiceGroup = svc.Group
	doc.ServiceBaseURL = svc.ResolvedBaseURL
	// The human override wins over the specification's own summary, exactly as
	// it does everywhere else the service is described.
	if doc.ServiceDescription = svc.Description; doc.ServiceDescription == "" {
		doc.ServiceDescription = svc.SpecSummary
	}
}
