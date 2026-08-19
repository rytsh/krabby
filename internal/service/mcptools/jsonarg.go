package mcptools

import (
	"encoding/json"
	"strings"
)

// unwrapJSONArg accepts a JSON value that a client delivered as a quoted
// string, returning the inner value.
//
// Some MCP clients stringify a nested tool argument: a body written as an
// object arrives as the string "{\"query\":\"_limit=1\"}". This is observed
// behaviour, not a precaution — a call_api_endpoint request that had succeeded
// minutes earlier began failing mid-session with a byte-identical body:
//
//	decode request message 1 as v1.QueryRequest; proto: syntax error
//	(line 1:1): unexpected token "{\"query\": \"_limit=1\"}"
//
// It bites gRPC hardest: protojson rejects a string where a message is due, so
// the endpoint becomes intermittently uncallable. rawConfig in docs.go already
// concedes the same client behaviour by taking config as a JSON string outright;
// a request body cannot make that trade, because it genuinely is arbitrary JSON.
//
// The object/array condition is what keeps this from being a guess. A payload
// that is legitimately the JSON string "hello" stays a string, because "hello"
// is not a composite value; only a string spelling out an object or array —
// which no caller means literally, and which the transport would otherwise
// reject — is unwrapped. Invalid inner JSON is left alone so the decoder that
// understands the format reports the real error.
func unwrapJSONArg(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}

	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, `"`) {
		return raw
	}

	var inner string
	if err := json.Unmarshal([]byte(trimmed), &inner); err != nil {
		return raw
	}

	inner = strings.TrimSpace(inner)
	if !strings.HasPrefix(inner, "{") && !strings.HasPrefix(inner, "[") {
		return raw
	}
	if !json.Valid([]byte(inner)) {
		return raw
	}

	return json.RawMessage(inner)
}
