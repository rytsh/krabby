package openapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/apicatalog"
)

// Call invokes one catalogued HTTP operation against the live service.
//
// The request is assembled from three sources with a deliberate precedence:
// the stored operation decides the method and the path shape, the service's
// provider config supplies the host and the standing credentials, and the
// caller fills parameter values and may override individual headers. A caller
// therefore cannot retarget the request — which is the property that makes it
// safe to hand this to a model — but can still say "the same endpoint, with my
// token".
//
// Credentials are the ones configured for fetching the document. That is a
// compromise worth naming: the package doc calls fetching a spec and calling
// the API it describes different acts, and they are. In practice an internal
// service publishes its spec behind the same gateway that guards the API, so
// reusing the configured token is right far more often than it is wrong — and
// when it is wrong the caller overrides Authorization per request.
func (p *Provider) Call(ctx context.Context, svc *apicatalog.Service, d *apicatalog.Detail, req apicatalog.CallRequest) (apicatalog.CallResponse, error) {
	var out apicatalog.CallResponse

	if d == nil {
		return out, errors.New("operation has no stored detail")
	}

	cfg, err := decodeConfig(svc.Config)
	if err != nil {
		return out, err
	}

	target, err := callURL(svc, d, req)
	if err != nil {
		return out, err
	}

	method := strings.ToUpper(strings.TrimSpace(d.Method))
	if method == "" {
		method = http.MethodGet
	}

	body, err := callBody(req)
	if err != nil {
		return out, err
	}

	ctx, cancel := context.WithTimeout(ctx, apicatalog.NormalizeTimeout(req.Timeout))
	defer cancel()

	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return out, fmt.Errorf("build request; %w", err)
	}

	applyCallHeaders(httpReq, cfg, d, req, len(body) > 0)

	out.Method = method
	out.URL = target

	started := time.Now()
	res, err := p.callClient(cfg, target).Do(httpReq)
	out.DurationMS = time.Since(started).Milliseconds()

	if err != nil {
		// A transport failure is reported through CallResponse rather than as a
		// Go error: "the host refused the connection" is a result the caller
		// asked for, not a malfunction of the catalog, and returning it as an
		// error would make the REST and MCP surfaces answer 500 for a working
		// feature.
		out.Error = err.Error()
		out.StatusText = "transport error"

		return out, nil
	}
	defer func() { _ = res.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(res.Body, apicatalog.MaxCallBodyBytes+1))
	if readErr != nil && len(raw) == 0 {
		out.Error = readErr.Error()
		out.StatusText = res.Status

		return out, nil
	}

	out.Status = res.StatusCode
	out.StatusText = res.Status
	out.OK = res.StatusCode >= 200 && res.StatusCode < 300
	out.ContentType = res.Header.Get("Content-Type")
	out.Headers = flattenHeaders(res.Header)
	out.BodyBytes = len(raw)
	out.Body, out.Truncated = apicatalog.TruncateBody(raw)
	out.DurationMS = time.Since(started).Milliseconds()

	if readErr != nil {
		out.Notes = append(out.Notes, "the response body was cut short: "+readErr.Error())
	}

	return out, nil
}

// callURL resolves the operation's templated path against the service's base
// URL and the caller's parameters.
func callURL(svc *apicatalog.Service, d *apicatalog.Detail, req apicatalog.CallRequest) (string, error) {
	// The service override wins over the base URL frozen into the detail at
	// ingest: an operator who repoints base_url expects the next call to go
	// there, not after the next sync.
	base := strings.TrimSpace(svc.BaseURL)
	if base == "" {
		base = strings.TrimSpace(svc.ResolvedBaseURL)
	}
	if base == "" {
		base = strings.TrimSpace(d.BaseURL)
	}
	if base == "" {
		return "", errors.New("the service has no base url: the document declares none, so set base_url on the service")
	}

	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse base url %q; %w", base, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("base url %q must be an http(s) URL", base)
	}

	path, err := apicatalog.ResolvePath(d.Path, req.PathParams)
	if err != nil {
		return "", err
	}

	target := strings.TrimRight(base, "/")
	if path != "" && !strings.HasPrefix(path, "/") {
		target += "/"
	}
	target += path

	return apicatalog.AppendQuery(target, req.Query)
}

// callBody returns the request payload, refusing one that is too large to be an
// interactive test.
func callBody(req apicatalog.CallRequest) ([]byte, error) {
	body := bytes.TrimSpace(req.Body)
	if len(body) == 0 {
		return nil, nil
	}
	if len(body) > apicatalog.MaxCallRequestBytes {
		return nil, fmt.Errorf("request body is %d bytes, over the %d limit", len(body), apicatalog.MaxCallRequestBytes)
	}

	return body, nil
}

// applyCallHeaders layers headers from least to most specific: the content type
// the operation documents, then the service's standing headers and credentials,
// then whatever the caller named. The order is the whole point — a per-request
// Authorization must beat the stored one, or "try this endpoint as another
// user" would be impossible without editing the service.
func applyCallHeaders(
	httpReq *http.Request,
	cfg resolvedConfig,
	d *apicatalog.Detail,
	req apicatalog.CallRequest,
	hasBody bool,
) {
	httpReq.Header.Set("Accept", "application/json, */*")

	if hasBody {
		contentType := "application/json"
		if d.RequestBody != nil && strings.TrimSpace(d.RequestBody.ContentType) != "" {
			contentType = d.RequestBody.ContentType
		}
		httpReq.Header.Set("Content-Type", contentType)
	}

	for name, value := range cfg.Headers {
		if name = strings.TrimSpace(name); name != "" {
			httpReq.Header.Set(name, value)
		}
	}

	switch {
	case cfg.User != "":
		httpReq.SetBasicAuth(cfg.User, cfg.Token)
	case cfg.Token != "":
		httpReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	for name, value := range req.Headers {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if value == "" {
			// An explicitly empty value removes a header the config set. Without
			// this there is no way to call an endpoint unauthenticated on a
			// service that stores a token.
			httpReq.Header.Del(name)

			continue
		}
		httpReq.Header.Set(name, value)
	}
}

// callClient builds the HTTP client for one call.
//
// It has no client-level timeout: the context already bounds the call, and a
// second, shorter deadline here would silently cap what MaxCallTimeout
// promises. Redirects are followed, but the credentials are stripped the moment
// one leaves the origin the operator configured — a redirect is under the
// peer's control, and forwarding a stored token to wherever it points is how a
// misconfigured service becomes a credential leak.
func (p *Provider) callClient(cfg resolvedConfig, target string) *http.Client {
	client := &http.Client{Transport: p.clientFor(cfg).Transport}

	client.CheckRedirect = func(redirect *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if !sameOrigin(target, redirect.URL.String()) {
			redirect.Header.Del("Authorization")
			redirect.Header.Del("Cookie")
			redirect.Header.Del("Proxy-Authorization")
		}

		return nil
	}

	return client
}

// sameOrigin reports whether two URLs share scheme, host and port.
func sameOrigin(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}

	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// flattenHeaders renders response headers as single strings, joining repeats
// the way they appear on the wire.
func flattenHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}

	out := make(map[string]string, len(h))
	for name, values := range h {
		out[name] = strings.Join(values, ", ")
	}

	return out
}
