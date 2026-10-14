// Package filesrc holds what the file-like web-source providers (fs, s3,
// webdav, gdrive, sharepoint) share: selecting convertible files and turning
// a listed file into a page through the shared converter, on top of
// websource.ListingSync's incremental protocol.
package filesrc

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/convert"
)

// DefaultMaxFileBytes bounds one downloaded file.
const DefaultMaxFileBytes = 32 << 20

// Download fetches one listed file's bytes and declared content type.
type Download func(ctx context.Context, item websource.ListedItem) (data []byte, contentType string, err error)

// Common is the config every file-like provider accepts.
type Common struct {
	Include      []string `json:"include,omitempty"`
	Exclude      []string `json:"exclude,omitempty"`
	MaxFileBytes int64    `json:"max_file_bytes,omitempty"`
	MaxFiles     int      `json:"max_files,omitempty"`
}

// Fields returns the descriptor fields of Common.
func Fields() []websource.Field {
	return []websource.Field{
		{Key: "include", Label: "Include globs", Kind: websource.FieldList, Placeholder: "docs/**, **/*.md"},
		{Key: "exclude", Label: "Exclude globs", Kind: websource.FieldList, Placeholder: "**/archive/**"},
		{Key: "max_file_bytes", Label: "Max file size (bytes)", Kind: websource.FieldNumber, Default: DefaultMaxFileBytes},
		{Key: "max_files", Label: "Max files (0 = no limit)", Kind: websource.FieldNumber},
	}
}

// Filter returns the key filter of the config.
func (c Common) Filter() websource.KeyFilter {
	return websource.KeyFilter{Include: c.Include, Exclude: c.Exclude}
}

// MaxBytes returns the effective per-file limit.
func (c Common) MaxBytes() int64 {
	if c.MaxFileBytes <= 0 || c.MaxFileBytes > convert.MaxBytes {
		return min(int64(DefaultMaxFileBytes), convert.MaxBytes)
	}

	return c.MaxFileBytes
}

// Select reports whether a listed file should be indexed: it passes the globs,
// has a convertible extension (or a convertible declared type) and fits the
// size limit when the listing knows the size.
func (c Common) Select(key, contentType string, size int64) bool {
	if size > c.MaxBytes() {
		return false
	}
	if !c.Filter().Match(key) {
		return false
	}

	return convert.Supported(key) || convert.Detect(contentType, "") != convert.FormatUnknown
}

// Signature fingerprints the selection for ListingSync.
func (c Common) Signature(extra ...string) string {
	return c.Filter().Signature(append(extra, fmt.Sprint(c.MaxBytes(), c.MaxFiles))...)
}

// ErrTooMany is returned when a listing exceeds max_files: the run must fail
// rather than index a prefix, or the files past the cut would be pruned.
var ErrTooMany = fmt.Errorf("listing exceeds max_files")

// Check enforces max_files on a complete listing.
func (c Common) Check(items []websource.ListedItem) error {
	if c.MaxFiles > 0 && len(items) > c.MaxFiles {
		return fmt.Errorf("%w (%d > %d); narrow include globs or raise the limit", ErrTooMany, len(items), c.MaxFiles)
	}

	return nil
}

// Sync runs ListingSync over items, converting each changed file.
func Sync(ctx context.Context, rawState json.RawMessage, signature string, items []websource.ListedItem, download Download, maxBytes int64, emit websource.Emit) (*websource.FetchResult, error) {
	return websource.ListingSync(ctx, rawState, signature, items, func(ctx context.Context, item websource.ListedItem) (websource.RemotePage, error) {
		data, contentType, err := download(ctx, item)
		if err != nil {
			return websource.RemotePage{}, err
		}
		if int64(len(data)) > maxBytes {
			return websource.RemotePage{}, fmt.Errorf("%s exceeds %d bytes", item.Key, maxBytes)
		}
		display := item.Path
		if display == "" {
			display = item.Key
		}
		doc, err := convert.ToMarkdown(data, convert.Options{ContentType: contentType, Name: display, BaseURL: htmlBase(item.URL)})
		if err != nil {
			return websource.RemotePage{}, err
		}
		title := item.Title
		if title == "" {
			title = doc.Title
		}
		if title == "" {
			title = path.Base(display)
		}

		return websource.RemotePage{Title: title, Markdown: withPath(doc.Markdown, display)}, nil
	}, emit)
}

func htmlBase(u string) string {
	if websource.ValidHTTPURL(u) == nil {
		return u
	}

	return ""
}

// withPath adds the file path so it is searchable and visible in excerpts. It
// goes after a leading "# " heading, so the manager does not add a second
// title heading above it.
func withPath(md, key string) string {
	line := "_Path: `" + strings.ReplaceAll(key, "`", "'") + "`_"
	if strings.HasPrefix(md, "# ") {
		heading, rest, _ := strings.Cut(md, "\n")
		return heading + "\n\n" + line + "\n\n" + strings.TrimLeft(rest, "\n")
	}

	return line + "\n\n" + md
}

// Preview summarizes a listing without downloading.
func Preview(items []websource.ListedItem, scanned int, limit int) websource.PreviewResult {
	return websource.PreviewResult{ItemCount: len(items), Scanned: scanned, Limit: limit, Truncated: limit > 0 && len(items) > limit}
}
