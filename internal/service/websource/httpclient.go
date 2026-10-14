package websource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/strutil"
)

// DefaultHTTPClient is the client providers use when they need no special
// transport.
func DefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}

// ApplyAuth applies the Atlassian-style convention used across providers: a
// user selects basic auth with token as the password, a bare token is sent as
// a Bearer token, and neither leaves the request anonymous.
func ApplyAuth(req *http.Request, user, token string) {
	switch {
	case user != "":
		req.SetBasicAuth(user, token)
	case token != "":
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// Response is a bounded HTTP response body.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	URL    string
}

// Do sends req and reads at most limit bytes of the body. Non-2xx responses
// become errors carrying a truncated body excerpt.
func Do(client *http.Client, req *http.Request, limit int64) (*Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "krabby-websource/1.0")
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s; %w", req.Method, redactURL(req.URL), err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s; %w", redactURL(req.URL), err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s response exceeds %d bytes", redactURL(req.URL), limit)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("%s %s: status %s: %s", req.Method, redactURL(req.URL), res.Status,
			strutil.Truncate(strings.TrimSpace(string(body)), 300))
	}
	finalURL := req.URL.String()
	if res.Request != nil && res.Request.URL != nil {
		finalURL = res.Request.URL.String()
	}

	return &Response{Status: res.StatusCode, Header: res.Header, Body: body, URL: finalURL}, nil
}

// GetJSON issues a GET and decodes the JSON body into out. headers are added
// verbatim (e.g. API version headers).
func GetJSON(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, out any) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request; %w", err)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := Do(client, req, 64<<20)
	if err != nil {
		return nil, err
	}
	if out != nil {
		if err := json.Unmarshal(res.Body, out); err != nil {
			return nil, fmt.Errorf("decode %s; %w", redactURL(req.URL), err)
		}
	}

	return res, nil
}

// redactURL drops query values, which commonly carry API keys.
func redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.User = nil
	if c.RawQuery != "" {
		c.RawQuery = "…"
	}

	return c.String()
}

// ValidHTTPURL reports whether raw is an absolute http(s) URL without
// embedded credentials.
func ValidHTTPURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL")
	}
	if u.User != nil {
		return fmt.Errorf("must not contain credentials")
	}

	return nil
}

// NextLink returns the rel="next" target of an RFC 8288 Link header.
func NextLink(h http.Header) string {
	for _, v := range h.Values("Link") {
		for part := range strings.SplitSeq(v, ",") {
			segs := strings.Split(part, ";")
			if len(segs) < 2 {
				continue
			}
			target := strings.Trim(strings.TrimSpace(segs[0]), "<>")
			for _, p := range segs[1:] {
				p = strings.ReplaceAll(strings.TrimSpace(p), " ", "")
				if p == `rel="next"` || p == "rel=next" {
					return target
				}
			}
		}
	}

	return ""
}
