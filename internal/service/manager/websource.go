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

	"github.com/worldline-go/hardloop"

	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/repofs"
	"github.com/rytsh/krabby/internal/service/websource"
)

// ErrNoWebSources is returned when web-source methods are called before the
// store has been attached.
var ErrNoWebSources = errors.New("web sources are not configured")

// validateWebSpecs checks that every cron spec parses (hardloop syntax, e.g.
// "0 2 * * *" or "@every 6h"), so create/update fail fast with a clear error
// instead of the scheduler silently dropping an unparseable schedule.
func validateWebSpecs(specs []string) error {
	for _, spec := range specs {
		if strings.TrimSpace(spec) == "" {
			continue // empty entries are ignored by EffectiveSpecs
		}
		if _, err := hardloop.ParseStandard(spec); err != nil {
			return fmt.Errorf("invalid cron spec %q; %w", spec, err)
		}
	}

	return nil
}

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

	if err := validateWebSpecs(col.Specs); err != nil {
		return err
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

	if err := validateWebSpecs(col.Specs); err != nil {
		return err
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
	col.State = existing.State // preserve the provider sync watermark
	if col.Description == "" {
		col.Description = existing.Description // blank keeps the stored summary
	}

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

// WebPageCount returns the number of page records in one collection without
// loading them, for listings that only need the size.
func (m *Manager) WebPageCount(ctx context.Context, name string) (int, error) {
	if m.webStore == nil {
		return 0, ErrNoWebSources
	}

	return m.webStore.CountPages(ctx, name, "")
}

// WebSourceTeams returns the distinct team tags of one collection, sorted, for
// the UI team filter. Intended for jira sources.
func (m *Manager) WebSourceTeams(ctx context.Context, name string) ([]string, error) {
	if m.webStore == nil {
		return nil, ErrNoWebSources
	}

	return m.webStore.Teams(ctx, name)
}

// WebPagesByTeam returns the page records of one collection whose Teams
// contain team (case-insensitive). An empty team returns all pages. Filtering
// runs at the store level. Prefer WebPagesPaged for user-facing listings; this
// unpaginated form is kept for callers that need the full matching set.
func (m *Manager) WebPagesByTeam(ctx context.Context, name, team string) ([]*websource.Page, error) {
	if m.webStore == nil {
		return nil, ErrNoWebSources
	}

	if strings.TrimSpace(team) == "" {
		return m.webStore.Pages(ctx, name)
	}

	// A very large upper bound acts as "all matching"; team-filtered sets are
	// small (jira squads), so this stays bounded in practice.
	pages, _, err := m.webStore.PagesPaged(ctx, name, team, 0, 1_000_000)

	return pages, err
}

// WebPagesPaged returns one page (by offset/limit) of a collection's page
// records plus the total count, optionally restricted to a team
// (case-insensitive). Filtering and paging happen at the store level, so a
// large collection is never fully loaded into memory.
func (m *Manager) WebPagesPaged(ctx context.Context, name, team string, offset, limit int) ([]*websource.Page, int, error) {
	if m.webStore == nil {
		return nil, 0, ErrNoWebSources
	}

	return m.webStore.PagesPaged(ctx, name, team, offset, limit)
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

// TriggerWebRefresh queues a background sync of one collection on the central
// work queue. Duplicate syncs of the same collection coalesce (queue dedup),
// replacing the previous in-flight de-dup set, and the queue's concurrency
// limit bounds how many syncs run at once.
func (m *Manager) TriggerWebRefresh(name string) {
	m.queue.Submit(m.webSyncTask(name))
}

// webSyncTask builds the queue task for a web-source sync, carrying a Spec (the
// scope key as ID) so the sync is persisted and rebuildable after a restart.
func (m *Manager) webSyncTask(name string) queue.Task {
	scope := websource.ScopeKey(name)

	return queue.Task{
		ID:    scope,
		Kind:  taskKindWebSync,
		Title: "Sync " + scope,
		Key:   taskKindWebSync + ":" + name,
		Spec:  queue.Spec{Kind: taskKindWebSync, ID: scope},
		Run: func(ctx context.Context) error {
			if err := m.RefreshWebSource(ctx, name); err != nil {
				slog.Error("refresh web source", "source", name, "error", err)

				return err
			}

			return nil
		},
	}
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
		// Cron-scheduled collections are driven by the scheduler's hardloop
		// cron set (see WebSourceSchedules); skip them here so they are not
		// also polled on the fixed interval-tick.
		if len(col.Specs) > 0 {
			continue
		}
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

// WebSourceSchedule is one web collection's cron schedule, used by the
// scheduler to build a hardloop cron per source (mirroring RepoSchedule).
type WebSourceSchedule struct {
	Name  string
	Specs []string
}

// WebSourceSchedules returns the effective cron schedules of every collection
// that has one (explicit Specs, or an "@every <RefreshInterval>" fallback).
// Collections with neither are manual-only and omitted. The scheduler rebuilds
// its web-source cron set from this on every reconcile tick, so UI/REST changes
// take effect without a restart.
func (m *Manager) WebSourceSchedules(ctx context.Context) []WebSourceSchedule {
	if m.webStore == nil {
		return nil
	}

	cols, err := m.webStore.ListCollections(ctx)
	if err != nil {
		slog.Error("list web sources for schedule", "error", err)

		return nil
	}

	out := make([]WebSourceSchedule, 0, len(cols))
	for _, col := range cols {
		specs := col.EffectiveSpecs()
		if len(specs) == 0 {
			continue
		}
		out = append(out, WebSourceSchedule{Name: col.Name, Specs: specs})
	}

	return out
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

	// Progress is published throughout the sync so the UI can show live state.
	// The fetch phase count is unknown up front (the provider streams pages), so
	// it is indeterminate; the index phase below reports embedded/total chunks.
	m.setProgress(scope, Progress{Phase: "fetch"})
	defer m.clearProgress(scope)

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

	result, err := fetcher.Fetch(ctx, col, pages, col.State)
	if err != nil {
		return fail(fmt.Errorf("fetch %s; %w", name, err))
	}

	// After fetch, report how many pages the provider returned this run so the
	// "fetch" phase shows a concrete count while markdown is written to disk.
	m.setProgress(scope, Progress{Phase: "write", Done: 0, Total: len(result.Pages)})

	dir := m.sourcesDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(fmt.Errorf("mkdir %s; %w", dir, err))
	}

	existing := map[string]*websource.Page{}
	for _, p := range pages {
		existing[p.Slug] = p
	}

	// changedPaths / removedPaths drive incremental re-embedding: only the
	// docs whose markdown actually changed (or were deleted) are re-indexed,
	// so a large collection is not fully re-embedded when a few items change.
	var changedPaths, removedPaths []string
	seen := map[string]bool{}
	// updatedAt maps each page's doc path ("<slug>.md") to its source
	// last-modified time, so indexing can stamp it onto the page's vectors.
	updatedAt := map[string]time.Time{}
	now := time.Now()

	total := len(result.Pages)
	for i, remote := range result.Pages {
		// Report write-phase progress every so often so a big source shows a
		// moving bar without thrashing the progress map on every page.
		if i%25 == 0 {
			m.setProgress(scope, Progress{Phase: "write", Done: i, Total: total})
		}
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

		rec.Teams = remote.Teams
		rec.UpdatedAt = remote.UpdatedAt
		updatedAt[remote.Slug+".md"] = remote.UpdatedAt

		markdown := withTitleHeading(remote.Markdown, rec.Title)
		hash := websource.Hash(markdown)
		file := filepath.Join(dir, remote.Slug+".md")

		if hash != rec.Hash || !fileExists(file) {
			if err := os.WriteFile(file, []byte(markdown), 0o644); err != nil {
				return fail(fmt.Errorf("write %s; %w", file, err))
			}

			rec.Hash = hash
			changedPaths = append(changedPaths, remote.Slug+".md")
		}

		rec.Status = websource.StatusReady
		rec.LastError = ""

		if err := m.webStore.UpsertPage(ctx, rec); err != nil {
			return fail(err)
		}
	}

	// Pruning of vanished records. In a full discovery fetch, any record not
	// seen this run is gone. In an incremental fetch, unseen means unchanged,
	// so only the provider's explicit Removed list is pruned.
	if col.Type != websource.TypePages {
		var prune []*websource.Page
		if result.Incremental {
			for _, slug := range result.Removed {
				if rec := existing[slug]; rec != nil {
					prune = append(prune, rec)
				}
			}
		} else {
			for slug, rec := range existing {
				if !seen[slug] {
					prune = append(prune, rec)
				}
			}
		}

		for _, rec := range prune {
			if err := m.webStore.DeletePage(ctx, rec.ID); err != nil {
				return fail(err)
			}

			_ = os.Remove(filepath.Join(dir, rec.Slug+".md"))
			removedPaths = append(removedPaths, rec.Slug+".md")
		}
	}

	// Reconcile the index against the docs on disk: re-embed any page whose
	// markdown exists but has no vectors yet. This repairs collections whose
	// first embed run was interrupted (e.g. a restart mid-sync): the markdown
	// was written and hashed, so a later incremental sync sees no change and
	// would otherwise never embed those pages, leaving them unsearchable.
	if missing := m.missingIndexedPaths(ctx, name, seen, changedPaths); len(missing) > 0 {
		slog.Info("web source reindex: repairing pages missing from the index",
			"source", name, "missing", len(missing))
		changedPaths = append(changedPaths, missing...)
		// Reconciled pages were not fetched this run (unchanged markdown), so
		// their UpdatedAt is not in the map yet; take it from the stored record.
		for _, path := range missing {
			slug := strings.TrimSuffix(path, ".md")
			if rec := existing[slug]; rec != nil {
				updatedAt[path] = rec.UpdatedAt
			}
		}
	}

	// Re-embed the changed/removed docs. If indexing fails (e.g. the embeddings
	// provider is down or a quota is exhausted even after retries), do NOT
	// advance the fetch watermark: the markdown is already on disk, but the
	// vectors are missing, so the next sync must re-attempt the same pages
	// rather than skip them as "already seen". The collection is marked as
	// errored so the failure is visible and a refresh retries the work.
	indexOK := true
	if len(changedPaths) > 0 || len(removedPaths) > 0 {
		if err := m.indexWebSourcePaths(ctx, name, changedPaths, removedPaths, updatedAt); err != nil {
			indexOK = false
			slog.Error("web source indexing failed; watermark not advanced",
				"source", name, "changed", len(changedPaths), "error", err)
		}
	}

	col.LastRefreshAt = time.Now()
	if indexOK {
		col.Status = websource.StatusReady
		col.LastError = ""
		if result.State != nil {
			col.State = result.State
		}
	} else {
		col.Status = websource.StatusError
		col.LastError = "indexing incomplete: embeddings failed; will retry on next sync"
		// Leave col.State unchanged so the watermark does not advance and the
		// unindexed pages are re-fetched and re-embedded next time.
	}

	if err := m.webStore.UpsertCollection(context.WithoutCancel(ctx), col); err != nil {
		return err
	}

	slog.Info("web source synced", "source", name,
		"fetched", len(result.Pages), "changed", len(changedPaths),
		"removed", len(removedPaths), "incremental", result.Incremental, "indexed", indexOK)

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

// missingIndexedPaths returns the doc paths ("<slug>.md") that exist on disk
// this run (seen) but have no vectors in the docs index, excluding those
// already queued for embedding (alreadyQueued). It powers the sync-time
// reconcile that repairs interrupted embed runs. RAG being disabled, or any
// scan error, yields no extra paths (best-effort; never blocks a sync).
func (m *Manager) missingIndexedPaths(ctx context.Context, name string, seen map[string]bool, alreadyQueued []string) []string {
	if len(seen) == 0 {
		return nil
	}

	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.rag == nil {
		return nil
	}

	indexed, err := d.rag.IndexedPaths(ctx, websource.ScopeKey(name))
	if err != nil {
		slog.Error("web source reindex: scan indexed paths", "source", name, "error", err)

		return nil
	}

	queued := make(map[string]struct{}, len(alreadyQueued))
	for _, p := range alreadyQueued {
		queued[p] = struct{}{}
	}

	var missing []string
	for slug := range seen {
		path := slug + ".md"
		if _, ok := indexed[path]; ok {
			continue // already embedded
		}
		if _, ok := queued[path]; ok {
			continue // already about to be embedded this run
		}
		missing = append(missing, path)
	}

	return missing
}

// indexWebSourcePaths incrementally updates only the changed/removed docs of a
// collection in the RAG index, so a large source (e.g. a JIRA project) is not
// fully re-embedded when a few items change. It returns an error when embedding
// or upserting fails so the caller can avoid advancing the fetch watermark past
// pages whose vectors were not written. A disabled RAG subsystem is not an
// error: files stay on disk for the next reindex-all.
func (m *Manager) indexWebSourcePaths(ctx context.Context, name string, changed, removed []string, updatedAt map[string]time.Time) error {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()

	if d.rag == nil {
		slog.Debug("rag disabled; web source not indexed", "source", name)

		return nil
	}

	scope := websource.ScopeKey(name)

	m.setActivity(scope, "docs_index")
	defer m.clearActivity(scope, "docs_index")

	// Publish live embedding progress so the UI can show a determinate bar
	// ("1200/22697 chunks embedded"). Cleared when this step returns.
	m.setProgress(scope, Progress{Phase: "index"})
	defer m.clearProgress(scope)
	onProgress := func(done, total int) {
		m.setProgress(scope, Progress{Phase: "index", Done: done, Total: total})
	}

	// Carry each page's source last-modified time onto its vectors so retrieval
	// can surface and weigh recency.
	opts := &rag.IndexOptions{
		UpdatedAt: func(path string) time.Time { return updatedAt[path] },
	}

	if err := d.rag.IndexPathsProgress(ctx, scope, m.sourcesDir(name), changed, removed, onProgress, opts); err != nil {
		slog.Error("index web source (incremental)", "source", name, "error", err)

		return err
	}

	return nil
}

// enqueueWebReindex submits a reindex task for every web-source collection so
// their vectors are rebuilt from the on-disk markdown under the global
// concurrency limit. Used after live settings updates (see reindexAll).
func (m *Manager) enqueueWebReindex(ctx context.Context) {
	if m.webStore == nil {
		return
	}

	cols, err := m.webStore.ListCollections(ctx)
	if err != nil {
		slog.Error("list web sources for reindex", "error", err)

		return
	}

	for _, col := range cols {
		m.queue.Submit(m.reindexTask(websource.ScopeKey(col.Name)))
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
