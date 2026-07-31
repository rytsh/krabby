package websource

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ValidateImageURL rejects protocols and literal hosts that must never be
// reached because an imported document referenced them. User-configured page
// URLs may intentionally target an intranet, but transitive image URLs are
// untrusted page content and must not become a localhost/metadata SSRF path.
func ValidateImageURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: image URL must be absolute http(s)", ErrImageUnsupported)
	}
	if u.User != nil {
		return fmt.Errorf("%w: image URL must not contain credentials", ErrImageUnsupported)
	}
	host := strings.ToLower(u.Hostname())
	if !allowPrivate && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		return fmt.Errorf("%w: localhost image URL", ErrImageUnsupported)
	}
	if ip := net.ParseIP(host); !allowPrivate && ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
		return fmt.Errorf("%w: private-network image URL", ErrImageUnsupported)
	}
	return nil
}

// SameOrigin reports whether two absolute URLs use the same scheme and host.
func SameOrigin(a, b string) bool {
	au, err := url.Parse(a)
	if err != nil {
		return false
	}
	bu, err := url.Parse(b)
	return err == nil && strings.EqualFold(au.Scheme, bu.Scheme) && strings.EqualFold(au.Host, bu.Host)
}

// ImageHTTPClient clones base with a direct transport that resolves and dials
// image hosts itself. This closes the DNS-rebinding gap between validating a
// hostname and connecting to it. privateHost may resolve to an internal address;
// every other hostname must resolve to a public address.
func ImageHTTPClient(base *http.Client, privateHost string) *http.Client {
	client := *base
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if current, ok := base.Transport.(*http.Transport); ok {
		transport = current.Clone()
	}
	// A proxy would resolve the destination itself and bypass the guarded dial.
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	privateHost = strings.ToLower(strings.TrimSpace(privateHost))
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split image address: %w", err)
		}
		allowPrivate := strings.EqualFold(host, privateHost)
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve image host: %w", err)
		}
		for _, resolved := range ips {
			if !allowPrivate && privateIP(resolved.IP) {
				continue
			}
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			err = dialErr
		}
		if err != nil {
			return nil, fmt.Errorf("dial image host: %w", err)
		}
		return nil, fmt.Errorf("%w: image host resolves to private network", ErrImageUnsupported)
	}
	client.Transport = transport
	return &client
}

func privateIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	// Shared carrier-grade NAT and benchmarking networks are not classified as
	// private by net.IP, but they are still non-public SSRF targets.
	return ip4[0] == 100 && ip4[1]&0xc0 == 64 || ip4[0] == 198 && ip4[1]&0xfe == 18
}

// ErrImageUnsupported marks an image that should be skipped permanently, such
// as an unsupported MIME type or an oversized response.
var ErrImageUnsupported = errors.New("unsupported image")

// ImageContent is one bounded image fetched by a source provider.
type ImageContent struct {
	Data          []byte
	MediaType     string
	Authenticated bool
}

// ImageFetcher is an optional provider capability used by vision enrichment.
// Implementations reuse the provider's existing authentication rules without
// exposing credentials to the manager or the vision provider.
type ImageFetcher interface {
	FetchImage(ctx context.Context, col *Collection, pageURL, imageURL string, maxBytes int64, allowAuthenticated bool) (ImageContent, error)
}

// MarkdownImage identifies one inline Markdown image and its source range.
type MarkdownImage struct {
	Start int
	End   int
	Alt   string
	URL   string
}

// MarkdownImages returns ordinary inline images (![alt](url)) in source order.
// Reference-style images are intentionally left untouched for the first vision
// implementation because resolving their definitions requires a full Markdown
// document parser.
func MarkdownImages(markdown string) []MarkdownImage {
	var images []MarkdownImage
	for i := 0; i+2 < len(markdown); i++ {
		if markdown[i] != '!' || markdown[i+1] != '[' {
			continue
		}

		labelEnd, ok := imageDelimiter(markdown, i+1, '[', ']')
		if !ok || labelEnd+1 >= len(markdown) || markdown[labelEnd+1] != '(' {
			continue
		}
		destinationEnd, ok := imageDelimiter(markdown, labelEnd+1, '(', ')')
		if !ok {
			continue
		}

		destination := markdown[labelEnd+2 : destinationEnd]
		url := imageDestination(destination)
		if url != "" {
			images = append(images, MarkdownImage{
				Start: i,
				End:   destinationEnd + 1,
				Alt:   markdown[i+2 : labelEnd],
				URL:   url,
			})
		}
		i = destinationEnd
	}

	return images
}

func imageDelimiter(text string, start int, open, close byte) (int, bool) {
	depth := 0
	for i := start; i < len(text); i++ {
		if text[i] == '\\' {
			i++
			continue
		}
		switch text[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

func imageDestination(raw string) string {
	raw = trimSpace(raw)
	if len(raw) >= 2 && raw[0] == '<' {
		for i := 1; i < len(raw); i++ {
			if raw[i] == '>' && raw[i-1] != '\\' {
				return raw[1:i]
			}
		}
	}

	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' {
			i++
			continue
		}
		if raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r' {
			return raw[:i]
		}
	}
	return raw
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
