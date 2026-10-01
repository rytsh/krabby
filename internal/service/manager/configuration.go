package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/settings"
)

// ErrManagerClosed is returned when live configuration is attempted during
// shutdown.
var ErrManagerClosed = errors.New("manager is shutting down")

// ErrNoSettingsStore is returned when config methods are called before a
// settings store has been attached.
var ErrNoSettingsStore = errors.New("settings store not configured")

// SetSettingsStore attaches the persisted settings store. Called once at
// startup, before Configure.
func (m *Manager) SetSettingsStore(s *settings.Store) {
	m.settings = s
}

// PollInterval returns the repo polling cadence from the runtime settings:
// the persisted value, one hour when unset, disabled (0) when negative.
func (m *Manager) PollInterval() time.Duration {
	const def = time.Hour

	if m.settings == nil {
		return def
	}

	s, err := m.settings.Get(context.Background())
	if err != nil {
		slog.Error("load poll interval", "error", err)

		return def
	}

	switch {
	case s.GitPollInterval < 0:
		return 0
	case s.GitPollInterval == 0:
		return def
	default:
		return s.GitPollInterval
	}
}

// RepoSchedules returns the effective repository poll schedules from the
// runtime settings: the configured per-namespace cron schedules, or a single
// fallback derived from GitPollInterval when none are configured. The scheduler
// reads this on every reconcile tick so UI/REST changes apply without a
// restart.
func (m *Manager) RepoSchedules() []settings.RepoSchedule {
	if m.settings == nil {
		return nil
	}

	s, err := m.settings.Get(context.Background())
	if err != nil {
		slog.Error("load repo schedules", "error", err)

		return nil
	}

	return s.EffectiveSchedules()
}

// RefreshNamespace queues a background refresh for every repo in ns (using the
// same namespace semantics as the registry: "" / "default" is the default
// bucket, "*" is every namespace). Called by the scheduler when a namespace's
// cron fires. Triggers coalesce per repo and the work queue bounds concurrency.
func (m *Manager) RefreshNamespace(ctx context.Context, ns string) error {
	repos, err := m.reg.ListNamespace(ctx, ns)
	if err != nil {
		return fmt.Errorf("list repos for namespace %q; %w", ns, err)
	}

	var errs []error
	for _, repo := range repos {
		if err := m.TriggerRefresh(repo.ID); err != nil {
			errs = append(errs, fmt.Errorf("enqueue refresh for %s; %w", repo.ID, err))
		}
	}

	return errors.Join(errs...)
}

// WebhookSecret returns the provider-neutral git webhook verification secret from the
// runtime settings ("" disables signature verification).
func (m *Manager) WebhookSecret() string {
	if m.settings == nil {
		return ""
	}

	s, err := m.settings.Get(context.Background())
	if err != nil {
		slog.Error("load webhook secret", "error", err)

		return ""
	}

	return s.WebhookSecret
}

// GetDocsConfig returns the current docs/RAG settings with secrets redacted.
func (m *Manager) GetDocsConfig(ctx context.Context) (settings.Redacted, error) {
	if m.settings == nil {
		return settings.Redacted{}, ErrNoSettingsStore
	}

	s, err := m.settings.Get(ctx)
	if err != nil {
		return settings.Redacted{}, err
	}

	return redactSettings(s), nil
}

// SetDocsConfig persists a settings patch (empty secrets keep existing values),
// then rebuilds the docs/RAG clients live. If the rebuild fails the previous
// working bundle stays active and the error is returned; the settings are still
// persisted so the user can correct them.
func (m *Manager) SetDocsConfig(ctx context.Context, patch settings.Settings) (settings.Redacted, error) {
	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()

	// A whole-record write cannot tell what changed, so it always reindexes.
	return m.setDocsConfig(ctx, patch, true)
}

// PatchDocsConfig atomically merges a presence-aware patch with persisted
// settings, then rebuilds clients. Concurrent patches cannot overwrite fields
// from a stale read.
func (m *Manager) PatchDocsConfig(ctx context.Context, patch settings.Patch) (settings.Redacted, error) {
	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()

	if m.settings == nil {
		return settings.Redacted{}, ErrNoSettingsStore
	}

	current, err := m.settings.Get(ctx)
	if err != nil {
		return settings.Redacted{}, err
	}

	next := patch.Apply(current)
	if patch.UIOnly() {
		// UI preferences are read only by the browser: nothing to rebuild.
		saved, err := m.settings.Set(ctx, next)
		if err != nil {
			return settings.Redacted{}, err
		}

		return redactSettings(saved), nil
	}

	if patch.RuntimeOnly() {
		saved, err := m.settings.Set(ctx, next)
		if err != nil {
			return settings.Redacted{}, err
		}

		// Runtime-only patches skip the client rebuild, so apply the queue
		// concurrency change here directly.
		m.SetTaskConcurrency(saved.TaskConcurrency)

		return redactSettings(saved), nil
	}

	// Observability changes rebuild the clients (the tracer is attached to
	// them) but touch nothing that went into a vector, so they must not drag
	// every repository through a reindex.
	return m.setDocsConfig(ctx, next, !patch.ObservabilityOnly())
}

// setDocsConfig persists next, rebuilds the clients, and optionally rebuilds
// the derived search indexes.
func (m *Manager) setDocsConfig(ctx context.Context, next settings.Settings, reindex bool) (settings.Redacted, error) {
	if m.settings == nil {
		return settings.Redacted{}, ErrNoSettingsStore
	}

	current, err := m.settings.Get(ctx)
	if err != nil {
		return settings.Redacted{}, err
	}
	imageChanged := webImageSettingsChanged(current, next)

	saved, err := m.settings.Set(ctx, next)
	if err != nil {
		return settings.Redacted{}, err
	}

	m.SetTaskConcurrency(saved.TaskConcurrency)

	if err := m.Configure(ctx, saved); err != nil {
		return redactSettings(saved), fmt.Errorf("settings saved but rebuild failed; %w", err)
	}

	// Existing repositories may be unchanged, so a normal refresh would return
	// before indexing. Rebuild derived docs/code indexes explicitly after live
	// settings changes (model, chunking, filters, or enablement).
	var enqueueErrs []error
	if reindex {
		if err := m.TriggerReindexAll(); err != nil {
			enqueueErrs = append(enqueueErrs, fmt.Errorf("settings saved but reindex enqueue failed; %w", err))
		}
	}
	if imageChanged {
		if err := m.triggerImageSourceRefreshes(ctx); err != nil {
			enqueueErrs = append(enqueueErrs, fmt.Errorf("settings saved but image refresh enqueue failed; %w", err))
		}
	}
	if err := errors.Join(enqueueErrs...); err != nil {
		return redactSettings(saved), err
	}

	return redactSettings(saved), nil
}

func webImageSettingsChanged(a, b settings.Settings) bool {
	aVision, bVision := visionLLMConfig(a), visionLLMConfig(b)
	return a.WebImageAnalysisEnabled != b.WebImageAnalysisEnabled ||
		strings.TrimRight(aVision.BaseURL, "/") != strings.TrimRight(bVision.BaseURL, "/") ||
		aVision.Model != bVision.Model ||
		strings.TrimSpace(a.WebImageModel) != strings.TrimSpace(b.WebImageModel) ||
		a.EffectiveWebImageMaxPerPage() != b.EffectiveWebImageMaxPerPage() ||
		a.EffectiveWebImageMaxBytes() != b.EffectiveWebImageMaxBytes() ||
		a.EffectiveWebImageMaxPixels() != b.EffectiveWebImageMaxPixels() ||
		a.WebImageAllowAuthenticated != b.WebImageAllowAuthenticated
}

func (m *Manager) triggerImageSourceRefreshes(ctx context.Context) error {
	if m.webStore == nil {
		return nil
	}
	collections, err := m.webStore.ListCollections(ctx)
	if err != nil {
		return fmt.Errorf("list web sources for image refresh; %w", err)
	}
	var errs []error
	for _, col := range collections {
		if col.AnalyzeImages {
			if err := m.TriggerWebFullRefresh(col.Name); err != nil {
				errs = append(errs, fmt.Errorf("enqueue image refresh for %s; %w", col.Name, err))
			}
		}
	}

	return errors.Join(errs...)
}

func redactSettings(s settings.Settings) settings.Redacted {
	// Installs migrated to the task_concurrency setting have 0 stored; present
	// the effective default so the UI shows the value actually applied.
	if s.TaskConcurrency <= 0 {
		s.TaskConcurrency = queue.DefaultConcurrency
	}

	r := s.Redact()
	r.DocsDefaultPrompt = docgen.DefaultPrompt

	return r
}
