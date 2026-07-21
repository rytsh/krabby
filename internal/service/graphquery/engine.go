package graphquery

import (
	"fmt"
	"os"
	"sync"
)

// Engine loads graphs on demand and caches them per graph path, reloading a graph
// only when its file mtime or size changes. This replaces the per-graph
// `python -m graphify.serve` process pool: queries are answered in-process.
type Engine struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	graph   *Graph
	mtimeNS int64
	size    int64
}

// NewEngine creates an empty engine.
func NewEngine() *Engine {
	return &Engine{entries: map[string]*cacheEntry{}}
}

// Graph returns the parsed graph for path, loading or reloading it if the file
// changed since it was last cached (mirrors serve.py's mtime+size hot-reload).
func (e *Engine) Graph(path string) (*Graph, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat graph %s; %w", path, err)
	}

	mtimeNS, size := fi.ModTime().UnixNano(), fi.Size()

	e.mu.RLock()
	if ent, ok := e.entries[path]; ok && ent.mtimeNS == mtimeNS && ent.size == size {
		g := ent.graph
		e.mu.RUnlock()

		return g, nil
	}
	e.mu.RUnlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	// Re-check under the write lock in case another goroutine reloaded.
	if ent, ok := e.entries[path]; ok && ent.mtimeNS == mtimeNS && ent.size == size {
		return ent.graph, nil
	}

	g, err := Load(path)
	if err != nil {
		// On transient read error keep serving a previously cached graph if any.
		if ent, ok := e.entries[path]; ok {
			return ent.graph, nil
		}

		return nil, err
	}

	e.entries[path] = &cacheEntry{graph: g, mtimeNS: mtimeNS, size: size}

	return g, nil
}

// Invalidate drops the cached graph for path (e.g. when a repo is removed).
func (e *Engine) Invalidate(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.entries, path)
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
		return g.GetNeighbors(argStr(args, "label"), argStr(args, "relation_filter")), nil
	case "get_community":
		return g.GetCommunity(argInt(args, "community_id")), nil
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
