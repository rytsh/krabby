package convert

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// maxPartBytes bounds one decompressed OOXML part, guarding against zip bombs.
const maxPartBytes = 64 << 20

type ooxml struct {
	files map[string]*zip.File
}

func openOOXML(data []byte) (*ooxml, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open office document; %w", err)
	}
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}

	return &ooxml{files: files}, nil
}

func (o *ooxml) open(name string) (io.ReadCloser, error) {
	f, ok := o.files[name]
	if !ok {
		return nil, fmt.Errorf("missing part %s", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}

	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(rc, maxPartBytes), rc}, nil
}

func (o *ooxml) coreTitle() string {
	rc, err := o.open("docProps/core.xml")
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	d := xml.NewDecoder(rc)
	inTitle := false
	for {
		tok, err := d.Token()
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			inTitle = t.Name.Local == "title"
		case xml.CharData:
			if inTitle {
				return strings.TrimSpace(string(t))
			}
		case xml.EndElement:
			inTitle = false
		}
	}
}

func attr(e xml.StartElement, local string) string {
	for _, a := range e.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}

	return ""
}

// docxToMarkdown renders paragraphs (headings, list items) and tables of
// word/document.xml.
func docxToMarkdown(data []byte) (string, string, error) {
	o, err := openOOXML(data)
	if err != nil {
		return "", "", err
	}
	rc, err := o.open("word/document.xml")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = rc.Close() }()

	var (
		out       strings.Builder
		para      strings.Builder
		style     string
		listLevel = -1
		inText    bool
		tableRows [][]string
		row       []string
		cell      strings.Builder
		tableDeep int
	)

	flushPara := func() {
		text := strings.TrimSpace(para.String())
		para.Reset()
		defer func() { style, listLevel = "", -1 }()
		if text == "" {
			return
		}
		if tableDeep > 0 {
			if cell.Len() > 0 {
				cell.WriteString(" ")
			}
			cell.WriteString(text)
			return
		}
		lower := strings.ToLower(style)
		switch {
		case lower == "title":
			out.WriteString("# " + text + "\n\n")
		case strings.HasPrefix(lower, "heading"):
			level, _ := strconv.Atoi(strings.TrimPrefix(lower, "heading"))
			level = min(max(level, 1), 5) + 1
			out.WriteString(strings.Repeat("#", level) + " " + text + "\n\n")
		case listLevel >= 0:
			out.WriteString(strings.Repeat("  ", listLevel) + "- " + text + "\n")
		default:
			out.WriteString(text + "\n\n")
		}
	}

	d := xml.NewDecoder(rc)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", fmt.Errorf("parse document.xml; %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tbl":
				tableDeep++
				if tableDeep == 1 {
					tableRows = nil
				}
			case "tr":
				if tableDeep == 1 {
					row = nil
				}
			case "tc":
				if tableDeep == 1 {
					cell.Reset()
				}
			case "pStyle":
				style = attr(t, "val")
			case "ilvl":
				listLevel, _ = strconv.Atoi(attr(t, "val"))
			case "numPr":
				if listLevel < 0 {
					listLevel = 0
				}
			case "t":
				inText = true
			case "tab":
				para.WriteString(" ")
			case "br", "cr":
				para.WriteString(" ")
			}
		case xml.CharData:
			if inText {
				para.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				flushPara()
			case "tc":
				if tableDeep == 1 {
					row = append(row, cell.String())
				}
			case "tr":
				if tableDeep == 1 {
					tableRows = append(tableRows, row)
				}
			case "tbl":
				tableDeep--
				if tableDeep == 0 {
					out.WriteString("\n" + renderTable(tableRows) + "\n")
				}
			}
		}
	}

	return o.coreTitle(), out.String(), nil
}

// pptxToMarkdown renders each slide's text as a section, in slide order.
func pptxToMarkdown(data []byte) (string, error) {
	o, err := openOOXML(data)
	if err != nil {
		return "", err
	}
	type slide struct {
		n    int
		name string
	}
	var slides []slide
	for name := range o.files {
		if !strings.HasPrefix(name, "ppt/slides/slide") || !strings.HasSuffix(name, ".xml") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "ppt/slides/slide"), ".xml"))
		if err != nil {
			continue
		}
		slides = append(slides, slide{n: n, name: name})
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].n < slides[j].n })

	var out strings.Builder
	for _, s := range slides {
		paras, err := o.paragraphs(s.name)
		if err != nil {
			return "", err
		}
		if len(paras) == 0 {
			continue
		}
		fmt.Fprintf(&out, "## Slide %d: %s\n\n", s.n, paras[0])
		for _, p := range paras[1:] {
			out.WriteString("- " + p + "\n")
		}
		out.WriteString("\n")
	}

	return out.String(), nil
}

// paragraphs collects the non-empty DrawingML paragraphs (a:p / a:t) of a part.
func (o *ooxml) paragraphs(name string) ([]string, error) {
	rc, err := o.open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var (
		out    []string
		para   strings.Builder
		inText bool
	)
	d := xml.NewDecoder(rc)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse %s; %w", name, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				inText = true
			}
		case xml.CharData:
			if inText {
				para.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if text := strings.TrimSpace(para.String()); text != "" {
					out = append(out, text)
				}
				para.Reset()
			}
		}
	}
}

// xlsxToMarkdown renders every worksheet as a table, in workbook order.
func xlsxToMarkdown(data []byte) (string, error) {
	o, err := openOOXML(data)
	if err != nil {
		return "", err
	}
	shared, err := o.sharedStrings()
	if err != nil {
		return "", err
	}
	sheets, err := o.sheets()
	if err != nil {
		return "", err
	}

	var out strings.Builder
	for _, sh := range sheets {
		rows, err := o.sheetRows(sh.target, shared)
		if err != nil {
			return "", err
		}
		if len(rows) == 0 {
			continue
		}
		out.WriteString("## " + sh.name + "\n\n" + renderTable(rows) + "\n")
	}

	return out.String(), nil
}

func (o *ooxml) sharedStrings() ([]string, error) {
	rc, err := o.open("xl/sharedStrings.xml")
	if err != nil {
		return nil, nil // optional part
	}
	defer func() { _ = rc.Close() }()

	var (
		out    []string
		cur    strings.Builder
		inText bool
	)
	d := xml.NewDecoder(rc)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse sharedStrings.xml; %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				cur.Reset()
			case "t":
				inText = true
			}
		case xml.CharData:
			if inText {
				cur.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "si":
				out = append(out, cur.String())
			}
		}
	}
}

type sheetRef struct {
	name   string
	target string
}

func (o *ooxml) sheets() ([]sheetRef, error) {
	rels := map[string]string{}
	if rc, err := o.open("xl/_rels/workbook.xml.rels"); err == nil {
		var doc struct {
			Rels []struct {
				ID     string `xml:"Id,attr"`
				Target string `xml:"Target,attr"`
			} `xml:"Relationship"`
		}
		err := xml.NewDecoder(rc).Decode(&doc)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("parse workbook rels; %w", err)
		}
		for _, r := range doc.Rels {
			target := r.Target
			if strings.HasPrefix(target, "/") {
				target = strings.TrimPrefix(target, "/")
			} else {
				target = path.Join("xl", target)
			}
			rels[r.ID] = target
		}
	}

	rc, err := o.open("xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var out []sheetRef
	d := xml.NewDecoder(rc)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse workbook.xml; %w", err)
		}
		t, ok := tok.(xml.StartElement)
		if !ok || t.Name.Local != "sheet" {
			continue
		}
		id := ""
		for _, a := range t.Attr {
			if a.Name.Local == "id" {
				id = a.Value
			}
		}
		target := rels[id]
		if target == "" {
			target = fmt.Sprintf("xl/worksheets/sheet%d.xml", len(out)+1)
		}
		out = append(out, sheetRef{name: attr(t, "name"), target: target})
	}
}

// columnIndex converts a cell reference ("C12") to its zero-based column.
func columnIndex(ref string) int {
	col := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		col = col*26 + int(r-'A'+1)
	}

	return col - 1
}

func (o *ooxml) sheetRows(name string, shared []string) ([][]string, error) {
	rc, err := o.open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var (
		rows     [][]string
		row      []string
		cellType string
		cellCol  int
		value    strings.Builder
		inValue  bool
	)
	d := xml.NewDecoder(rc)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse %s; %w", name, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = nil
			case "c":
				cellType = attr(t, "t")
				cellCol = columnIndex(attr(t, "r"))
				if cellCol < 0 {
					cellCol = len(row)
				}
				value.Reset()
			case "v", "t":
				inValue = true
			}
		case xml.CharData:
			if inValue {
				value.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v", "t":
				inValue = false
			case "c":
				v := value.String()
				if cellType == "s" {
					if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < len(shared) {
						v = shared[i]
					}
				}
				if cellCol > 16384 {
					continue
				}
				for len(row) <= cellCol {
					row = append(row, "")
				}
				row[cellCol] = v
			case "row":
				if strings.TrimSpace(strings.Join(row, "")) != "" {
					rows = append(rows, row)
				}
				if len(rows) > maxTableRows+1 {
					return rows, nil
				}
			}
		}
	}

	return rows, nil
}
