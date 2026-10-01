package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/mcpclient"
	"github.com/rytsh/krabby/internal/service/registry"
)

func (m *Manager) SetBigPictures(store *bigpicture.Store) { m.bigPictures = store }

func (m *Manager) ListBigPictures(ctx context.Context, opts bigpicture.ListOptions) (bigpicture.Page, error) {
	if m.bigPictures == nil {
		return bigpicture.Page{}, errors.New("big picture store unavailable")
	}
	return m.bigPictures.List(ctx, opts)
}

func (m *Manager) BigPicture(ctx context.Context, name string) (*bigpicture.Picture, error) {
	if m.bigPictures == nil {
		return nil, errors.New("big picture store unavailable")
	}
	return m.bigPictures.Get(ctx, name)
}

func (m *Manager) SaveBigPicture(ctx context.Context, name string, cfg bigpicture.Config) (*bigpicture.Picture, error) {
	// Serialize graph edits so concurrent reciprocal references cannot both pass
	// cycle validation against the old graph.
	defer m.lockKey("bigpicture-source-graph")()
	if m.bigPictures == nil {
		return nil, errors.New("big picture store unavailable")
	}
	cfg, err := bigpicture.NormalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := m.validatePictureSources(ctx, cfg.Sources); err != nil {
		return nil, err
	}
	if err := m.validatePictureGraph(ctx, cfg.Name, cfg.Sources); err != nil {
		return nil, err
	}
	return m.bigPictures.Save(ctx, name, cfg)
}

func (m *Manager) validatePictureGraph(ctx context.Context, name string, sources []bigpicture.Source) error {
	active, visited := map[string]bool{}, map[string]bool{}
	var visit func(string, []bigpicture.Source) error
	visit = func(current string, refs []bigpicture.Source) error {
		if active[current] {
			return fmt.Errorf("%w: Big Picture source references must not form a cycle", bigpicture.ErrInvalid)
		}
		if visited[current] {
			return nil
		}
		active[current] = true
		for _, source := range refs {
			if source.Kind != "bigpicture" {
				continue
			}
			if active[source.Ref] {
				return fmt.Errorf("%w: Big Picture source references must not form a cycle", bigpicture.ErrInvalid)
			}
			if visited[source.Ref] {
				continue
			}
			child, err := m.BigPicture(ctx, source.Ref)
			if err != nil {
				return err
			}
			if err := visit(child.Name, child.Sources); err != nil {
				return err
			}
		}
		delete(active, current)
		visited[current] = true
		return nil
	}
	return visit(name, sources)
}

func (m *Manager) validatePictureSources(ctx context.Context, sources []bigpicture.Source) error {
	for _, source := range sources {
		found := false
		var err error
		switch source.Kind {
		case "bigpicture":
			if m.bigPictures != nil {
				picture, lookupErr := m.bigPictures.Get(ctx, source.Ref)
				found, err = picture != nil, lookupErr
				if errors.Is(err, bigpicture.ErrNotFound) {
					err = nil
				}
			}
		case "namespace":
			if m.reg != nil {
				groups, lookupErr := m.reg.Namespaces(ctx)
				err = lookupErr
				for _, group := range groups {
					if group.Namespace == source.Ref {
						found = true
						break
					}
				}
			}
		case "repo":
			if m.reg != nil {
				repo, lookupErr := m.reg.Get(ctx, source.Ref)
				found, err = repo != nil, lookupErr
			}
		case "web":
			if m.webStore != nil {
				col, lookupErr := m.webStore.GetCollection(ctx, source.Ref)
				found, err = col != nil, lookupErr
			}
		case "api":
			if m.apiStore != nil {
				service, lookupErr := m.apiStore.GetService(ctx, source.Ref)
				found, err = service != nil, lookupErr
			}
		case "mcp":
			if m.externalMCPs != nil {
				connection, lookupErr := m.externalMCPs.Get(ctx, source.Ref)
				found, err = connection != nil, lookupErr
				if errors.Is(err, mcpclient.ErrNotFound) {
					err = nil
				}
			}
		}
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: source %s:%s does not exist", bigpicture.ErrInvalid, source.Kind, source.Ref)
		}
	}
	return nil
}

func (m *Manager) DeleteBigPicture(ctx context.Context, name string, expectedVersion uint64) error {
	defer m.lockKey(bigpicture.ScopeKey(name))()
	if m.bigPictures == nil {
		return errors.New("big picture store unavailable")
	}
	p, err := m.bigPictures.Get(ctx, name)
	if err != nil {
		return err
	}
	if err := m.bigPictures.Delete(ctx, name, expectedVersion); err != nil {
		return err
	}
	b, release := m.acquireDocs()
	defer release()
	for _, key := range p.IndexKeys() {
		if m.docsText != nil {
			if err := m.docsText.DeleteRepo(ctx, key); err != nil {
				slog.Warn("clean deleted big picture text index", "name", name)
			}
		}
		if b != nil && b.store != nil {
			if err := b.store.DeleteRepo(ctx, key); err != nil {
				slog.Warn("clean deleted big picture vector index", "name", name)
			}
		}
	}
	return nil
}

func (m *Manager) PublishBigPicture(ctx context.Context, name string, pub bigpicture.Publication) (*bigpicture.Snapshot, error) {
	defer m.lockKey(bigpicture.ScopeKey(name))()
	if m.bigPictures == nil {
		return nil, errors.New("big picture store unavailable")
	}
	snapshot, err := m.bigPictures.Publish(ctx, name, pub)
	if err != nil {
		return nil, err
	}
	if m.queue != nil {
		if err := m.scheduleReindex(bigpicture.ScopeKey(name)); err != nil {
			slog.Warn("big picture published; index enqueue failed", "name", name)
		}
	}
	return snapshot, nil
}

func (m *Manager) BigPictureSnapshot(ctx context.Context, name, revision string) (*bigpicture.Snapshot, error) {
	if m.bigPictures == nil {
		return nil, errors.New("big picture store unavailable")
	}
	return m.bigPictures.Snapshot(ctx, name, revision)
}

func (m *Manager) ReadBigPictureDocument(ctx context.Context, name, revision, path string, offset int64, maxBytes int) (*bigpicture.DocumentRead, error) {
	if m.bigPictures == nil {
		return nil, errors.New("big picture store unavailable")
	}
	return m.bigPictures.ReadDocument(ctx, name, revision, path, offset, maxBytes)
}

type PictureSourceOption struct {
	bigpicture.Source
	Title     string `json:"title"`
	Namespace string `json:"namespace,omitempty"`
	Status    string `json:"status,omitempty"`
}

type PictureSourcePage struct {
	Items   []PictureSourceOption `json:"items"`
	Total   int                   `json:"total"`
	Page    int                   `json:"page"`
	PerPage int                   `json:"per_page"`
}

func (m *Manager) BigPictureSourceOptions(ctx context.Context, kind, text string, page, perPage int) (PictureSourcePage, error) {
	page = max(1, page)
	if page > 1000000 {
		return PictureSourcePage{}, fmt.Errorf("%w: source page exceeds limit", bigpicture.ErrInvalid)
	}
	if perPage <= 0 {
		perPage = 20
	}
	perPage = min(100, perPage)
	out := PictureSourcePage{Items: []PictureSourceOption{}, Page: page, PerPage: perPage}
	if kind == "repo" {
		if m.reg == nil {
			return out, nil
		}
		repos, total, err := m.reg.ListPaged(ctx, registry.ListOptions{Page: page, PerPage: perPage, Search: text, Namespace: "*"})
		if err != nil {
			return out, err
		}
		out.Total = total
		for _, repo := range repos {
			out.Items = append(out.Items, PictureSourceOption{Source: bigpicture.Source{Kind: kind, Ref: repo.ID}, Title: repo.ID, Namespace: bigpicture.NormalizeNamespace(repo.Namespace), Status: repo.Status})
		}
		return out, nil
	}
	options := []PictureSourceOption{}
	switch kind {
	case "bigpicture":
		if m.bigPictures == nil {
			return out, nil
		}
		result, err := m.bigPictures.List(ctx, bigpicture.ListOptions{Namespace: "*", Query: text, Page: page, PerPage: perPage})
		if err != nil {
			return out, err
		}
		out.Total = result.Total
		for _, item := range result.Items {
			status := "not published"
			if item.CurrentRevision != "" {
				status = "published"
			}
			out.Items = append(out.Items, PictureSourceOption{Source: bigpicture.Source{Kind: kind, Ref: item.Name}, Title: item.Title, Namespace: item.Namespace, Status: status})
		}
		return out, nil
	case "namespace":
		if m.reg != nil {
			groups, err := m.reg.Namespaces(ctx)
			if err != nil {
				return out, err
			}
			for _, group := range groups {
				options = append(options, PictureSourceOption{Source: bigpicture.Source{Kind: kind, Ref: group.Namespace}, Title: group.Description, Status: fmt.Sprintf("%d repositories", group.Count)})
			}
		}
	case "web":
		if m.webStore != nil {
			items, err := m.webStore.ListCollections(ctx)
			if err != nil {
				return out, err
			}
			for _, item := range items {
				options = append(options, PictureSourceOption{Source: bigpicture.Source{Kind: kind, Ref: item.Name}, Title: item.Name, Status: item.Status})
			}
		}
	case "api":
		if m.apiStore != nil {
			items, err := m.apiStore.ListServices(ctx)
			if err != nil {
				return out, err
			}
			for _, item := range items {
				options = append(options, PictureSourceOption{Source: bigpicture.Source{Kind: kind, Ref: item.Name}, Title: item.Title, Status: item.Status})
			}
		}
	case "mcp":
		if m.externalMCPs != nil {
			items, err := m.externalMCPs.List(ctx)
			if err != nil {
				return out, err
			}
			for _, item := range items {
				status := "enabled"
				if item.Disabled {
					status = "disabled"
				}
				options = append(options, PictureSourceOption{Source: bigpicture.Source{Kind: kind, Ref: item.Name}, Title: item.Description, Status: status})
			}
		}
	default:
		return out, fmt.Errorf("%w: source kind must be repo, namespace, bigpicture, web, api or mcp", bigpicture.ErrInvalid)
	}
	text = strings.ToLower(strings.TrimSpace(text))
	options = slices.DeleteFunc(options, func(option PictureSourceOption) bool {
		return !strings.Contains(strings.ToLower(option.Ref+" "+option.Title), text)
	})
	slices.SortFunc(options, func(a, b PictureSourceOption) int { return strings.Compare(a.Ref, b.Ref) })
	out.Total = len(options)
	if page <= (len(options)+perPage-1)/perPage {
		start := (page - 1) * perPage
		out.Items = options[start:min(start+perPage, len(options))]
	}
	return out, nil
}
