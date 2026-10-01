package manager

import (
	"context"
	"errors"

	"github.com/rytsh/krabby/internal/service/mcpclient"
)

// SetExternalMCPs is called during startup before HTTP requests are served.
func (m *Manager) SetExternalMCPs(store *mcpclient.Store) { m.externalMCPs = store }

func (m *Manager) ListExternalMCPs(ctx context.Context) ([]mcpclient.View, error) {
	if m.externalMCPs == nil {
		return nil, errors.New("external MCP store unavailable")
	}
	return m.externalMCPs.List(ctx)
}

func (m *Manager) SaveExternalMCP(ctx context.Context, name string, cfg mcpclient.Config) (mcpclient.View, error) {
	if m.externalMCPs == nil {
		return mcpclient.View{}, errors.New("external MCP store unavailable")
	}
	return m.externalMCPs.Save(ctx, name, cfg)
}

func (m *Manager) DeleteExternalMCP(ctx context.Context, name string) error {
	if m.externalMCPs == nil {
		return errors.New("external MCP store unavailable")
	}
	return m.externalMCPs.Delete(ctx, name)
}

func (m *Manager) DiscoverExternalMCP(ctx context.Context, name string, cfg mcpclient.Config) (mcpclient.Discovery, error) {
	if m.externalMCPs == nil {
		return mcpclient.Discovery{}, errors.New("external MCP store unavailable")
	}
	c, err := m.externalMCPs.Preview(ctx, name, cfg)
	if err != nil {
		return mcpclient.Discovery{}, err
	}
	return mcpclient.Discover(ctx, c), nil
}
