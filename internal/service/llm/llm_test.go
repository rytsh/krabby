package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rytsh/krabby/internal/config"
)

func TestNewNotConfigured(t *testing.T) {
	if _, err := New(config.LLM{}); err == nil {
		t.Fatal("expected ErrNotConfigured for empty base url")
	}
}

func TestCompleteAndPing(t *testing.T) {
	var gotAuth, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path

		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message Message `json:"message"`
			}{{Message: Message{Role: "assistant", Content: "pong"}}},
		})
	}))
	defer srv.Close()

	c, err := New(config.LLM{BaseURL: srv.URL, APIKey: "secret", Model: "test-model"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if out != "pong" {
		t.Fatalf("got %q want pong", out)
	}

	if gotAuth != "Bearer secret" {
		t.Fatalf("auth header = %q", gotAuth)
	}

	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestCompleteHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	c, _ := New(config.LLM{BaseURL: srv.URL, Model: "m"})

	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestCompleteStreamingConcatenatesDeltas(t *testing.T) {
	var gotAccept string
	var gotStream bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")

		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotStream = req.Stream

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		lines := []string{
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":", "}}]}`,
			`data: {"choices":[{"delta":{"content":"world"}}]}`,
			`data: [DONE]`,
		}
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()

	c, err := New(config.LLM{BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "Hello, world" {
		t.Fatalf("streamed content = %q, want %q", out, "Hello, world")
	}
	if !gotStream {
		t.Error("request should set stream=true")
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept header = %q, want text/event-stream", gotAccept)
	}
}

func TestCompleteStreamingAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"error":{"message":"boom"}}` + "\n\n"))
	}))
	defer srv.Close()

	c, _ := New(config.LLM{BaseURL: srv.URL, Model: "m"})
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("expected error from streamed api error payload")
	}
}
