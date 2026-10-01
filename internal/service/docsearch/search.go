// Package docsearch coordinates lexical and semantic document retrieval.
// Repository authorization, index warming and source metadata stay with callers.
package docsearch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/searchutil"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

const (
	ModeHybrid   = "hybrid"
	ModeSemantic = "semantic"
	ModeLexical  = "lexical"
)

// NormalizeMode validates a mode without choosing a default backend.
func NormalizeMode(mode string) (string, error) {
	switch mode = strings.ToLower(strings.TrimSpace(mode)); mode {
	case "", ModeHybrid, ModeSemantic, ModeLexical:
		return mode, nil
	default:
		return "", fmt.Errorf("mode must be hybrid, semantic or lexical")
	}
}

// LexicalIndex is the read-only surface required for BM25 retrieval.
type LexicalIndex interface {
	FrequentTerms(context.Context) searchutil.StopWords
	Search(context.Context, vectorstore.Filter, string, int) ([]rag.Doc, error)
}

// SemanticSearch lets the owner lease live clients for the duration of a search,
// without exposing bundle or resource lifecycle details to the retriever.
type SemanticSearch func(context.Context, vectorstore.Filter, string, int) ([]rag.Doc, rag.RetrieveTiming, error)

type Retriever struct {
	lexical  LexicalIndex
	semantic SemanticSearch
}

func New(lexical LexicalIndex, semantic SemanticSearch) *Retriever {
	return &Retriever{lexical: lexical, semantic: semantic}
}

// Request carries a resolved scope filter and an immutable tuning snapshot.
// Mode may be empty; semantic is preferred when its backend is available.
type Request struct {
	Filter   vectorstore.Filter
	Question string
	Mode     string
	TopDocs  int
	Config   config.RAG
}

type Result struct {
	Docs          []rag.Doc
	Mode          string
	SemanticTook  time.Duration
	LexicalTook   time.Duration
	SemanticSplit rag.RetrieveTiming
}

// Search runs hybrid arms concurrently with equal candidate depths. A failure
// cancels the other arm; hybrid never silently returns one ranker's partial answer.
func (r *Retriever) Search(ctx context.Context, req Request) (Result, error) {
	mode, err := NormalizeMode(req.Mode)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(req.Question) == "" {
		return Result{}, errors.New("question is empty")
	}
	if mode == "" {
		mode = ModeLexical
		if r.semantic != nil {
			mode = ModeSemantic
		}
	}
	if mode != ModeSemantic && r.lexical == nil {
		return Result{}, errors.New("lexical docs search is not configured")
	}
	if mode != ModeLexical && r.semantic == nil {
		return Result{}, fmt.Errorf("semantic docs search is not enabled; use mode %q", ModeLexical)
	}

	topDocs := req.TopDocs
	if topDocs <= 0 {
		topDocs = req.Config.TopDocs
	}
	if topDocs <= 0 {
		topDocs = rag.DefaultTopDocs
	}
	topDocs = min(topDocs, rag.MaxTopDocs)
	fetch := max(hybridCandidates(req.Config), topDocs)

	result := Result{Mode: mode}
	var lexicalDocs, semanticDocs []rag.Doc
	group, groupCtx := errgroup.WithContext(ctx)
	if mode != ModeLexical {
		group.Go(func() error {
			started := time.Now()
			docs, split, err := r.semantic(groupCtx, req.Filter, req.Question, fetch)
			semanticDocs, result.SemanticTook, result.SemanticSplit = docs, time.Since(started), split
			return err
		})
	}
	if mode != ModeSemantic {
		group.Go(func() error {
			started := time.Now()
			docs, err := r.searchLexical(groupCtx, req.Filter, req.Question, fetch, req.Config)
			lexicalDocs, result.LexicalTook = docs, time.Since(started)
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return Result{}, err
	}

	switch mode {
	case ModeSemantic:
		result.Docs = trimDocs(semanticDocs, topDocs)
	case ModeLexical:
		result.Docs = trimDocs(lexicalDocs, topDocs)
	default:
		result.Docs = fuseDocs(lexicalDocs, semanticDocs, topDocs, fuseParamsFor(req.Config))
	}
	return result, nil
}

// Frequent-term filtering only optimizes query cost. Retry without it when it
// removes all matches; never retry a storage failure or an unchanged query.
func (r *Retriever) searchLexical(ctx context.Context, filter vectorstore.Filter, question string, fetch int, cfg config.RAG) ([]rag.Doc, error) {
	stop := r.lexical.FrequentTerms(ctx).Merge(searchutil.NewStopWords(cfg.LexicalStopWords))
	query := searchutil.LexicalQuery(question, stop)
	docs, err := r.lexical.Search(ctx, filter, query, fetch)
	if err != nil || len(docs) > 0 {
		return docs, err
	}
	if unfiltered := searchutil.LexicalQuery(question, nil); unfiltered != query {
		return r.lexical.Search(ctx, filter, unfiltered, fetch)
	}
	return docs, nil
}

func trimDocs(docs []rag.Doc, topDocs int) []rag.Doc {
	if len(docs) > topDocs {
		return docs[:topDocs]
	}
	return docs
}
