package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/observability/langfuse"
	"github.com/rytsh/krabby/internal/service/embedder"
	"github.com/rytsh/krabby/internal/service/llm"
	"github.com/rytsh/krabby/internal/service/settings"
)

// TestResult reports the outcome of a connectivity/credentials test.
type TestResult struct {
	OK        bool   `json:"ok"`
	Model     string `json:"model,omitempty"`
	Dim       int    `json:"dim,omitempty"` // embedder only
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// mergeSecrets fills blank secret fields in patch from the currently stored
// settings, so the UI can test un-saved changes without re-sending stored
// secrets (typed key wins; blank = use stored).
func (m *Manager) mergeSecrets(ctx context.Context, patch settings.Settings) (settings.Settings, error) {
	if m.settings == nil {
		return patch, nil
	}

	cur, err := m.settings.Get(ctx)
	if err != nil {
		return patch, err
	}

	if patch.LLMAPIKey == "" {
		patch.LLMAPIKey = cur.LLMAPIKey
	}

	if patch.EmbedAPIKey == "" {
		patch.EmbedAPIKey = cur.EmbedAPIKey
	}

	if patch.CodeEmbedAPIKey == "" {
		patch.CodeEmbedAPIKey = cur.CodeEmbedAPIKey
	}

	if patch.LangfuseSecretKey == "" {
		patch.LangfuseSecretKey = cur.LangfuseSecretKey
	}

	return patch, nil
}

// TestLangfuse validates the Langfuse host and project keys using the given
// (un-saved) settings. Blank secrets fall back to the stored value; nothing is
// persisted.
//
// It calls the public projects endpoint rather than the OTLP one: OTLP accepts
// a batch and answers 207 regardless of whether the credentials resolve to a
// project, so it cannot distinguish a working key from a typo. The projects
// endpoint authenticates with the same Basic credentials and answers 401 on a
// bad key, which is the question being asked.
func (m *Manager) TestLangfuse(ctx context.Context, patch settings.Settings) TestResult {
	s, err := m.mergeSecrets(ctx, patch)
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	cfg := langfuseConfig(s)

	host := strings.TrimRight(strings.TrimSpace(cfg.Host), "/")
	if host == "" || cfg.PublicKey == "" || cfg.SecretKey == "" {
		return TestResult{Error: "langfuse host, public key and secret key are required"}
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/api/public/projects", nil)
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	req.SetBasicAuth(cfg.PublicKey, cfg.SecretKey)

	start := time.Now()
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	res := TestResult{LatencyMS: time.Since(start).Milliseconds()}

	if err != nil {
		res.Error = err.Error()

		return res
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Error = fmt.Sprintf("langfuse http %d; %s", resp.StatusCode, strings.TrimSpace(string(body)))

		return res
	}

	// Report which project the keys resolve to: the most common misconfiguration
	// is a valid key pair pointed at the wrong project.
	//
	// A decode failure is fatal rather than cosmetic. Anything with a catch-all
	// route answers 200 to this path — krabby's own SPA fallback does — so
	// without checking the shape the test would pass against any web server
	// that happens to be running at the configured address.
	var projects struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}

	if jerr := json.Unmarshal(body, &projects); jerr != nil {
		res.Error = fmt.Sprintf("%s did not answer with a Langfuse project list; check the host", host)

		return res
	}

	if len(projects.Data) > 0 {
		res.Model = projects.Data[0].Name
	}

	// Valid keys do not imply a reachable OTLP endpoint. The two live on
	// different paths, and a Langfuse older than v3.22.0 serves the API while
	// answering 404 for OTLP — which would leave the test green while every
	// export was silently discarded.
	if err := probeOTLP(ctx, host, cfg, timeout); err != nil {
		res.Error = err.Error()

		return res
	}

	res.OK = true

	return res
}

// probeOTLP verifies that the traces endpoint exists and accepts the
// credentials.
//
// The body is empty on purpose: zero bytes is a valid, empty
// ExportTraceServiceRequest in protobuf, so the probe writes no trace and
// leaves no residue. Only the endpoint's existence is being asked about.
func probeOTLP(ctx context.Context, host string, cfg config.Langfuse, timeout time.Duration) error {
	endpoint := host + langfuse.TracesPath

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/x-protobuf")
	req.SetBasicAuth(cfg.PublicKey, cfg.SecretKey)

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("otlp endpoint %s unreachable; %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("otlp endpoint %s returned 404; the Langfuse instance is older than v3.22.0 or the host is wrong", endpoint)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("otlp endpoint %s rejected the project keys (http %d)", endpoint, resp.StatusCode)
	case resp.StatusCode >= 500:
		return fmt.Errorf("otlp endpoint %s returned http %d", endpoint, resp.StatusCode)
	}

	// A 2xx or a 4xx about the payload both prove the endpoint is there and
	// the credentials pass, which is all this probe can establish.
	return nil
}

// TestLLM validates the chat LLM using the given (un-saved) settings. Blank
// secrets fall back to the stored value. It never persists anything.
func (m *Manager) TestLLM(ctx context.Context, patch settings.Settings) TestResult {
	s, err := m.mergeSecrets(ctx, patch)
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	client, err := llm.New(llmConfig(s))
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	start := time.Now()
	err = client.Ping(ctx)
	res := TestResult{
		Model:     client.Model(),
		LatencyMS: time.Since(start).Milliseconds(),
	}

	if err != nil {
		res.Error = err.Error()

		return res
	}

	res.OK = true

	return res
}

// TestEmbedder validates the embeddings endpoint using the given (un-saved)
// settings. Blank secrets fall back to the stored value. It never persists.
func (m *Manager) TestEmbedder(ctx context.Context, patch settings.Settings) TestResult {
	return m.testEmbedderConnection(ctx, patch, embedderConfig)
}

// TestCodeEmbedder validates the code embeddings endpoint using the given
// (un-saved) settings. Blank secrets fall back to the stored value; a blank
// code embedder falls back to the docs embedder settings. It never persists.
func (m *Manager) TestCodeEmbedder(ctx context.Context, patch settings.Settings) TestResult {
	return m.testEmbedderConnection(ctx, patch, codeEmbedderConfig)
}

// Keep secret merging, probing and result reporting identical for both clients;
// only the settings adapter differs (including the code-to-docs fallback).
func (m *Manager) testEmbedderConnection(ctx context.Context, patch settings.Settings, configFor func(settings.Settings) config.Embedder) TestResult {
	s, err := m.mergeSecrets(ctx, patch)
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	client, err := embedder.New(configFor(s))
	if err != nil {
		return TestResult{Error: err.Error()}
	}

	start := time.Now()
	err = client.Ping(ctx)
	res := TestResult{
		Model:     client.Model(),
		LatencyMS: time.Since(start).Milliseconds(),
	}

	if err != nil {
		res.Error = err.Error()

		return res
	}

	res.OK = true
	res.Dim = client.Dim()

	return res
}
