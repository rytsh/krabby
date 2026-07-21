// Package servepool manages long-lived `python -m graphify.serve` MCP servers,
// one per graph.json, spawned lazily and killed when idle. The python server
// hot-reloads graph.json on mtime change, so entries survive graph rebuilds.
//
// graphify.serve speaks MCP over stdio and takes the graph path as its sole
// positional argument, so each server is a child process we talk to over
// stdin/stdout via mcp.CommandTransport.
package servepool

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const callTimeout = 60 * time.Second

// Pool spawns and reuses graphify MCP servers keyed by graph path.
type Pool struct {
	python      string
	idleTimeout time.Duration
	version     string

	mu      sync.Mutex
	entries map[string]*entry

	baseCtx context.Context //nolint:containedctx // background lifecycle for spawned processes
}

type entry struct {
	cancel   context.CancelFunc
	session  *mcp.ClientSession
	lastUsed time.Time
}

// New creates a pool. baseCtx bounds the lifetime of all spawned processes.
func New(baseCtx context.Context, python, version string, idleTimeout time.Duration) *Pool {
	p := &Pool{
		python:      python,
		idleTimeout: idleTimeout,
		version:     version,
		entries:     map[string]*entry{},
		baseCtx:     baseCtx,
	}

	go p.janitor(baseCtx)

	return p
}

// CallTool proxies a tool call to the MCP server that serves graphPath.
func (p *Pool) CallTool(ctx context.Context, graphPath, name string, args map[string]any) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	session, err := p.session(ctx, graphPath)
	if err != nil {
		return nil, err
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		// Session may be stale (process died); respawn once and retry.
		p.invalidateStale(graphPath, session)

		session, serr := p.session(ctx, graphPath)
		if serr != nil {
			return nil, fmt.Errorf("call tool %s; %w", name, err)
		}

		res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return nil, fmt.Errorf("call tool %s; %w", name, err)
		}
	}

	return res, nil
}

func (p *Pool) session(ctx context.Context, graphPath string) (*mcp.ClientSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.entries[graphPath]; ok {
		e.lastUsed = time.Now()

		return e.session, nil
	}

	e, err := p.spawn(ctx, graphPath)
	if err != nil {
		return nil, err
	}

	p.entries[graphPath] = e

	return e.session, nil
}

func (p *Pool) spawn(ctx context.Context, graphPath string) (*entry, error) {
	// baseCtx bounds the process lifetime; cancel kills it via CommandContext.
	procCtx, cancel := context.WithCancel(p.baseCtx)

	// graphify.serve speaks MCP over stdio and reads the graph path from
	// argv[1] only; extra flags are rejected, so pass just the path.
	cmd := exec.CommandContext(procCtx, p.python, "-m", "graphify.serve", graphPath)

	client := mcp.NewClient(&mcp.Implementation{Name: "krabby", Version: p.version}, nil)
	transport := &mcp.CommandTransport{Command: cmd}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		cancel()

		return nil, fmt.Errorf("connect graphify serve for %s; %w", graphPath, err)
	}

	slog.Info("spawned graphify mcp server", "graph", graphPath, "pid", cmd.Process.Pid)

	return &entry{
		cancel:   cancel,
		session:  session,
		lastUsed: time.Now(),
	}, nil
}

func (p *Pool) invalidate(graphPath string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.entries[graphPath]; ok {
		p.stop(graphPath, e)
	}
}

// invalidateStale kills the entry only if it still holds the given session,
// preventing concurrent retries from killing each other's freshly spawned replacements.
func (p *Pool) invalidateStale(graphPath string, stale *mcp.ClientSession) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.entries[graphPath]; ok && e.session == stale {
		p.stop(graphPath, e)
	}
}

// Invalidate kills the server for graphPath (e.g. when a repo is removed).
func (p *Pool) Invalidate(graphPath string) { p.invalidate(graphPath) }

func (p *Pool) stop(key string, e *entry) {
	_ = e.session.Close()
	e.cancel()
	delete(p.entries, key)

	slog.Info("stopped graphify mcp server", "graph", key)
}

// StopAll terminates every spawned server.
func (p *Pool) StopAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for key, e := range p.entries {
		p.stop(key, e)
	}
}

func (p *Pool) janitor(ctx context.Context) {
	if p.idleTimeout <= 0 {
		return
	}

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.mu.Lock()

			for key, e := range p.entries {
				if time.Since(e.lastUsed) > p.idleTimeout {
					p.stop(key, e)
				}
			}

			p.mu.Unlock()
		}
	}
}
