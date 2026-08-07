package apicatalog

import "encoding/json"

// Flattening limits.
//
// They exist because an API schema is a graph, not a tree: $ref lets a type
// reference itself directly or through a cycle, and "expand everything" on a
// real-world spec either never terminates or produces a document larger than
// the context it was meant to fit in. The limits are enforced where the schema
// is flattened — once, at ingest — rather than hoped for at every read.
//
// The numbers are chosen against what a caller actually needs to build a
// request: four levels reaches the fields of an object inside an array inside a
// wrapper, which covers the overwhelming majority of request bodies, and forty
// properties is past the point where a model is reading rather than scanning.
// Anything cut is marked Truncated so the reader knows to consult the spec
// instead of assuming it saw everything.
const (
	MaxSchemaDepth      = 4
	MaxSchemaProperties = 40
	// MaxDetailBytes bounds the marshalled Detail. A single endpoint that does
	// not fit in this is one no model will read in full anyway.
	MaxDetailBytes = 24 << 10
)

// Detail is the pre-rendered structured payload of one operation: everything
// needed to understand and construct a call, and nothing that requires going
// back to the source document.
type Detail struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	BaseURL string `json:"base_url,omitempty"`

	Summary     string   `json:"summary,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Deprecated  bool     `json:"deprecated,omitempty"`

	Parameters  []Param    `json:"parameters,omitempty"`
	RequestBody *Body      `json:"request_body,omitempty"`
	Responses   []Response `json:"responses,omitempty"`

	// Security lists the auth schemes the operation accepts. It is structured
	// rather than prose because the request recipe has to turn it into an
	// actual header: "bearer" becomes Authorization, an apiKey in a header
	// becomes that header by name, and getting it wrong makes the generated
	// command silently unauthenticated.
	Security []SecurityScheme `json:"security,omitempty"`

	// Request is the ready-to-run recipe, the answer to "how do I call this".
	Request Recipe `json:"request"`

	// Truncated reports that a flattening limit cut something. It propagates
	// up from the schemas so a reader sees one honest flag at the top rather
	// than having to hunt for the nested one.
	Truncated bool `json:"truncated,omitempty"`

	// Notes carries provider-supplied caveats, e.g. that a base URL was
	// overridden or that the operation is a streaming RPC.
	Notes []string `json:"notes,omitempty"`
}

// Param is one path, query, header or cookie parameter.
type Param struct {
	Name        string   `json:"name"`
	In          string   `json:"in"`
	Required    bool     `json:"required,omitempty"`
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Default     string   `json:"default,omitempty"`
	Example     string   `json:"example,omitempty"`
}

// Body is a request body in one content type.
type Body struct {
	ContentType string  `json:"content_type,omitempty"`
	Required    bool    `json:"required,omitempty"`
	Description string  `json:"description,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

// Response is one documented response status.
type Response struct {
	Status      string  `json:"status"`
	Description string  `json:"description,omitempty"`
	ContentType string  `json:"content_type,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

// Schema is the flattened, depth-limited view of a JSON schema.
//
// Properties is an ordered slice rather than a map, and that is load-bearing:
// the rendered markdown is hashed to decide whether an operation needs
// re-embedding, and Go map iteration order is randomized per run. A map here
// would make every sync produce a different hash for an unchanged endpoint and
// re-embed the entire catalog on every refresh.
type Schema struct {
	Type        string   `json:"type,omitempty"`
	Format      string   `json:"format,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Required    []string `json:"required,omitempty"`
	Nullable    bool     `json:"nullable,omitempty"`

	Properties []Property `json:"properties,omitempty"`
	Items      *Schema    `json:"items,omitempty"`

	// Ref is the schema's source name when it came from a component, kept so a
	// truncated branch still tells the reader what to look up.
	Ref string `json:"ref,omitempty"`
	// Truncated marks a branch cut by a depth or property limit.
	Truncated bool `json:"truncated,omitempty"`
}

// Property is one named field of an object schema.
type Property struct {
	Name     string  `json:"name"`
	Required bool    `json:"required,omitempty"`
	Schema   *Schema `json:"schema,omitempty"`
}

// SecurityScheme is one auth requirement of an operation.
type SecurityScheme struct {
	// Name is the scheme's name in the document ("bearerAuth", "api_key").
	Name string `json:"name"`
	// Type is the OpenAPI security type: http, apiKey, oauth2, openIdConnect,
	// mutualTLS.
	Type string `json:"type,omitempty"`
	// Scheme is the HTTP auth scheme when Type is http: bearer, basic.
	Scheme string `json:"scheme,omitempty"`
	// In and ParamName locate an apiKey: header, query or cookie.
	In        string `json:"in,omitempty"`
	ParamName string `json:"param_name,omitempty"`
	// Scopes are the OAuth2 scopes the operation requires.
	Scopes      []string `json:"scopes,omitempty"`
	Description string   `json:"description,omitempty"`
}

// Recipe is a ready-to-run request.
type Recipe struct {
	// Command is a complete shell command (curl or grpcurl) with placeholder
	// values, safe to copy, edit and run.
	Command string `json:"command,omitempty"`
	// URL is the resolved request URL with path parameters still templated.
	URL string `json:"url,omitempty"`
	// Headers are the headers the command sets, as "Name: value" strings.
	Headers []string `json:"headers,omitempty"`
	// Body is the generated example request body, if the operation takes one.
	Body json.RawMessage `json:"body,omitempty"`
}
