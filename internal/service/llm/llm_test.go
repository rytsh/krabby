package llm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/config"
)

func TestNewNotConfigured(t *testing.T) {
	if _, err := New(config.LLM{}); err == nil {
		t.Fatal("expected ErrNotConfigured for empty base url")
	}
}

func TestMarshalRequestJSON(t *testing.T) {
	c, err := New(config.LLM{BaseURL: "https://llm.example", Model: "test-model"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name     string
		messages []Message
		want     string
	}{
		{
			name:     "text content remains a string",
			messages: []Message{{Role: "user", Content: "hello"}},
			want:     `{"model":"test-model","messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "multimodal content is an array",
			messages: []Message{{Role: "user", Parts: []ContentPart{
				TextPart("describe this"),
				ImageURLPart("data:image/png;base64,aW1hZ2U=", "high"),
			}}},
			want: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2U=","detail":"high"}}]}],"stream":true,"stream_options":{"include_usage":true}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.marshalRequest(tt.messages, 0)
			if err != nil {
				t.Fatalf("marshalRequest: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("request JSON:\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestTraceMessagesRedactImageURLs(t *testing.T) {
	image := []byte("private image bytes")
	encoded := base64.StdEncoding.EncodeToString(image)
	remoteSecret := "remote-query-secret"
	messages := []Message{{Role: "user", Parts: []ContentPart{
		TextPart("compare these images"),
		ImageURLPart("data:image/png;base64,"+encoded, "high"),
		{
			Type:     "image_url",
			Text:     encoded, // Invalid extra data must not bypass image redaction.
			ImageURL: &ImageURL{URL: "https://cdn.example/image.jpg?token=" + remoteSecret + "&signature=private", Detail: "low"},
		},
	}}}

	got, err := json.Marshal(traceMessages(messages))
	if err != nil {
		t.Fatalf("marshal trace input: %v", err)
	}
	traceInput := string(got)
	for _, secret := range []string{encoded, remoteSecret, "signature=private", "cdn.example"} {
		if strings.Contains(traceInput, secret) {
			t.Errorf("trace input contains sensitive image URL data %q: %s", secret, traceInput)
		}
	}

	sum := sha256.Sum256(image)
	wantMetadata := fmt.Sprintf(`"source":"data_url","detail":"high","mime":"image/png","bytes":%d,"sha256":"%x"`, len(image), sum)
	if !strings.Contains(traceInput, wantMetadata) {
		t.Errorf("trace input lacks safe data URL metadata %s: %s", wantMetadata, traceInput)
	}
	if !strings.Contains(traceInput, `"source":"remote_url","detail":"low"`) {
		t.Errorf("trace input lacks redacted remote URL metadata: %s", traceInput)
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

// TestStreamUsageCapture checks that token accounting and time-to-first-token
// survive the SSE parser. Both are what makes an exported trace worth anything.
func TestStreamUsageCapture(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "text/event-stream")
		// A role-only preamble, then content, then the usage-only final chunk
		// exactly as OpenAI emits it under stream_options.include_usage.
		_, _ = io.WriteString(w, "data: {\"model\":\"served-model\",\"choices\":[{\"delta\":{}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3,\"total_tokens\":14}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c, err := New(config.LLM{BaseURL: srv.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	text, meta, err := c.CompleteOp(context.Background(), "chat.summary", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("CompleteOp: %v", err)
	}

	if text != "hello" {
		t.Fatalf("text = %q want hello", text)
	}

	if got, ok := gotBody["stream_options"].(map[string]any); !ok || got["include_usage"] != true {
		t.Fatalf("stream_options not requested: %v", gotBody["stream_options"])
	}

	if meta.Usage.PromptTokens != 11 || meta.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v", meta.Usage)
	}

	if meta.Model != "served-model" {
		t.Fatalf("model = %q, want the model the server reported", meta.Model)
	}

	if meta.FirstTokenAt.IsZero() {
		t.Fatal("first token time not recorded")
	}

	if meta.Attempts != 1 {
		t.Fatalf("attempts = %d want 1", meta.Attempts)
	}
}

// A gateway that rejects stream_options must not cost the completion: the
// client drops the field and replays once, then keeps it off.
func TestStreamOptionsRejectionFallsBack(t *testing.T) {
	var (
		requests int
		withOpts int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++

		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		if _, ok := body["stream_options"]; ok {
			withOpts++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"unknown parameter: stream_options"}}`)

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c, err := New(config.LLM{BaseURL: srv.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if out != "ok" {
		t.Fatalf("text = %q want ok", out)
	}

	if withOpts != 1 {
		t.Fatalf("stream_options sent %d times, want exactly one probe", withOpts)
	}

	// The latch must hold: a second call may not re-probe.
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "again"}}); err != nil {
		t.Fatalf("second Complete: %v", err)
	}

	if withOpts != 1 {
		t.Fatalf("stream_options re-probed after latching (%d times)", withOpts)
	}
}

// A generic 400 must not be mistaken for a stream_options rejection, or one bad
// prompt would silently disable usage reporting for the process lifetime.
func TestUnrelatedBadRequestDoesNotLatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"model not found"}}`)
	}))
	defer srv.Close()

	c, err := New(config.LLM{BaseURL: srv.URL, Model: "nope"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("expected an error")
	}

	if c.noStreamOpts.Load() {
		t.Fatal("an unrelated 400 latched the stream_options fallback")
	}
}

// Gateways that ignore stream:true and answer with a plain JSON body still
// report usage; the fallback path must pick it up.
func TestNonStreamedFallbackCapturesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	c, err := New(config.LLM{BaseURL: srv.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	text, meta, err := c.CompleteOp(context.Background(), "chat", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("CompleteOp: %v", err)
	}

	if text != "pong" {
		t.Fatalf("text = %q", text)
	}

	if meta.Usage.PromptTokens != 7 || meta.Usage.CompletionTokens != 2 {
		t.Fatalf("usage = %+v", meta.Usage)
	}
}
