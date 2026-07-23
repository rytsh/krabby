package graphquery

import (
	"container/list"
	"fmt"
	"os"
	"sync"
)

// parsedSizeFactor estimates how much larger a parsed Graph (maps, adjacency
// lists, per-node/edge structs, idf cache) is than the raw graph.json bytes it
// came from. The parsed representation carries pointers, map overhead and both
// in/out adjacency copies, so it is materially bigger than the file. 3x is a
// deliberately conservative estimate used only to bound the LRU budget; exact
// accounting is unnecessary for eviction to protect RSS.
const parsedSizeFactor = 3

// Engine loads graphs on demand and caches them per graph path, reloading a graph
// only when its file mtime or size changes. This replaces the per-graph
// `python -m graphify.serve` process pool: queries are answered in-process.
//
// The cache is bounded by an estimated-memory budget (maxBytes): once the sum of
// cached graphs' estimated sizes exceeds it, the least-recently-used graphs are
// evicted. Evicted graphs are transparently reloaded on the next query, so
// correctness is unaffected — only cold-query latency. A budget of 0 disables
// eviction (unbounded, retaining the original behaviour).
type Engine struct {
	mu       sync.Mutex
	entries  map[string]*list.Element // path -> element in lru
	lru      *list.List               // front = most-recently-used
	curBytes int64
	maxBytes int64
}

type cacheEntry struct {
	path    string
	graph   *Graph
	mtimeNS int64
	size    int64 // source graph.json byte size
	est     int64 // estimated in-memory bytes (size * parsedSizeFactor)
}

// NewEngine creates an empty engine with the given estimated-memory budget in
// bytes. maxBytes <= 0 disables eviction (unbounded cache).
func NewEngine(maxBytes int64) *Engine {
	return &Engine{
		entries:  map[string]*list.Element{},
		lru:      list.New(),
		maxBytes: maxBytes,
	}
}

// Graph returns the parsed graph for path, loading or reloading it if the file
// changed since it was last cached (mirrors serve.py's mtime+size hot-reload).
// A cache hit promotes the entry to most-recently-used; a miss loads the graph
// and evicts least-recently-used entries until the memory budget is satisfied.
func (e *Engine) Graph(path string) (*Graph, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat graph %s; %w", path, err)
	}

	mtimeNS, size := fi.ModTime().UnixNano(), fi.Size()

	e.mu.Lock()
	defer e.mu.Unlock()

	if el, ok := e.entries[path]; ok {
		ent := el.Value.(*cacheEntry)
		if ent.mtimeNS == mtimeNS && ent.size == size {
			e.lru.MoveToFront(el)

			return ent.graph, nil
		}
		// Stale: drop it before reloading so curBytes stays accurate.
		e.removeElement(el)
	}

	g, err := Load(path)
	if err != nil {
		// On transient read error keep serving a previously cached graph if any.
		if el, ok := e.entries[path]; ok {
			return el.Value.(*cacheEntry).graph, nil
		}

		return nil, err
	}

	ent := &cacheEntry{
		path:    path,
		graph:   g,
		mtimeNS: mtimeNS,
		size:    size,
		est:     size * parsedSizeFactor,
	}
	el := e.lru.PushFront(ent)
	e.entries[path] = el
	e.curBytes += ent.est

	e.evictLocked()

	return g, nil
}

// evictLocked drops least-recently-used entries until the cache fits the budget.
// The most-recently-used entry (front) is never evicted even if it alone exceeds
// the budget, so a single oversized graph is still served rather than thrashing.
// Callers must hold e.mu.
func (e *Engine) evictLocked() {
	if e.maxBytes <= 0 {
		return
	}

	for e.curBytes > e.maxBytes && e.lru.Len() > 1 {
		e.removeElement(e.lru.Back())
	}
}

// removeElement unlinks a cache element and updates the byte accounting.
// Callers must hold e.mu.
func (e *Engine) removeElement(el *list.Element) {
	ent := el.Value.(*cacheEntry)
	e.lru.Remove(el)
	delete(e.entries, ent.path)
	e.curBytes -= ent.est
}

// Invalidate drops the cached graph for path (e.g. when a repo is removed).
func (e *Engine) Invalidate(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if el, ok := e.entries[path]; ok {
		e.removeElement(el)
	}
}

// Call dispatches a tool by name against the graph at path, returning the tool's
// text output. This mirrors the MCP tool surface previously proxied to python.
func (e *Engine) Call(path, tool string, args map[string]any) (string, error) {
	g, err := e.Graph(path)
	if err != nil {
		return "", err
	}

	switch tool {
	case "query_graph":
		return g.QueryGraph(argStr(args, "question"), QueryGraphOpts{
			Mode:          argStr(args, "mode"),
			Depth:         argInt(args, "depth"),
			TokenBudget:   argInt(args, "token_budget"),
			ContextFilter: argStrSlice(args, "context_filter"),
		}), nil
	case "get_node":
		return g.GetNode(argStr(args, "label")), nil
	case "get_neighbors":
		return g.GetNeighborsPage(argStr(args, "label"), argStr(args, "relation_filter"), argInt(args, "page"), argInt(args, "per_page")), nil
	case "get_community":
		return g.GetCommunityPage(argInt(args, "community_id"), argInt(args, "page"), argInt(args, "per_page")), nil
	case "god_nodes":
		return g.GodNodes(argInt(args, "top_n")), nil
	case "graph_stats":
		return g.GraphStats(), nil
	case "shortest_path":
		return g.ShortestPath(argStr(args, "source"), argStr(args, "target"), argInt(args, "max_hops")), nil
	default:
		return "", fmt.Errorf("unknown graph tool: %s", tool)
	}
}

func argStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}

	return ""
}

func argInt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

func argStrSlice(m map[string]any, k string) []string {
	switch v := m[k].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}

		return out
	default:
		return nil
	}
}
