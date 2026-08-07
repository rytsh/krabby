package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/progress"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/rag"
)

// ErrNoAPICatalog is returned when catalog methods are called before the store
// has been attached.
var ErrNoAPICatalog = errors.New("api catalog is not configured")

// SetAPICatalog attaches the catalog store and the provider per service kind.
// Called once at startup.
func (m *Manager) SetAPICatalog(store *apicatalog.Store, providers map[string]apicatalog.Provider) {
	m.apiStore = store
	m.apiProviders = providers
}

// APIServiceKinds returns the registered service kinds.
func (m *Manager) APIServiceKinds() []string {
	kinds := make([]string, 0, len(m.apiProviders))
	for kind := range m.apiProviders {
		kinds = append(kinds, kind)
	}

	return kinds
}

// apisDir returns the markdown projection directory of one service.
func (m *Manager) apisDir(name string) string {
	return filepath.Join(m.apisRootDir, name)
}

// serviceLock serialises read-modify-write cycles on one service record.
//
// It is a different lock from the sync lock for the same reason web sources
// keep them apart: a sync holds the sync lock for a whole sweep, and a config
// edit that waited on it would block the HTTP request for the duration. Lock
// order where both are taken: sync lock first, then this one.
func (m *Manager) serviceLock(name string) *sync.Mutex {
	return m.lockFor(apicatalog.ScopeKey(name) + "#record")
}

// mutateService applies fn to the current stored record under the record lock
// and writes the result back, so a sync that started before an edit cannot
// write its stale snapshot back over it.
func (m *Manager) mutateService(ctx context.Context, name string, fn func(*apicatalog.Service) error) error {
	l := m.serviceLock(name)
	l.Lock()
	defer l.Unlock()

	svc, err := m.apiStore.GetService(ctx, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("api service %s not found", name)
	}

	if err := fn(svc); err != nil {
		return err
	}

	return m.apiStore.UpsertService(ctx, svc)
}

// ---- groups ----------------------------------------------------------------

// APIGroups returns every group with its service count and description.
func (m *Manager) APIGroups(ctx context.Context) ([]apicatalog.GroupSummary, error) {
	if m.apiStore == nil {
		return nil, ErrNoAPICatalog
	}

	return m.apiStore.Groups(ctx)
}

// UpsertAPIGroup creates or updates a group's description.
func (m *Manager) UpsertAPIGroup(ctx context.Context, name, description string) (*apicatalog.Group, error) {
	if m.apiStore == nil {
		return nil, ErrNoAPICatalog
	}

	return m.apiStore.UpsertGroup(ctx, name, description)
}

// DeleteAPIGroup removes a group's description record. Services keep their
// group tag.
func (m *Manager) DeleteAPIGroup(ctx context.Context, name string) error {
	if m.apiStore == nil {
		return ErrNoAPICatalog
	}

	return m.apiStore.DeleteGroup(ctx, name)
}

// ---- services --------------------------------------------------------------

// AddAPIService validates and stores a new service, then triggers its first
// sync in the background.
func (m *Manager) AddAPIService(ctx context.Context, svc *apicatalog.Service) error {
	if m.apiStore == nil {
		return ErrNoAPICatalog
	}

	if !apicatalog.ValidName(svc.Name) {
		return fmt.Errorf("invalid service name %q (want lowercase [a-z0-9._-])", svc.Name)
	}

	svc.Group = apicatalog.NormalizeGroup(svc.Group)
	if svc.Group != "" && !apicatalog.ValidName(svc.Group) {
		return fmt.Errorf("invalid group name %q (want lowercase [a-z0-9._-])", svc.Group)
	}

	if err := validateWebSpecs(svc.Specs); err != nil {
		return err
	}

	provider, ok := m.apiProviders[svc.Kind]
	if !ok {
		return fmt.Errorf("unknown api service kind %q", svc.Kind)
	}

	config, err := provider.MergeConfig(nil, svc.Config)
	if err != nil {
		return err
	}
	svc.Config = config

	// The exists-check and the insert are one critical section, or two
	// concurrent creates of the same name both "succeed" and the second
	// silently overwrites the first.
	l := m.serviceLock(svc.Name)
	l.Lock()

	if existing, err := m.apiStore.GetService(ctx, svc.Name); err != nil {
		l.Unlock()

		return err
	} else if existing != nil {
		l.Unlock()

		return fmt.Errorf("api service %s already exists", svc.Name)
	}

	svc.Status = apicatalog.StatusPending
	svc.CreatedAt = time.Now()

	err = m.apiStore.UpsertService(ctx, svc)
	l.Unlock()

	if err != nil {
		return err
	}

	m.TriggerAPIRefresh(svc.Name)

	return nil
}

// UpdateAPIService applies a partial update: the envelope fields the client
// actually sent (see apicatalog.ServiceUpdate) plus the provider config, which
// the provider merges under the same rules. Anything the client did not
// mention — including the sync watermark, status and creation time — survives.
//
// An override change forces a full re-sync rather than waiting for the
// watermark to move. The document has not changed, but what krabby renders
// from it has, and the conditional-fetch machinery exists precisely to avoid
// re-reading an unchanged document — so without the force, an edited base URL
// or spec patch would sit invisible until the upstream spec happened to change.
func (m *Manager) UpdateAPIService(ctx context.Context, name string, update apicatalog.ServiceUpdate, config json.RawMessage) error {
	if m.apiStore == nil {
		return ErrNoAPICatalog
	}

	name = strings.TrimSpace(strings.ToLower(name))

	rerender := false
	err := m.mutateService(ctx, name, func(svc *apicatalog.Service) error {
		stored := svc.Config

		changed, err := update.Apply(svc)
		if err != nil {
			return err
		}
		rerender = changed

		if err := validateWebSpecs(svc.Specs); err != nil {
			return err
		}

		provider, ok := m.apiProviders[svc.Kind] // the kind is immutable once created
		if !ok {
			return fmt.Errorf("no provider for api service kind %q", svc.Kind)
		}

		merged, err := provider.MergeConfig(stored, config)
		if err != nil {
			return err
		}
		if string(merged) != string(stored) {
			rerender = true
		}
		svc.Config = merged

		return nil
	})

	if err == nil && rerender {
		m.TriggerAPIFullRefresh(name)
	}

	return err
}

// DeleteAPIService removes a service, its operations, its markdown projections
// and its search index entries.
func (m *Manager) DeleteAPIService(ctx context.Context, name string) error {
	if m.apiStore == nil {
		return ErrNoAPICatalog
	}

	scope := apicatalog.ScopeKey(name)
	defer m.lockKey(scope)()

	l := m.serviceLock(name)
	l.Lock()
	err := m.apiStore.DeleteService(ctx, name)
	l.Unlock()

	if err != nil {
		return err
	}

	if err := os.RemoveAll(m.apisDir(name)); err != nil {
		slog.Warn("remove api service directory", "service", name, "error", err)
	}

	m.dropAPIIndex(ctx, scope)

	return nil
}

// dropAPIIndex removes a deleted service's entries from every configured search
// index. A failure is logged rather than returned: the record and the markdown
// are already gone, and refusing to complete the delete would leave a service
// that cannot be removed at all.
func (m *Manager) dropAPIIndex(ctx context.Context, scope string) {
	d, release := m.acquireDocs()
	defer release()

	if d.rag != nil {
		if err := d.rag.DeleteRepo(ctx, scope); err != nil {
			slog.Warn("drop api service vectors", "scope", scope, "error", err)
		}
	}
	if m.docsText != nil {
		if err := m.docsText.DeleteRepo(ctx, scope); err != nil {
			slog.Warn("drop api service text index", "scope", scope, "error", err)
		}
	}
}

// APIService returns one service record, or nil when it does not exist.
func (m *Manager) APIService(ctx context.Context, name string) (*apicatalog.Service, error) {
	if m.apiStore == nil {
		return nil, ErrNoAPICatalog
	}

	return m.apiStore.GetService(ctx, name)
}

// APIServicesPaged lists services filtered by group and name substring.
func (m *Manager) APIServicesPaged(ctx context.Context, group, search string, offset, limit int) ([]*apicatalog.Service, int, error) {
	if m.apiStore == nil {
		return nil, 0, ErrNoAPICatalog
	}

	return m.apiStore.ServicesPaged(ctx, group, search, offset, limit)
}

// APIOperationsPaged lists one service's operations filtered by search, tag and
// method.
func (m *Manager) APIOperationsPaged(ctx context.Context, service, search, tag, method string, offset, limit int) ([]*apicatalog.Operation, int, error) {
	if m.apiStore == nil {
		return nil, 0, ErrNoAPICatalog
	}

	return m.apiStore.OperationsPaged(ctx, service, search, tag, method, offset, limit)
}

// APIOperation resolves one operation of a service by id, slug or
// "METHOD /path".
func (m *Manager) APIOperation(ctx context.Context, service, handle string) (*apicatalog.Operation, error) {
	if m.apiStore == nil {
		return nil, ErrNoAPICatalog
	}

	return m.apiStore.FindOperation(ctx, service, handle)
}

// APITags returns the distinct tags of one service's operations.
func (m *Manager) APITags(ctx context.Context, service string) ([]string, error) {
	if m.apiStore == nil {
		return nil, ErrNoAPICatalog
	}

	return m.apiStore.Tags(ctx, service)
}

// APIServiceConfigView returns a provider-owned, redacted config for the REST
// API and UI.
func (m *Manager) APIServiceConfigView(svc *apicatalog.Service) any {
	if svc == nil {
		return nil
	}
	provider := m.apiProviders[svc.Kind]
	if provider == nil {
		return nil
	}

	return provider.ConfigView(svc.Config)
}

// TestAPIServiceConfig probes unsaved config without persisting or indexing
// anything, so a user can confirm a URL points at the document they meant.
//
// When name names an existing service, its stored secrets back-fill the probe:
// the redacted config the UI holds has no token in it, so a test typed against
// an existing service would otherwise always fail on authentication.
func (m *Manager) TestAPIServiceConfig(ctx context.Context, name, kind string, config, patch json.RawMessage) (apicatalog.PreviewResult, error) {
	var out apicatalog.PreviewResult

	if m.apiStore == nil {
		return out, ErrNoAPICatalog
	}

	provider, ok := m.apiProviders[kind]
	if !ok {
		return out, fmt.Errorf("unknown api service kind %q", kind)
	}

	previewer, ok := provider.(apicatalog.Previewer)
	if !ok {
		return out, fmt.Errorf("api service kind %q does not support testing", kind)
	}

	merged := config
	if name = strings.TrimSpace(strings.ToLower(name)); name != "" {
		svc, err := m.apiStore.GetService(ctx, name)
		if err != nil {
			return out, err
		}
		if svc != nil && svc.Kind == kind {
			merged, err = provider.MergeConfig(svc.Config, config)
			if err != nil {
				return out, err
			}
		}
	}

	if merged == nil || len(merged) == 0 {
		return out, errors.New("config is required")
	}

	return previewer.Preview(ctx, merged, patch)
}

// ---- scheduling ------------------------------------------------------------

// TriggerAPIRefresh queues a background sync of one service on the central work
// queue. Duplicate syncs of the same service coalesce.
func (m *Manager) TriggerAPIRefresh(name string) {
	m.queue.Submit(m.apiSyncTaskMode(name, false))
}

// TriggerAPIFullRefresh queues a sync that ignores the provider watermark for
// one run, used when an override changed what the same document renders to.
func (m *Manager) TriggerAPIFullRefresh(name string) {
	m.queue.Submit(m.apiSyncTaskMode(name, true))
}

func (m *Manager) apiSyncTaskMode(name string, forceFull bool) queue.Task {
	scope := apicatalog.ScopeKey(name)
	key := taskKindAPISync + ":" + name
	spec := queue.Spec{Kind: taskKindAPISync, ID: scope}
	if forceFull {
		key += ":full"
		spec.Params = map[string]string{"force_full": "true"}
	}

	return queue.Task{
		ID:    scope,
		Kind:  taskKindAPISync,
		Title: "Sync " + scope,
		Key:   key,
		Spec:  spec,
		Run: func(ctx context.Context) error {
			if err := m.refreshAPIService(ctx, name, forceFull); err != nil {
				slog.Error("refresh api service", "service", name, "error", err)

				return err
			}

			return nil
		},
	}
}

// APISchedule is one service's cron schedule, used by the scheduler to build a
// hardloop cron per service.
type APISchedule struct {
	Name  string
	Specs []string
}

// APISchedules returns the effective cron schedules of every service that has
// one. The scheduler rebuilds its cron set from this on every reconcile tick,
// so UI/REST changes apply live.
func (m *Manager) APISchedules(ctx context.Context) []APISchedule {
	if m.apiStore == nil {
		return nil
	}

	services, err := m.apiStore.ListServices(ctx)
	if err != nil {
		slog.Error("list api services for schedule", "error", err)

		return nil
	}

	out := make([]APISchedule, 0, len(services))
	for _, svc := range services {
		specs := svc.EffectiveSpecs()
		if len(specs) == 0 {
			continue
		}
		out = append(out, APISchedule{Name: svc.Name, Specs: specs})
	}

	return out
}

// RefreshDueAPIServices triggers a sync for every service whose refresh
// interval has elapsed. Called by the scheduler for interval-only services.
func (m *Manager) RefreshDueAPIServices(ctx context.Context) {
	if m.apiStore == nil {
		return
	}

	services, err := m.apiStore.ListServices(ctx)
	if err != nil {
		slog.Error("list api services for schedule", "error", err)

		return
	}

	now := time.Now()
	for _, svc := range services {
		// Cron-scheduled services are driven by the scheduler's hardloop cron
		// set; skip them here so they are not also polled on the interval tick.
		if len(svc.Specs) > 0 || svc.RefreshInterval <= 0 {
			continue
		}
		if svc.Status == apicatalog.StatusFetching {
			continue
		}

		if svc.LastRefreshAt.IsZero() || now.Sub(svc.LastRefreshAt) >= svc.RefreshInterval {
			m.TriggerAPIRefresh(svc.Name)
		}
	}
}

// RefreshAPIService synchronously syncs one service.
func (m *Manager) RefreshAPIService(ctx context.Context, name string) error {
	return m.refreshAPIService(ctx, name, false)
}

// refreshAPIService fetches a service's document, writes changed operation
// projections to disk and reindexes what changed.
//
// The shape mirrors the web-source sync deliberately: same locking, same
// streaming write, same "Complete licenses deletion" rule, same "do not advance
// the watermark when indexing failed" rule. Those are not incidental — each one
// is a correctness property that was arrived at once and should not be
// re-derived differently here.
func (m *Manager) refreshAPIService(ctx context.Context, name string, forceFull bool) error {
	if m.apiStore == nil {
		return ErrNoAPICatalog
	}

	scope := apicatalog.ScopeKey(name)

	defer m.lockKey(scope)()

	svc, err := m.apiStore.GetService(ctx, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("api service %s not found", name)
	}

	provider, ok := m.apiProviders[svc.Kind]
	if !ok {
		return fmt.Errorf("no provider for api service kind %q", svc.Kind)
	}

	m.setActivity(scope, "sync")
	defer m.clearActivity(scope, "sync")

	m.setProgress(scope, Progress{Phase: "fetch"})
	defer m.clearAllProgress(scope)

	setSyncState := func(ctx context.Context, apply func(*apicatalog.Service)) {
		if err := m.mutateService(ctx, name, func(cur *apicatalog.Service) error {
			apply(cur)

			return nil
		}); err != nil {
			slog.Error("persist api service sync state", "service", name, "error", err)
		}
	}

	setSyncState(ctx, func(cur *apicatalog.Service) {
		cur.Status = apicatalog.StatusFetching
		cur.LastError = ""
	})

	fail := func(ferr error) error {
		if errors.Is(ferr, context.Canceled) {
			ferr = ErrCancelled
		}
		setSyncState(context.WithoutCancel(ctx), func(cur *apicatalog.Service) {
			cur.Status = apicatalog.StatusError
			cur.LastError = ferr.Error()
			// Stamp the attempt, not just the outcome, so a service with a bad
			// URL waits out its interval instead of being retried every minute.
			cur.LastRefreshAt = time.Now()
		})

		return ferr
	}

	stored, err := m.apiStore.Operations(ctx, name)
	if err != nil {
		return fail(err)
	}

	dir := m.apisDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(fmt.Errorf("mkdir %s; %w", dir, err))
	}

	existing := make(map[string]*apicatalog.Operation, len(stored))
	for _, op := range stored {
		existing[op.OpSlug] = op
	}

	var changedPaths, removedPaths []string
	seen := map[string]bool{}
	updatedAt := map[string]time.Time{}
	now := time.Now()

	var (
		written  int
		writeErr error
	)

	fetchCtx := progress.With(ctx, func(done, total int) {
		m.setProgress(scope, Progress{Phase: "fetch", Done: done, Total: total})
	})

	write := func(remote apicatalog.RemoteOperation) error {
		// A slug is derived from the document's method and path, both of which
		// are attacker-controlled if the document is. It becomes a store key
		// and a filename, so it is checked before it is trusted; one malformed
		// operation is dropped rather than failing the whole service, and is
		// deliberately not marked seen so a later prune cannot act on its name.
		if !apicatalog.ValidSlug(remote.OpSlug) {
			slog.Warn("api operation skipped: unsafe slug",
				"service", name, "slug", remote.OpSlug, "operation", remote.OperationID)

			return nil
		}

		written++
		seen[remote.OpSlug] = true

		rec := existing[remote.OpSlug]
		if rec == nil {
			rec = &apicatalog.Operation{
				ID:      apicatalog.OperationID(name, remote.OpSlug),
				Service: name,
				OpSlug:  remote.OpSlug,
			}
		}

		if remote.Err != nil {
			// Keep the previous definition; record the failure on the service.
			slog.Warn("api operation render failed",
				"service", name, "operation", remote.OperationID, "error", remote.Err)

			return nil
		}

		rec.OperationID = remote.OperationID
		rec.Method = remote.Method
		rec.Path = remote.Path
		rec.Summary = remote.Summary
		rec.Description = remote.Description
		rec.Tags = remote.Tags
		rec.Deprecated = remote.Deprecated
		rec.Detail = remote.Detail
		rec.UpdatedAt = now
		updatedAt[remote.OpSlug+".md"] = now

		hash := apicatalog.Hash(remote.Markdown)

		file, err := apicatalog.OperationFile(dir, remote.OpSlug)
		if err != nil {
			writeErr = err

			return writeErr
		}

		if hash != rec.Hash || !fileExists(file) {
			if err := os.WriteFile(file, []byte(remote.Markdown), 0o644); err != nil {
				writeErr = fmt.Errorf("write %s; %w", file, err)

				return writeErr
			}

			rec.Hash = hash
			changedPaths = append(changedPaths, remote.OpSlug+".md")
		}

		if err := m.apiStore.UpsertOperation(ctx, rec); err != nil {
			writeErr = err

			return writeErr
		}

		existing[remote.OpSlug] = rec

		return nil
	}

	state := svc.State
	if forceFull {
		state = nil
	}

	result, err := provider.Fetch(fetchCtx, svc, state, write)
	if writeErr != nil {
		return fail(writeErr)
	}
	if err != nil {
		return fail(fmt.Errorf("fetch %s; %w", name, err))
	}

	m.clearProgress(scope, "fetch")

	// An unchanged document emitted nothing. Treating that as a complete sweep
	// would prune every operation and report the API as having no endpoints, so
	// the whole reconcile is skipped and only the timestamp and watermark move.
	if result.Unchanged {
		if err := m.mutateService(context.WithoutCancel(ctx), name, func(cur *apicatalog.Service) error {
			cur.Status = apicatalog.StatusReady
			cur.LastError = ""
			cur.LastRefreshAt = time.Now()
			if result.State != nil {
				cur.State = result.State
			}

			return nil
		}); err != nil {
			return fmt.Errorf("persist sync result for %s; %w", name, err)
		}

		slog.Debug("api service unchanged", "service", name)

		return nil
	}

	// Deleting a stored operation because it was not seen is only sound when
	// the provider enumerated the whole document.
	if result.Complete {
		for slug, rec := range existing {
			if seen[slug] {
				continue
			}

			file, ferr := apicatalog.OperationFile(dir, rec.OpSlug)
			if err := m.apiStore.DeleteOperation(ctx, rec.ID); err != nil {
				return fail(err)
			}
			delete(existing, slug)

			if ferr != nil {
				slog.Warn("api operation record has an unsafe slug; removing the record only",
					"service", name, "slug", rec.OpSlug)

				continue
			}

			_ = os.Remove(file)
			removedPaths = append(removedPaths, rec.OpSlug+".md")
		}
	}

	// Repair operations whose markdown exists but whose vectors do not — an
	// interrupted embed run, or an index migration that dropped rows.
	reconcile := make(map[string]bool, len(existing))
	for slug := range existing {
		reconcile[slug] = true
	}
	if missing := m.missingIndexedAPIPaths(ctx, name, dir, reconcile, changedPaths); len(missing) > 0 {
		slog.Info("api service reindex: repairing operations missing from the index",
			"service", name, "missing", len(missing))
		changedPaths = append(changedPaths, missing...)
		for _, path := range missing {
			slug := strings.TrimSuffix(path, ".md")
			if rec := existing[slug]; rec != nil {
				updatedAt[path] = rec.UpdatedAt
			}
		}
	}

	indexOK := true
	if len(changedPaths) > 0 || len(removedPaths) > 0 {
		if err := m.indexAPIServicePaths(ctx, name, changedPaths, removedPaths, updatedAt); err != nil {
			indexOK = false
			slog.Error("api service indexing failed; watermark not advanced",
				"service", name, "changed", len(changedPaths), "error", err)
		}
	}

	if err := m.mutateService(context.WithoutCancel(ctx), name, func(cur *apicatalog.Service) error {
		cur.LastRefreshAt = time.Now()
		cur.OperationCount = len(existing)

		// Document metadata is refreshed from the spec, but never over the
		// human-written fields: Description and BaseURL are overrides, and a
		// sync that overwrote them would silently undo the operator's edit.
		if result.Info.Title != "" {
			cur.Title = result.Info.Title
		}
		if result.Info.Version != "" {
			cur.Version = result.Info.Version
		}
		cur.SpecSummary = result.Info.Summary
		cur.ResolvedBaseURL = result.Info.ResolvedURL

		if indexOK {
			cur.Status = apicatalog.StatusReady
			cur.LastError = ""
			if result.State != nil {
				cur.State = result.State
			}

			return nil
		}

		cur.Status = apicatalog.StatusError
		cur.LastError = "indexing incomplete; will retry on next sync"
		// Leave State unchanged so the watermark does not advance.

		return nil
	}); err != nil {
		return fmt.Errorf("persist sync result for %s; %w", name, err)
	}

	slog.Info("api service synced", "service", name,
		"operations", written, "changed", len(changedPaths),
		"removed", len(removedPaths), "complete", result.Complete, "indexed", indexOK)

	return nil
}

// missingIndexedAPIPaths returns markdown paths absent from any configured docs
// index, excluding paths already queued this run.
func (m *Manager) missingIndexedAPIPaths(ctx context.Context, name, dir string, candidates map[string]bool, alreadyQueued []string) []string {
	if len(candidates) == 0 {
		return nil
	}

	d, release := m.acquireDocs()
	defer release()
	if d.rag == nil && m.docsText == nil {
		return nil
	}

	scope := apicatalog.ScopeKey(name)

	indexedSets := make([]map[string]struct{}, 0, 2)
	if d.rag != nil {
		indexed, err := d.rag.IndexedPaths(ctx, scope)
		if err != nil {
			slog.Error("api service reindex: scan vector paths", "service", name, "error", err)

			return nil
		}
		indexedSets = append(indexedSets, indexed)
	}
	if m.docsText != nil {
		indexed, err := m.docsText.IndexedPaths(ctx, scope)
		if err != nil {
			slog.Error("api service reindex: scan text paths", "service", name, "error", err)

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
			continue
		}
		if !fileExists(filepath.Join(dir, path)) {
			continue
		}

		missing = append(missing, path)
	}

	return missing
}

// indexAPIServicePaths incrementally updates changed/removed operation
// projections in every configured search index. An index failure prevents the
// fetch watermark from advancing.
func (m *Manager) indexAPIServicePaths(ctx context.Context, name string, changed, removed []string, updatedAt map[string]time.Time) error {
	d, release := m.acquireDocs()
	defer release()

	if d.rag == nil && m.docsText == nil {
		slog.Debug("docs search disabled; api service not indexed", "service", name)

		return nil
	}

	scope := apicatalog.ScopeKey(name)

	m.setActivity(scope, "docs_index")
	defer m.clearActivity(scope, "docs_index")

	m.setProgress(scope, Progress{Phase: "index"})
	defer m.clearProgress(scope, "index")
	onProgress := func(done, total int) {
		m.setProgress(scope, Progress{Phase: "index", Done: done, Total: total})
	}

	opts := &rag.IndexOptions{
		UpdatedAt:           func(path string) time.Time { return updatedAt[path] },
		KeepMarkdownTargets: d.ragCfg.KeepMarkdownTargets,
	}

	dir := m.apisDir(name)

	var errs []error
	if m.docsText != nil {
		if err := m.docsText.IndexPaths(ctx, scope, dir, changed, removed, opts); err != nil {
			errs = append(errs, fmt.Errorf("index api service %s text; %w", name, err))
		}
		if err := m.docsText.RefreshStats(ctx); err != nil {
			slog.Warn("refresh docs search stats", "service", name, "error", err)
		}
	}
	if d.rag != nil {
		if err := d.rag.IndexPathsProgress(ctx, scope, dir, changed, removed, onProgress, opts); err != nil {
			errs = append(errs, fmt.Errorf("index api service %s vectors; %w", name, err))
		}
	}

	return errors.Join(errs...)
}

// indexAPIService rebuilds one service's configured lexical and semantic docs
// indexes from its on-disk markdown. Semantic indexing is skipped when RAG is
// disabled.
func (m *Manager) indexAPIService(ctx context.Context, name string) {
	d, release := m.acquireDocs()
	defer release()

	if d.rag == nil && m.docsText == nil {
		slog.Debug("docs search disabled; api service not indexed", "service", name)

		return
	}

	scope := apicatalog.ScopeKey(name)

	m.setActivity(scope, "docs_index")
	defer m.clearActivity(scope, "docs_index")

	if err := m.indexDocs(ctx, d, scope, m.apisDir(name)); err != nil {
		slog.Error("index api service", "service", name, "error", err)
	}
}

// enqueueAPIReindex submits a reindex task for every service so its indexes are
// rebuilt from the on-disk markdown under the global concurrency limit. Used
// after live settings updates.
func (m *Manager) enqueueAPIReindex(ctx context.Context) {
	if m.apiStore == nil {
		return
	}

	services, err := m.apiStore.ListServices(ctx)
	if err != nil {
		slog.Error("list api services for reindex", "error", err)

		return
	}

	for _, svc := range services {
		m.queue.Submit(m.reindexTask(apicatalog.ScopeKey(svc.Name)))
	}
}
