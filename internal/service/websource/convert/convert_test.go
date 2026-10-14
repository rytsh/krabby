package convert

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestDetect(t *testing.T) {
	tests := []struct {
		ct, name string
		want     Format
	}{
		{"text/html; charset=utf-8", "", FormatHTML},
		{"application/octet-stream", "guide.PDF", FormatPDF},
		{"", "notes/readme.md?raw=1", FormatMarkdown},
		{"", "a.rst", FormatText},
		{"", "deck.pptx", FormatPPTX},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "", FormatXLSX},
		{"", "binary.exe", FormatUnknown},
	}
	for _, tt := range tests {
		if got := Detect(tt.ct, tt.name); got != tt.want {
			t.Errorf("Detect(%q, %q) = %q, want %q", tt.ct, tt.name, got, tt.want)
		}
	}
}

func TestTextFormats(t *testing.T) {
	doc, err := ToMarkdown([]byte("<html><head><title>T</title></head><body><h2>Hi</h2><p>x</p></body></html>"), Options{ContentType: "text/html"})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "T" || !strings.Contains(doc.Markdown, "## Hi") {
		t.Fatalf("html = %+v", doc)
	}

	doc, err = ToMarkdown([]byte("name,qty\nfoo,1\n\"a|b\",2\n"), Options{Name: "inv.csv"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Markdown, "| name | qty |") || !strings.Contains(doc.Markdown, `a\|b`) || doc.Title != "inv" {
		t.Fatalf("csv = %+v", doc)
	}

	doc, err = ToMarkdown([]byte(`{"a":1}`), Options{ContentType: "application/json"})
	if err != nil || !strings.HasPrefix(doc.Markdown, "```json") {
		t.Fatalf("json = %+v %v", doc, err)
	}

	doc, err = ToMarkdown([]byte("# Heading\n\nbody"), Options{Name: "x.md"})
	if err != nil || doc.Title != "Heading" {
		t.Fatalf("md = %+v %v", doc, err)
	}

	if _, err := ToMarkdown([]byte{0, 1, 2, 3, 0xff}, Options{Name: "x.bin"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("binary err = %v", err)
	}
}

func TestDOCX(t *testing.T) {
	data := zipOf(t, map[string]string{
		"docProps/core.xml": `<cp:coreProperties xmlns:cp="c" xmlns:dc="d"><dc:title>Runbook</dc:title></cp:coreProperties>`,
		"word/document.xml": `<w:document xmlns:w="w"><w:body>
<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Recovery</w:t></w:r></w:p>
<w:p><w:r><w:t>Restart the </w:t></w:r><w:r><w:t>service.</w:t></w:r></w:p>
<w:p><w:pPr><w:numPr><w:ilvl w:val="1"/></w:numPr></w:pPr><w:r><w:t>nested item</w:t></w:r></w:p>
<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Host</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Port</w:t></w:r></w:p></w:tc></w:tr>
<w:tr><w:tc><w:p><w:r><w:t>db</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>5432</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
</w:body></w:document>`,
	})
	doc, err := ToMarkdown(data, Options{Name: "r.docx"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Recovery", "Restart the service.", "  - nested item", "| Host | Port |", "| db | 5432 |"} {
		if !strings.Contains(doc.Markdown, want) {
			t.Fatalf("docx missing %q:\n%s", want, doc.Markdown)
		}
	}
	if doc.Title != "Runbook" {
		t.Fatalf("title = %q", doc.Title)
	}
}

func TestPPTXAndXLSX(t *testing.T) {
	pptx := zipOf(t, map[string]string{
		"ppt/slides/slide2.xml": `<p:sld xmlns:p="p" xmlns:a="a"><a:p><a:r><a:t>Second</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide1.xml": `<p:sld xmlns:p="p" xmlns:a="a"><a:p><a:r><a:t>Intro</a:t></a:r></a:p><a:p><a:r><a:t>point one</a:t></a:r></a:p></p:sld>`,
	})
	doc, err := ToMarkdown(pptx, Options{Name: "d.pptx"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Markdown, "## Slide 1: Intro\n\n- point one") || strings.Index(doc.Markdown, "Slide 1") > strings.Index(doc.Markdown, "Slide 2") {
		t.Fatalf("pptx = %s", doc.Markdown)
	}

	xlsx := zipOf(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="r"><sheets><sheet name="Inventory" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst><si><t>name</t></si><si><t>qty</t></si><si><t>bolt</t></si></sst>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row><row r="2"><c r="A2" t="s"><v>2</v></c><c r="C2"><v>7</v></c></row></sheetData></worksheet>`,
	})
	doc, err = ToMarkdown(xlsx, Options{Name: "i.xlsx"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Markdown, "## Inventory") || !strings.Contains(doc.Markdown, "| bolt |  | 7 |") {
		t.Fatalf("xlsx = %s", doc.Markdown)
	}
}

func TestMalformedPDFDoesNotPanic(t *testing.T) {
	if _, err := ToMarkdown([]byte("%PDF-1.4 garbage"), Options{}); err == nil {
		t.Fatal("expected error")
	}
}
