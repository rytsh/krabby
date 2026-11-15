package mcptools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Secrets passed to admin tools must not reach Langfuse; the rest of the call
// stays readable, because a trace that hides everything explains nothing.
func TestRedactArguments(t *testing.T) {
	raw := `{
		"pattern": "github.com/acme",
		"secret": "ghp_abc",
		"llm_api_key": "sk-1",
		"langfuse_secret_key": "lf-1",
		"llm_api_key_set": true,
		"token_budget": 4000,
		"clear_bearer_token": true,
		"bearer_token": "b-1",
		"headers": {"X-Api-Key": "k-1"},
		"config": {"url": "https://wiki.example.com", "api_token": "t-1", "user": "ray"},
		"items": [{"session_token": "s-1", "title": "kept"}],
		"empty_token": ""
	}`

	got := redactArguments([]byte(raw))
	for _, secret := range []string{"ghp_abc", "sk-1", "lf-1", "b-1", "k-1", "t-1", "s-1"} {
		if strings.Contains(got, secret) {
			t.Errorf("secret %q survived redaction: %s", secret, got)
		}
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("redacted arguments are not JSON: %v", err)
	}
	if m["pattern"] != "github.com/acme" || m["llm_api_key_set"] != true || m["token_budget"] != float64(4000) || m["clear_bearer_token"] != true {
		t.Errorf("non-secret arguments were changed: %s", got)
	}
	if cfg := m["config"].(map[string]any); cfg["url"] != "https://wiki.example.com" || cfg["user"] != "ray" {
		t.Errorf("nested non-secret config was changed: %v", cfg)
	}
	if items := m["items"].([]any); items[0].(map[string]any)["title"] != "kept" {
		t.Errorf("array element lost a non-secret field: %v", items)
	}
	if m["empty_token"] != "" {
		t.Errorf("an empty value was replaced; it carries nothing to hide: %v", m["empty_token"])
	}

	for _, in := range []string{"not json", `"a string"`, `[1,2]`} {
		if got := redactArguments([]byte(in)); got != redactedValue {
			t.Errorf("redactArguments(%q) = %q, want it withheld", in, got)
		}
	}
	if redactArguments(nil) != "" {
		t.Error("no arguments should trace as none")
	}
}

// A panicking tool must fail its own call, not the process.
func TestRecoverMiddlewareContainsAPanic(t *testing.T) {
	handler := recoverMiddleware("test")(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		panic("tool handler bug")
	})

	res, err := handler(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "explode"}})
	if err == nil || res != nil {
		t.Fatalf("panic surfaced as res=%v err=%v, want an error", res, err)
	}
}
