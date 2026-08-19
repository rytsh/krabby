package mcptools

import (
	"encoding/json"
	"testing"
	"time"
)

func TestUnwrapJSONArg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"object passes through", `{"query":"_limit=1"}`, `{"query":"_limit=1"}`},
		{"array passes through", `[{"a":1}]`, `[{"a":1}]`},
		{"stringified object unwrapped", `"{\"query\":\"_limit=1\"}"`, `{"query":"_limit=1"}`},
		{"stringified array unwrapped", `"[{\"a\":1}]"`, `[{"a":1}]`},
		{"padded stringified object unwrapped", `"  {\"a\":1}  "`, `{"a":1}`},
		{"plain string kept", `"hello"`, `"hello"`},
		{"quoted number kept", `"42"`, `"42"`},
		{"string of broken json kept", `"{not json"`, `"{not json"`},
		{"number kept", `7`, `7`},
		{"null kept", `null`, `null`},
		{"empty kept", ``, ``},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := string(unwrapJSONArg(json.RawMessage(tt.in))); got != tt.want {
				t.Errorf("unwrapJSONArg(%s) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

// The unwrap must not invent a value the caller did not write: it fires only
// when the quoted text is itself a composite, so a body that legitimately is a
// JSON string survives intact.
func TestUnwrapJSONArgKeepsScalarStrings(t *testing.T) {
	t.Parallel()

	for _, in := range []string{`"true"`, `""`, `" "`, `"[unterminated"`, `"null"`} {
		if got := string(unwrapJSONArg(json.RawMessage(in))); got != in {
			t.Errorf("unwrapJSONArg(%s) = %s, want it unchanged", in, got)
		}
	}
}

// The regression this exists for, exercised through the same conversion the
// call_api_endpoint handler performs: a body the client stringified must reach
// the provider as a message protojson can decode.
func TestCallRequestDecodesStringifiedBody(t *testing.T) {
	t.Parallel()

	stringified, err := json.Marshal(`{"query":"_limit=1"}`)
	if err != nil {
		t.Fatal(err)
	}

	args := callAPIEndpointArgs{
		Service:  "transaction",
		Endpoint: "/v1.TransactionService/GetTransactions",
		Body:     stringified,
	}

	var msg struct {
		Query string `json:"query"`
	}
	body := args.callRequest().Body
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("body reaching the provider is not a decodable message: %v (got %s)", err, body)
	}
	if msg.Query != "_limit=1" {
		t.Errorf("query = %q, want _limit=1", msg.Query)
	}
}

// A body already sent as an object must pass through byte-for-byte, and the
// other request fields must survive the conversion.
func TestCallRequestPassesObjectBodyThrough(t *testing.T) {
	t.Parallel()

	args := callAPIEndpointArgs{
		Service:    "transaction",
		Endpoint:   "/v1.TransactionService/GetTransactions",
		Body:       json.RawMessage(`{"query":"_limit=1"}`),
		Query:      map[string]string{"a": "b"},
		Headers:    map[string]string{"X-Test": "1"},
		PathParams: map[string]string{"id": "7"},
		TimeoutSec: 5,
	}

	req := args.callRequest()
	if string(req.Body) != `{"query":"_limit=1"}` {
		t.Errorf("body = %s, want it untouched", req.Body)
	}
	if req.Timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", req.Timeout)
	}
	if req.Query["a"] != "b" || req.Headers["X-Test"] != "1" || req.PathParams["id"] != "7" {
		t.Errorf("request fields lost: %+v", req)
	}
}
