package apicatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Calling limits.
//
// A catalogued call is an interactive act — a human pressing "Send", or a model
// checking one endpoint — not a data pipeline. Every limit below is sized for
// that: enough to see what the endpoint really returns, far too little to use
// krabby as a proxy or to blow up the context of whoever asked.
const (
	// MaxCallBodyBytes bounds the response payload kept and returned. Past this
	// the body is cut and CallResponse.Truncated is set.
	MaxCallBodyBytes = 256 << 10

	// MaxCallRequestBytes bounds the request payload a caller may send.
	MaxCallRequestBytes = 1 << 20

	// DefaultCallTimeout applies when the caller names none.
	DefaultCallTimeout = 30 * time.Second
	// MaxCallTimeout is the ceiling a caller may ask for.
	MaxCallTimeout = 2 * time.Minute

	// MaxStreamMessages bounds how many messages a streaming gRPC call will
	// send or collect before it stops.
	MaxStreamMessages = 100
)

// CallRequest is one invocation of a catalogued operation.
//
// It carries only the parts a caller legitimately varies: the values that fill
// the operation's own parameters, extra headers, and a body. What is *not* here
// is as deliberate as what is: no method, no host, no path. Those come from the
// stored operation and the service's provider config, so a caller — including a
// model holding this tool — can exercise the catalog but cannot turn krabby
// into a general-purpose request forwarder.
type CallRequest struct {
	// PathParams fill the {name} placeholders of the operation path. Values are
	// escaped as single path segments, so a value can never introduce a
	// segment, a query or a host.
	PathParams map[string]string `json:"path_params,omitempty"`
	// Query are query-string parameters, appended to whatever the operation
	// path already carries.
	Query map[string]string `json:"query,omitempty"`
	// Headers are request headers; for gRPC they become call metadata. They are
	// applied last, so they override what the provider config sets — which is
	// what makes "same service, different token" possible without editing the
	// service.
	Headers map[string]string `json:"headers,omitempty"`
	// Body is the request payload. For HTTP it is sent as-is under the
	// operation's content type. For gRPC it is protojson: an object is one
	// message, and a JSON array is a sequence of messages for a
	// client-streaming method.
	Body json.RawMessage `json:"body,omitempty"`
	// Timeout bounds the call. Zero means DefaultCallTimeout; anything above
	// MaxCallTimeout is clamped to it.
	Timeout time.Duration `json:"-"`
}

// CallResponse is what came back.
//
// One shape covers both transports because the useful questions are the same:
// did it succeed, what did the peer say, and what did it send. HTTP fills
// Status; gRPC leaves it zero and puts the code in StatusText. Body is always
// text — a decoded JSON payload for the usual case, and the raw bytes rendered
// as a string when the peer answered with something else, because "it returned
// HTML" is an answer a caller needs to see rather than a decode error.
type CallResponse struct {
	// Status is the HTTP status code, or 0 for gRPC.
	Status int `json:"status,omitempty"`
	// StatusText is "200 OK" for HTTP, or the gRPC code name ("OK",
	// "NotFound"). It is the one field that is always populated.
	StatusText string `json:"status_text"`
	// OK reports a successful call: a 2xx status, or gRPC code OK.
	OK bool `json:"ok"`

	// Method and URL record what was actually sent, after path parameters and
	// query were resolved. A caller comparing this with what it intended is how
	// a wrong path parameter gets noticed.
	Method string `json:"method,omitempty"`
	URL    string `json:"url,omitempty"`

	// Headers are the response headers, or the gRPC response metadata.
	Headers map[string]string `json:"headers,omitempty"`
	// ContentType is the response media type, when the peer declared one.
	ContentType string `json:"content_type,omitempty"`

	// Body is the response payload as text, cut at MaxCallBodyBytes. For a
	// streaming gRPC call it is a JSON array of the collected messages.
	Body string `json:"body,omitempty"`
	// BodyBytes is the size of the payload before any cut.
	BodyBytes int `json:"body_bytes,omitempty"`
	// Truncated reports that Body was cut, or that a stream stopped at
	// MaxStreamMessages.
	Truncated bool `json:"truncated,omitempty"`
	// MessageCount is how many messages a streaming gRPC call collected.
	MessageCount int `json:"message_count,omitempty"`

	// DurationMS is the wall time of the call.
	DurationMS int64 `json:"duration_ms"`

	// Error is a transport-level failure: the call never produced a status.
	// A 500 response is not an error here — it is a successful call with an
	// unsuccessful status, and conflating the two hides which one happened.
	Error string `json:"error,omitempty"`

	// Notes carries caveats worth surfacing, e.g. that a streaming method was
	// invoked or that credentials came from the service config.
	Notes []string `json:"notes,omitempty"`
}

// Caller is an optional provider capability: it invokes one catalogued
// operation against the live service.
//
// It is separate from Provider for the same reason Previewer is — not every
// kind can do it, and a kind that cannot should fail with "not supported"
// rather than be forced to implement a stub. It takes the whole Service because
// only the provider may see the unredacted Config, which is where the target
// and the credentials live.
type Caller interface {
	Call(ctx context.Context, svc *Service, d *Detail, req CallRequest) (CallResponse, error)
}

// NormalizeTimeout clamps a requested timeout into the allowed band.
func NormalizeTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultCallTimeout
	}
	if d > MaxCallTimeout {
		return MaxCallTimeout
	}

	return d
}

// pathParamRe matches an OpenAPI path placeholder, "{invoiceId}".
var pathParamRe = regexp.MustCompile(`\{([^{}/]+)\}`)

// ResolvePath substitutes {name} placeholders in a templated path.
//
// Values are escaped with url.PathEscape, which encodes "/" as well. That is
// the security property of this function: a path parameter is data filling one
// segment, and no value of it — "../../admin", "x?a=b", "//evil.host/" — can
// change the shape of the request. Callers that genuinely need a slash-bearing
// parameter are describing a different endpoint, not this one.
//
// A placeholder with no value is an error rather than an empty segment: silently
// requesting /v1/invoices// is a worse outcome than being told what is missing.
func ResolvePath(template string, params map[string]string) (string, error) {
	lookup := make(map[string]string, len(params))
	for name, value := range params {
		lookup[strings.TrimSpace(name)] = value
	}

	var missing []string
	out := pathParamRe.ReplaceAllStringFunc(template, func(match string) string {
		name := strings.TrimSpace(match[1 : len(match)-1])
		value, ok := lookup[name]
		if !ok || value == "" {
			missing = append(missing, name)

			return match
		}

		return url.PathEscape(value)
	})

	if len(missing) > 0 {
		sort.Strings(missing)

		return "", fmt.Errorf("missing path parameter(s): %s", strings.Join(missing, ", "))
	}

	return out, nil
}

// AppendQuery adds parameters to a URL that may already carry some.
//
// Existing values are kept: an operation path is allowed to hardcode a query
// (some specs do), and dropping it because the caller supplied an unrelated
// parameter would change the endpoint being called.
func AppendQuery(rawURL string, params map[string]string) (string, error) {
	if len(params) == 0 {
		return rawURL, nil
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse request url %q; %w", rawURL, err)
	}

	q := parsed.Query()
	for name, value := range params {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		q.Set(name, value)
	}
	parsed.RawQuery = q.Encode()

	return parsed.String(), nil
}

// MissingRequired reports the required parameters of an operation that the
// request leaves unset, so a caller is told what the endpoint needs instead of
// receiving the peer's 400.
//
// Only path and query are checked. A required header is frequently the auth
// header, which the provider config supplies without the caller ever naming it,
// and a required cookie is not something this surface models.
func MissingRequired(d *Detail, req CallRequest) []string {
	if d == nil {
		return nil
	}

	var missing []string
	for _, p := range d.Parameters {
		if !p.Required {
			continue
		}

		switch strings.ToLower(p.In) {
		case "path":
			if req.PathParams[p.Name] == "" {
				missing = append(missing, "path:"+p.Name)
			}
		case "query":
			if _, ok := req.Query[p.Name]; !ok {
				missing = append(missing, "query:"+p.Name)
			}
		}
	}

	sort.Strings(missing)

	return missing
}

// TruncateBody cuts a payload to MaxCallBodyBytes, reporting whether it did.
//
// The cut is on bytes rather than runes and may split a multi-byte character;
// that is acceptable for a diagnostic view and is preferable to scanning a
// quarter-megabyte payload to find a rune boundary nobody will notice.
func TruncateBody(body []byte) (string, bool) {
	if len(body) <= MaxCallBodyBytes {
		return string(body), false
	}

	return string(body[:MaxCallBodyBytes]), true
}
