package grpcreflect

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	testpb "google.golang.org/grpc/reflection/grpc_testing"

	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// searchServer is the minimal implementation needed to register the service;
// the reflection walk never calls it.
type searchServer struct {
	testpb.UnimplementedSearchServiceServer
}

// startServer runs a real gRPC server with reflection enabled on a loopback
// port.
//
// A real server rather than a mock: the whole point of this provider is that it
// speaks the actual reflection protocol, and a hand-written fake would only
// prove that krabby agrees with krabby.
func startServer(t *testing.T) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := grpc.NewServer()
	testpb.RegisterSearchServiceServer(server, &searchServer{})
	reflection.Register(server)

	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	return lis.Addr().String()
}

func configFor(target string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"target":%q,"plaintext":true}`, target))
}

func collect(t *testing.T, svc *apicatalog.Service) (map[string]apicatalog.RemoteOperation, *apicatalog.FetchResult) {
	t.Helper()

	ops := map[string]apicatalog.RemoteOperation{}
	res, err := New().Fetch(context.Background(), svc, svc.State, func(op apicatalog.RemoteOperation) error {
		ops[op.OperationID] = op

		return nil
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	return ops, res
}

func decodeDetail(t *testing.T, op apicatalog.RemoteOperation) apicatalog.Detail {
	t.Helper()

	var d apicatalog.Detail
	if err := json.Unmarshal(op.Detail, &d); err != nil {
		t.Fatalf("decode detail of %s; %v", op.OperationID, err)
	}

	return d
}

func TestFetchEnumeratesMethods(t *testing.T) {
	target := startServer(t)

	svc := &apicatalog.Service{Name: "search", Kind: apicatalog.KindGRPC, Config: configFor(target)}
	ops, res := collect(t, svc)

	if !res.Complete {
		t.Fatalf("Complete = false, want true")
	}
	if res.Info.ResolvedURL != target {
		t.Errorf("ResolvedURL = %q, want %q", res.Info.ResolvedURL, target)
	}

	unary, ok := ops["/grpc.testing.SearchService/Search"]
	if !ok {
		t.Fatalf("Search method not emitted; got %v", keys(ops))
	}
	if _, ok := ops["/grpc.testing.SearchService/StreamingSearch"]; !ok {
		t.Fatalf("StreamingSearch method not emitted; got %v", keys(ops))
	}

	// The reflection service must never appear in the catalog: it describes the
	// mechanism, not the API.
	for id := range ops {
		if strings.Contains(id, "grpc.reflection") {
			t.Errorf("reflection service was catalogued: %s", id)
		}
	}

	if unary.Method != apicatalog.MethodGRPC {
		t.Errorf("Method = %q, want %q", unary.Method, apicatalog.MethodGRPC)
	}
	if len(unary.Tags) != 1 || unary.Tags[0] != "grpc.testing.SearchService" {
		t.Errorf("Tags = %v, want the service name", unary.Tags)
	}

	d := decodeDetail(t, unary)

	if d.RequestBody == nil || d.RequestBody.Schema == nil {
		t.Fatalf("request schema missing")
	}
	if !propertyNames(d.RequestBody.Schema)["query"] {
		t.Errorf("request schema is missing the query field: %+v", d.RequestBody.Schema)
	}

	if len(d.Responses) != 1 || d.Responses[0].Schema == nil {
		t.Fatalf("response schema missing: %+v", d.Responses)
	}
	// A repeated message field must flatten to an array of the nested message.
	results := findProperty(d.Responses[0].Schema, "results")
	if results == nil || results.Schema == nil || results.Schema.Type != "array" {
		t.Fatalf("repeated field did not become an array: %+v", results)
	}
	if !propertyNames(results.Schema.Items)["snippets"] {
		t.Errorf("nested message fields missing: %+v", results.Schema.Items)
	}

	if !strings.HasPrefix(d.Request.Command, "grpcurl -plaintext") {
		t.Errorf("recipe is not a grpcurl command:\n%s", d.Request.Command)
	}
	if !strings.Contains(d.Request.Command, "grpc.testing.SearchService/Search") {
		t.Errorf("recipe does not name the full method:\n%s", d.Request.Command)
	}
}

// TestStreamingIsAnnotated: a caller who does not notice a method is streaming
// writes a client that silently drops every message after the first.
func TestStreamingIsAnnotated(t *testing.T) {
	target := startServer(t)

	svc := &apicatalog.Service{Name: "search", Kind: apicatalog.KindGRPC, Config: configFor(target)}
	ops, _ := collect(t, svc)

	d := decodeDetail(t, ops["/grpc.testing.SearchService/StreamingSearch"])

	var noted bool
	for _, note := range d.Notes {
		if strings.Contains(note, "streaming") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("a bidirectional streaming RPC was not annotated: %v", d.Notes)
	}

	unary := decodeDetail(t, ops["/grpc.testing.SearchService/Search"])
	for _, note := range unary.Notes {
		if strings.Contains(note, "streaming") {
			t.Errorf("a unary RPC was annotated as streaming: %v", unary.Notes)
		}
	}
}

// TestUnchangedByFingerprint: a server that has not been redeployed must not
// cause the catalog to re-embed itself on every poll.
func TestUnchangedByFingerprint(t *testing.T) {
	target := startServer(t)

	svc := &apicatalog.Service{Name: "search", Kind: apicatalog.KindGRPC, Config: configFor(target)}

	_, res := collect(t, svc)
	if res.Unchanged {
		t.Fatalf("first fetch reported Unchanged")
	}
	svc.State = res.State

	ops, res := collect(t, svc)
	if !res.Unchanged {
		t.Fatalf("an unchanged server was not detected as unchanged")
	}
	if len(ops) != 0 {
		t.Errorf("an unchanged fetch emitted %d operations, want 0", len(ops))
	}
}

func TestServiceFilterAndOverrides(t *testing.T) {
	target := startServer(t)

	svc := &apicatalog.Service{
		Name:   "search",
		Kind:   apicatalog.KindGRPC,
		Config: json.RawMessage(fmt.Sprintf(`{"target":%q,"plaintext":true,"services":["grpc.testing.SearchService"]}`, target)),
		Operations: map[string]apicatalog.OperationOverride{
			"/grpc.testing.SearchService/Search":          {Summary: "Run a search"},
			"/grpc.testing.SearchService/StreamingSearch": {Hidden: true},
		},
	}

	ops, _ := collect(t, svc)

	if _, hidden := ops["/grpc.testing.SearchService/StreamingSearch"]; hidden {
		t.Errorf("a hidden method was still emitted")
	}
	if got := ops["/grpc.testing.SearchService/Search"].Summary; got != "Run a search" {
		t.Errorf("summary override not applied: %q", got)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr bool
	}{
		{name: "ok", config: `{"target":"api.internal:443"}`},
		{name: "missing target", config: `{}`, wantErr: true},
		{name: "has scheme", config: `{"target":"https://api.internal:443"}`, wantErr: true},
		{name: "no port", config: `{"target":"api.internal"}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := New().Validate(json.RawMessage(tt.config))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigViewRedactsToken(t *testing.T) {
	p := New()

	raw, err := p.MergeConfig(nil, json.RawMessage(`{"target":"x:443","token":"s3cret"}`))
	if err != nil {
		t.Fatalf("MergeConfig() error = %v", err)
	}

	view, err := json.Marshal(p.ConfigView(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(view), "s3cret") {
		t.Fatalf("ConfigView leaked the token: %s", view)
	}

	merged, err := p.MergeConfig(raw, json.RawMessage(`{"target":"y:443","token":""}`))
	if err != nil {
		t.Fatalf("MergeConfig() error = %v", err)
	}
	cfg, err := decodeConfig(merged)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "s3cret" {
		t.Errorf("token = %q, want the stored secret preserved by a blank update", cfg.Token)
	}
}

func TestPreview(t *testing.T) {
	target := startServer(t)

	out, err := New().Preview(context.Background(), configFor(target), nil)
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if out.OperationCount != 2 {
		t.Errorf("OperationCount = %d, want 2", out.OperationCount)
	}
	if out.BaseURL != target {
		t.Errorf("BaseURL = %q, want %q", out.BaseURL, target)
	}
}

// ---- helpers ---------------------------------------------------------------

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

func propertyNames(s *apicatalog.Schema) map[string]bool {
	out := map[string]bool{}
	if s == nil {
		return out
	}
	for _, p := range s.Properties {
		out[p.Name] = true
	}

	return out
}

func findProperty(s *apicatalog.Schema, name string) *apicatalog.Property {
	if s == nil {
		return nil
	}
	for i := range s.Properties {
		if s.Properties[i].Name == name {
			return &s.Properties[i]
		}
	}

	return nil
}
