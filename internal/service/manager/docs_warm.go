package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/websource"
)

// WarmDocsSearch builds the persistent lexical index for existing markdown.
// It is local-only (no LLM or embedder calls) and safe to run in the background.
//
// Once it completes, every key that has markdown has a lexical index and the
// indexing pipeline keeps it that way, so queries stop checking (see
// SearchDocs).
func (m *Manager) WarmDocsSearch(ctx context.Context) error {
	if m.docsText == nil {
		return nil
	}

	if err := m.ensureDocsTextForSearch(ctx, ScopeAll, "", registry.NamespaceAll); err != nil {
		return err
	}

	if err := m.docsText.RefreshStats(ctx); err != nil {
		return err
	}

	m.docsTextWarmed.Store(true)

	return nil
}

// ensureDocsTextForSearch makes upgrade-safe lexical searches: installations
// with markdown created before the docs_search bucket existed are indexed on
// demand for exactly the keys participating in this query.
func (m *Manager) ensureDocsTextForSearch(ctx context.Context, scope, key, namespace string) error {
	if m.docsText == nil {
		return nil
	}

	if key != "" {
		if name := websource.CollectionName(key); name != "" {
			if m.sourcesRootDir == "" {
				return nil
			}

			return m.ensureDocsTextKey(ctx, key, m.sourcesDir(name))
		}

		if name := apicatalog.ServiceName(key); name != "" {
			if m.apisRootDir == "" {
				return nil
			}

			return m.ensureDocsTextKey(ctx, key, m.apisDir(name))
		}

		repo, err := m.reg.Get(ctx, key)
		if err != nil || repo == nil {
			return err
		}
		docsDir, err := m.docsDirForRepo(repo)
		if err != nil {
			return err
		}

		return m.ensureDocsTextKey(ctx, repo.ID, docsDir)
	}

	var errs []error
	if scope == "" || scope == ScopeAll || scope == ScopeRepos {
		repos, err := m.reg.List(ctx)
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, repo := range repos {
				if !strings.EqualFold(strings.TrimSpace(namespace), registry.NamespaceAll) && !repoInNamespace(repo, namespace) {
					continue
				}
				docsDir, derr := m.docsDirForRepo(repo)
				if derr == nil {
					derr = m.ensureDocsTextKey(ctx, repo.ID, docsDir)
				}
				if derr != nil {
					errs = append(errs, fmt.Errorf("warm docs text for %s; %w", repo.ID, derr))
				}
			}
		}
	}

	if (scope == "" || scope == ScopeAll || scope == ScopeSources) && m.webStore != nil && m.sourcesRootDir != "" {
		collections, err := m.webStore.ListCollections(ctx)
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, collection := range collections {
				key := websource.ScopeKey(collection.Name)
				if err := m.ensureDocsTextKey(ctx, key, m.sourcesDir(collection.Name)); err != nil {
					errs = append(errs, fmt.Errorf("warm docs text for %s; %w", key, err))
				}
			}
		}
	}

	if (scope == "" || scope == ScopeAll || scope == ScopeAPIs) && m.apiStore != nil && m.apisRootDir != "" {
		services, err := m.apiStore.ListServices(ctx)
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, svc := range services {
				key := apicatalog.ScopeKey(svc.Name)
				if err := m.ensureDocsTextKey(ctx, key, m.apisDir(svc.Name)); err != nil {
					errs = append(errs, fmt.Errorf("warm docs text for %s; %w", key, err))
				}
			}
		}
	}

	return errors.Join(errs...)
}

// ensureDocsTextKey backfills one key's lexical index, but never at the cost of
// the query it serves. The per-key lock is also held for the whole of a web
// source sync or a repo refresh/generate, so taking it unconditionally made a
// search block until an unrelated write job finished (and a scope-wide search
// blocks on every key it walks). Reads therefore probe first and only take the
// lock opportunistically: a key that is already indexed never touches it, and a
// key owned by a running job is skipped, because that job indexes it on
// completion anyway. Worst case the query runs against what is indexed now.
func (m *Manager) ensureDocsTextKey(ctx context.Context, key, docsDir string) error {
	// Keys already known to be indexed are never probed again: the pipeline
	// only ever adds to a key's index, so the answer cannot go back to "no".
	if _, ok := m.docsTextKeys.Load(key); ok {
		return nil
	}

	if !dirHasMarkdown(docsDir) {
		return nil
	}

	has, err := m.docsText.HasRepo(ctx, key)
	if err != nil {
		return err
	}
	if has {
		m.docsTextKeys.Store(key, struct{}{})

		return nil
	}

	// TryLock, not lockKey: a concurrent backfill of the same key is already
	// doing this work, so walking away is correct and waiting would only stall
	// a search behind it.
	release, ok := m.tryLockKey(key)
	if !ok {
		return nil
	}
	defer release()

	// Re-probe under the lock: a concurrent backfill may have finished while
	// this call was between the probe and the lock.
	if has, err = m.docsText.HasRepo(ctx, key); err != nil {
		return err
	}
	if !has {
		if err := m.docsText.IndexWithOptions(ctx, key, docsDir, &rag.IndexOptions{
			KeepMarkdownTargets: m.ragConfigSnapshot().KeepMarkdownTargets,
		}); err != nil {
			return err
		}
	}

	m.docsTextKeys.Store(key, struct{}{})

	return nil
}
