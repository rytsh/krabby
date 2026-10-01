package docsearch

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/searchutil"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

type lexicalStub struct {
	terms  searchutil.StopWords
	search func(context.Context, vectorstore.Filter, string, int) ([]rag.Doc, error)
}

func (s lexicalStub) FrequentTerms(context.Context) searchutil.StopWords { return s.terms }
func (s lexicalStub) Search(ctx context.Context, filter vectorstore.Filter, query string, limit int) ([]rag.Doc, error) {
	return s.search(ctx, filter, query, limit)
}

func TestHybridOverlapsArmsWithEqualDepthAndScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type call struct {
		filter vectorstore.Filter
		depth  int
	}
	started := make(chan call, 2)
	release := make(chan struct{})
	filter := vectorstore.Filter{Keys: []string{"acme/repo", "web:wiki"}}
	wait := func(ctx context.Context, filter vectorstore.Filter, depth int) error {
		started <- call{filter, depth}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	lexical := lexicalStub{search: func(ctx context.Context, filter vectorstore.Filter, _ string, depth int) ([]rag.Doc, error) {
		if err := wait(ctx, filter, depth); err != nil {
			return nil, err
		}
		return []rag.Doc{{Repo: "acme/repo", Path: "a.md"}}, nil
	}}
	split := rag.RetrieveTiming{Embed: time.Millisecond, Vector: 2 * time.Millisecond}
	semantic := func(ctx context.Context, filter vectorstore.Filter, _ string, depth int) ([]rag.Doc, rag.RetrieveTiming, error) {
		if err := wait(ctx, filter, depth); err != nil {
			return nil, rag.RetrieveTiming{}, err
		}
		return []rag.Doc{{Repo: "web:wiki", Path: "b.md"}}, split, nil
	}
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := New(lexical, semantic).Search(ctx, Request{
			Filter: filter, Mode: ModeHybrid, Question: "gateway", TopDocs: 2,
			Config: config.RAG{HybridCandidates: 17},
		})
		done <- outcome{result, err}
	}()
	for range 2 {
		select {
		case call := <-started:
			if call.depth != 17 || !reflect.DeepEqual(call.filter, filter) {
				t.Fatalf("ranker input = %+v", call)
			}
		case <-ctx.Done():
			t.Fatal("both rankers did not start before either was released")
		}
	}
	close(release)
	got := <-done
	if got.err != nil || len(got.result.Docs) != 2 || got.result.Mode != ModeHybrid || got.result.SemanticSplit != split {
		t.Fatalf("hybrid result = %+v, %v", got.result, got.err)
	}
}

func TestHybridFailureCancelsOtherArm(t *testing.T) {
	for _, failing := range []string{ModeLexical, ModeSemantic} {
		t.Run(failing, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			active := make(chan struct{})
			canceled := make(chan struct{})
			wantErr := errors.New("ranker failed")
			retrieve := func(ctx context.Context, arm string) error {
				if arm == failing {
					select {
					case <-active:
						return wantErr
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				close(active)
				<-ctx.Done()
				close(canceled)
				return ctx.Err()
			}
			lexical := lexicalStub{search: func(ctx context.Context, _ vectorstore.Filter, _ string, _ int) ([]rag.Doc, error) {
				return nil, retrieve(ctx, ModeLexical)
			}}
			semantic := func(ctx context.Context, _ vectorstore.Filter, _ string, _ int) ([]rag.Doc, rag.RetrieveTiming, error) {
				return nil, rag.RetrieveTiming{}, retrieve(ctx, ModeSemantic)
			}
			result, err := New(lexical, semantic).Search(ctx, Request{Mode: ModeHybrid, Question: "gateway"})
			if !errors.Is(err, wantErr) || len(result.Docs) != 0 {
				t.Fatalf("failed hybrid = %+v, %v", result, err)
			}
			select {
			case <-canceled:
			default:
				t.Fatal("failed search did not wait for the canceled arm")
			}
		})
	}
}

func TestLexicalRetryPolicy(t *testing.T) {
	wantErr := errors.New("index failed")
	for _, tc := range []struct {
		name    string
		stop    searchutil.StopWords
		first   []rag.Doc
		err     error
		queries []string
	}{
		{name: "retry empty filtered query", stop: searchutil.NewStopWords([]string{"payment"}), queries: []string{"gateway OR timeout", "payment OR gateway OR timeout"}},
		{name: "do not retry a hit", stop: searchutil.NewStopWords([]string{"payment"}), first: []rag.Doc{{Path: "hit.md"}}, queries: []string{"gateway OR timeout"}},
		{name: "do not retry storage error", stop: searchutil.NewStopWords([]string{"payment"}), err: wantErr, queries: []string{"gateway OR timeout"}},
		{name: "do not repeat unchanged query", queries: []string{"payment OR gateway OR timeout"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queries []string
			lexical := lexicalStub{terms: tc.stop, search: func(_ context.Context, _ vectorstore.Filter, query string, _ int) ([]rag.Doc, error) {
				queries = append(queries, query)
				if len(queries) == 1 {
					return tc.first, tc.err
				}
				return []rag.Doc{{Path: "retry.md"}}, nil
			}}
			result, err := New(lexical, nil).Search(context.Background(), Request{Mode: ModeLexical, Question: "payment gateway timeout"})
			if !errors.Is(err, tc.err) || !reflect.DeepEqual(queries, tc.queries) {
				t.Fatalf("result = %+v, error = %v, queries = %v", result, err, queries)
			}
			if len(queries) == 2 && (len(result.Docs) != 1 || result.Docs[0].Path != "retry.md") {
				t.Fatal("retried results were discarded")
			}
		})
	}
}

func TestResultLimitsAndDefaultMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		requested  int
		configured int
		want       int
	}{
		{name: "package default", want: rag.DefaultTopDocs},
		{name: "configured default", configured: 2, want: 2},
		{name: "request wins", requested: 1, configured: 2, want: 1},
		{name: "request capped", requested: rag.MaxTopDocs + 10, want: rag.MaxTopDocs},
		{name: "configured default capped", configured: rag.MaxTopDocs + 10, want: rag.MaxTopDocs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, semanticEnabled := range []bool{false, true} {
				var depth int
				docs := make([]rag.Doc, rag.MaxTopDocs+10)
				lexical := lexicalStub{search: func(_ context.Context, _ vectorstore.Filter, _ string, limit int) ([]rag.Doc, error) {
					depth = limit
					return docs, nil
				}}
				var semantic SemanticSearch
				if semanticEnabled {
					semantic = func(_ context.Context, _ vectorstore.Filter, _ string, limit int) ([]rag.Doc, rag.RetrieveTiming, error) {
						depth = limit
						return docs, rag.RetrieveTiming{}, nil
					}
				}
				result, err := New(lexical, semantic).Search(context.Background(), Request{
					Question: "gateway", TopDocs: tc.requested, Config: config.RAG{TopDocs: tc.configured, HybridCandidates: 1},
				})
				wantMode := ModeLexical
				if semanticEnabled {
					wantMode = ModeSemantic
				}
				if err != nil || len(result.Docs) != tc.want || depth != tc.want || result.Mode != wantMode {
					t.Fatalf("result = %+v, %v, candidate depth = %d", result, err, depth)
				}
			}
		})
	}
}

func TestSearchRejectsInvalidRequestsAndMissingBackends(t *testing.T) {
	for _, req := range []Request{
		{Question: "gateway", Mode: "invalid"},
		{Question: "  ", Mode: ModeLexical},
		{Question: "gateway", Mode: ModeLexical},
		{Question: "gateway", Mode: ModeSemantic},
		{Question: "gateway", Mode: ModeHybrid},
	} {
		if _, err := New(nil, nil).Search(context.Background(), req); err == nil {
			t.Errorf("request %+v unexpectedly succeeded", req)
		}
	}
}
