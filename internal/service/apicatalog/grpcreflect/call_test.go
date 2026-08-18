package grpcreflect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	testpb "google.golang.org/grpc/reflection/grpc_testing"
	"google.golang.org/grpc/status"

	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// echoSearchServer implements the test service with observable behaviour: the
// unary call echoes the query, the streaming call echoes every message, and a
// magic query triggers an error so status propagation is testable.
type echoSearchServer struct {
	testpb.UnimplementedSearchServiceServer

	lastAuth string
}

func (s *echoSearchServer) Search(ctx context.Context, req *testpb.SearchRequest) (*testpb.SearchResponse, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("authorization"); len(v) > 0 {
			s.lastAuth = v[0]
		} else {
			s.lastAuth = ""
		}
	}

	if req.GetQuery() == "explode" {
		return nil, status.Error(codes.FailedPrecondition, "told to explode")
	}

	return &testpb.SearchResponse{
		Results: []*testpb.SearchResponse_Result{{Url: "https://x/" + req.GetQuery()}},
	}, nil
}

func (s *echoSearchServer) StreamingSearch(stream testpb.SearchService_StreamingSearchServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&testpb.SearchResponse{
			Results: []*testpb.SearchResponse_Result{{Url: "https://x/" + req.GetQuery()}},
		}); err != nil {
			return err
		}
	}
}

func startCallServer(t *testing.T) (string, *echoSearchServer) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	impl := &echoSearchServer{}
	server := grpc.NewServer()
	testpb.RegisterSearchServiceServer(server, impl)
	reflection.Register(server)

	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	return lis.Addr().String(), impl
}

func callService(target string) *apicatalog.Service {
	return &apicatalog.Service{
		Name:   "search",
		Kind:   apicatalog.KindGRPC,
		Config: json.RawMessage(fmt.Sprintf(`{"target":%q,"plaintext":true,"token":"stored-token"}`, target)),
	}
}

func grpcDetail(path string) *apicatalog.Detail {
	return &apicatalog.Detail{Method: apicatalog.MethodGRPC, Path: path}
}

func TestGRPCCallUnary(t *testing.T) {
	target, impl := startCallServer(t)

	res, err := New().Call(context.Background(), callService(target),
		grpcDetail("/grpc.testing.SearchService/Search"),
		apicatalog.CallRequest{Body: json.RawMessage(`{"query":"hello"}`)})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}

	if !res.OK || res.StatusText != "OK" {
		t.Fatalf("response = %+v, want OK", res)
	}
	if !strings.Contains(res.Body, "https://x/hello") {
		t.Fatalf("body = %q, want the echoed query", res.Body)
	}
	if res.MessageCount != 1 {
		t.Fatalf("MessageCount = %d, want 1", res.MessageCount)
	}
	// A unary response is one object, not a one-element array.
	if strings.HasPrefix(strings.TrimSpace(res.Body), "[") {
		t.Fatalf("unary response came back as an array:\n%s", res.Body)
	}
	if impl.lastAuth != "Bearer stored-token" {
		t.Fatalf("authorization = %q, want the stored token", impl.lastAuth)
	}
}

// TestGRPCCallEmptyBody: many RPCs take an empty message, and requiring "{}"
// would be noise. The default must be one empty message, not zero messages.
func TestGRPCCallEmptyBody(t *testing.T) {
	target, _ := startCallServer(t)

	res, err := New().Call(context.Background(), callService(target),
		grpcDetail("/grpc.testing.SearchService/Search"), apicatalog.CallRequest{})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !res.OK {
		t.Fatalf("response = %+v, want OK", res)
	}
}

func TestGRPCCallBidiStreaming(t *testing.T) {
	target, _ := startCallServer(t)

	res, err := New().Call(context.Background(), callService(target),
		grpcDetail("/grpc.testing.SearchService/StreamingSearch"),
		apicatalog.CallRequest{Body: json.RawMessage(`[{"query":"a"},{"query":"b"},{"query":"c"}]`)})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}

	if !res.OK {
		t.Fatalf("response = %+v, want OK", res)
	}
	if res.MessageCount != 3 {
		t.Fatalf("MessageCount = %d, want 3", res.MessageCount)
	}
	// A streaming response is always an array, regardless of message count.
	if !strings.HasPrefix(strings.TrimSpace(res.Body), "[") {
		t.Fatalf("streaming response is not an array:\n%s", res.Body)
	}
	for _, q := range []string{"https://x/a", "https://x/b", "https://x/c"} {
		if !strings.Contains(res.Body, q) {
			t.Fatalf("body missing %q:\n%s", q, res.Body)
		}
	}
}

// TestGRPCCallStatusIsAResult: a FailedPrecondition from the server is the
// answer, not a krabby failure.
func TestGRPCCallStatusIsAResult(t *testing.T) {
	target, _ := startCallServer(t)

	res, err := New().Call(context.Background(), callService(target),
		grpcDetail("/grpc.testing.SearchService/Search"),
		apicatalog.CallRequest{Body: json.RawMessage(`{"query":"explode"}`)})
	if err != nil {
		t.Fatalf("Call() error = %v, want the status in the response", err)
	}

	if res.OK || res.StatusText != "FailedPrecondition" {
		t.Fatalf("response = %+v, want FailedPrecondition", res)
	}
	if !strings.Contains(res.Error, "told to explode") {
		t.Fatalf("Error = %q, want the server's message", res.Error)
	}
}

func TestGRPCCallMetadataOverride(t *testing.T) {
	target, impl := startCallServer(t)

	// The caller's authorization must replace the stored token.
	_, err := New().Call(context.Background(), callService(target),
		grpcDetail("/grpc.testing.SearchService/Search"),
		apicatalog.CallRequest{
			Body:    json.RawMessage(`{"query":"x"}`),
			Headers: map[string]string{"Authorization": "Bearer caller-token"},
		})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if impl.lastAuth != "Bearer caller-token" {
		t.Fatalf("authorization = %q, want the caller's token to win", impl.lastAuth)
	}

	// And an explicitly empty one must remove it.
	_, err = New().Call(context.Background(), callService(target),
		grpcDetail("/grpc.testing.SearchService/Search"),
		apicatalog.CallRequest{
			Body:    json.RawMessage(`{"query":"x"}`),
			Headers: map[string]string{"Authorization": ""},
		})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if impl.lastAuth != "" {
		t.Fatalf("authorization = %q, want it removed", impl.lastAuth)
	}
}

func TestGRPCCallRejectsWrongShapes(t *testing.T) {
	target, _ := startCallServer(t)
	svc := callService(target)

	// An array body on a unary method is a caller mistake, refused before any
	// message is sent.
	_, err := New().Call(context.Background(), svc,
		grpcDetail("/grpc.testing.SearchService/Search"),
		apicatalog.CallRequest{Body: json.RawMessage(`[{"query":"a"},{"query":"b"}]`)})
	if err == nil || !strings.Contains(err.Error(), "not client-streaming") {
		t.Fatalf("error = %v, want a not-client-streaming refusal", err)
	}

	// A field the message does not have must be refused by protojson, telling
	// the caller which message shape was expected.
	_, err = New().Call(context.Background(), svc,
		grpcDetail("/grpc.testing.SearchService/Search"),
		apicatalog.CallRequest{Body: json.RawMessage(`{"nope":1}`)})
	if err == nil || !strings.Contains(err.Error(), "grpc.testing.SearchRequest") {
		t.Fatalf("error = %v, want a decode error naming the message", err)
	}

	// An unknown method is refused after reflection, naming the service.
	_, err = New().Call(context.Background(), svc,
		grpcDetail("/grpc.testing.SearchService/Missing"), apicatalog.CallRequest{})
	if err == nil || !strings.Contains(err.Error(), "no method Missing") {
		t.Fatalf("error = %v, want an unknown-method refusal", err)
	}
}
