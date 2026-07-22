package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/service/websource"
)

// ErrNoWebSources is returned when web-source methods are called before the
// store has been attached.
var ErrNoWebSources = errors.New("web sources are not configured")

// SetWebSources attaches the web-source store and the fetcher per collection
// type. Called once at startup.
func (m *Manager) SetWebSources(store *websource.Store, fetchers map[string]websource.Fetcher) {
	m.webStore = store
	m.webFetchers = fetchers
}

// WebSourceTypes returns the registered collection types.
func (m *Manager) WebSourceTypes() []string {
	types := make([]string, 0, len(m.webFetchers))
	for t := range m.webFetchers {
		types = append(types, t)
	}

	return types
}

// sourcesDir returns the markdown content directory of one collection.
func (m *Manager) sourcesDir(name string) string {
	return filepath.Join(m.sourcesRootDir, name)
}

// AddWebCollection validates and stores a new collection, then triggers its
// first sync in the background.
func (m *Manager) AddWebCollection(ctx context.Context, col *websource.Collection) error {
	if m.webStore == nil {
		return ErrNoWebSources
	}

	if !websource.ValidName(col.Name) {
		return fmt.Errorf("invalid collection name %q (want lowercase [a-z0-9._-])", col.Name)
	}

	fetcher, ok := m.webFetchers[col.Type]
	if !ok {
		return fmt.Errorf("unknown source type %q", col.Type)
	}
	config, err := fetcher.MergeConfig(nil, col.Config)
	if err != nil {
		return err
	}
	col.Config = config

	if existing, err := m.webStore.GetCollection(ctx, col.Name); err != nil {
		return err
	} else if existing != nil {
		return fmt.Errorf("collection %s already exists", col.Name)
	}

	col.Status = websource.StatusPending
	col.CreatedAt = time.Now()

	if err := m.webStore.UpsertCollection(ctx, col); err != nil {
		return err
	}

	m.TriggerWebRefresh(col.Name)

	return nil
}

// UpdateWebCollection replaces the mutable configuration of a collection. An
// empty inbound Confluence API token keeps the stored one.
func (m *Manager) UpdateWebCollection(ctx context.Context, col *websource.Collection) error {
	if m.webStore == nil {
		return ErrNoWebSources
	}

	existing, err := m.webStore.GetCollection(ctx, col.Name)
	if err != nil {
		return err
	}

	if existing == nil {
		return fmt.Errorf("collection %s not found", col.Name)
	}

	col.Type = existing.Type // the type is immutable once created
	fetcher, ok := m.webFetchers[col.Type]
	if !ok {
		return fmt.Errorf("no fetcher for source type %q", col.Type)
	}
	config, err := fetcher.MergeConfig(existing.Config, col.Config)
	if err != nil {
		return err
	}
	col.Config = config
	col.Status = existing.Status
	col.LastError = existing.LastError
	col.LastRefreshAt = existing.LastRefreshAt
	col.CreatedAt = existing.CreatedAt

	return m.webStore.UpsertCollection(ctx, col)
}

// WebSourceConfigView returns a provider-owned, redacted config shape for the
// REST API. The common manager does not inspect provider-specific settings.
func (m *Manager) WebSourceConfigView(col *websource.Collection) any {
	if col == nil {
		return nil
	}
	fetcher := m.webFetchers[col.Type]
	if fetcher == nil {
		return nil
	}
	return fetcher.ConfigView(col.Config)
}

// DeleteWebCollection removes the collection, its pages, files and vectors.
func (m *Manager) DeleteWebCollection(ctx context.Context, name string) error {
	if m.webStore == nil {
		return ErrNoWebSources
	}

	scope := websource.ScopeKey(name)

	l := m.lock(scope)
	l.Lock()
	defer l.Unlock()

	col, err := m.webStore.GetCollection(ctx, name)
	if err != nil {
		return err
	}

	if col == nil {
		return fmt.Errorf("collection %s not found", name)
	}

	// Best-effort: drop the collection's vectors from the docs index.
	d, releaseDocs := m.acquireDocs()
	if d.rag != nil {
		if err := d.rag.DeleteRepo(ctx, scope); err != nil {
			slog.Error("delete web source from docs index", "source", name, "error", err)
		}
	}
	releaseDocs()

	if err := m.webStore.DeleteCollection(ctx, name); err != nil {
		return err
	}

	dir := m.sourcesDir(name)
	if m.sourcesRootDir != "" && filepath.HasPrefix(dir, m.sourcesRootDir) {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove source content %s; %w", dir, err)
		}
	}

	return nil
}

// ListWebCollections returns all collections.
func (m *Manager) ListWebCollections(ctx context.Context) ([]*websource.Collection, error) {
	if m.webStore == nil {
		return []*websource.Collection{}, nil
	}

	return m.webStore.ListCollections(ctx)
}

// WebCollection returns one collection, or nil when it does not exist.
func (m *Manager) WebCollection(ctx context.Context, name string) (*websource.Collection, error) {
	if m.webStore == nil {
		return nil, ErrNoWebSources
	}

	return m.webStore.GetCollection(ctx, name)
}

// WebPages returns the page records of one collection.
func (m *Manager) WebPages(ctx context.Context, name string) ([]*websource.Page, error) {
	if m.webStore == nil {
		return nil, ErrNoWebSources
	}

	return m.webStore.Pages(ctx, name)
}

// AddWebPage registers a page URL on a "pages" collection and triggers a
// background sync.
func (m *Manager) AddWebPage(ctx context.Context, name, pageURL string) (*websource.Page, error) {
	if m.webStore == nil {
		return nil, ErrNoWebSources
	}

	col, err := m.webStore.GetCollection(ctx, name)
	if err != nil {
		return nil, err
	}

	if col == nil {
		return nil, fmt.Errorf("collection %s not found", name)
	}

	if col.Type != websource.TypePages {
		return nil, fmt.Errorf("collection %s is type %q; pages are discovered, not added manually", name, col.Type)
	}

	pageURL = strings.TrimSpace(pageURL)
	if !strings.HasPrefix(pageURL, "http://") && !strings.HasPrefix(pageURL, "https://") {
		return nil, fmt.Errorf("page url must be http(s): %q", pageURL)
	}

	slug := slugForURL(pageURL)

	page := &websource.Page{
		ID:         websource.PageID(name, slug),
		Collection: name,
		Slug:       slug,
		URL:        pageURL,
		Status:     websource.StatusPending,
	}

	if err := m.webStore.UpsertPage(ctx, page); err != nil {
		return nil, err
	}

	m.TriggerWebRefresh(name)

	return page, nil
}

// DeleteWebPage removes a page record, its markdown file, and reindexes.
func (m *Manager) DeleteWebPage(ctx context.Context, name, slug string) error {
	if m.webStore == nil {
		return ErrNoWebSources
	}

	scope := websource.ScopeKey(name)

	l := m.lock(scope)
	l.Lock()
	defer l.Unlock()

	if err := m.webStore.DeletePage(ctx, websource.PageID(name, slug)); err != nil {
		return err
	}

	_ = os.Remove(filepath.Join(m.sourcesDir(name), slug+".md"))

	m.indexWebSource(ctx, name)

	return nil
}

// WebSourceDoc reads one synced markdown document, sandboxed to the
// collection's content directory.
func (m *Manager) WebSourceDoc(ctx context.Context, name, docPath string) (*repofs.FileContent, error) {
	if m.webStore == nil {
		return nil, ErrNoWebSources
	}

	col, err := m.webStore.GetCollection(ctx, name)
	if err != nil {
		return nil, err
	}

	if col == nil {
		return nil, fmt.Errorf("collection %s not found", name)
	}

	return repofs.ReadFile(m.sourcesDir(name), docPath, 0, 0)
}

// TriggerWebRefresh starts a background sync of one collection.
func (m *Manager) TriggerWebRefresh(name string) {
	m.webJobMu.Lock()
	if _, running := m.webJobs[name]; running {
		m.webJobMu.Unlock()

		return
	}
	m.webJobs[name] = struct{}{}
	m.webJobMu.Unlock()

	if !m.startWork() {
		m.webJobMu.Lock()
		delete(m.webJobs, name)
		m.webJobMu.Unlock()

		return
	}

	go func() {
		defer m.wg.Done()
		defer func() {
			m.webJobMu.Lock()
			delete(m.webJobs, name)
			m.webJobMu.Unlock()
		}()

		if err := m.RefreshWebSource(m.baseCtx, name); err != nil {
			slog.Error("refresh web source", "source", name, "error", err)
		}
	}()
}

// RefreshDueWebSources triggers a sync for every collection whose refresh
// interval has elapsed. Called by the scheduler.
func (m *Manager) RefreshDueWebSources(ctx context.Context) {
	if m.webStore == nil {
		return
	}

	cols, err := m.webStore.ListCollections(ctx)
	if err != nil {
		slog.Error("list web sources for schedule", "error", err)

		return
	}

	now := time.Now()
	for _, col := range cols {
		if col.RefreshInterval <= 0 {
			continue
		}

		if col.Status == websource.StatusFetching {
			continue
		}

		if col.LastRefreshAt.IsZero() || now.Sub(col.LastRefreshAt) >= col.RefreshInterval {
			m.TriggerWebRefresh(col.Name)
		}
	}
}

// RefreshWebSource synchronously fetches a collection, writes changed pages
// to disk and reindexes the collection when anything changed.
func (m *Manager) RefreshWebSource(ctx context.Context, name string) error {
	if m.webStore == nil {
		return ErrNoWebSources
	}

	scope := websource.ScopeKey(name)

	l := m.lock(scope)
	l.Lock()
	defer l.Unlock()

	col, err := m.webStore.GetCollection(ctx, name)
	if err != nil {
		return err
	}

	if col == nil {
		return fmt.Errorf("collection %s not found", name)
	}

	fetcher, ok := m.webFetchers[col.Type]
	if !ok {
		return fmt.Errorf("no fetcher for source type %q", col.Type)
	}

	m.setActivity(scope, "sync")
	defer m.clearActivity(scope, "sync")

	col.Status = websource.StatusFetching
	col.LastError = ""
	_ = m.webStore.UpsertCollection(ctx, col)

	fail := func(ferr error) error {
		col.Status = websource.StatusError
		col.LastError = ferr.Error()
		_ = m.webStore.UpsertCollection(context.WithoutCancel(ctx), col)

		return ferr
	}

	pages, err := m.webStore.Pages(ctx, name)
	if err != nil {
		return fail(err)
	}

	remotes, err := fetcher.Fetch(ctx, col, pages)
	if err != nil {
		return fail(fmt.Errorf("fetch %s; %w", name, err))
	}

	dir := m.sourcesDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(fmt.Errorf("mkdir %s; %w", dir, err))
	}

	existing := map[string]*websource.Page{}
	for _, p := range pages {
		existing[p.Slug] = p
	}

	changed := false
	seen := map[string]bool{}
	now := time.Now()

	for _, remote := range remotes {
		seen[remote.Slug] = true

		rec := existing[remote.Slug]
		if rec == nil {
			rec = &websource.Page{
				ID:         websource.PageID(name, remote.Slug),
				Collection: name,
				Slug:       remote.Slug,
			}
		}

		rec.URL = remote.URL
		rec.LastFetchAt = now

		if remote.Err != nil {
			// Keep the previous content; record the failure.
			rec.Status = websource.StatusError
			rec.LastError = remote.Err.Error()
			_ = m.webStore.UpsertPage(ctx, rec)

			continue
		}

		if remote.Title != "" {
			rec.Title = remote.Title
		}

		markdown := withTitleHeading(remote.Markdown, rec.Title)
		hash := websource.Hash(markdown)
		file := filepath.Join(dir, remote.Slug+".md")

		if hash != rec.Hash || !fileExists(file) {
			if err := os.WriteFile(file, []byte(markdown), 0o644); err != nil {
				return fail(fmt.Errorf("write %s; %w", file, err))
			}

			rec.Hash = hash
			changed = true
		}

		rec.Status = websource.StatusReady
		rec.LastError = ""

		if err := m.webStore.UpsertPage(ctx, rec); err != nil {
			return fail(err)
		}
	}

	// Discovery types own the page set: records that vanished remotely are
	// dropped. URL-list collections keep every user-registered page.
	if col.Type != websource.TypePages {
		for slug, rec := range existing {
			if seen[slug] {
				continue
			}

			if err := m.webStore.DeletePage(ctx, rec.ID); err != nil {
				return fail(err)
			}

			_ = os.Remove(filepath.Join(dir, slug+".md"))
			changed = true
		}
	}

	if changed {
		m.indexWebSource(ctx, name)
	}

	col.Status = websource.StatusReady
	col.LastError = ""
	col.LastRefreshAt = time.Now()

	if err := m.webStore.UpsertCollection(context.WithoutCancel(ctx), col); err != nil {
		return err
	}

	slog.Info("web source synced", "source", name, "pages", len(remotes), "changed", changed)

	return nil
}

// indexWebSource (re)indexes a collection's markdown into the docs RAG index.
// A disabled RAG subsystem is not an error: files stay on disk and are picked
// up by the next reindex-all after RAG is enabled.
func (m *Manager) indexWebSource(ctx context.Context, name string) {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()

	if d.rag == nil {
		slog.Debug("rag disabled; web source not indexed", "source", name)

		return
	}

	scope := websource.ScopeKey(name)

	m.setActivity(scope, "docs_index")
	defer m.clearActivity(scope, "docs_index")

	if err := d.rag.Index(ctx, scope, m.sourcesDir(name)); err != nil {
		slog.Error("index web source", "source", name, "error", err)
	}
}

// reindexAllWebSources rebuilds the vectors of every collection from the
// on-disk markdown. Used after live settings updates.
func (m *Manager) reindexAllWebSources(ctx context.Context) {
	if m.webStore == nil {
		return
	}

	cols, err := m.webStore.ListCollections(ctx)
	if err != nil {
		slog.Error("list web sources for reindex", "error", err)

		return
	}

	for _, col := range cols {
		scope := websource.ScopeKey(col.Name)

		l := m.lock(scope)
		l.Lock()
		m.indexWebSource(ctx, col.Name)
		l.Unlock()
	}
}

// withTitleHeading prepends "# title" when the markdown does not already
// start with a heading, so chunking and retrieval get a proper document title.
func withTitleHeading(markdown, title string) string {
	trimmed := strings.TrimSpace(markdown)
	if title == "" || strings.HasPrefix(trimmed, "#") {
		return trimmed + "\n"
	}

	return "# " + title + "\n\n" + trimmed + "\n"
}

// slugForURL derives a stable page slug from a URL: the slugified
// host+path, suffixed with a short hash so distinct URLs never collide.
func slugForURL(pageURL string) string {
	base := pageURL
	if _, rest, ok := strings.Cut(pageURL, "://"); ok {
		base = rest
	}

	return websource.Slugify(base) + "-" + websource.Hash(pageURL)[:8]
}
