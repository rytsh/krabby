// Package grpcreflect implements the "grpc" API-catalog provider: it enumerates
// a live gRPC server through the server reflection service and emits one
// catalog operation per RPC method.
//
// Reflection rather than .proto files or a checked-in descriptor set, because
// reflection describes what is actually deployed. A repository's protos
// describe what someone intends to deploy, and the gap between the two is
// exactly the thing a caller gets wrong.
//
// Both reflection versions are spoken. v1 is tried first; servers that only
// register the older v1alpha service — still common outside Go — are retried on
// that. The two protos are wire-compatible, so one request builder serves both
// through a marshal/unmarshal adapter.
package grpcreflect

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/worldline-go/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	v1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/rytsh/krabby/internal/nullx"
	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// dialTimeout bounds connecting and the whole reflection exchange. A server
// that cannot describe itself in this long is not one a catalog can track.
const dialTimeout = 60 * time.Second

// maxDescriptorFiles bounds how many descriptor files one sync will pull. A
// reflection walk follows imports transitively, and a misconfigured target can
// otherwise stream indefinitely.
const maxDescriptorFiles = 5000

// reflectionServices are the service names the reflection API itself exposes.
// They describe the reflection mechanism, not the API, so they are never
// catalogued.
var reflectionServices = map[string]bool{
	"grpc.reflection.v1.ServerReflection":      true,
	"grpc.reflection.v1alpha.ServerReflection": true,
	"grpc.health.v1.Health":                    true,
	"grpc.channelz.v1.Channelz":                true,
}

// Provider enumerates one gRPC server per service.
type Provider struct{}

// New creates the provider.
func New() *Provider { return &Provider{} }

// Config is owned entirely by this provider.
type Config struct {
	// Target is the gRPC address, "host:port".
	Target types.Null[string] `json:"target"`

	// Plaintext disables TLS. Internal service meshes routinely terminate TLS
	// at the sidecar and speak h2c behind it.
	Plaintext types.Null[bool] `json:"plaintext,omitempty"`
	// InsecureSkipVerify keeps TLS but stops verifying the certificate, for
	// servers behind a private CA the krabby host does not trust.
	InsecureSkipVerify types.Null[bool] `json:"insecure_skip_verify,omitempty"`
	// ServerName overrides the TLS SNI/verification name.
	ServerName types.Null[string] `json:"server_name,omitempty"`

	// Token is sent as "authorization: Bearer <token>" metadata on the
	// reflection call. Write-only.
	Token types.Null[string] `json:"token,omitempty"`
	// Metadata is extra call metadata for the reflection request.
	Metadata types.Null[map[string]string] `json:"metadata,omitempty"`

	// Services restricts the catalog to these fully-qualified service names.
	// Empty catalogues everything the server reports.
	Services types.Null[[]string] `json:"services,omitempty"`
}

type resolvedConfig struct {
	Target             string
	Plaintext          bool
	InsecureSkipVerify bool
	ServerName         string
	Token              string
	Metadata           map[string]string
	Services           []string
}

func (c Config) resolve() resolvedConfig {
	return resolvedConfig{
		Target:             strings.TrimSpace(c.Target.ValueOrZero()),
		Plaintext:          c.Plaintext.ValueOrZero(),
		InsecureSkipVerify: c.InsecureSkipVerify.ValueOrZero(),
		ServerName:         strings.TrimSpace(c.ServerName.ValueOrZero()),
		Token:              c.Token.ValueOrZero(), // never trimmed
		Metadata:           c.Metadata.ValueOrZero(),
		Services:           c.Services.ValueOrZero(),
	}
}

type configView struct {
	Target             string            `json:"target"`
	Plaintext          bool              `json:"plaintext,omitempty"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify,omitempty"`
	ServerName         string            `json:"server_name,omitempty"`
	TokenSet           bool              `json:"token_set"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Services           []string          `json:"services,omitempty"`
}

func decodeConfig(raw json.RawMessage) (resolvedConfig, error) {
	cfg, err := decodeRawConfig(raw)
	if err != nil {
		return resolvedConfig{}, err
	}

	return cfg.resolve(), nil
}

func decodeRawConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode grpc config; %w", err)
		}
	}

	return cfg, nil
}

// ---- apicatalog.Provider ----------------------------------------------------

func (p *Provider) Validate(raw json.RawMessage) error {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return err
	}

	if cfg.Target == "" {
		return errors.New("grpc target is required")
	}
	if strings.Contains(cfg.Target, "://") {
		return errors.New("grpc target must be host:port, without a scheme")
	}
	if !strings.Contains(cfg.Target, ":") {
		return errors.New("grpc target must include a port")
	}

	return nil
}

func (p *Provider) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	next, err := decodeRawConfig(update)
	if err != nil {
		return nil, err
	}

	if len(current) != 0 {
		prev, err := decodeRawConfig(current)
		if err != nil {
			return nil, err
		}

		next.Target = nullx.Merge(next.Target, prev.Target)
		next.Plaintext = nullx.Merge(next.Plaintext, prev.Plaintext)
		next.InsecureSkipVerify = nullx.Merge(next.InsecureSkipVerify, prev.InsecureSkipVerify)
		next.ServerName = nullx.Merge(next.ServerName, prev.ServerName)
		next.Metadata = nullx.Merge(next.Metadata, prev.Metadata)
		next.Services = nullx.Merge(next.Services, prev.Services)

		// Tokens are write-only: an absent or blank incoming token keeps the
		// stored one.
		if next.Token.ValueOrZero() == "" {
			next.Token = prev.Token
		}
	}

	raw, err := json.Marshal(next)
	if err != nil {
		return nil, fmt.Errorf("encode grpc config; %w", err)
	}
	if err := p.Validate(raw); err != nil {
		return nil, err
	}

	return raw, nil
}

func (p *Provider) ConfigView(raw json.RawMessage) any {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return nil
	}

	return configView{
		Target:             cfg.Target,
		Plaintext:          cfg.Plaintext,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		ServerName:         cfg.ServerName,
		TokenSet:           cfg.Token != "",
		Metadata:           cfg.Metadata,
		Services:           cfg.Services,
	}
}

// syncState is the opaque watermark persisted between syncs.
type syncState struct {
	// Fingerprint hashes the sorted descriptor set. A server whose deployment
	// has not changed produces the same bytes, so the catalog skips re-render.
	Fingerprint string `json:"fingerprint,omitempty"`
	V           int    `json:"v,omitempty"`
}

// renderGeneration is bumped whenever the rendered output shape changes.
const renderGeneration = 1

// Fetch enumerates the server and emits one operation per RPC method.
func (p *Provider) Fetch(ctx context.Context, svc *apicatalog.Service, rawState json.RawMessage, emit apicatalog.Emit) (*apicatalog.FetchResult, error) {
	cfg, err := decodeConfig(svc.Config)
	if err != nil {
		return nil, err
	}
	if cfg.Target == "" {
		return nil, errors.New("grpc target is required")
	}

	var state syncState
	if len(rawState) != 0 {
		_ = json.Unmarshal(rawState, &state)
	}
	fresh := state.V < renderGeneration

	set, services, err := describe(ctx, cfg)
	if err != nil {
		return nil, err
	}

	fingerprint, err := fingerprintSet(set)
	if err != nil {
		return nil, err
	}

	if !fresh && fingerprint == state.Fingerprint {
		next, err := json.Marshal(syncState{Fingerprint: fingerprint, V: renderGeneration})
		if err != nil {
			return nil, fmt.Errorf("encode grpc sync state; %w", err)
		}

		return &apicatalog.FetchResult{Unchanged: true, State: next}, nil
	}

	files, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("build proto registry; %w", err)
	}

	info := apicatalog.ServiceInfo{
		Title:       svc.Name,
		ResolvedURL: resolveTarget(svc.BaseURL, cfg.Target),
	}

	if err := walk(svc, files, services, cfg, info.ResolvedURL, emit); err != nil {
		return nil, err
	}

	next, err := json.Marshal(syncState{Fingerprint: fingerprint, V: renderGeneration})
	if err != nil {
		return nil, fmt.Errorf("encode grpc sync state; %w", err)
	}

	return &apicatalog.FetchResult{Complete: true, Info: info, State: next}, nil
}

// Preview probes unsaved config, reporting what the server says it serves.
func (p *Provider) Preview(ctx context.Context, raw json.RawMessage, _ json.RawMessage) (apicatalog.PreviewResult, error) {
	var out apicatalog.PreviewResult

	if err := p.Validate(raw); err != nil {
		return out, err
	}
	cfg, err := decodeConfig(raw)
	if err != nil {
		return out, err
	}

	set, services, err := describe(ctx, cfg)
	if err != nil {
		return out, err
	}

	files, err := protodesc.NewFiles(set)
	if err != nil {
		return out, fmt.Errorf("build proto registry; %w", err)
	}

	out.BaseURL = cfg.Target
	for _, name := range services {
		sd := lookupService(files, name)
		if sd == nil {
			continue
		}

		methods := sd.Methods()
		out.OperationCount += methods.Len()
		if len(out.Sample) < 10 {
			out.Sample = append(out.Sample, "/"+string(sd.FullName())+"/"+string(methods.Get(0).Name()))
		}
	}

	return out, nil
}

// walk emits one operation per method of every catalogued service.
func walk(
	svc *apicatalog.Service,
	files *protoregistry.Files,
	services []string,
	cfg resolvedConfig,
	target string,
	emit apicatalog.Emit,
) error {
	security := grpcSecurity(cfg)

	for _, name := range services {
		sd := lookupService(files, name)
		if sd == nil {
			continue
		}

		methods := sd.Methods()
		for i := range methods.Len() {
			md := methods.Get(i)

			f := newFlattener()
			fullMethod := "/" + string(sd.FullName()) + "/" + string(md.Name())

			d := &apicatalog.Detail{
				Method:      apicatalog.MethodGRPC,
				Path:        fullMethod,
				BaseURL:     target,
				Summary:     string(sd.Name()) + "." + string(md.Name()),
				Description: comment(md),
				Tags:        []string{string(sd.FullName())},
				Security:    security,
				RequestBody: &apicatalog.Body{
					ContentType: "application/json",
					Required:    true,
					Schema:      f.message(md.Input(), 0),
				},
				Responses: []apicatalog.Response{{
					Status:      "OK",
					ContentType: "application/json",
					Schema:      f.message(md.Output(), 0),
				}},
			}

			if md.IsStreamingClient() || md.IsStreamingServer() {
				d.Notes = append(d.Notes, streamingNote(md))
			}
			if md.Options() != nil && md.Options().(*descriptorpb.MethodOptions).GetDeprecated() {
				d.Deprecated = true
			}

			if svc.BaseURL != "" {
				d.Notes = append(d.Notes, "Target overridden in krabby: "+svc.BaseURL)
			}

			// Overrides are applied here rather than in a shared helper because
			// the recipe differs: gRPC needs grpcurl, not curl.
			operationID := fullMethod
			override, ok := svc.Override(operationID)
			if ok {
				if override.Hidden {
					continue
				}
				if override.Summary != "" {
					d.Summary = override.Summary
				}
				if override.Description != "" {
					d.Description = override.Description
				}
				if len(override.Tags) > 0 {
					d.Tags = override.Tags
				}
				d.Notes = append(d.Notes, "Some fields on this method were overridden in krabby.")
			}

			if f.truncated {
				d.Truncated = true
			}

			apicatalog.BuildGRPCRecipe(d, target, cfg.Plaintext)

			detail, err := apicatalog.EncodeDetail(d)
			if err != nil {
				return err
			}

			if err := emit(apicatalog.RemoteOperation{
				OpSlug:      apicatalog.OpSlug(apicatalog.MethodGRPC, fullMethod),
				OperationID: operationID,
				Method:      apicatalog.MethodGRPC,
				Path:        fullMethod,
				Summary:     d.Summary,
				Description: d.Description,
				Tags:        d.Tags,
				Deprecated:  d.Deprecated,
				Detail:      detail,
				Markdown:    apicatalog.Markdown(svc.Name, d),
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

func streamingNote(md protoreflect.MethodDescriptor) string {
	switch {
	case md.IsStreamingClient() && md.IsStreamingServer():
		return "Bidirectional streaming RPC: the example shows one request message of the stream."
	case md.IsStreamingClient():
		return "Client-streaming RPC: the example shows one request message of the stream."
	default:
		return "Server-streaming RPC: the response is a stream of the message shown."
	}
}

// comment returns the leading proto comment of a descriptor, when the server's
// descriptors carry source info. Most do not — protoc strips it unless asked —
// so this is a bonus, not something to depend on.
func comment(d protoreflect.Descriptor) string {
	loc := d.ParentFile().SourceLocations().ByDescriptor(d)

	return strings.TrimSpace(loc.LeadingComments)
}

func lookupService(files *protoregistry.Files, name string) protoreflect.ServiceDescriptor {
	desc, err := files.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		return nil
	}

	sd, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok || sd.Methods().Len() == 0 {
		return nil
	}

	return sd
}

func grpcSecurity(cfg resolvedConfig) []apicatalog.SecurityScheme {
	if cfg.Token == "" {
		return nil
	}

	return []apicatalog.SecurityScheme{{
		Name: "authorization", Type: "http", Scheme: "bearer",
		Description: "Sent as gRPC metadata.",
	}}
}

func resolveTarget(override, target string) string {
	if override != "" {
		return strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(override, "https://"), "http://"), "/")
	}

	return target
}

// fingerprintSet hashes a descriptor set deterministically.
//
// The files are sorted by name first: reflection returns them in whatever order
// the server walked its imports, and that order is not stable across restarts.
// Hashing the unsorted set would report every restart as a change and re-embed
// the whole catalog.
func fingerprintSet(set *descriptorpb.FileDescriptorSet) (string, error) {
	sorted := &descriptorpb.FileDescriptorSet{File: make([]*descriptorpb.FileDescriptorProto, len(set.File))}
	copy(sorted.File, set.File)
	sort.Slice(sorted.File, func(i, j int) bool {
		return sorted.File[i].GetName() < sorted.File[j].GetName()
	})

	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(sorted)
	if err != nil {
		return "", fmt.Errorf("fingerprint descriptors; %w", err)
	}

	return apicatalog.Hash(string(raw)), nil
}

// ---- reflection transport ---------------------------------------------------

// describe connects to the target and returns its descriptor set plus the
// service names worth cataloguing.
func describe(ctx context.Context, cfg resolvedConfig) (*descriptorpb.FileDescriptorSet, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, err := dial(cfg)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()

	ctx = withMetadata(ctx, cfg)

	client, err := openStream(ctx, conn)
	if err != nil {
		return nil, nil, err
	}

	names, err := client.listServices()
	if err != nil {
		return nil, nil, err
	}

	wanted := selectServices(names, cfg.Services)
	if len(wanted) == 0 {
		return nil, nil, errors.New("the server reports no catalogable services")
	}

	seen := map[string]*descriptorpb.FileDescriptorProto{}
	for _, name := range wanted {
		if err := client.collectSymbol(name, seen); err != nil {
			return nil, nil, err
		}
	}

	set := &descriptorpb.FileDescriptorSet{File: make([]*descriptorpb.FileDescriptorProto, 0, len(seen))}
	for _, file := range seen {
		set.File = append(set.File, file)
	}

	return set, wanted, nil
}

// selectServices drops the reflection plumbing and applies the configured
// allow-list.
func selectServices(reported, allowed []string) []string {
	allow := map[string]bool{}
	for _, name := range allowed {
		if name = strings.TrimSpace(name); name != "" {
			allow[name] = true
		}
	}

	out := make([]string, 0, len(reported))
	for _, name := range reported {
		if reflectionServices[name] {
			continue
		}
		if len(allow) > 0 && !allow[name] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)

	return out
}

func dial(cfg resolvedConfig) (*grpc.ClientConn, error) {
	var creds grpc.DialOption
	if cfg.Plaintext {
		creds = grpc.WithTransportCredentials(insecure.NewCredentials())
	} else {
		tlsCfg := &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // operator-set, for private CAs
			ServerName:         cfg.ServerName,
		}
		creds = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	}

	conn, err := grpc.NewClient(cfg.Target, creds)
	if err != nil {
		return nil, fmt.Errorf("dial %s; %w", cfg.Target, err)
	}

	return conn, nil
}

func withMetadata(ctx context.Context, cfg resolvedConfig) context.Context {
	md := metadata.MD{}
	for name, value := range cfg.Metadata {
		md.Set(strings.ToLower(strings.TrimSpace(name)), value)
	}
	if cfg.Token != "" {
		md.Set("authorization", "Bearer "+cfg.Token)
	}
	if len(md) == 0 {
		return ctx
	}

	return metadata.NewOutgoingContext(ctx, md)
}

// reflectStream is the version-independent view of a reflection exchange.
type reflectStream interface {
	send(*v1.ServerReflectionRequest) error
	recv() (*v1.ServerReflectionResponse, error)
}

// v1Stream speaks the current reflection service.
type v1Stream struct {
	stream grpc.BidiStreamingClient[v1.ServerReflectionRequest, v1.ServerReflectionResponse]
}

func (s v1Stream) send(req *v1.ServerReflectionRequest) error { return s.stream.Send(req) }

func (s v1Stream) recv() (*v1.ServerReflectionResponse, error) { return s.stream.Recv() }

// v1AlphaStream speaks the legacy reflection service, translating through the
// wire format.
//
// The v1 and v1alpha protos are field-for-field identical — v1alpha was
// promoted, not redesigned — so a marshal/unmarshal round trip is a faithful
// conversion and needs no per-field mapping to keep in sync.
type v1AlphaStream struct {
	stream grpc.BidiStreamingClient[v1alpha.ServerReflectionRequest, v1alpha.ServerReflectionResponse]
}

func (s v1AlphaStream) send(req *v1.ServerReflectionRequest) error {
	raw, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode reflection request; %w", err)
	}

	var out v1alpha.ServerReflectionRequest
	if err := proto.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("convert reflection request; %w", err)
	}

	return s.stream.Send(&out)
}

func (s v1AlphaStream) recv() (*v1.ServerReflectionResponse, error) {
	in, err := s.stream.Recv()
	if err != nil {
		return nil, err
	}

	raw, err := proto.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("encode reflection response; %w", err)
	}

	var out v1.ServerReflectionResponse
	if err := proto.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("convert reflection response; %w", err)
	}

	return &out, nil
}

// reflectClient drives one reflection exchange.
type reflectClient struct {
	stream reflectStream
	files  int
}

// openStream opens a v1 reflection stream, falling back to v1alpha when the
// server does not implement v1.
//
// The fallback has to probe with a real request, not just open the stream: gRPC
// does not report an unimplemented service until the first message is
// exchanged, so a stream to a v1alpha-only server opens cleanly and fails on
// use.
func openStream(ctx context.Context, conn *grpc.ClientConn) (*reflectClient, error) {
	if stream, err := v1.NewServerReflectionClient(conn).ServerReflectionInfo(ctx); err == nil {
		client := &reflectClient{stream: v1Stream{stream: stream}}
		if _, err := client.listServices(); err == nil {
			return client, nil
		} else if !isUnimplemented(err) {
			return nil, err
		}
	}

	stream, err := v1alpha.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("open reflection stream; %w", err)
	}

	return &reflectClient{stream: v1AlphaStream{stream: stream}}, nil
}

func isUnimplemented(err error) bool {
	return status.Code(err) == codes.Unimplemented
}

func (c *reflectClient) listServices() ([]string, error) {
	if err := c.stream.send(&v1.ServerReflectionRequest{
		MessageRequest: &v1.ServerReflectionRequest_ListServices{ListServices: ""},
	}); err != nil {
		return nil, fmt.Errorf("request service list; %w", err)
	}

	resp, err := c.stream.recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the server closed the reflection stream without answering")
		}

		return nil, fmt.Errorf("read service list; %w", err)
	}

	if errResp := resp.GetErrorResponse(); errResp != nil {
		return nil, fmt.Errorf("reflection error: %s", errResp.GetErrorMessage())
	}

	list := resp.GetListServicesResponse()
	if list == nil {
		return nil, errors.New("the server did not answer the service list request")
	}

	names := make([]string, 0, len(list.GetService()))
	for _, svc := range list.GetService() {
		names = append(names, svc.GetName())
	}

	return names, nil
}

// collectSymbol fetches the descriptor file containing a symbol plus, by
// following its imports, everything it depends on.
func (c *reflectClient) collectSymbol(symbol string, seen map[string]*descriptorpb.FileDescriptorProto) error {
	if err := c.stream.send(&v1.ServerReflectionRequest{
		MessageRequest: &v1.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol},
	}); err != nil {
		return fmt.Errorf("request descriptors for %s; %w", symbol, err)
	}

	resp, err := c.stream.recv()
	if err != nil {
		return fmt.Errorf("read descriptors for %s; %w", symbol, err)
	}
	if errResp := resp.GetErrorResponse(); errResp != nil {
		return fmt.Errorf("reflection error for %s: %s", symbol, errResp.GetErrorMessage())
	}

	return c.absorb(resp, seen)
}

// collectFile fetches one descriptor file by name.
func (c *reflectClient) collectFile(name string, seen map[string]*descriptorpb.FileDescriptorProto) error {
	if err := c.stream.send(&v1.ServerReflectionRequest{
		MessageRequest: &v1.ServerReflectionRequest_FileByFilename{FileByFilename: name},
	}); err != nil {
		return fmt.Errorf("request descriptor %s; %w", name, err)
	}

	resp, err := c.stream.recv()
	if err != nil {
		return fmt.Errorf("read descriptor %s; %w", name, err)
	}
	if errResp := resp.GetErrorResponse(); errResp != nil {
		// A missing import is not fatal: protodesc will fail later only if the
		// gap actually matters, and refusing the whole server because one
		// well-known type could not be re-served is worse than trying.
		return nil
	}

	return c.absorb(resp, seen)
}

// absorb records the descriptor files in a response and recursively fetches any
// import that is still missing.
func (c *reflectClient) absorb(resp *v1.ServerReflectionResponse, seen map[string]*descriptorpb.FileDescriptorProto) error {
	fdResp := resp.GetFileDescriptorResponse()
	if fdResp == nil {
		return nil
	}

	var pending []string

	for _, raw := range fdResp.GetFileDescriptorProto() {
		if c.files >= maxDescriptorFiles {
			return fmt.Errorf("the server returned more than %d descriptor files", maxDescriptorFiles)
		}

		fd := &descriptorpb.FileDescriptorProto{}
		if err := proto.Unmarshal(raw, fd); err != nil {
			return fmt.Errorf("decode file descriptor; %w", err)
		}
		if _, ok := seen[fd.GetName()]; ok {
			continue
		}

		seen[fd.GetName()] = fd
		c.files++

		for _, dep := range fd.GetDependency() {
			if _, ok := seen[dep]; !ok {
				pending = append(pending, dep)
			}
		}
	}

	// Recurse only after the current response is fully recorded, so a diamond
	// import is fetched once instead of once per path to it.
	for _, dep := range pending {
		if _, ok := seen[dep]; ok {
			continue
		}
		if err := c.collectFile(dep, seen); err != nil {
			return err
		}
	}

	return nil
}
