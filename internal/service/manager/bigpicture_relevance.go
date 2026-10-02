package manager

import (
	"context"
	"log/slog"
	"strings"
	"unicode"

	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/searchutil"
	"github.com/rytsh/krabby/internal/service/vectorstore"
	"github.com/rytsh/krabby/internal/service/websource"
)

// pictureMCPCandidates bounds how many granted MCP resources are read before
// relevance filtering keeps the best of them.
const pictureMCPCandidates = 16

// pictureMinSourceBytes is the smallest research share any source gets, no
// matter how many sources a workspace has; condensing handles the overflow.
const pictureMinSourceBytes = 48 << 10

// pictureNoteCache binds the note cache to one workspace instance.
type pictureNoteCache struct {
	store   *bigpicture.Store
	picture *bigpicture.Picture
}

func (c pictureNoteCache) Note(key string) (string, bool) { return c.store.Note(c.picture, key) }

func (c pictureNoteCache) SaveNote(key, text string) error {
	return c.store.SaveNote(c.picture, key, text)
}

// matchRepoPattern returns the repositories whose id matches a normalized
// repo_pattern source.
func matchRepoPattern(pattern string, repos []*registry.Repo) []*registry.Repo {
	matched := []*registry.Repo{}
	for _, repo := range repos {
		if bigpicture.MatchRepoPattern(pattern, repo.ID) {
			matched = append(matched, repo)
		}
	}
	return matched
}

func pictureScopeKey(source bigpicture.Source) string {
	if source.Kind == "api" {
		return apicatalog.ScopeKey(source.Ref)
	}
	return websource.ScopeKey(source.Ref)
}

// pictureRelevanceTerms derives the vocabulary that decides whether a
// Jira/Confluence page or MCP resource belongs in this picture: the workspace
// title, description and prompt plus the names of the selected repositories,
// since tickets and pages usually mention the services they concern. Terms
// common across the indexed corpus are dropped because they match everything.
func (m *Manager) pictureRelevanceTerms(ctx context.Context, p *bigpicture.Picture, repoIDs []string) []string {
	var stop searchutil.StopWords
	if m.docsText != nil {
		stop = m.docsText.FrequentTerms(ctx)
	}
	text := []string{p.Title, p.Description, p.Prompt}
	for _, id := range repoIDs {
		text = append(text, id[strings.LastIndex(id, "/")+1:])
	}
	seen := map[string]bool{}
	terms := []string{}
	for _, word := range strings.FieldsFunc(strings.Join(text, " "), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		word = strings.ToLower(word)
		if len([]rune(word)) < 3 || stop[word] || pictureGenericTerms[word] || seen[word] {
			continue
		}
		seen[word] = true
		terms = append(terms, word)
	}
	return terms
}

// pictureGenericTerms are words that research prompts use to describe the
// task rather than the system, so they say nothing about relevance.
var pictureGenericTerms = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "how": true, "what": true, "which": true,
	"explain": true, "describe": true, "document": true, "documents": true, "documentation": true,
	"architecture": true, "overview": true, "system": true, "systems": true, "service": true,
	"services": true, "page": true, "pages": true, "separate": true, "detail": true, "detailed": true,
	"cite": true, "sources": true, "source": true, "flows": true, "flow": true, "all": true,
	"from": true, "into": true, "about": true, "this": true, "that": true, "their": true, "are": true,
}

// pictureRelevanceScore counts distinct relevance terms present in text.
func pictureRelevanceScore(terms []string, text string) int {
	lower := strings.ToLower(text)
	score := 0
	for _, term := range terms {
		if strings.Contains(lower, term) {
			score++
		}
	}
	return score
}

// pictureRelevantPaths searches one indexed collection with the relevance
// terms and returns up to limit distinct document paths, best first. searched
// is false when no index or terms exist, so the caller falls back to sampling.
func (m *Manager) pictureRelevantPaths(ctx context.Context, scope string, terms []string, limit int) (paths []string, searched bool) {
	if m.docsText == nil || len(terms) == 0 {
		return nil, false
	}
	if ok, err := m.docsText.HasRepo(ctx, scope); err != nil || !ok {
		return nil, false
	}
	// OR the terms so BM25 ranks pages by how many of them they mention.
	query := strings.Join(terms[:min(len(terms), 12)], " OR ")
	docs, err := m.docsText.Search(ctx, vectorstore.FilterKey(scope), query, limit*4)
	if err != nil {
		slog.Warn("big picture relevance search failed", "scope", scope, "error", err)
		return nil, false
	}
	seen := map[string]bool{}
	for _, doc := range docs {
		if seen[doc.Path] || isPictureSecretPath(doc.Path) {
			continue
		}
		seen[doc.Path] = true
		paths = append(paths, doc.Path)
		if len(paths) >= limit {
			break
		}
	}
	return paths, true
}
