package convert

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// maxTableRows bounds tables rendered from CSV/XLSX: past this many rows the
// document is a data dump rather than prose, and the remainder is summarized.
const maxTableRows = 2000

func decodeCharset(data []byte, contentType string) (string, error) {
	r, err := charset.NewReader(bytes.NewReader(data), contentType)
	if err != nil {
		return "", err
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}

	return string(out), nil
}

func htmlTitle(doc string) string {
	z := html.NewTokenizer(strings.NewReader(doc))
	inTitle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken:
			name, _ := z.TagName()
			inTitle = string(name) == "title"
		case html.TextToken:
			if inTitle {
				return strings.TrimSpace(string(z.Text()))
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if string(name) == "head" {
				return ""
			}
			inTitle = false
		}
	}
}

func csvToMarkdown(data []byte, tsv bool) (string, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	if tsv {
		r.Comma = '\t'
	}
	var rows [][]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		rows = append(rows, rec)
	}

	return renderTable(rows), nil
}

// renderTable renders rows as a GitHub table; the first row is the header.
func renderTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	width := 0
	for _, r := range rows {
		width = max(width, len(r))
	}
	if width == 0 {
		return ""
	}
	cell := func(s string) string {
		s = strings.ReplaceAll(strings.TrimSpace(s), "\n", "<br>")
		return strings.ReplaceAll(s, "|", `\|`)
	}
	line := func(r []string) string {
		cells := make([]string, width)
		for i := range cells {
			if i < len(r) {
				cells[i] = cell(r[i])
			}
		}
		return "| " + strings.Join(cells, " | ") + " |\n"
	}

	var b strings.Builder
	b.WriteString(line(rows[0]))
	b.WriteString("|" + strings.Repeat(" --- |", width) + "\n")
	body := rows[1:]
	extra := 0
	if len(body) > maxTableRows {
		extra = len(body) - maxTableRows
		body = body[:maxTableRows]
	}
	for _, r := range body {
		b.WriteString(line(r))
	}
	if extra > 0 {
		fmt.Fprintf(&b, "\n_%d more rows not shown._\n", extra)
	}

	return b.String()
}

var pdfSpaces = regexp.MustCompile(`[ \t]+`)

func pdfToMarkdown(data []byte) (out string, err error) {
	// The PDF parser panics on some malformed inputs; one bad file must not
	// take a whole sync down.
	defer func() {
		if r := recover(); r != nil {
			out, err = "", fmt.Errorf("malformed pdf: %v", r)
		}
	}()

	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fonts := map[string]*pdf.Font{}
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		for _, name := range p.Fonts() {
			if _, ok := fonts[name]; !ok {
				f := p.Font(name)
				fonts[name] = &f
			}
		}
		text, err := p.GetPlainText(fonts)
		if err != nil {
			continue
		}
		text = strings.TrimSpace(pdfSpaces.ReplaceAllString(text, " "))
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## Page %d\n\n%s", i, text)
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("pdf has no extractable text (scanned images need OCR)")
	}

	return b.String(), nil
}
