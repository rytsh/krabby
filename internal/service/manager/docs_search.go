package manager

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/docsearch"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/service/websource"
)

// Docs search scopes: everything, repository docs, web sources or the API
// catalog.
const (
	ScopeAll     = "all"
	ScopeRepos   = "repos"
	ScopeSources = "sources"
	ScopeAPIs    = "apis"
)

// Documentation search modes. Semantic is the default when configured;
// otherwise lexical is used. Hybrid explicitly combines both ranks with RRF.
const (
	DocsSearchHybrid   = docsearch.ModeHybrid
	DocsSearchSemantic = docsearch.ModeSemantic
	DocsSearchLexical  = docsearch.ModeLexical
)

// NormalizeDocsSearchMode validates a public docs search mode. An empty mode is
// left empty and resolved per request by resolveDocsSearchMode, because the
// default depends on what the installation has configured.
func NormalizeDocsSearchMode(mode string) (string, error) {
	return docsearch.NormalizeMode(mode)
}

// resolveDocsSearchMode picks the mode for a request that did not name one.
//
// Semantic is the default. The lexical arm's cost is proportional to how much
// of the corpus shares the question's vocabulary, and a large single-domain
// collection (a JIRA project of tens of thousands of tickets, say) is exactly
// the case where every term is common — so BM25 there scans most of the index
// while the vector search stays bounded by its ANN structure. Hybrid pays the
// lexical cost too, since it waits for both arms.
//
// Lexical remains the better tool for exact keys, error codes and identifiers,
// and hybrid for combining the two; both stay one parameter away. An
// installation with no embedder gets lexical, the only mode that works there.
func (m *Manager) resolveDocsSearchMode(mode string) string {
	if mode != "" {
		return mode
	}

	d, release := m.acquireDocs()
	defer release()

	if d.rag != nil {
		return DocsSearchSemantic
	}

	return DocsSearchLexical
}

// docsFilter translates a scope + optional key into a vector-store filter.
// key may be a repo id, a web-source scope key ("web:<name>") or an API-catalog
// scope key ("api:<name>"); when set it wins over the scope.
func docsFilter(scope, key string) (vectorstore.Filter, error) {
	if key != "" {
		return vectorstore.FilterKey(key), nil
	}

	switch scope {
	case "", ScopeAll:
		return vectorstore.Filter{}, nil
	case ScopeRepos:
		return vectorstore.Filter{Kind: vectorstore.KindRepo}, nil
	case ScopeSources:
		return vectorstore.Filter{Kind: vectorstore.KindWeb}, nil
	case ScopeAPIs:
		return vectorstore.Filter{Kind: vectorstore.KindAPI}, nil
	default:
		return vectorstore.Filter{}, fmt.Errorf("unknown scope %q (want all, repos, sources or apis)", scope)
	}
}

// SearchDocs returns bounded markdown excerpts using hybrid, semantic or
// lexical retrieval. scope selects all/repos/sources; key restricts to one repo
// or web:<collection> and wins over scope. Namespace scopes only repository
// docs. topDocs <= 0 uses the default.
func (m *Manager) SearchDocs(ctx context.Context, scope, key, namespace, mode, question string, topDocs int) (rag.DocsPage, error) {
	searchStarted := time.Now()
	key = strings.TrimSpace(key)

	ctx, cancel := withSearchTimeout(ctx, semanticSearchTimeout)
	defer cancel()

	mode, err := NormalizeDocsSearchMode(mode)
	if err != nil {
		return rag.DocsPage{}, err
	}
	mode = m.resolveDocsSearchMode(mode)
	page := rag.DocsPage{Results: []rag.Doc{}, Mode: mode}

	if err := m.validateDocsKey(ctx, key); err != nil {
		return rag.DocsPage{}, err
	}

	filter, err := docsFilter(scope, key)
	if err != nil {
		return rag.DocsPage{}, err
	}
	if strings.TrimSpace(question) == "" {
		return rag.DocsPage{}, errors.New("question is empty")
	}

	ragCfg := m.ragConfigSnapshot()

	filter, emptyScope, err := m.docsNamespaceFilter(ctx, scope, key, namespace, filter)
	if err != nil {
		return rag.DocsPage{}, err
	}
	if emptyScope {
		page.Note = fmt.Sprintf("namespace %s holds nothing indexed, so nothing was searched; retry with namespace \"*\" to search every namespace.",
			displayNamespace(namespace))

		return page, nil
	}

	if mode != DocsSearchSemantic {
		if m.docsText == nil {
			return rag.DocsPage{}, errors.New("lexical docs search is not configured")
		}
		// The lexical index is maintained by the indexing pipeline. The only
		// case a query could have to repair is an installation upgraded from
		// before the index existed, and the startup warm handles that once, so
		// this is skipped as soon as it has run. Probing per query and per key
		// in scope costs more than the search it guards.
		if !m.docsTextWarmed.Load() {
			if err := m.ensureDocsTextForSearch(ctx, scope, key, namespace); err != nil {
				return rag.DocsPage{}, err
			}
		}
	}

	var lexical docsearch.LexicalIndex
	if m.docsText != nil {
		lexical = m.docsText
	}
	result, err := docsearch.New(lexical, m.retrieveSemanticCandidates).Search(ctx, docsearch.Request{
		Filter: filter, Question: question, Mode: mode, TopDocs: topDocs, Config: ragCfg,
	})
	if err != nil {
		return rag.DocsPage{}, err
	}
	docs := result.Docs

	m.enrichDocSources(ctx, docs)

	logDocsSearch(mode, scope, key, time.Since(searchStarted), result.SemanticTook, result.LexicalTook, result.SemanticSplit, len(docs))

	if len(docs) > 0 {
		page.Results = docs

		return page, nil
	}

	page.Note = m.emptyDocsNote(mode, scope, key, namespace)

	return page, nil
}

// emptyDocsNote explains a docs search that matched nothing.
//
// Every branch below is a state an agent can act on, and none of them is
// distinguishable from "the documentation does not cover this" without being
// said out loud. The mode is named because it is not always the one asked for,
// and the scope because the default namespace is the easiest way to search a
// fraction of the corpus by accident.
func (m *Manager) emptyDocsNote(mode, scope, key, namespace string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "no document matched (mode %s", mode)
	switch {
	case key != "":
		fmt.Fprintf(&b, ", scope_key %s", key)
	case scope != "":
		fmt.Fprintf(&b, ", scope %s", scope)
	}
	if namespace != "" {
		fmt.Fprintf(&b, ", namespace %s", displayNamespace(namespace))
	}
	b.WriteString("). ")

	if mode == DocsSearchSemantic {
		b.WriteString("Semantic retrieval ranks the embedded documents, so a repository whose docs were generated but never embedded has nothing to rank: try mode 'lexical', or check the docs_index stage with repo_status. ")
	}
	if strings.HasPrefix(key, websource.ScopePrefix) || scope == ScopeSources {
		b.WriteString("This searched Krabby's indexed collection, not live Jira/Confluence. A missing hit does not prove the upstream item is absent: the collection may be filtered, incomplete or not synced. Use list_sources/get_source to inspect coverage, or the provider's live MCP when available.")
	} else {
		b.WriteString("Widen with namespace \"*\", or use list_docs to see whether the scope holds any document at all. Indexed source collections are not exhaustive live Jira/Confluence results.")
	}

	return b.String()
}

func (m *Manager) validateDocsKey(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	if strings.HasPrefix(key, websource.ScopePrefix) {
		name := websource.CollectionName(key)
		if name == "" || m.webStore == nil {
			return fmt.Errorf("unknown web source scope %q; use list_sources and pass its scope_key", key)
		}
		col, err := m.webStore.GetCollection(ctx, name)
		if err != nil {
			return err
		}
		if col == nil {
			return fmt.Errorf("unknown web source scope %q; use list_sources and pass its scope_key", key)
		}
		return nil
	}
	if strings.HasPrefix(key, apicatalog.ScopePrefix) {
		name := apicatalog.ServiceName(key)
		if name == "" || m.apiStore == nil {
			return fmt.Errorf("unknown api scope %q; use list_api_services and pass api:<name>", key)
		}
		svc, err := m.apiStore.GetService(ctx, name)
		if err != nil {
			return err
		}
		if svc == nil {
			return fmt.Errorf("unknown api scope %q; use list_api_services and pass api:<name>", key)
		}
		return nil
	}
	if m.reg == nil {
		return fmt.Errorf("unknown repository scope %q; use list_repos and pass its exact id", key)
	}
	repo, err := m.reg.Get(ctx, key)
	if err != nil {
		return err
	}
	if repo == nil {
		return fmt.Errorf("unknown repository scope %q; use list_repos and pass its exact id", key)
	}
	return nil
}

// docsNamespaceFilter resolves a repository namespace before retrieval. Web
// sources and catalogued APIs remain eligible for scope=all, but
// out-of-namespace repositories are never allowed to consume the bounded
// candidate window.
func (m *Manager) docsNamespaceFilter(ctx context.Context, scope, key, namespace string, filter vectorstore.Filter) (vectorstore.Filter, bool, error) {
	// Scopes that select no repository at all have nothing for a repository
	// namespace to narrow, so the filter passes through untouched. Building a
	// key allow-list for them would instead exclude everything they target.
	if key != "" || strings.EqualFold(strings.TrimSpace(namespace), registry.NamespaceAll) ||
		scope == ScopeSources || scope == ScopeAPIs {
		return filter, false, nil
	}
	repos, err := m.reg.ListNamespace(ctx, namespace)
	if err != nil {
		return vectorstore.Filter{}, false, err
	}
	keys := make([]string, 0, len(repos))
	for _, repo := range repos {
		keys = append(keys, repo.ID)
	}
	if scope == "" || scope == ScopeAll {
		if m.webStore != nil {
			collections, err := m.webStore.ListCollections(ctx)
			if err != nil {
				return vectorstore.Filter{}, false, err
			}
			for _, col := range collections {
				keys = append(keys, websource.ScopeKey(col.Name))
			}
		}
		// Without this the allow-list silently excludes every catalogued API,
		// so a default scope=all search can never return an endpoint document.
		if m.apiStore != nil {
			services, err := m.apiStore.ListServices(ctx)
			if err != nil {
				return vectorstore.Filter{}, false, err
			}
			for _, svc := range services {
				keys = append(keys, apicatalog.ScopeKey(svc.Name))
			}
		}
	}
	if len(keys) == 0 {
		return vectorstore.Filter{}, true, nil
	}
	sort.Strings(keys)
	return vectorstore.Filter{Keys: keys}, false, nil
}

// ragConfigSnapshot returns the live docs retrieval tuning. It is read from the
// bundle so a settings update swaps it atomically together with the clients.
func (m *Manager) ragConfigSnapshot() config.RAG {
	d, release := m.acquireDocs()
	defer release()

	return d.ragCfg
}

// retrieveSemanticCandidates runs the embedding arm under the bundle lease,
// reporting how the arm's time split between the embedder and the vector
// index so a slow search says which of the two was responsible.
func (m *Manager) retrieveSemanticCandidates(
	ctx context.Context,
	filter vectorstore.Filter,
	question string,
	candidates int,
) ([]rag.Doc, rag.RetrieveTiming, error) {
	d, release := m.acquireDocs()
	defer release()

	if d.rag == nil {
		return nil, rag.RetrieveTiming{}, fmt.Errorf("semantic docs search is not enabled; use mode %q", DocsSearchLexical)
	}

	return d.rag.RetrieveCandidatesTimed(ctx, filter, question, candidates)
}

// filterDocsByNamespace keeps web-source and API-catalog docs (neither is
// namespaced) and repo docs whose repo is in the namespace, then trims to
// topDocs. It resolves the namespace's repo set once and matches doc.Repo
// against it.
func (m *Manager) filterDocsByNamespace(ctx context.Context, docs []rag.Doc, namespace string, topDocs int) ([]rag.Doc, error) {
	repos, err := m.reg.List(ctx)
	if err != nil {
		return nil, err
	}

	inNamespace := map[string]struct{}{}
	for _, repo := range repos {
		if repoInNamespace(repo, namespace) {
			inNamespace[repo.ID] = struct{}{}
		}
	}

	out := docs[:0]
	for _, doc := range docs {
		// Web sources and catalogued APIs belong to no namespace, so a
		// namespace filter must pass them through rather than drop them for
		// failing a repo-set lookup they can never satisfy.
		if strings.HasPrefix(doc.Repo, websource.ScopePrefix) || strings.HasPrefix(doc.Repo, apicatalog.ScopePrefix) {
			out = append(out, doc)
			continue
		}
		if _, ok := inNamespace[doc.Repo]; ok {
			out = append(out, doc)
		}
	}

	if topDocs > 0 && len(out) > topDocs {
		out = out[:topDocs]
	}

	return out, nil
}
