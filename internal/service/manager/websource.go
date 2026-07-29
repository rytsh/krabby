package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/worldline-go/hardloop"

	"github.com/rytsh/krabby/internal/service/progress"
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

// collectionLock serialises read-modify-write cycles on one collection record.
//
// It is deliberately a different lock from the sync lock. RefreshWebSource
// holds the sync lock for a whole sweep — minutes on a large project — so a
// config edit that waited for it would hang the HTTP request for the duration.
// This lock is only ever held across a store read and its matching write.
//
// Lock order, where both are taken: sync lock first, then this one. Nothing
// takes them in the opposite order.
func (m *Manager) collectionLock(name string) *sync.Mutex {
	return m.lock(websource.ScopeKey(name) + "#record")
}

// mutateCollection applies fn to the current stored record under the record
// lock and writes the result back.
//
// Every writer goes through it because the alternative — read a record, work
// for a while, write the whole thing back — loses whatever else was written in
// between. That is not theoretical here: a sync reads the collection at the top
// of a sweep and writes it back at the end, so a config or schedule edit made
// during the sweep would be silently reverted, and an update that read the
// record just before a delete would re-insert it afterwards, leaving a
// collection with no pages, no directory and no index that nothing can clean up.
func (m *Manager) mutateCollection(ctx context.Context, name string, fn func(*websource.Collection) error) error {
	l := m.collectionLock(name)
	l.Lock()
	defer l.Unlock()

	col, err := m.webStore.GetCollection(ctx, name)
	if err != nil {
		return err
	}

	if col == nil {
		return fmt.Errorf("collection %s not found", name)
	}

	if err := fn(col); err != nil {
		return err
	}

	return m.webStore.UpsertCollection(ctx, col)
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

	// The exists-check and the insert are one critical section, or two
	// concurrent creates of the same name both "succeed" and the second
	// silently overwrites the first.
	l := m.collectionLock(col.Name)
	l.Lock()

	if existing, err := m.webStore.GetCollection(ctx, col.Name); err != nil {
		l.Unlock()

		return err
	} else if existing != nil {
		l.Unlock()

		return fmt.Errorf("collection %s already exists", col.Name)
	}

	col.Status = websource.StatusPending
	col.CreatedAt = time.Now()

	err = m.webStore.UpsertCollection(ctx, col)
	l.Unlock()

	if err != nil {
		return err
	}

	m.TriggerWebRefresh(col.Name)

	return nil
}

// UpdateWebCollection applies a partial update to a collection: the envelope
// fields the client actually sent (see websource.CollectionUpdate) plus the
// provider config, which the fetcher merges under the same rules. Anything the
// client did not mention — including the sync watermark, status and creation
// time — is preserved.
//
// config may be nil, meaning "no config change"; a blank write-only secret
// inside it keeps the stored one, which is the fetcher's business.
func (m *Manager) UpdateWebCollection(ctx context.Context, name string, update websource.CollectionUpdate, config json.RawMessage) error {
	if m.webStore == nil {
		return ErrNoWebSources
	}

	name = strings.TrimSpace(strings.ToLower(name))

	// Start from the stored record so every unmentioned field survives by
	// construction rather than by remembering to copy it back — and read it
	// under the record lock so "stored" means now, not before whatever else
	// was writing to this collection.
	return m.mutateCollection(ctx, name, func(col *websource.Collection) error {
		stored := col.Config

		if err := update.Apply(col); err != nil {
			return err
		}

		if err := validateWebSpecs(col.Specs); err != nil {
			return err
		}

		fetcher, ok := m.webFetchers[col.Type] // the type is immutable once created
		if !ok {
			return fmt.Errorf("no fetcher for source type %q", col.Type)
		}

		merged, err := fetcher.MergeConfig(stored, config)
		if err != nil {
			return err
		}
		col.Config = merged

		return nil
	})
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

// WebSourceTestResult reports a read-only validation of unsaved JIRA or
// Confluence config. No collection, page, activity or index state is changed.
type WebSourceTestResult struct {
	OK        bool   `json:"ok"`
	Type      string `json:"type,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
	websource.PreviewResult
}

// TestWebSource merges an unsaved provider config with the stored config when
// editing (so a blank write-only token keeps working), then previews the full
// configured scope without persisting or queueing work.
func (m *Manager) TestWebSource(ctx context.Context, sourceType, existingName string, patch json.RawMessage) WebSourceTestResult {
	result := WebSourceTestResult{Type: sourceType}
	fetcher := m.webFetchers[sourceType]
	if fetcher == nil {
		result.Error = fmt.Sprintf("unknown source type %q", sourceType)

		return result
	}

	var current json.RawMessage
	if existingName != "" {
		if m.webStore == nil {
			result.Error = ErrNoWebSources.Error()

			return result
		}
		col, err := m.webStore.GetCollection(ctx, strings.TrimSpace(strings.ToLower(existingName)))
		if err != nil {
			result.Error = err.Error()

			return result
		}
		if col == nil {
			result.Error = fmt.Sprintf("collection %s not found", existingName)

			return result
		}
		if col.Type != sourceType {
			result.Error = fmt.Sprintf("collection %s is type %q, not %q", existingName, col.Type, sourceType)

			return result
		}
		current = col.Config
	}

	config, err := fetcher.MergeConfig(current, patch)
	if err != nil {
		result.Error = err.Error()

		return result
	}
	previewer, ok := fetcher.(websource.Previewer)
	if !ok {
		result.Error = fmt.Sprintf("source type %q does not support config testing", sourceType)

		return result
	}

	start := time.Now()
	preview, err := previewer.Preview(ctx, config)
	result.LatencyMS = time.Since(start).Milliseconds()
	result.PreviewResult = preview
	if err != nil {
		result.Error = err.Error()

		return result
	}

	result.OK = true

	return result
}

// DeleteWebCollection removes the collection, its pages, files and indexes.
func (m *Manager) DeleteWebCollection(ctx context.Context, name string) error {
	if m.webStore == nil {
		return ErrNoWebSources
	}

	scope := websource.ScopeKey(name)

	l := m.lock(scope)
	l.Lock()
	defer l.Unlock()

	// Held for the whole teardown, not just the store read: an update that
	// slipped in between the record delete and here would re-insert the
	// collection, leaving a record with no pages, no directory and no index
	// entries — and no path that ever cleans it up.
	rl := m.collectionLock(name)
	rl.Lock()
	defer rl.Unlock()

	col, err := m.webStore.GetCollection(ctx, name)
	if err != nil {
		return err
	}

	if col == nil {
		return fmt.Errorf("collection %s not found", name)
	}

	// Best-effort: drop the collection from both docs indexes.
	d, releaseDocs := m.acquireDocs()
	if d.rag != nil {
		if err := d.rag.DeleteRepo(ctx, scope); err != nil {
			slog.Error("delete web source from docs index", "source", name, "error", err)
		}
	}
	if m.docsText != nil {
		if err := m.docsText.DeleteRepo(ctx, scope); err != nil {
			slog.Error("delete web source from docs text index", "source", name, "error", err)
		}
	}
	releaseDocs()

	if err := m.webStore.DeleteCollection(ctx, name); err != nil {
		return err
	}

	// The name is already ValidName-checked on create, but this guards an
	// os.RemoveAll: use a separator-aware containment test rather than
	// filepath.HasPrefix, which is deprecated precisely because "…/srcs-evil"
	// passes a prefix test against "…/srcs".
	dir := m.sourcesDir(name)
	if m.sourcesRootDir != "" && withinDir(m.sourcesRootDir, dir) {
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

// WebPage returns one persisted page record, or nil when it does not exist.
func (m *Manager) WebPage(ctx context.Context, name, slug string) (*websource.Page, error) {
	if m.webStore == nil {
		return nil, ErrNoWebSources
	}

	return m.webStore.GetPage(ctx, websource.PageID(name, slug))
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

// SitemapImportResult summarizes a sitemap import for the REST/UI caller.
type SitemapImportResult struct {
	Discovered int `json:"discovered"`
	Added      int `json:"added"`
	Existing   int `json:"existing"`
}

// ImportWebSitemap discovers page URLs from a sitemap and registers them on a
// user-managed URL collection. All records are added before one background sync
// is queued, avoiding one full collection sync per imported URL.
func (m *Manager) ImportWebSitemap(ctx context.Context, name, sitemapURL string) (SitemapImportResult, error) {
	var result SitemapImportResult
	if m.webStore == nil {
		return result, ErrNoWebSources
	}

	col, err := m.webStore.GetCollection(ctx, name)
	if err != nil {
		return result, err
	}
	if col == nil {
		return result, fmt.Errorf("collection %s not found", name)
	}
	if col.Type != websource.TypePages {
		return result, fmt.Errorf("collection %s is type %q; sitemaps are only supported for pages", name, col.Type)
	}

	importer, ok := m.webFetchers[col.Type].(websource.SitemapFetcher)
	if !ok {
		return result, fmt.Errorf("source type %q does not support sitemaps", col.Type)
	}
	urls, err := importer.SitemapURLs(ctx, strings.TrimSpace(sitemapURL))
	if err != nil {
		return result, err
	}
	result.Discovered = len(urls)

	scope := websource.ScopeKey(name)
	l := m.lock(scope)
	l.Lock()
	defer func() {
		l.Unlock()
		if result.Added > 0 {
			m.TriggerWebRefresh(name)
		}
	}()

	// The collection may have been deleted while its sitemap was being fetched.
	col, err = m.webStore.GetCollection(ctx, name)
	if err != nil {
		return result, err
	}
	if col == nil {
		return result, fmt.Errorf("collection %s not found", name)
	}

	for _, pageURL := range urls {
		slug := slugForURL(pageURL)
		existing, err := m.webStore.GetPage(ctx, websource.PageID(name, slug))
		if err != nil {
			return result, err
		}
		if existing != nil {
			result.Existing++

			continue
		}

		page := &websource.Page{
			ID:         websource.PageID(name, slug),
			Collection: name,
			Slug:       slug,
			URL:        pageURL,
			Status:     websource.StatusPending,
		}
		if err := m.webStore.UpsertPage(ctx, page); err != nil {
			return result, err
		}
		result.Added++
	}

	return result, nil
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

	// Validate before touching anything: slug arrives from a query parameter,
	// and it is about to name a file to delete.
	file, err := websource.PageFile(m.sourcesDir(name), slug)
	if err != nil {
		return err
	}

	if err := m.webStore.DeletePage(ctx, websource.PageID(name, slug)); err != nil {
		return err
	}

	_ = os.Remove(file)

	// Only this page's vectors and text rows go; rebuilding the collection
	// would re-embed every remaining document to delete one.
	if err := m.indexWebSourcePaths(ctx, name, nil, []string{slug + ".md"}, nil); err != nil {
		return fmt.Errorf("drop index entries for %s; %w", slug, err)
	}

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
// that has one (explicit Specs, or an "@every <RefreshInterval>" fallback),
// plus its provider's full-reconciliation cron. Collections with no automatic
// refresh remain manual-only and are omitted. The scheduler rebuilds its cron
// set from this on every reconcile tick, so UI/REST changes apply live.
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
		if scheduler, ok := m.webFetchers[col.Type].(websource.FullResyncScheduler); ok {
			fullSpec, err := scheduler.FullResyncSpec(col.Config)
			if err != nil {
				slog.Error("read web source full resync schedule", "source", col.Name, "error", err)
			} else if fullSpec != "" && !slices.Contains(specs, fullSpec) {
				specs = append(specs, fullSpec)
			}
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
	// Fetching and persisting are one step — a page is written as it arrives —
	// so they share the fetch phase, reported by the provider as it walks; the
	// index phase below then reports embedded/total chunks.
	m.setProgress(scope, Progress{Phase: "fetch"})
	// The sync owns the scope's phases end to end, so clear them all: an early
	// return between phases must not leave a stale bar on screen.
	defer m.clearAllProgress(scope)

	// col is this sweep's snapshot: its Config and State drive the fetch, and a
	// config edit landing mid-sweep applies to the next one. Writes, though, go
	// through mutateCollection and touch only the fields the sync owns, so the
	// snapshot can never be written back wholesale over someone else's edit.
	setSyncState := func(ctx context.Context, apply func(*websource.Collection)) {
		if err := m.mutateCollection(ctx, name, func(cur *websource.Collection) error {
			apply(cur)

			return nil
		}); err != nil {
			slog.Error("persist web source sync state", "source", name, "error", err)
		}
	}

	setSyncState(ctx, func(cur *websource.Collection) {
		cur.Status = websource.StatusFetching
		cur.LastError = ""
	})

	fail := func(ferr error) error {
		if errors.Is(ferr, context.Canceled) {
			ferr = ErrCancelled
		}
		setSyncState(context.WithoutCancel(ctx), func(cur *websource.Collection) {
			cur.Status = websource.StatusError
			cur.LastError = ferr.Error()
			// Stamp the attempt, not just the outcome. The interval poll fires
			// every minute and re-triggers anything whose LastRefreshAt is
			// older than its interval, so leaving it untouched on failure turns
			// a source with bad credentials or an unreachable host into a
			// once-a-minute retry loop against the remote API — the fastest way
			// to get an Atlassian token rate-limited — while holding one of the
			// few queue slots. A failed attempt waits out the same interval as
			// a good one.
			cur.LastRefreshAt = time.Now()
		})

		return ferr
	}

	pages, err := m.webStore.Pages(ctx, name)
	if err != nil {
		return fail(err)
	}

	// Providers that page through a known result set (JIRA's issue count,
	// Confluence's page count, the registered URL list) report as they go, so
	// the fetch phase shows a determinate bar and a time estimate instead of an
	// open-ended spinner. Providers that cannot simply never report.
	fetchCtx := progress.With(ctx, func(done, total int) {
		m.setProgress(scope, Progress{Phase: "fetch", Done: done, Total: total})
	})

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

	// Pages are written as the provider streams them, so a 35k-ticket project
	// never sits in memory as one slice. Only the small per-page bookkeeping
	// below (slugs and paths) accumulates.
	//
	// writeErr carries a sink failure back out: the fetcher propagates it
	// verbatim, and the caller must not label it as a fetch failure.
	var (
		written  int
		writeErr error
	)

	write := func(remote websource.RemotePage) error {
		// A slug comes from the remote provider (an issue key, a page id) and
		// becomes both a store key and a filename, so it is checked before it
		// is trusted. One malformed item is dropped with a warning rather than
		// failing the sync: the rest of the collection is still worth syncing,
		// and it must not be recorded as seen either, or a later prune would
		// try to delete the file it names.
		if !websource.ValidSlug(remote.Slug) {
			slog.Warn("web source page skipped: unsafe slug",
				"source", name, "slug", remote.Slug)

			return nil
		}

		written++
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

			return nil
		}

		if remote.Title != "" {
			rec.Title = remote.Title
		}

		rec.Teams = remote.Teams
		rec.UpdatedAt = remote.UpdatedAt
		updatedAt[remote.Slug+".md"] = remote.UpdatedAt

		markdown := withTitleHeading(remote.Markdown, rec.Title)
		hash := websource.Hash(markdown)

		file, err := websource.PageFile(dir, remote.Slug)
		if err != nil {
			writeErr = err

			return writeErr
		}

		if hash != rec.Hash || !fileExists(file) {
			if err := os.WriteFile(file, []byte(markdown), 0o644); err != nil {
				writeErr = fmt.Errorf("write %s; %w", file, err)

				return writeErr
			}

			rec.Hash = hash
			changedPaths = append(changedPaths, remote.Slug+".md")
		}

		rec.Status = websource.StatusReady
		rec.LastError = ""

		if err := m.webStore.UpsertPage(ctx, rec); err != nil {
			writeErr = err

			return writeErr
		}

		// Keep the record available to the reconcile pass below: it is now the
		// current state of this page, and for a new page it is the only copy.
		existing[remote.Slug] = rec

		return nil
	}

	result, err := fetcher.Fetch(fetchCtx, col, pages, col.State, write)
	if writeErr != nil {
		return fail(writeErr)
	}
	if err != nil {
		return fail(fmt.Errorf("fetch %s; %w", name, err))
	}

	m.clearProgress(scope, "fetch")

	// Pruning of vanished records. Deleting a stored page because it was not
	// seen is only sound when the provider enumerated the whole collection, so
	// it hangs entirely off the Complete guarantee: an incremental fetch
	// (unseen means unchanged) and a truncated sweep (unseen means past the
	// cap) both leave the stored set alone and prune only what the provider
	// explicitly reported as removed.
	prune := map[string]*websource.Page{}

	for _, slug := range result.Removed {
		if rec := existing[slug]; rec != nil && !seen[slug] {
			prune[slug] = rec
		}
	}

	if result.Complete {
		for slug, rec := range existing {
			if !seen[slug] {
				prune[slug] = rec
			}
		}
	}

	for _, rec := range prune {
		// Records written before slugs were validated can still name a path
		// outside the collection; drop the record but never act on the name.
		file, ferr := websource.PageFile(dir, rec.Slug)
		if ferr != nil {
			slog.Warn("web source page record has an unsafe slug; removing the record only",
				"source", name, "slug", rec.Slug)
		}

		if err := m.webStore.DeletePage(ctx, rec.ID); err != nil {
			return fail(err)
		}

		if ferr != nil {
			continue
		}

		_ = os.Remove(file)
		removedPaths = append(removedPaths, rec.Slug+".md")
	}

	// Reconcile the index against every page currently on disk, not just the
	// pages fetched this run: re-embed any whose markdown exists but has no
	// vectors yet. Incremental sources (Confluence/JIRA) return only items
	// changed since the watermark, so on a routine sync the fetched set (seen)
	// is a small subset — usually empty. Keying the reconcile off seen alone
	// would then never repair pages whose markdown exists but whose vectors are
	// absent: an interrupted embed run, or a vector-store migration that dropped
	// rows, would leave them unsearchable until the next periodic full resync.
	// Build the candidate set from all persisted page records (which mirror the
	// on-disk markdown), minus those pruned this run, plus any new pages seen.
	reconcile := make(map[string]bool, len(existing)+len(seen))
	for slug := range existing {
		reconcile[slug] = true
	}
	for _, path := range removedPaths {
		delete(reconcile, strings.TrimSuffix(path, ".md"))
	}
	for slug := range seen {
		reconcile[slug] = true
	}

	if missing := m.missingIndexedPaths(ctx, name, dir, reconcile, changedPaths); len(missing) > 0 {
		slog.Info("web source reindex: repairing pages missing from the index",
			"source", name, "missing", len(missing))
		changedPaths = append(changedPaths, missing...)
		// Reconciled pages were not necessarily fetched this run (unchanged
		// markdown), so their UpdatedAt may not be in the map yet; take it from
		// the stored record so the recency stamp is preserved on re-embed.
		for _, path := range missing {
			slug := strings.TrimSuffix(path, ".md")
			if rec := existing[slug]; rec != nil {
				updatedAt[path] = rec.UpdatedAt
			}
		}
	}

	// Reindex the changed/removed docs. If indexing fails (e.g. the embeddings
	// provider is down or a local index write fails), do NOT
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

	if err := m.mutateCollection(context.WithoutCancel(ctx), name, func(cur *websource.Collection) error {
		cur.LastRefreshAt = time.Now()

		if indexOK {
			cur.Status = websource.StatusReady
			cur.LastError = ""
			if result.State != nil {
				cur.State = result.State
			}

			return nil
		}

		cur.Status = websource.StatusError
		cur.LastError = "indexing incomplete; will retry on next sync"
		// Leave State unchanged so the watermark does not advance and the
		// unindexed pages are re-fetched and indexed next time.

		return nil
	}); err != nil {
		// The sweep itself succeeded, so this is a storage failure, not a sync
		// failure — but the status is now whatever the opening write left
		// (fetching), which the interval poll skips. Say so plainly.
		return fmt.Errorf("persist sync result for %s; %w", name, err)
	}

	slog.Info("web source synced", "source", name,
		"fetched", written, "changed", len(changedPaths),
		"removed", len(removedPaths), "complete", result.Complete, "indexed", indexOK)

	return nil
}

// indexWebSource rebuilds a collection's configured lexical and semantic docs
// indexes. Semantic indexing is skipped when RAG is disabled.
func (m *Manager) indexWebSource(ctx context.Context, name string) {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()

	if d.rag == nil && m.docsText == nil {
		slog.Debug("docs search disabled; web source not indexed", "source", name)

		return
	}

	scope := websource.ScopeKey(name)

	m.setActivity(scope, "docs_index")
	defer m.clearActivity(scope, "docs_index")

	if err := m.indexDocs(ctx, d, scope, m.sourcesDir(name)); err != nil {
		slog.Error("index web source", "source", name, "error", err)
	}
}

// missingIndexedPaths returns markdown paths absent from any configured docs
// index, excluding paths already queued this run. It repairs interrupted runs
// and post-migration gaps without blocking sync on scan errors.
func (m *Manager) missingIndexedPaths(ctx context.Context, name, dir string, candidates map[string]bool, alreadyQueued []string) []string {
	if len(candidates) == 0 {
		return nil
	}

	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.rag == nil && m.docsText == nil {
		return nil
	}

	indexedSets := make([]map[string]struct{}, 0, 2)
	if d.rag != nil {
		indexed, err := d.rag.IndexedPaths(ctx, websource.ScopeKey(name))
		if err != nil {
			slog.Error("web source reindex: scan vector paths", "source", name, "error", err)
			return nil
		}
		indexedSets = append(indexedSets, indexed)
	}
	if m.docsText != nil {
		indexed, err := m.docsText.IndexedPaths(ctx, websource.ScopeKey(name))
		if err != nil {
			slog.Error("web source reindex: scan text paths", "source", name, "error", err)
			return nil
		}
		indexedSets = append(indexedSets, indexed)
	}

	queued := make(map[string]struct{}, len(alreadyQueued))
	for _, p := range alreadyQueued {
		queued[p] = struct{}{}
	}

	var missing []string
	for slug := range candidates {
		path := slug + ".md"
		indexedEverywhere := true
		for _, indexed := range indexedSets {
			if _, ok := indexed[path]; !ok {
				indexedEverywhere = false
				break
			}
		}
		if indexedEverywhere {
			continue
		}
		if _, ok := queued[path]; ok {
			continue // already about to be embedded this run
		}
		if !fileExists(filepath.Join(dir, path)) {
			continue // no markdown to embed (e.g. a pending, never-fetched page)
		}
		missing = append(missing, path)
	}

	return missing
}

// indexWebSourcePaths incrementally updates changed/removed docs in every
// configured search index, avoiding a full rebuild for large JIRA projects. An
// index failure prevents the fetch watermark from advancing.
func (m *Manager) indexWebSourcePaths(ctx context.Context, name string, changed, removed []string, updatedAt map[string]time.Time) error {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()

	if d.rag == nil && m.docsText == nil {
		slog.Debug("docs search disabled; web source not indexed", "source", name)

		return nil
	}

	scope := websource.ScopeKey(name)

	m.setActivity(scope, "docs_index")
	defer m.clearActivity(scope, "docs_index")

	// Publish live embedding progress so the UI can show a determinate bar
	// ("1200/22697 chunks embedded"). Cleared when this step returns.
	m.setProgress(scope, Progress{Phase: "index"})
	defer m.clearProgress(scope, "index")
	onProgress := func(done, total int) {
		m.setProgress(scope, Progress{Phase: "index", Done: done, Total: total})
	}

	// Carry each page's source last-modified time onto its vectors so retrieval
	// can surface and weigh recency.
	opts := &rag.IndexOptions{
		UpdatedAt: func(path string) time.Time { return updatedAt[path] },
	}

	var errs []error
	if m.docsText != nil {
		if err := m.docsText.IndexPaths(ctx, scope, m.sourcesDir(name), changed, removed, opts); err != nil {
			errs = append(errs, fmt.Errorf("index web source %s text; %w", name, err))
		}
		// Query tuning is derived from the corpus, so it follows the corpus.
		// A failure here only costs lexical query speed, never correctness.
		if err := m.docsText.RefreshStats(ctx); err != nil {
			slog.Warn("refresh docs search stats", "source", name, "error", err)
		}
	}
	if d.rag != nil {
		if err := d.rag.IndexPathsProgress(ctx, scope, m.sourcesDir(name), changed, removed, onProgress, opts); err != nil {
			errs = append(errs, fmt.Errorf("index web source %s vectors; %w", name, err))
		}
	}

	return errors.Join(errs...)
}

// enqueueWebReindex submits a reindex task for every web-source collection so
// its indexes are rebuilt from the on-disk markdown under the global
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
