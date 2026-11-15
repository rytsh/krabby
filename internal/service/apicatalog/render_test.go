package apicatalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func invoiceDetail() *Detail {
	return &Detail{
		Method:      "POST",
		Path:        "/v1/invoices",
		BaseURL:     "https://billing.example.com/",
		Summary:     "Create an invoice",
		Description: "Creates a draft invoice for a customer.",
		Tags:        []string{"invoices", "billing"},
		Parameters: []Param{
			{Name: "dry_run", In: "query", Required: true, Type: "boolean"},
			{Name: "expand", In: "query", Type: "string"},
			{Name: "Idempotency-Key", In: "header", Required: true, Type: "string", Example: "key-123"},
			{Name: "X-Trace", In: "header", Type: "string"},
		},
		RequestBody: &Body{
			ContentType: "application/json",
			Required:    true,
			Schema: &Schema{
				Properties: []Property{
					{Name: "customer_id", Required: true, Schema: &Schema{Type: "string", Format: "uuid", Description: "Who is billed."}},
					{Name: "amount", Required: true, Schema: &Schema{Type: "integer"}},
					{Name: "currency", Schema: &Schema{Type: "string", Enum: []string{"EUR", "USD"}}},
					{Name: "lines", Schema: &Schema{Type: "array", Items: &Schema{Properties: []Property{
						{Name: "sku", Schema: &Schema{Type: "string", Description: "Stock unit."}},
					}}}},
				},
			},
		},
		Responses: []Response{
			{Status: "201", Description: "Created", Schema: &Schema{Properties: []Property{
				{Name: "id", Schema: &Schema{Type: "string"}},
			}}},
			{Status: "422", Description: "Validation failed"},
		},
		Security: []SecurityScheme{{Name: "bearerAuth", Type: "http", Scheme: "bearer"}},
	}
}

func TestBuildRecipe(t *testing.T) {
	d := invoiceDetail()
	BuildRecipe(d)

	// No doubled slash where the base URL meets the path, and only the
	// required query parameter: a recipe must run as pasted.
	if want := "https://billing.example.com/v1/invoices?dry_run=<boolean>"; d.Request.URL != want {
		t.Errorf("URL = %q, want %q", d.Request.URL, want)
	}

	wantHeaders := []string{
		"Content-Type: application/json",
		"Idempotency-Key: key-123",
		"Authorization: Bearer $TOKEN",
	}
	if strings.Join(d.Request.Headers, "|") != strings.Join(wantHeaders, "|") {
		t.Errorf("headers = %q, want %q", d.Request.Headers, wantHeaders)
	}

	if !json.Valid(d.Request.Body) {
		t.Fatalf("example body is not valid JSON: %s", d.Request.Body)
	}

	cmd := d.Request.Command
	if !strings.HasPrefix(cmd, "curl -X POST 'https://billing.example.com/v1/invoices?dry_run=<boolean>'") {
		t.Errorf("command = %q", cmd)
	}
	for _, h := range wantHeaders {
		if !strings.Contains(cmd, "-H '"+h+"'") {
			t.Errorf("command does not set header %q:\n%s", h, cmd)
		}
	}
	if !strings.Contains(cmd, "-d '{") {
		t.Errorf("command carries no body:\n%s", cmd)
	}
	if strings.Contains(cmd, "expand") || strings.Contains(cmd, "X-Trace") {
		t.Errorf("command includes optional parameters:\n%s", cmd)
	}
}

func TestBuildRecipeWithoutBodyOrQuery(t *testing.T) {
	d := &Detail{Method: "get", Path: "/v1/health", BaseURL: "https://api.example.com"}
	BuildRecipe(d)

	if d.Request.Command != "curl -X GET 'https://api.example.com/v1/health'" {
		t.Errorf("command = %q", d.Request.Command)
	}
	if d.Request.Body != nil || len(d.Request.Headers) != 0 {
		t.Errorf("a body-less GET got a body or headers: %+v", d.Request)
	}
}

// A placeholder should be the most specific thing the spec offers.
func TestPlaceholderPrecedence(t *testing.T) {
	for name, tt := range map[string]struct {
		p    Param
		want string
	}{
		"example first": {Param{Example: "ex", Default: "def", Enum: []string{"a"}, Type: "string"}, "ex"},
		"then default":  {Param{Default: "def", Enum: []string{"a"}, Type: "string"}, "def"},
		"then enum":     {Param{Enum: []string{"a", "b"}, Type: "string"}, "a"},
		"then type":     {Param{Type: "integer"}, "<integer>"},
		"then generic":  {Param{}, "<value>"},
	} {
		if got := placeholderFor(tt.p); got != tt.want {
			t.Errorf("%s: placeholder = %q, want %q", name, got, tt.want)
		}
	}
}

// A scheme that does not travel in a header must not invent one: the command
// would look authenticated and not be.
func TestSecurityHeader(t *testing.T) {
	for name, tt := range map[string]struct {
		sec  SecurityScheme
		want string
	}{
		"bearer":            {SecurityScheme{Type: "http", Scheme: "Bearer"}, "Authorization: Bearer $TOKEN"},
		"basic":             {SecurityScheme{Type: "HTTP", Scheme: "basic"}, "Authorization: Basic $BASIC_AUTH"},
		"other http scheme": {SecurityScheme{Type: "http", Scheme: "digest"}, "Authorization: $CREDENTIAL"},
		"oauth2":            {SecurityScheme{Type: "oauth2"}, "Authorization: Bearer $TOKEN"},
		"openid connect":    {SecurityScheme{Type: "openIdConnect"}, "Authorization: Bearer $TOKEN"},
		"api key in header": {SecurityScheme{Type: "apiKey", In: "Header", ParamName: "X-API-Key"}, "X-API-Key: $API_KEY"},
		"api key in query":  {SecurityScheme{Type: "apiKey", In: "query", ParamName: "key"}, ""},
		"api key unnamed":   {SecurityScheme{Type: "apiKey", In: "header"}, ""},
		"mutual tls":        {SecurityScheme{Type: "mutualTLS"}, ""},
		"unknown":           {SecurityScheme{}, ""},
	} {
		if got := securityHeader(tt.sec); got != tt.want {
			t.Errorf("%s: header = %q, want %q", name, got, tt.want)
		}
	}
}

// The command is meant to be pasted into a shell. A quote in a spec-supplied
// value must not be able to end the string and start a new command.
func TestCurlCommandQuotesHostileValues(t *testing.T) {
	d := &Detail{
		Method:  "GET",
		Path:    "/v1/search",
		BaseURL: "https://api.example.com",
		Parameters: []Param{
			{Name: "q", In: "query", Required: true, Example: "it's'; rm -rf ~ #"},
			{Name: "X-Note", In: "header", Required: true, Default: "a'b"},
		},
	}
	BuildRecipe(d)

	cmd := d.Request.Command
	if !strings.Contains(cmd, `it'\''s'\''; rm -rf ~ #`) {
		t.Errorf("single quotes in the URL were not escaped:\n%s", cmd)
	}
	if !strings.Contains(cmd, `-H 'X-Note: a'\''b'`) {
		t.Errorf("single quote in a header was not escaped:\n%s", cmd)
	}

	// Stripping every escaped quote must leave an even number of quotes:
	// anything else means a string was left open or closed early.
	if n := strings.Count(strings.ReplaceAll(cmd, `'\''`, ""), "'"); n%2 != 0 {
		t.Errorf("unbalanced quoting (%d bare quotes):\n%s", n, cmd)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":     `'plain'`,
		"":          `''`,
		"a b":       `'a b'`,
		"it's":      `'it'\''s'`,
		"$HOME`id`": "'$HOME`id`'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestBuildGRPCRecipe(t *testing.T) {
	d := &Detail{
		Method: MethodGRPC,
		Path:   "/billing.v1.Invoices/Create",
		RequestBody: &Body{Schema: &Schema{Properties: []Property{
			{Name: "customer_id", Schema: &Schema{Type: "string"}},
		}}},
		Security: []SecurityScheme{
			{Name: "bearer", Type: "http", Scheme: "bearer"},
			{Name: "mtls", Type: "mutualTLS"},
		},
	}
	BuildGRPCRecipe(d, "billing.internal:443", false)

	cmd := d.Request.Command
	if !strings.HasPrefix(cmd, "grpcurl \\\n") {
		t.Errorf("command = %q", cmd)
	}
	if strings.Contains(cmd, "-plaintext") {
		t.Error("a TLS target was given -plaintext")
	}
	// gRPC metadata keys are lowercase.
	if !strings.Contains(cmd, "-H 'authorization:Bearer $TOKEN'") {
		t.Errorf("metadata header missing or not lowercased:\n%s", cmd)
	}
	// grpcurl takes the method without its leading slash, after the target.
	if !strings.HasSuffix(cmd, "'billing.internal:443' 'billing.v1.Invoices/Create'") {
		t.Errorf("target and method are not the trailing arguments:\n%s", cmd)
	}
	if !strings.Contains(cmd, `-d '{`) {
		t.Errorf("command carries no request message:\n%s", cmd)
	}
	if len(d.Request.Headers) != 1 {
		t.Errorf("headers = %q, want only the scheme that maps to one", d.Request.Headers)
	}
	if d.Request.URL != "billing.internal:443/billing.v1.Invoices/Create" {
		t.Errorf("URL = %q", d.Request.URL)
	}
}

func TestBuildGRPCRecipePlaintextWithoutBody(t *testing.T) {
	d := &Detail{Method: MethodGRPC, Path: "/health.v1.Health/Check"}
	BuildGRPCRecipe(d, "localhost:9090", true)

	if want := "grpcurl -plaintext \\\n  'localhost:9090' 'health.v1.Health/Check'"; d.Request.Command != want {
		t.Errorf("command = %q, want %q", d.Request.Command, want)
	}
}

// The example mirrors the order the spec documents its fields in. A map would
// sort them, and Go's randomized map order would also change the rendered
// document, and with it the hash, on every sync.
func TestExampleJSONKeepsPropertyOrder(t *testing.T) {
	got := string(ExampleJSON(invoiceDetail().RequestBody.Schema))

	want := `{
  "customer_id": "00000000-0000-0000-0000-000000000000",
  "amount": 0,
  "currency": "EUR",
  "lines": [
    {
      "sku": "string"
    }
  ]
}`
	if got != want {
		t.Errorf("example =\n%s\nwant\n%s", got, want)
	}

	for range 20 {
		if again := string(ExampleJSON(invoiceDetail().RequestBody.Schema)); again != got {
			t.Fatal("ExampleJSON is not deterministic")
		}
	}
}

func TestExampleJSONValues(t *testing.T) {
	for name, tt := range map[string]struct {
		schema *Schema
		want   string
	}{
		"nil schema":               {nil, `null`},
		"boolean":                  {&Schema{Type: "boolean"}, `true`},
		"number":                   {&Schema{Type: "number"}, `0`},
		"null":                     {&Schema{Type: "null"}, `null`},
		"plain string":             {&Schema{Type: "string"}, `"string"`},
		"untyped is a string":      {&Schema{}, `"string"`},
		"enum":                     {&Schema{Type: "string", Enum: []string{"open", "paid"}}, `"open"`},
		"date-time":                {&Schema{Type: "string", Format: "date-time"}, `"2024-01-01T00:00:00Z"`},
		"email":                    {&Schema{Type: "string", Format: "email"}, `"user@example.com"`},
		"password is a var":        {&Schema{Type: "string", Format: "password"}, `"$PASSWORD"`},
		"empty object":             {&Schema{Type: "object"}, `{}`},
		"array without items":      {&Schema{Type: "array"}, `[]`},
		"array inferred":           {&Schema{Items: &Schema{Type: "integer"}}, "[\n  0\n]"},
		"type is case-insensitive": {&Schema{Type: "Boolean"}, `true`},
	} {
		got := string(ExampleJSON(tt.schema))
		if got != tt.want {
			t.Errorf("%s: example = %s, want %s", name, got, tt.want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("%s: example %s is not valid JSON", name, got)
		}
	}
}

// Property names come from the spec and end up inside JSON.
func TestExampleJSONEscapesNames(t *testing.T) {
	got := ExampleJSON(&Schema{Properties: []Property{
		{Name: `quote"and\slash`, Schema: &Schema{Type: "string", Enum: []string{"line\nbreak"}}},
	}})

	var decoded map[string]string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("example is not valid JSON: %v\n%s", err, got)
	}
	if decoded[`quote"and\slash`] != "line\nbreak" {
		t.Errorf("decoded = %v", decoded)
	}
}

func TestMarkdown(t *testing.T) {
	d := invoiceDetail()
	BuildRecipe(d)

	md := Markdown("billing", d)

	for _, want := range []string{
		"# POST /v1/invoices — Create an invoice\n",
		"Service: `billing`\n",
		"Tags: invoices, billing\n",
		"Base URL: https://billing.example.com/\n",
		"Creates a draft invoice for a customer.",
		"## Parameters\n",
		"| `dry_run` | query | yes | boolean |  |",
		"| `expand` | query |  | string |  |",
		"## Request body\n",
		"Content type: `application/json` (required)",
		"| `customer_id` | string (uuid) | yes | Who is billed. |",
		// Fields of objects inside an array are listed under their path, so
		// their descriptions are searchable too.
		"| `lines[].sku` | string |  | Stock unit. |",
		"## Responses\n",
		"### 201 — Created\n",
		"### 422 — Validation failed\n",
		"## Authentication\n",
		"- `bearerAuth` — HTTP bearer authentication",
		"## Example request\n\n```sh\ncurl -X POST ",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown is missing %q\n---\n%s", want, md)
		}
	}

	if strings.Contains(md, "Deprecated") || strings.Contains(md, "omitted") {
		t.Errorf("markdown carries a warning that does not apply:\n%s", md)
	}
}

// The rendered document is hashed to decide whether to re-embed; it has to be
// identical for identical input.
func TestMarkdownIsDeterministic(t *testing.T) {
	render := func() string {
		d := invoiceDetail()
		BuildRecipe(d)

		return Markdown("billing", d)
	}

	first := render()
	for range 20 {
		if render() != first {
			t.Fatal("Markdown differs between runs for the same operation")
		}
	}
}

func TestMarkdownWarnings(t *testing.T) {
	md := Markdown("billing", &Detail{
		Method: "GET", Path: "/v1/old", Deprecated: true, Truncated: true,
		Notes: []string{"Base URL overridden."},
	})

	for _, want := range []string{"# GET /v1/old\n", "> **Deprecated.**", "Some schema detail was omitted", "> Base URL overridden."} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown is missing %q\n---\n%s", want, md)
		}
	}
	for _, section := range []string{"## Parameters", "## Request body", "## Responses", "## Authentication", "## Example request"} {
		if strings.Contains(md, section) {
			t.Errorf("empty operation rendered a %q section", section)
		}
	}
}

func TestMarkdownListsEnumValuesOfParameters(t *testing.T) {
	md := Markdown("billing", &Detail{Method: "GET", Path: "/v1/invoices", Parameters: []Param{
		{Name: "status", In: "query", Type: "string", Description: "Filter.", Enum: []string{"open", "paid"}},
	}})

	if !strings.Contains(md, "| `status` | query |  | string | Filter. One of: open, paid. |") {
		t.Errorf("enum values not listed:\n%s", md)
	}
}

func TestDescribeSecurity(t *testing.T) {
	for name, tt := range map[string]struct {
		sec  SecurityScheme
		want string
	}{
		"http":        {SecurityScheme{Name: "b", Type: "http", Scheme: "bearer"}, "`b` — HTTP bearer authentication"},
		"api key":     {SecurityScheme{Name: "k", Type: "apiKey", In: "header", ParamName: "X-Key"}, "`k` — API key in header `X-Key`"},
		"oauth2":      {SecurityScheme{Name: "o", Type: "oauth2", Scopes: []string{"read", "write"}}, "`o` — OAuth2, scopes: read, write"},
		"oauth2 bare": {SecurityScheme{Name: "o", Type: "oauth2"}, "`o` — OAuth2"},
		"oidc":        {SecurityScheme{Name: "i", Type: "openIdConnect"}, "`i` — OpenID Connect"},
		"mtls":        {SecurityScheme{Name: "m", Type: "mutualTLS"}, "`m` — mutual TLS"},
		"other":       {SecurityScheme{Name: "x", Type: "custom"}, "`x` — custom"},
		"untyped":     {SecurityScheme{Name: "x"}, "`x`"},
		"described":   {SecurityScheme{Name: "b", Type: "http", Scheme: "basic", Description: "Use a\nservice account."}, "`b` — HTTP basic authentication. Use a service account."},
	} {
		if got := describeSecurity(tt.sec); got != tt.want {
			t.Errorf("%s: %q, want %q", name, got, tt.want)
		}
	}
}

// A newline or a pipe in a description would end the table row early and
// spill the rest into the document.
func TestCellText(t *testing.T) {
	if got := cellText("line one\r\nline two\n  with | pipe  "); got != `line one line two with \| pipe` {
		t.Errorf("cellText = %q", got)
	}

	long := cellText(strings.Repeat("word ", 200))
	if !strings.HasSuffix(long, "…") || len(long) > 300+len("…") {
		t.Errorf("long cell is %d bytes and ends %q", len(long), long[len(long)-8:])
	}
}

func TestEncodeDetailKeepsASmallDetailWhole(t *testing.T) {
	d := invoiceDetail()

	raw, err := EncodeDetail(d)
	if err != nil {
		t.Fatal(err)
	}

	var back Detail
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Truncated || back.RequestBody == nil || back.Responses[0].Schema == nil {
		t.Errorf("a small detail lost content: %+v", back)
	}
}

func bigSchema(bytes int) *Schema {
	return &Schema{Type: "string", Description: strings.Repeat("d", bytes)}
}

// What a caller sends matters more than what comes back, so the response
// schemas go first.
func TestEncodeDetailDropsResponseSchemasFirst(t *testing.T) {
	d := invoiceDetail()
	d.Responses = []Response{{Status: "200", Description: "OK", Schema: bigSchema(MaxDetailBytes)}}

	raw, err := EncodeDetail(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > MaxDetailBytes {
		t.Fatalf("encoded detail is %d bytes, over the %d limit", len(raw), MaxDetailBytes)
	}

	var back Detail
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Truncated {
		t.Error("a trimmed detail is not marked truncated")
	}
	if back.Responses[0].Schema != nil {
		t.Error("the oversized response schema was kept")
	}
	if back.Responses[0].Status != "200" || back.Responses[0].Description != "OK" {
		t.Errorf("the response itself was lost: %+v", back.Responses)
	}
	if back.RequestBody == nil || back.RequestBody.Schema == nil {
		t.Error("the request body was dropped although dropping responses was enough")
	}
}

func TestEncodeDetailDropsRequestBodyAsLastResort(t *testing.T) {
	d := invoiceDetail()
	d.RequestBody.Schema = bigSchema(MaxDetailBytes)

	raw, err := EncodeDetail(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > MaxDetailBytes {
		t.Fatalf("encoded detail is %d bytes, over the %d limit", len(raw), MaxDetailBytes)
	}

	var back Detail
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Truncated || back.RequestBody != nil {
		t.Errorf("oversized request body survived: truncated=%v body=%v", back.Truncated, back.RequestBody != nil)
	}
	if len(back.Notes) == 0 || !strings.Contains(back.Notes[len(back.Notes)-1], "Schemas omitted") {
		t.Errorf("the reader is not told schemas were omitted: %q", back.Notes)
	}
	if back.Method != "POST" || back.Path != "/v1/invoices" || len(back.Parameters) != 4 {
		t.Errorf("more than the schemas was dropped: %+v", back)
	}
}

// EncodeDetail trims a copy. Both providers render the indexed markdown from
// the same Detail afterwards; if encoding emptied it, an oversized operation
// would lose its response examples from search without any note saying so.
func TestEncodeDetailDoesNotModifyItsArgument(t *testing.T) {
	d := invoiceDetail()
	d.Responses = []Response{{Status: "200", Schema: bigSchema(MaxDetailBytes)}}
	d.RequestBody.Schema = bigSchema(MaxDetailBytes)
	notes := len(d.Notes)
	before := Markdown("billing", d)

	if _, err := EncodeDetail(d); err != nil {
		t.Fatal(err)
	}

	if after := Markdown("billing", d); after != before {
		t.Errorf("markdown rendered after encoding differs: %d bytes before, %d after", len(before), len(after))
	}

	if d.Truncated {
		t.Error("the caller's detail was marked truncated")
	}
	if d.RequestBody == nil || d.RequestBody.Schema == nil {
		t.Error("the caller's request body was removed")
	}
	if len(d.Notes) != notes {
		t.Errorf("the caller's notes grew to %q", d.Notes)
	}
	if d.Responses[0].Schema == nil {
		t.Error("the caller's response schema was removed")
	}
}
