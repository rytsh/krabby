// Package llm is an OpenAI-compatible chat-completions client used by the doc
// generator and the config test endpoints.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/config"
)

// ErrNotConfigured is returned when the LLM has no base URL configured.
var ErrNotConfigured = errors.New("llm not configured (set llm.base_url)")

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// Client talks to an OpenAI-compatible /chat/completions endpoint.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// New builds a chat client from config. Returns ErrNotConfigured when no base
// URL is set so callers can disable doc generation gracefully.
func New(cfg config.LLM) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, ErrNotConfigured
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	return &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// Model returns the configured chat model name.
func (c *Client) Model() string { return c.model }

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// Complete sends messages and returns the assistant's reply text.
func (c *Client) Complete(ctx context.Context, messages []Message) (string, error) {
	return c.complete(ctx, messages, 0)
}

func (c *Client) complete(ctx context.Context, messages []Message, maxTokens int) (string, error) {
	body, err := json.Marshal(chatRequest{Model: c.model, Messages: messages, MaxTokens: maxTokens})
	if err != nil {
		return "", fmt.Errorf("marshal chat request; %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("new chat request; %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat request; %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("chat http %d; %s", resp.StatusCode, apiErrMsg(raw))
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode chat response; %w", err)
	}

	if out.Error != nil {
		return "", fmt.Errorf("chat api error; %s", out.Error.Message)
	}

	if len(out.Choices) == 0 {
		return "", errors.New("chat response had no choices")
	}

	return out.Choices[0].Message.Content, nil
}

// Ping performs a minimal completion to validate the endpoint, credentials and
// model. It returns the model that answered (echoed from config) and any error.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.complete(ctx, []Message{{Role: "user", Content: "ping"}}, 1)

	return err
}

// apiErrMsg extracts a human-readable error message from an OpenAI-style error
// body, falling back to the raw payload.
func apiErrMsg(raw []byte) string {
	var out struct {
		Error *apiError `json:"error"`
	}

	if err := json.Unmarshal(raw, &out); err == nil && out.Error != nil && out.Error.Message != "" {
		return out.Error.Message
	}

	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "..."
	}

	return s
}
