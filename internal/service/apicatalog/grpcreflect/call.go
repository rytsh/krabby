package grpcreflect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// Call invokes one catalogued RPC against the live server.
//
// Unlike the HTTP provider, nothing about the method is usable from storage:
// the catalog keeps a flattened, depth-limited *view* of the request message,
// which is enough to describe a call and not nearly enough to encode one. So a
// call re-runs reflection for the one service it needs, builds the real
// descriptor, and encodes the body through it. That costs a round trip per
// call, and it buys correctness — the message on the wire is shaped by what the
// server says it accepts right now, not by a summary written at last sync.
//
// All four RPC kinds are supported. Streaming is expressed through the body:
// a JSON array is a sequence of request messages, and a server-streaming
// response comes back as a JSON array of what arrived.
func (p *Provider) Call(ctx context.Context, svc *apicatalog.Service, d *apicatalog.Detail, req apicatalog.CallRequest) (apicatalog.CallResponse, error) {
	var out apicatalog.CallResponse

	if d == nil {
		return out, errors.New("operation has no stored detail")
	}

	cfg, err := decodeConfig(svc.Config)
	if err != nil {
		return out, err
	}
	if cfg.Target == "" {
		return out, errors.New("grpc target is required")
	}
	// A base URL override repoints the call the same way it repoints the
	// recipe, so an operator can catalog staging and call production.
	if svc.BaseURL != "" {
		cfg.Target = resolveTarget(svc.BaseURL, cfg.Target)
	}

	service, method, err := splitFullMethod(d.Path)
	if err != nil {
		return out, err
	}

	ctx, cancel := context.WithTimeout(ctx, apicatalog.NormalizeTimeout(req.Timeout))
	defer cancel()

	conn, err := dial(cfg)
	if err != nil {
		return out, err
	}
	defer func() { _ = conn.Close() }()

	md, err := resolveMethod(ctx, conn, cfg, service, method)
	if err != nil {
		return out, err
	}

	inputs, err := decodeMessages(req.Body, md)
	if err != nil {
		return out, err
	}
	if !md.IsStreamingClient() && len(inputs) != 1 {
		return out, fmt.Errorf("%s takes exactly one request message, got %d", d.Path, len(inputs))
	}

	out.Method = apicatalog.MethodGRPC
	out.URL = cfg.Target + d.Path
	if md.IsStreamingClient() || md.IsStreamingServer() {
		out.Notes = append(out.Notes, streamingNote(md))
	}

	started := time.Now()
	invokeStream(callMetadata(ctx, cfg, req.Headers), conn, d.Path, md, inputs, &out)
	out.DurationMS = time.Since(started).Milliseconds()

	return out, nil
}

// invokeStream runs the RPC and fills the response.
//
// Every RPC kind goes through grpc.ClientConn.NewStream, including unary: a
// unary call is a stream that carries exactly one message each way, and using
// one path for all four kinds removes the branch where three of them are tested
// and the fourth is not.
func invokeStream(
	ctx context.Context,
	conn *grpc.ClientConn,
	fullMethod string,
	md protoreflect.MethodDescriptor,
	inputs []*dynamicpb.Message,
	out *apicatalog.CallResponse,
) {
	desc := &grpc.StreamDesc{
		StreamName:    string(md.Name()),
		ServerStreams: md.IsStreamingServer(),
		ClientStreams: md.IsStreamingClient(),
	}

	stream, err := conn.NewStream(ctx, desc, fullMethod)
	if err != nil {
		fail(out, err)

		return
	}

	for _, in := range inputs {
		if err := stream.SendMsg(in); err != nil {
			// io.EOF from SendMsg means the server closed early; the real
			// reason is on RecvMsg, so fall through and collect it there.
			if !errors.Is(err, io.EOF) {
				fail(out, err)

				return
			}

			break
		}
	}
	if err := stream.CloseSend(); err != nil {
		fail(out, err)

		return
	}

	messages := make([]json.RawMessage, 0, 1)
	var recvErr error

	for len(messages) < apicatalog.MaxStreamMessages {
		msg := dynamicpb.NewMessage(md.Output())
		if err := stream.RecvMsg(msg); err != nil {
			if !errors.Is(err, io.EOF) {
				recvErr = err
			}

			break
		}

		encoded, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(msg)
		if err != nil {
			recvErr = fmt.Errorf("encode response message; %w", err)

			break
		}
		messages = append(messages, encoded)
	}

	if len(messages) == apicatalog.MaxStreamMessages {
		out.Truncated = true
		out.Notes = append(out.Notes,
			fmt.Sprintf("stopped after %d messages; the stream may have had more", apicatalog.MaxStreamMessages))
	}

	st := status.Convert(recvErr)
	out.StatusText = st.Code().String()
	out.OK = recvErr == nil
	if !out.OK {
		out.Error = st.Message()
	}

	out.Headers = mergeMetadata(header(stream), trailer(stream))
	out.MessageCount = len(messages)
	out.ContentType = "application/json"
	renderMessages(out, messages, md.IsStreamingServer())
}

// renderMessages puts the collected responses into the body.
//
// A server-streaming call always produces an array, even when it happened to
// yield one message: the shape of the answer must depend on the method, not on
// how many messages this particular run saw, or a caller cannot write code
// against it.
func renderMessages(out *apicatalog.CallResponse, messages []json.RawMessage, streaming bool) {
	var raw []byte

	switch {
	case streaming:
		encoded, err := json.MarshalIndent(messages, "", "  ")
		if err != nil {
			out.Notes = append(out.Notes, "could not render the collected messages: "+err.Error())

			return
		}
		raw = encoded
	case len(messages) == 1:
		raw = messages[0]
	default:
		return
	}

	out.BodyBytes = len(raw)

	body, truncated := apicatalog.TruncateBody(raw)
	out.Body = body
	if truncated {
		out.Truncated = true
	}
}

func fail(out *apicatalog.CallResponse, err error) {
	st := status.Convert(err)
	out.StatusText = st.Code().String()
	out.Error = st.Message()
	out.OK = false
}

// header and trailer read a stream's metadata without letting a failure there
// mask the call's own result.
func header(stream grpc.ClientStream) metadata.MD {
	md, err := stream.Header()
	if err != nil {
		return nil
	}

	return md
}

func trailer(stream grpc.ClientStream) metadata.MD { return stream.Trailer() }

func mergeMetadata(sets ...metadata.MD) map[string]string {
	out := map[string]string{}
	for _, md := range sets {
		for name, values := range md {
			out[name] = strings.Join(values, ", ")
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// callMetadata layers the caller's headers over the service's standing
// metadata, matching the HTTP provider: a per-request "authorization" replaces
// the configured token instead of arriving alongside it.
func callMetadata(ctx context.Context, cfg resolvedConfig, headers map[string]string) context.Context {
	md := metadata.MD{}
	for name, value := range cfg.Metadata {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			md.Set(name, value)
		}
	}
	if cfg.Token != "" {
		md.Set("authorization", "Bearer "+cfg.Token)
	}

	for name, value := range headers {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if value == "" {
			delete(md, name)

			continue
		}
		md.Set(name, value)
	}

	if len(md) == 0 {
		return ctx
	}

	return metadata.NewOutgoingContext(ctx, md)
}

// splitFullMethod parses "/pkg.Service/Method".
func splitFullMethod(path string) (string, string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(path), "/")
	service, method, ok := strings.Cut(trimmed, "/")
	if !ok || service == "" || method == "" {
		return "", "", fmt.Errorf("%q is not a gRPC method path (want /package.Service/Method)", path)
	}

	return service, method, nil
}

// resolveMethod re-runs reflection on an open connection and returns the
// descriptor of one method.
//
// Only the target service's symbol is collected, not the whole server: a call
// needs one method's transitive imports, and pulling every service's
// descriptors on every call would make a request to a large server cost more in
// reflection than in the call itself.
func resolveMethod(
	ctx context.Context,
	conn *grpc.ClientConn,
	cfg resolvedConfig,
	service, method string,
) (protoreflect.MethodDescriptor, error) {
	client, _, err := openStream(withMetadata(ctx, cfg), conn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.stream.close() }()

	seen := map[string]*descriptorpb.FileDescriptorProto{}
	if err := client.collectSymbol(service, seen); err != nil {
		return nil, err
	}

	set := &descriptorpb.FileDescriptorSet{File: make([]*descriptorpb.FileDescriptorProto, 0, len(seen))}
	for _, file := range seen {
		set.File = append(set.File, file)
	}

	files, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("build proto registry; %w", err)
	}

	sd := lookupService(files, service)
	if sd == nil {
		return nil, fmt.Errorf("the server does not serve %s", service)
	}

	md := sd.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		return nil, fmt.Errorf("%s has no method %s", service, method)
	}

	return md, nil
}

// decodeMessages turns the request body into protobuf messages.
//
// A JSON array is a sequence of request messages, which is the only spelling
// that lets a client-streaming call be expressed in a single request body. An
// object is one message, and an absent body is one empty message — many RPCs
// take google.protobuf.Empty, and forcing "{}" to be typed out would be noise.
func decodeMessages(body json.RawMessage, md protoreflect.MethodDescriptor) ([]*dynamicpb.Message, error) {
	raw := strings.TrimSpace(string(body))
	if raw == "" || raw == "null" {
		raw = "{}"
	}
	if len(raw) > apicatalog.MaxCallRequestBytes {
		return nil, fmt.Errorf("request body is %d bytes, over the %d limit", len(raw), apicatalog.MaxCallRequestBytes)
	}

	var parts []json.RawMessage
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &parts); err != nil {
			return nil, fmt.Errorf("decode request messages; %w", err)
		}
		if !md.IsStreamingClient() {
			return nil, fmt.Errorf("%s is not client-streaming: send one object, not an array", md.FullName())
		}
	} else {
		parts = []json.RawMessage{json.RawMessage(raw)}
	}

	if len(parts) > apicatalog.MaxStreamMessages {
		return nil, fmt.Errorf("request carries %d messages, over the %d limit", len(parts), apicatalog.MaxStreamMessages)
	}

	out := make([]*dynamicpb.Message, 0, len(parts))
	for i, part := range parts {
		msg := dynamicpb.NewMessage(md.Input())
		if err := (protojson.UnmarshalOptions{}).Unmarshal(part, msg); err != nil {
			return nil, fmt.Errorf("decode request message %d as %s; %w", i+1, md.Input().FullName(), err)
		}
		out = append(out, msg)
	}

	return out, nil
}
