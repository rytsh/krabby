package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/searchutil"
)

// ErrCodeRAGDisabled is returned when source-code semantic search is off.
var ErrCodeRAGDisabled = errors.New("code rag subsystem is not enabled")

// namespaceScope describes how a namespace-restricted cross-repo search should
// run once the namespace is resolved to concrete repos.
//
//   - all: no restriction (the caller passed NamespaceAll or an explicit repo).
//   - single: exactly one repo in the namespace; search that repo directly.
//   - repos/set: several repos; search broadly and keep only these.
type namespaceScope struct {
	all    bool
	single string
	repos  []string
	set    map[string]struct{}
}

func (s namespaceScope) contains(repo string) bool {
	_, ok := s.set[repo]
	return ok
}

// fetch enlarges the requested topK for a post-filtered search so the filters
// applied after retrieval do not starve the result set. filtered says whether
// anything will be dropped afterwards; without a post-filter the widening
// would only pay for candidates nobody looks at.
func (s namespaceScope) fetch(topK int, filtered bool) int {
	if topK <= 0 {
		topK = 10
	}
	if !filtered {
		return topK
	}

	// The ceiling matches coderag.maxSearchResults; asking for more would be
	// clamped one layer down and the widening would silently not happen.
	return min(topK*4, 200)
}

// namespaceScope resolves a repoID/namespace pair. An explicit repoID or
// NamespaceAll yields an unrestricted (all) scope; otherwise it lists the repos
// in the namespace and reports whether it is empty, a single repo, or several.
//
// A named repository is checked against the registry. Without that check a
// mistyped or untracked id scopes the search to a prefix nothing carries, and
// the caller is handed an empty page that reads as "this code does not exist"
// - the one answer a search must never invent. A namespace typo has always
// been an error; the id is the more likely typo of the two.
func (m *Manager) namespaceScope(ctx context.Context, repoID, namespace string) (namespaceScope, error) {
	if repoID = strings.TrimSpace(repoID); repoID != "" {
		repo, err := m.reg.Get(ctx, repoID)
		if err != nil {
			return namespaceScope{}, err
		}
		if repo == nil {
			return namespaceScope{}, fmt.Errorf("repository %q is not tracked; list_repos names the tracked ids, and omitting repo searches the default namespace", repoID)
		}

		return namespaceScope{all: true}, nil
	}

	if strings.EqualFold(strings.TrimSpace(namespace), registry.NamespaceAll) {
		return namespaceScope{all: true}, nil
	}

	repos, err := m.reg.List(ctx)
	if err != nil {
		return namespaceScope{}, err
	}

	scope := namespaceScope{set: map[string]struct{}{}}
	for _, repo := range repos {
		if repoInNamespace(repo, namespace) {
			scope.repos = append(scope.repos, repo.ID)
			scope.set[repo.ID] = struct{}{}
		}
	}
	sort.Strings(scope.repos)

	switch len(scope.repos) {
	case 0:
		return namespaceScope{}, fmt.Errorf("no repository in namespace %s; retry with namespace \"*\" to search all", displayNamespace(namespace))
	case 1:
		scope.single = scope.repos[0]
	}

	return scope, nil
}

func trimSnippets(snippets []coderag.Snippet, topK int) []coderag.Snippet {
	if topK <= 0 {
		topK = 10
	}
	if len(snippets) > topK {
		return snippets[:topK]
	}

	return snippets
}

// SearchCode returns the code snippets most relevant to a query. repoID == ""
// searches across the repos in namespace (empty or "default" == the default
// bucket; NamespaceAll searches every repo). topK <= 0 uses the code RAG
// default.
//
// Like the other two modes it honours a path glob and reports the index state
// of the repositories behind the page. Both matter more here than elsewhere: a
// vector index is built separately from the text one, so an empty semantic
// result on a repository whose embeddings were never built is otherwise
// indistinguishable from "this concept is not in the code".
func (m *Manager) SearchCode(
	ctx context.Context,
	repoID, namespace, query string,
	opts coderag.SemanticOptions,
) (coderag.SemanticPage, error) {
	d, releaseDocs := m.acquireDocs()
	defer releaseDocs()
	if d.codeRag == nil || d.codeStore == nil {
		return coderag.SemanticPage{}, ErrCodeRAGDisabled
	}

	ctx, cancel := withSearchTimeout(ctx, semanticSearchTimeout)
	defer cancel()

	scope, err := m.namespaceScope(ctx, repoID, namespace)
	if err != nil {
		return coderag.SemanticPage{}, err
	}

	matchPath, err := coderag.Scope{Path: opts.Path}.PathMatcher()
	if err != nil {
		return coderag.SemanticPage{}, err
	}

	searchStarted := time.Now()

	byNamespace := scope.single == "" && !scope.all
	searchRepo := repoID
	if scope.single != "" {
		searchRepo = scope.single
	}
	fetch := scope.fetch(opts.TopK, byNamespace || matchPath != nil)

	snippets, split, err := d.codeRag.RetrieveTimed(ctx, searchRepo, query, fetch)
	if err != nil {
		return coderag.SemanticPage{}, err
	}

	if byNamespace || matchPath != nil {
		out := snippets[:0]
		for _, s := range snippets {
			if byNamespace && !scope.contains(s.Repo) {
				continue
			}
			if matchPath != nil && !matchPath(s.Path) {
				continue
			}
			out = append(out, s)
		}
		snippets = trimSnippets(out, opts.TopK)
	}

	logCodeSearch(repoID, namespace, time.Since(searchStarted), split, len(snippets))

	page := coderag.SemanticPage{Results: snippets}
	page.Indexed = m.repoIndexStates(ctx, m.indexedRepos(snippetRepos(snippets), m.scopedRepoIDs(ctx, repoID, scope)))

	return page, nil
}

// scopedRepoIDs lists the repositories a scope covers, for reporting the index
// state behind an empty result. A failure here costs the report, never the
// search: the hits are already resolved by the time it runs.
func (m *Manager) scopedRepoIDs(ctx context.Context, repoID string, scope namespaceScope) []string {
	ids, err := m.codeScopeRepos(ctx, repoID, scope)
	if err != nil {
		return nil
	}

	return ids
}

// SearchCodeText performs BM25 full-text search over the local bw index,
// scoped to a repository, a namespace or everything, and optionally to a path
// glob.
//
// bw ANDs a bare query's terms, so the raw query is first rewritten into an OR
// chain plus required identifiers — the same rewrite the documentation index
// uses, and for the same reason: passing a question verbatim requires every
// one of its words inside a single 3000-character chunk, which almost always
// matched nothing. Identifier-shaped words (`bw.ErrNotFound`, `PAY-1842`) stay
// required, so an exact lookup does not lose precision to the loosening.
//
// Terms the indexed source itself shows to be ubiquitous are dropped from that
// chain: a source corpus is full of `func`, `return` and `error`, each costing
// a posting-list walk proportional to the whole index while contributing
// almost nothing to the ranking. Filtering is an optimisation and is never
// allowed to cost a result, so a filtered query that comes back empty is
// retried unfiltered.
func (m *Manager) SearchCodeText(
	ctx context.Context,
	repoID, namespace, query string,
	opts coderag.TextSearchOptions,
) (page coderag.SearchPage, err error) {
	timing := newCodeSearchTiming()
	defer func() {
		timing.finish(ctx, slog.Default(), "text", repoID, namespace, len(page.Results), page.Total, err)
	}()
	if m.codeText == nil {
		return coderag.SearchPage{}, errors.New("normal code search is not configured")
	}

	ctx, cancel := withSearchTimeout(ctx, searchTimeout)
	defer cancel()

	filter, repos, err := m.codeScope(ctx, repoID, namespace, opts.Path, timing)
	if err != nil {
		return coderag.SearchPage{}, err
	}
	opts.KeyFilter = filter

	timing.enter("prepare")
	stop := m.codeText.FrequentTerms(ctx)
	rewritten := searchutil.LexicalQuery(query, stop)

	timing.enter("index")
	page, err = m.codeText.Search(ctx, rewritten, opts)
	if err != nil {
		return page, err
	}
	if page.Total == 0 {
		if unfiltered := searchutil.LexicalQuery(query, nil); unfiltered != rewritten {
			timing.enter("retry")
			timing.retried = true
			// The retry's failure must not be reported as an empty result: the
			// docstring promises filtering never costs a result, and a store
			// error presented as total=0 reads as a definitive "no such code".
			retry, rerr := m.codeText.Search(ctx, unfiltered, opts)
			if rerr != nil {
				return page, rerr
			}
			page = retry
		}
	}

	timing.enter("metadata")
	page.Indexed = m.repoIndexStates(ctx, m.indexedRepos(snippetRepos(page.Results), repos))

	return page, nil
}

// SearchCodeRegex runs a regular-expression search over the local trigram
// index, scoped the same way.
func (m *Manager) SearchCodeRegex(
	ctx context.Context,
	repoID, namespace, pattern string,
	opts coderag.RegexOptions,
) (page coderag.RegexPage, err error) {
	timing := newCodeSearchTiming()
	defer func() {
		timing.finish(ctx, slog.Default(), "regex", repoID, namespace, len(page.Results), page.Total, err)
	}()
	if m.codeText == nil {
		return coderag.RegexPage{}, errors.New("regex code search is not configured")
	}

	ctx, cancel := withSearchTimeout(ctx, searchTimeout)
	defer cancel()

	filter, repos, err := m.codeScope(ctx, repoID, namespace, opts.Path, timing)
	if err != nil {
		return coderag.RegexPage{}, err
	}

	timing.enter("index")
	page, err = m.codeText.SearchRegex(ctx, filter, pattern, opts)
	if err != nil {
		return page, err
	}

	timing.enter("metadata")
	hitRepos := make([]string, 0, len(page.Results))
	for _, hit := range page.Results {
		hitRepos = append(hitRepos, hit.Repo)
	}
	page.Indexed = m.repoIndexStates(ctx, m.indexedRepos(hitRepos, repos))

	return page, nil
}

// codeScope resolves a request's repo/namespace/path arguments into the chunk
// key filter every search mode shares, plus the concrete repository ids the
// search was scoped to.
//
// It also warms the index when the request named a single repository: that
// repo's index may still be building in the background at startup, and a query
// about one repo answered from a half-built index is a wrong answer. A
// cross-repo scope is not warmed here — see ensureCodeIndexForSearch.
func (m *Manager) codeScope(
	ctx context.Context,
	repoID, namespace, pathGlob string,
	timing *codeSearchTiming,
) (func(string) bool, []string, error) {
	timing.enter("scope")
	nsScope, err := m.namespaceScope(ctx, repoID, namespace)
	if err != nil {
		return nil, nil, err
	}

	timing.enter("warm")
	if err := m.ensureCodeIndexForSearch(ctx, repoID, nsScope); err != nil {
		return nil, nil, err
	}

	timing.enter("scope")
	repos, err := m.codeScopeRepos(ctx, repoID, nsScope)
	if err != nil {
		return nil, nil, err
	}

	// An unconstrained scope passes no repository list: with every tracked
	// repository in scope the prefix test can only ever succeed, so paying it
	// per hit buys nothing. An anchored path pattern is the exception — it
	// needs the list to know where the repository prefix ends.
	scope := coderag.Scope{Repos: repos, Path: pathGlob}
	if nsScope.all && repoID == "" && !strings.Contains(pathGlob, "/") {
		scope.Repos = nil
	}

	filter, err := scope.KeyFilter()
	if err != nil {
		return nil, nil, err
	}

	return filter, repos, nil
}

// codeScopeRepos lists the repository ids a scope covers.
func (m *Manager) codeScopeRepos(ctx context.Context, repoID string, scope namespaceScope) ([]string, error) {
	switch {
	case scope.single != "":
		return []string{scope.single}, nil
	case len(scope.repos) > 0:
		return scope.repos, nil
	case repoID != "":
		return []string{repoID}, nil
	}

	repos, err := m.reg.List(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(repos))
	for _, r := range repos {
		ids = append(ids, r.ID)
	}

	return ids, nil
}

// repoIndexStatesLimit bounds how many repositories a result page reports the
// index state of beyond the ones that produced hits. A page carries at most
// PerPage distinct repositories, so the cap applies to the two cases that are
// scope-sized rather than page-sized: an empty result, and the list of repos
// still building. In both, "which repositories did this even look at" stops
// being useful long before it stops being long.
const repoIndexStatesLimit = 10

// repoIndexStates reports each repository's code-index state.
func (m *Manager) repoIndexStates(ctx context.Context, repos []string) []coderag.RepoIndex {
	if len(repos) == 0 {
		return nil
	}

	out := make([]coderag.RepoIndex, 0, len(repos))
	for _, id := range repos {
		repo, err := m.reg.Get(ctx, id)
		if err != nil || repo == nil {
			// A repository removed between the search and this read simply has
			// no index state to report; the hits it produced stay valid.
			continue
		}
		stage := repo.Stages.CodeIndex
		_, pending := m.codeWarmLock(id)
		out = append(out, coderag.RepoIndex{
			Repo:      id,
			Commit:    stage.Commit,
			IndexedAt: stage.FinishedAt,
			// The clone having moved past the indexed commit is the one thing
			// a caller cannot infer from the hit itself.
			Stale: stage.Commit != "" && repo.LastCommit != "" && stage.Commit != repo.LastCommit,
			// A cross-repo search no longer blocks on a first build, so the
			// page has to say which repos it could not read yet.
			Pending: pending,
		})
	}

	return out
}

// indexedRepos picks the repositories whose index state a page should report.
//
// Always the ones that produced hits. Plus any repository in scope whose first
// index is still being built: a cross-repo search does not wait for those, so
// leaving them out would present "not searched yet" as "nothing matched here",
// which is the one thing the caller cannot recover from the results. When
// nothing matched at all the scope itself is reported, for the same reason.
func (m *Manager) indexedRepos(hits, scoped []string) []string {
	seen := make(map[string]struct{}, len(hits))
	out := make([]string, 0, len(hits))

	add := func(id string) bool {
		if id == "" {
			return false
		}
		if _, dup := seen[id]; dup {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)

		return true
	}

	if len(hits) == 0 {
		for _, id := range scoped {
			if len(out) >= repoIndexStatesLimit {
				break
			}
			add(id)
		}

		return out
	}

	for _, id := range hits {
		add(id)
	}

	// The cap counts only the repositories added for being unbuilt; the hits
	// are already bounded by PerPage.
	unbuilt := 0
	for _, id := range scoped {
		if unbuilt >= repoIndexStatesLimit {
			break
		}
		if _, building := m.codeWarmLock(id); building && add(id) {
			unbuilt++
		}
	}

	return out
}

func snippetRepos(snippets []coderag.Snippet) []string {
	out := make([]string, 0, len(snippets))
	for _, s := range snippets {
		out = append(out, s.Repo)
	}

	return out
}

// ensureCodeIndexForSearch warms the normal code index on demand, but only for
// a search that named one repository.
//
// Waiting is the right answer for a single repo: the question is about that
// repo, so an index that is still building makes a partial result a wrong
// answer rather than an incomplete one, and the wait is bounded by one clone.
//
// A scope spanning many repositories is the opposite. Warming every one of them
// here puts a full chunk-and-index pass per repo inside the request, in series,
// behind the same per-repo permits the background warm holds — so the first
// cross-repo query after a bulk import waits for the entire corpus to be built
// and reads as a hang. Nothing about that wait is load-bearing: those repos are
// already being warmed in the background, and a page reports what is still
// pending (repoIndexStates), so the caller can tell "no matches" apart from
// "not indexed yet" without the request paying for the difference.
//
// This mirrors ensureDocsTextKey, which resolved the same problem on the
// lexical docs path.
func (m *Manager) ensureCodeIndexForSearch(ctx context.Context, repoID string, scope namespaceScope) error {
	target := scope.single
	if target == "" && scope.all && repoID != "" {
		// Explicit single repo.
		target = repoID
	}
	if target == "" {
		// Cross-repo search: report, do not block. See above.
		return nil
	}

	if _, pending := m.codeWarmLock(target); !pending {
		return nil
	}

	repo, err := m.reg.Get(ctx, target)
	if err != nil {
		return fmt.Errorf("load repo %s for code index warmup: %w", target, err)
	}
	if repo == nil {
		// A repository can be removed after namespace resolution or the
		// startup list. It no longer participates in this search.
		m.clearCodeWarmPending(target)

		return nil
	}
	if err := m.ensureCodeIndex(ctx, target, repo.Path); err != nil {
		return fmt.Errorf("warm code index for %s: %w", target, err)
	}

	return nil
}
