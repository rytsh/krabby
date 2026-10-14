// Package convert turns documents of common formats into Markdown for the
// docs index: HTML, Markdown, plain text, reStructuredText, AsciiDoc, CSV,
// JSON/YAML, PDF and the Office Open XML family (DOCX, PPTX, XLSX).
//
// It is shared by every web-source provider that handles files or arbitrary
// HTTP payloads (pages, push, http, crawl, fs, s3, webdav, mcp, gdrive,
// sharepoint), so a format supported once is supported everywhere.
package convert

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/rytsh/krabby/internal/service/websource"
)

// MaxBytes is the largest document accepted for conversion.
const MaxBytes = 32 << 20

// ErrUnsupported marks a format this package cannot convert.
var ErrUnsupported = errors.New("unsupported document format")

// Format identifies a document format.
type Format string

const (
	FormatUnknown  Format = ""
	FormatHTML     Format = "html"
	FormatMarkdown Format = "markdown"
	FormatText     Format = "text"
	FormatCSV      Format = "csv"
	FormatJSON     Format = "json"
	FormatYAML     Format = "yaml"
	FormatPDF      Format = "pdf"
	FormatDOCX     Format = "docx"
	FormatPPTX     Format = "pptx"
	FormatXLSX     Format = "xlsx"
)

var byMIME = map[string]Format{
	"text/html":             FormatHTML,
	"application/xhtml+xml": FormatHTML,
	"text/markdown":         FormatMarkdown,
	"text/x-markdown":       FormatMarkdown,
	"markdown":              FormatMarkdown,
	"text/plain":            FormatText,
	"text/x-rst":            FormatText,
	"text/asciidoc":         FormatText,
	"text/x-asciidoc":       FormatText,
	"text/csv":              FormatCSV,
	"application/csv":       FormatCSV,
	"application/json":      FormatJSON,
	"text/json":             FormatJSON,
	"application/yaml":      FormatYAML,
	"application/x-yaml":    FormatYAML,
	"text/yaml":             FormatYAML,
	"text/x-yaml":           FormatYAML,
	"application/pdf":       FormatPDF,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   FormatDOCX,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": FormatPPTX,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         FormatXLSX,
}

var byExt = map[string]Format{
	".html": FormatHTML, ".htm": FormatHTML, ".xhtml": FormatHTML,
	".md": FormatMarkdown, ".markdown": FormatMarkdown, ".mdx": FormatMarkdown,
	".txt": FormatText, ".text": FormatText, ".rst": FormatText, ".adoc": FormatText,
	".asciidoc": FormatText, ".org": FormatText, ".log": FormatText,
	".csv": FormatCSV, ".tsv": FormatCSV,
	".json": FormatJSON,
	".yaml": FormatYAML, ".yml": FormatYAML,
	".pdf":  FormatPDF,
	".docx": FormatDOCX,
	".pptx": FormatPPTX,
	".xlsx": FormatXLSX,
}

// Extensions lists the file extensions Detect recognizes.
func Extensions() []string {
	out := make([]string, 0, len(byExt))
	for ext := range byExt {
		out = append(out, ext)
	}

	return out
}

// Detect picks a format from a content type and, failing that, a file name.
// Generic content types (octet-stream, binary/…) defer to the name, and a
// final content sniff recognizes PDF, ZIP-based Office files cannot be told
// apart without a name so they stay unknown.
func Detect(contentType, name string) Format {
	mediaType := strings.ToLower(strings.TrimSpace(contentType))
	if parsed, _, err := mime.ParseMediaType(mediaType); err == nil {
		mediaType = parsed
	} else {
		mediaType, _, _ = strings.Cut(mediaType, ";")
		mediaType = strings.TrimSpace(mediaType)
	}
	if f, ok := byMIME[mediaType]; ok {
		return f
	}
	if f, ok := byExt[strings.ToLower(path.Ext(stripQuery(name)))]; ok {
		return f
	}

	return FormatUnknown
}

func stripQuery(name string) string {
	name, _, _ = strings.Cut(name, "?")
	name, _, _ = strings.Cut(name, "#")

	return name
}

// Supported reports whether a file name has a convertible extension.
func Supported(name string) bool {
	_, ok := byExt[strings.ToLower(path.Ext(stripQuery(name)))]

	return ok
}

// Document is the converted result.
type Document struct {
	Title    string
	Markdown string
	Format   Format
}

// Options tune one conversion.
type Options struct {
	// ContentType is the declared media type (may be empty).
	ContentType string
	// Name is a file name or URL used for detection and as a title fallback.
	Name string
	// BaseURL resolves relative links in HTML.
	BaseURL string
	// Readability extracts the main article from full HTML pages. Leave it off
	// for content-only HTML fragments (Confluence storage format, API bodies).
	Readability bool
}

// ToMarkdown converts data to Markdown. The title is extracted when the
// format carries one (HTML <title>, DOCX core properties, first heading) and
// otherwise derived from the name.
func ToMarkdown(data []byte, opts Options) (Document, error) {
	if len(data) > MaxBytes {
		return Document{}, fmt.Errorf("document exceeds %d bytes", MaxBytes)
	}

	format := Detect(opts.ContentType, opts.Name)
	if format == FormatUnknown && genericContentType(opts.ContentType) {
		format = sniff(data)
	}

	doc := Document{Format: format}
	var err error

	switch format {
	case FormatHTML:
		html := decodeText(data, opts.ContentType)
		if opts.Readability {
			doc.Title, doc.Markdown, err = websource.ExtractArticle(html, opts.BaseURL)
		} else {
			doc.Title = htmlTitle(html)
			doc.Markdown, err = websource.MarkdownFromHTML(html, opts.BaseURL)
		}
	case FormatMarkdown, FormatText:
		doc.Markdown = strings.TrimSpace(decodeText(data, opts.ContentType))
	case FormatCSV:
		doc.Markdown, err = csvToMarkdown(data, strings.HasSuffix(strings.ToLower(stripQuery(opts.Name)), ".tsv"))
	case FormatJSON:
		doc.Markdown = fence("json", decodeText(data, opts.ContentType))
	case FormatYAML:
		doc.Markdown = fence("yaml", decodeText(data, opts.ContentType))
	case FormatPDF:
		doc.Markdown, err = pdfToMarkdown(data)
	case FormatDOCX:
		doc.Title, doc.Markdown, err = docxToMarkdown(data)
	case FormatPPTX:
		doc.Markdown, err = pptxToMarkdown(data)
	case FormatXLSX:
		doc.Markdown, err = xlsxToMarkdown(data)
	default:
		return Document{}, fmt.Errorf("%w (content type %q, name %q)", ErrUnsupported, opts.ContentType, path.Base(opts.Name))
	}
	if err != nil {
		return Document{}, fmt.Errorf("convert %s; %w", format, err)
	}

	doc.Markdown = strings.TrimSpace(strings.ToValidUTF8(doc.Markdown, ""))
	if doc.Title == "" {
		doc.Title = firstHeading(doc.Markdown)
	}
	if doc.Title == "" {
		doc.Title = titleFromName(opts.Name)
	}

	return doc, nil
}

// genericContentType reports a declared type that says nothing about the
// format, so content sniffing may decide.
func genericContentType(ct string) bool {
	ct, _, _ = strings.Cut(strings.ToLower(strings.TrimSpace(ct)), ";")
	switch strings.TrimSpace(ct) {
	case "", "application/octet-stream", "binary/octet-stream", "application/binary":
		return true
	}

	return false
}

// sniff recognizes PDF, HTML and plain UTF-8 text by content when neither the
// content type nor the name decided the format.
func sniff(data []byte) Format {
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		return FormatPDF
	}
	detected := http.DetectContentType(data)
	switch {
	case strings.HasPrefix(detected, "text/html"):
		return FormatHTML
	case strings.HasPrefix(detected, "text/plain") && utf8.Valid(data):
		return FormatText
	}

	return FormatUnknown
}

func decodeText(data []byte, contentType string) string {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if utf8.Valid(data) {
		return string(data)
	}
	if decoded, err := decodeCharset(data, contentType); err == nil {
		return decoded
	}

	return strings.ToValidUTF8(string(data), "")
}

func fence(lang, body string) string {
	body = strings.TrimSpace(body)
	marker := "```"
	for strings.Contains(body, marker) {
		marker += "`"
	}

	return marker + lang + "\n" + body + "\n" + marker
}

func firstHeading(md string) string {
	for line := range strings.SplitSeq(md, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}

	return ""
}

func titleFromName(name string) string {
	name = stripQuery(name)
	base := path.Base(strings.TrimRight(name, "/"))
	if base == "." || base == "/" {
		return ""
	}

	return strings.TrimSuffix(base, path.Ext(base))
}
