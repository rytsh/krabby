// Package mcpclient manages outbound, administrator-configured MCP connections.
// It is separate from mcptools, which serves Krabby's own MCP catalogs.
package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"
	"golang.org/x/net/http/httpguts"
)

var (
	ErrInvalid  = errors.New("invalid MCP connection")
	ErrNotFound = errors.New("MCP connection not found")
	ErrExists   = errors.New("MCP connection already exists")
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
)

// Connection is persisted in the state DB. Credentials are write-only, not
// encrypted at rest; protect the Krabby data volume as for its other secrets.
type Connection struct {
	Name           string            `bw:"name,pk" json:"name"`
	Description    string            `bw:"description" json:"description"`
	URL            string            `bw:"url" json:"url"`
	TimeoutSeconds int               `bw:"timeout_seconds" json:"timeout_seconds"`
	Disabled       bool              `bw:"disabled" json:"disabled"`
	BearerToken    string            `bw:"bearer_token" json:"-"`
	Headers        map[string]string `bw:"headers" json:"-"`
	AllowedTools   []string          `bw:"allowed_tools" json:"allowed_tools"`
	// Exact resource URIs or URI templates, never implicit wildcard grants.
	AllowedResources []string  `bw:"allowed_resources" json:"allowed_resources"`
	UpdatedAt        time.Time `bw:"updated_at" json:"updated_at"`
}

// Config replaces non-secret settings. Blank token keeps the saved token;
// clear_bearer_token explicitly clears it. Omitted headers keep all headers,
// {} clears them, and blank values preserve the matching saved header value.
// Changing URL drops old credentials and grants before applying this config.
type Config struct {
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	URL              string            `json:"url"`
	TimeoutSeconds   int               `json:"timeout_seconds"`
	Disabled         bool              `json:"disabled"`
	BearerToken      string            `json:"bearer_token"`
	ClearBearerToken bool              `json:"clear_bearer_token"`
	Headers          map[string]string `json:"headers"`
	AllowedTools     []string          `json:"allowed_tools"`
	AllowedResources []string          `json:"allowed_resources"`
}

type View struct {
	Connection
	BearerTokenSet bool     `json:"bearer_token_set"`
	HeaderNames    []string `json:"header_names"`
}

func Redact(c *Connection) View {
	copy := *c
	copy.BearerToken, copy.Headers = "", nil
	copy.AllowedTools = slices.Clone(c.AllowedTools)
	copy.AllowedResources = slices.Clone(c.AllowedResources)
	if copy.AllowedTools == nil {
		copy.AllowedTools = []string{}
	}
	if copy.AllowedResources == nil {
		copy.AllowedResources = []string{}
	}
	names := slices.Sorted(maps.Keys(c.Headers))
	if names == nil {
		names = []string{}
	}
	return View{Connection: copy, BearerTokenSet: c.BearerToken != "", HeaderNames: names}
}

func merge(current *Connection, cfg Config) (*Connection, error) {
	c := &Connection{
		Name: strings.TrimSpace(cfg.Name), Description: strings.TrimSpace(cfg.Description),
		URL: strings.TrimSpace(cfg.URL), TimeoutSeconds: cfg.TimeoutSeconds, Disabled: cfg.Disabled,
		AllowedTools: cleanList(cfg.AllowedTools), AllowedResources: cleanList(cfg.AllowedResources),
	}
	if !namePattern.MatchString(c.Name) {
		return nil, fmt.Errorf("%w: name must be 1–128 lowercase letters, digits, dots, underscores or hyphens", ErrInvalid)
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" {
		return nil, fmt.Errorf("%w: URL must be an absolute HTTP(S) URL without credentials, query or fragment; use headers for authentication", ErrInvalid)
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 30
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 120 {
		return nil, fmt.Errorf("%w: timeout_seconds must be between 1 and 120", ErrInvalid)
	}
	if current != nil && current.URL == c.URL {
		c.BearerToken = current.BearerToken
		c.Headers = maps.Clone(current.Headers)
	}
	if current != nil && current.URL != c.URL {
		c.AllowedTools, c.AllowedResources = []string{}, []string{}
	}
	if cfg.ClearBearerToken {
		c.BearerToken = ""
	}
	if cfg.BearerToken != "" {
		c.BearerToken = cfg.BearerToken
	}
	if strings.ContainsAny(c.BearerToken, "\r\n") {
		return nil, fmt.Errorf("%w: invalid bearer token", ErrInvalid)
	}
	if cfg.Headers != nil {
		headers := make(map[string]string, len(cfg.Headers))
		for key, value := range cfg.Headers {
			if !httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(value) || len(value) > 8192 {
				return nil, fmt.Errorf("%w: invalid custom header", ErrInvalid)
			}
			key = http.CanonicalHeaderKey(key)
			switch strings.ToLower(key) {
			case "host", "content-type", "accept", "content-length", "connection", "transfer-encoding", "cookie", "proxy-authorization", "upgrade", "te", "trailer":
				return nil, fmt.Errorf("%w: protocol and cookie headers cannot be overridden", ErrInvalid)
			}
			if strings.HasPrefix(strings.ToLower(key), "mcp-") || strings.HasPrefix(strings.ToLower(key), "proxy-") {
				return nil, fmt.Errorf("%w: protocol headers cannot be overridden", ErrInvalid)
			}
			if _, exists := headers[key]; exists {
				return nil, fmt.Errorf("%w: duplicate custom header", ErrInvalid)
			}
			if value == "" {
				value = c.Headers[key]
			}
			if value == "" {
				return nil, fmt.Errorf("%w: a new custom header needs a value", ErrInvalid)
			}
			headers[key] = value
		}
		c.Headers = headers
	}
	if c.BearerToken != "" && c.Headers["Authorization"] != "" {
		return nil, fmt.Errorf("%w: use either bearer token or custom Authorization, not both", ErrInvalid)
	}
	if len(c.Headers) > 20 || len(c.BearerToken) > 8192 || len(c.AllowedTools) > 500 || len(c.AllowedResources) > 1000 || len(c.Description) > 4096 || len(c.URL) > 2048 {
		return nil, fmt.Errorf("%w: configuration exceeds size limits", ErrInvalid)
	}
	return c, nil
}

func cleanList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

type Store struct {
	bucket *bw.Bucket[Connection]
	mu     sync.Mutex // serialize read/merge/write and deletion
}

func New(db *bw.DB) (*Store, error) {
	bucket, err := bw.RegisterBucket[Connection](db, "external_mcps", bw.WithVersion[Connection](1))
	if err != nil {
		return nil, fmt.Errorf("register external MCP bucket; %w", err)
	}
	return &Store{bucket: bucket}, nil
}

func (s *Store) Get(ctx context.Context, name string) (*Connection, error) {
	c, err := s.bucket.Get(ctx, name)
	if errors.Is(err, bw.ErrNotFound) {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *Store) List(ctx context.Context) ([]View, error) {
	q, err := query.Parse("_limit=10000")
	if err != nil {
		return nil, err
	}
	items, err := s.bucket.Find(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(items))
	for _, c := range items {
		out = append(out, Redact(c))
	}
	slices.SortFunc(out, func(a, b View) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Preview merges a draft with a saved record without writing it or contacting
// the remote endpoint. An empty name means this is a new connection.
func (s *Store) Preview(ctx context.Context, name string, cfg Config) (*Connection, error) {
	var current *Connection
	if name != "" {
		var err error
		current, err = s.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		if cfg.Name != "" && cfg.Name != name {
			return nil, fmt.Errorf("%w: connection name cannot change", ErrInvalid)
		}
		cfg.Name = name
	}
	return merge(current, cfg)
}

func (s *Store) Save(ctx context.Context, name string, cfg Config) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.Preview(ctx, name, cfg)
	if err != nil {
		return View{}, err
	}
	if name == "" {
		if _, err := s.Get(ctx, c.Name); err == nil {
			return View{}, ErrExists
		} else if !errors.Is(err, ErrNotFound) {
			return View{}, err
		}
	}
	c.UpdatedAt = time.Now().UTC()
	if err := s.bucket.Insert(ctx, c); err != nil {
		return View{}, err
	}
	return Redact(c), nil
}

func (s *Store) Delete(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Get(ctx, name); err != nil {
		return err
	}
	return s.bucket.Delete(ctx, name)
}
