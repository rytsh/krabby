// Package openapi implements the "openapi" API-catalog provider: it fetches an
// OpenAPI 2.0/3.x document over HTTP, applies the service's overrides and emits
// one catalog operation per endpoint.
//
// Both specification versions are supported through libopenapi's two high-level
// models. The version-specific code is only the plumbing — where a request body
// hangs, how content types are declared, how security schemes are spelled — and
// it is deliberately kept thin; the expensive part, flattening a schema graph
// into a bounded tree, is shared (see schema.go).
//
// Auth for the document itself is basic or bearer, taken from the provider
// config. It is separate from whatever the *described* API requires: fetching a
// spec from an authenticated docs server and calling the API it describes are
// different acts with different credentials.
package openapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pb33f/libopenapi"
	"github.com/worldline-go/types"

	"github.com/rytsh/krabby/internal/nullx"
	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// maxDocumentBytes bounds a fetched specification. Real documents run to a few
// megabytes at the extreme; anything past this limit is a misconfigured URL
// pointing at something that is not a spec.
const maxDocumentBytes = 32 << 20

// fetchTimeout bounds one document fetch. Parsing is not covered by it — a
// large spec takes real CPU time and cancelling halfway wastes the download.
const fetchTimeout = 60 * time.Second

// Provider fetches and parses one OpenAPI document per service.
type Provider struct {
	client *http.Client
}

// New creates the provider.
func New() *Provider {
	return &Provider{client: &http.Client{Timeout: fetchTimeout}}
}

// Config is owned entirely by this provider. Every field is a types.Null so a
// partial update merges precisely onto the stored config: absent = keep,
// null = clear, value = override.
type Config struct {
	// URL is where the document is served.
	URL types.Null[string] `json:"url"`

	// User and Token authenticate the fetch of the document itself. A non-empty
	// user selects basic auth; a bare token is sent as a bearer.
	User  types.Null[string] `json:"user,omitempty"`
	Token types.Null[string] `json:"token,omitempty"`

	// Headers are extra request headers for the fetch, e.g. a gateway key.
	Headers types.Null[map[string]string] `json:"headers,omitempty"`

	// InsecureSkipVerify disables TLS verification for the fetch. Internal
	// documentation servers routinely carry a private CA that the krabby host
	// does not trust, and refusing to fetch is not more secure than fetching
	// with a flag the operator set knowingly.
	InsecureSkipVerify types.Null[bool] `json:"insecure_skip_verify,omitempty"`
}

// resolvedConfig is the plain, validated view used by the fetch logic.
type resolvedConfig struct {
	URL                string
	User               string
	Token              string
	Headers            map[string]string
	InsecureSkipVerify bool
}

func (c Config) resolve() resolvedConfig {
	return resolvedConfig{
		URL:  strings.TrimSpace(c.URL.ValueOrZero()),
		User: strings.TrimSpace(c.User.ValueOrZero()),
		// Never trimmed: trimming a secret silently corrupts tokens with
		// meaningful whitespace and turns " " into "", which MergeConfig reads
		// as "keep the stored one".
		Token:              c.Token.ValueOrZero(),
		Headers:            c.Headers.ValueOrZero(),
		InsecureSkipVerify: c.InsecureSkipVerify.ValueOrZero(),
	}
}

// configView is the redacted config the REST API and UI see.
type configView struct {
	URL                string            `json:"url"`
	User               string            `json:"user,omitempty"`
	TokenSet           bool              `json:"token_set"`
	Headers            map[string]string `json:"headers,omitempty"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify,omitempty"`
}

func decodeConfig(raw json.RawMessage) (resolvedConfig, error) {
	cfg, err := decodeRawConfig(raw)
	if err != nil {
		return resolvedConfig{}, err
	}

	return cfg.resolve(), nil
}

// decodeRawConfig keeps the nullable fields so MergeConfig can tell set fields
// from absent ones.
func decodeRawConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode openapi config; %w", err)
		}
	}

	return cfg, nil
}

// ---- apicatalog.Provider ----------------------------------------------------

func (p *Provider) Validate(raw json.RawMessage) error {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return err
	}

	if cfg.URL == "" {
		return errors.New("openapi url is required")
	}
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return errors.New("openapi url must be an http(s) URL")
	}

	for name := range cfg.Headers {
		if strings.TrimSpace(name) == "" {
			return errors.New("openapi headers has an entry with an empty name")
		}
	}

	return nil
}

// MergeConfig merges an update onto the stored config so a partial update does
// not wipe connection settings. A blank token keeps the stored secret, since
// tokens are write-only and the redacted view never returns one.
func (p *Provider) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	next, err := decodeRawConfig(update)
	if err != nil {
		return nil, err
	}

	if len(current) != 0 {
		prev, err := decodeRawConfig(current)
		if err != nil {
			return nil, err
		}

		next.URL = nullx.Merge(next.URL, prev.URL)
		next.User = nullx.Merge(next.User, prev.User)
		next.Headers = nullx.Merge(next.Headers, prev.Headers)
		next.InsecureSkipVerify = nullx.Merge(next.InsecureSkipVerify, prev.InsecureSkipVerify)

		// Tokens are write-only: an absent or blank incoming token keeps the
		// stored one; only a non-empty value replaces it.
		if next.Token.ValueOrZero() == "" {
			next.Token = prev.Token
		}
	}

	raw, err := json.Marshal(next)
	if err != nil {
		return nil, fmt.Errorf("encode openapi config; %w", err)
	}
	if err := p.Validate(raw); err != nil {
		return nil, err
	}

	return raw, nil
}

func (p *Provider) ConfigView(raw json.RawMessage) any {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return nil
	}

	return configView{
		URL:                cfg.URL,
		User:               cfg.User,
		TokenSet:           cfg.Token != "",
		Headers:            cfg.Headers,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}
}

// syncState is the opaque watermark persisted between syncs.
type syncState struct {
	// ETag and LastModified are the document's HTTP validators. When the server
	// honours them a refresh costs one 304 instead of a re-parse.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	// Fingerprint is a hash of the fetched bytes, the fallback for servers that
	// send no validators — which is most internal ones.
	Fingerprint string `json:"fingerprint,omitempty"`
	// V is the render generation of the operations this state describes. A
	// stored state older than renderGeneration forces a re-render even when the
	// document itself has not moved, so a change to how krabby renders an
	// endpoint reaches existing catalogs.
	V int `json:"v,omitempty"`
}

// renderGeneration is bumped whenever the rendered markdown or Detail shape
// changes, invalidating every stored operation.
const renderGeneration = 1

// Fetch downloads the document, applies the service's overrides and emits one
// operation per endpoint.
func (p *Provider) Fetch(ctx context.Context, svc *apicatalog.Service, rawState json.RawMessage, emit apicatalog.Emit) (*apicatalog.FetchResult, error) {
	cfg, err := decodeConfig(svc.Config)
	if err != nil {
		return nil, err
	}
	if cfg.URL == "" {
		return nil, errors.New("openapi url is required")
	}

	// A corrupt or legacy state must degrade to a full sync, never fail it.
	var state syncState
	if len(rawState) != 0 {
		_ = json.Unmarshal(rawState, &state)
	}

	fresh := state.V < renderGeneration

	doc, meta, err := p.get(ctx, cfg, state, fresh)
	if err != nil {
		return nil, err
	}

	if meta.notModified {
		return &apicatalog.FetchResult{Unchanged: true, State: rawState}, nil
	}

	fingerprint := apicatalog.Hash(string(doc))
	if !fresh && fingerprint == state.Fingerprint {
		// The server sent no validators but the bytes are identical. Skipping
		// here is what keeps a poll-every-hour service from re-embedding a
		// static document 24 times a day.
		next, err := json.Marshal(syncState{
			ETag: meta.etag, LastModified: meta.lastModified,
			Fingerprint: fingerprint, V: renderGeneration,
		})
		if err != nil {
			return nil, fmt.Errorf("encode openapi sync state; %w", err)
		}

		return &apicatalog.FetchResult{Unchanged: true, State: next}, nil
	}

	patched, err := apicatalog.ApplyMergePatch(doc, svc.SpecPatch)
	if err != nil {
		return nil, err
	}

	info, err := p.walk(svc, patched, emit)
	if err != nil {
		return nil, err
	}

	next, err := json.Marshal(syncState{
		ETag: meta.etag, LastModified: meta.lastModified,
		Fingerprint: fingerprint, V: renderGeneration,
	})
	if err != nil {
		return nil, fmt.Errorf("encode openapi sync state; %w", err)
	}

	return &apicatalog.FetchResult{Complete: true, Info: info, State: next}, nil
}

// Preview validates unsaved config by fetching and parsing the document without
// persisting or indexing anything.
func (p *Provider) Preview(ctx context.Context, raw json.RawMessage, patch json.RawMessage) (apicatalog.PreviewResult, error) {
	var out apicatalog.PreviewResult

	if err := p.Validate(raw); err != nil {
		return out, err
	}
	cfg, err := decodeConfig(raw)
	if err != nil {
		return out, err
	}

	doc, _, err := p.get(ctx, cfg, syncState{}, true)
	if err != nil {
		return out, err
	}

	patched, err := apicatalog.ApplyMergePatch(doc, patch)
	if err != nil {
		return out, err
	}

	svc := &apicatalog.Service{Name: "preview"}
	var sample []string
	info, err := p.walk(svc, patched, func(op apicatalog.RemoteOperation) error {
		out.OperationCount++
		if len(sample) < 10 {
			sample = append(sample, op.Method+" "+op.Path)
		}

		return nil
	})
	if err != nil {
		return out, err
	}

	out.Title = info.Title
	out.Version = info.Version
	out.Summary = info.Summary
	out.BaseURL = info.ResolvedURL
	out.Sample = sample

	return out, nil
}

// walk dispatches to the version-specific walker.
func (p *Provider) walk(svc *apicatalog.Service, doc json.RawMessage, emit apicatalog.Emit) (apicatalog.ServiceInfo, error) {
	document, err := libopenapi.NewDocument(doc)
	if err != nil {
		return apicatalog.ServiceInfo{}, fmt.Errorf("parse api document; %w", err)
	}
	defer document.Release()

	version := document.GetVersion()
	if strings.HasPrefix(version, "2") {
		model, err := document.BuildV2Model()
		if err != nil {
			return apicatalog.ServiceInfo{}, fmt.Errorf("build swagger 2 model; %w", err)
		}

		return walkV2(svc, &model.Model, emit)
	}

	model, err := document.BuildV3Model()
	if err != nil {
		// A spec with unresolvable remote $refs still builds a usable model;
		// libopenapi reports the failures but returns the document. Refusing
		// the whole catalog because one component could not be reached would
		// make the feature unusable against real internal specs.
		if model == nil {
			return apicatalog.ServiceInfo{}, fmt.Errorf("build openapi model; %w", err)
		}
	}

	return walkV3(svc, &model.Model, emit)
}

// fetchMeta carries the HTTP validators of one document fetch.
type fetchMeta struct {
	etag         string
	lastModified string
	notModified  bool
}

// get downloads the document, sending conditional-request validators unless
// force is set.
func (p *Provider) get(ctx context.Context, cfg resolvedConfig, state syncState, force bool) ([]byte, fetchMeta, error) {
	var meta fetchMeta

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		return nil, meta, fmt.Errorf("build request; %w", err)
	}
	req.Header.Set("Accept", "application/json, application/yaml, text/yaml, */*")

	for name, value := range cfg.Headers {
		req.Header.Set(name, value)
	}

	switch {
	case cfg.User != "":
		req.SetBasicAuth(cfg.User, cfg.Token)
	case cfg.Token != "":
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	if !force {
		if state.ETag != "" {
			req.Header.Set("If-None-Match", state.ETag)
		}
		if state.LastModified != "" {
			req.Header.Set("If-Modified-Since", state.LastModified)
		}
	}

	res, err := p.clientFor(cfg).Do(req)
	if err != nil {
		return nil, meta, fmt.Errorf("fetch api document; %w", err)
	}
	defer res.Body.Close()

	meta.etag = res.Header.Get("ETag")
	meta.lastModified = res.Header.Get("Last-Modified")

	if res.StatusCode == http.StatusNotModified {
		meta.notModified = true
		// Preserve the stored validators: a 304 carries no body and often no
		// ETag, and dropping them would make every subsequent poll a full GET.
		if meta.etag == "" {
			meta.etag = state.ETag
		}
		if meta.lastModified == "" {
			meta.lastModified = state.LastModified
		}

		return nil, meta, nil
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxDocumentBytes))
	if err != nil {
		return nil, meta, fmt.Errorf("read api document; %w", err)
	}

	if res.StatusCode != http.StatusOK {
		return nil, meta, fmt.Errorf("fetch api document: status %s: %s", res.Status, truncate(string(body), 300))
	}
	if len(body) == 0 {
		return nil, meta, errors.New("api document is empty")
	}

	return body, meta, nil
}

// clientFor returns the HTTP client for a config, building a TLS-relaxed one
// only when the config asks for it so the default path keeps verification.
func (p *Provider) clientFor(cfg resolvedConfig) *http.Client {
	if !cfg.InsecureSkipVerify {
		return p.client
	}

	transport, _ := http.DefaultTransport.(*http.Transport)
	if transport == nil {
		return p.client
	}
	clone := transport.Clone()
	if clone.TLSClientConfig == nil {
		clone.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		clone.TLSClientConfig = clone.TLSClientConfig.Clone()
	}
	clone.TLSClientConfig.InsecureSkipVerify = true

	return &http.Client{Timeout: fetchTimeout, Transport: clone}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "…"
}
