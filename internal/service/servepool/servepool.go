// Package servepool manages long-lived `python -m graphify.serve` MCP servers,
// one per graph.json, spawned lazily and killed when idle. The python server
// hot-reloads graph.json on mtime change, so entries survive graph rebuilds.
package servepool

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	readyTimeout  = 30 * time.Second
	readyInterval = 300 * time.Millisecond
	callTimeout   = 60 * time.Second
)

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
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	session  *mcp.ClientSession
	port     int
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
		p.invalidate(graphPath)

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
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("find free port; %w", err)
	}

	procCtx, cancel := context.WithCancel(p.baseCtx)

	cmd := exec.CommandContext(procCtx, p.python,
		"-m", "graphify.serve",
		"--transport", "http",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--stateless",
		graphPath,
	)
	cmd.Cancel = func() error { return cmd.Process.Kill() }

	if err := cmd.Start(); err != nil {
		cancel()

		return nil, fmt.Errorf("start graphify serve; %w", err)
	}

	slog.Info("spawned graphify mcp server", "graph", graphPath, "port", port, "pid", cmd.Process.Pid)

	go func() { _ = cmd.Wait() }()

	session, err := p.connect(ctx, port)
	if err != nil {
		cancel()

		return nil, fmt.Errorf("connect graphify serve for %s; %w", graphPath, err)
	}

	return &entry{
		cmd:      cmd,
		cancel:   cancel,
		session:  session,
		port:     port,
		lastUsed: time.Now(),
	}, nil
}

func (p *Pool) connect(ctx context.Context, port int) (*mcp.ClientSession, error) {
	deadline := time.Now().Add(readyTimeout)

	var lastErr error

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		client := mcp.NewClient(&mcp.Implementation{Name: "krabby", Version: p.version}, nil)
		transport := &mcp.StreamableClientTransport{
			Endpoint:             fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			DisableStandaloneSSE: true,
		}

		session, err := client.Connect(ctx, transport, nil)
		if err == nil {
			return session, nil
		}

		lastErr = err

		time.Sleep(readyInterval)
	}

	return nil, fmt.Errorf("server not ready; %w", lastErr)
}

func (p *Pool) invalidate(graphPath string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.entries[graphPath]; ok {
		p.stop(graphPath, e)
	}
}

// Invalidate kills the server for graphPath (e.g. when a repo is removed).
func (p *Pool) Invalidate(graphPath string) { p.invalidate(graphPath) }

func (p *Pool) stop(key string, e *entry) {
	_ = e.session.Close()
	e.cancel()
	delete(p.entries, key)

	slog.Info("stopped graphify mcp server", "graph", key, "port", e.port)
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

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port, nil //nolint:forcetypeassert
}
