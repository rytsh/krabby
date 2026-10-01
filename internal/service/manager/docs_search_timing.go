package manager

import (
	"log/slog"
	"time"

	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/rag"
)

// docsSearchSlowThreshold is when one search is worth a log line of its own.
// Retrieval cost grows with how much of the corpus shares the question's
// vocabulary, which is a property of the data rather than of the query, so the
// only way to know whether it has become a problem for a given installation is
// to record it.
const docsSearchSlowThreshold = 500 * time.Millisecond

// logDocsSearch records what a search cost, per ranker. Debug for the normal
// case, a warning past the threshold, so a corpus that has outgrown the lexical
// index announces itself instead of being felt as "search is sluggish".
//
// The semantic arm is broken down further into the embedder round trip and the
// vector search. Those two degrade for unrelated reasons and are fixed in
// different places, so a bare semantic_ms leaves the first diagnostic question
// unanswered.
func logDocsSearch(
	mode, scope, key string,
	total, semantic, lexical time.Duration,
	split rag.RetrieveTiming,
	results int,
) {
	attrs := []any{
		"mode", mode,
		"total_ms", total.Milliseconds(),
		"results", results,
	}
	if key != "" {
		attrs = append(attrs, "key", key)
	} else if scope != "" {
		attrs = append(attrs, "scope", scope)
	}
	if semantic > 0 {
		attrs = append(attrs, "semantic_ms", semantic.Milliseconds())
	}
	if split.Embed > 0 {
		attrs = append(attrs, "embed_ms", split.Embed.Milliseconds())
	}
	if split.Vector > 0 {
		attrs = append(attrs, "vector_ms", split.Vector.Milliseconds())
	}
	if lexical > 0 {
		attrs = append(attrs, "lexical_ms", lexical.Milliseconds())
	}

	if total >= docsSearchSlowThreshold {
		slog.Warn("slow docs search", attrs...)

		return
	}

	slog.Debug("docs search", attrs...)
}

// logCodeSearch is logDocsSearch for the semantic code arm.
//
// Code search had no timing of its own, so the only way to find out whether a
// slow code query was the embedder or the index was to attach a profiler to a
// running server. Same threshold and same split as the docs arm, so the two
// are comparable in one log stream.
func logCodeSearch(repo, namespace string, total time.Duration, split coderag.RetrieveTiming, results int) {
	attrs := []any{
		"total_ms", total.Milliseconds(),
		"results", results,
	}
	if repo != "" {
		attrs = append(attrs, "repo", repo)
	} else if namespace != "" {
		attrs = append(attrs, "namespace", namespace)
	}
	if split.Embed > 0 {
		attrs = append(attrs, "embed_ms", split.Embed.Milliseconds())
	}
	if split.Vector > 0 {
		attrs = append(attrs, "vector_ms", split.Vector.Milliseconds())
	}

	if total >= docsSearchSlowThreshold {
		slog.Warn("slow code search", attrs...)

		return
	}

	slog.Debug("code search", attrs...)
}
