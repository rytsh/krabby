// Package sqlsrc implements the "sql" web-source type: a read-only query
// against PostgreSQL or MySQL whose rows become documents through a template —
// runbook tables, service inventories (CMDB), on-call rosters, feature flags.
//
// The query runs inside a read-only transaction with a statement timeout, and
// only a single SELECT/WITH statement is accepted, so a misconfigured source
// cannot modify the database.
package sqlsrc

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/rytsh/krabby/internal/service/progress"
	"github.com/rytsh/krabby/internal/service/websource"
)

// Type is the collection type served by this fetcher.
const Type = "sql"

const (
	defaultMaxRows = 50_000
	queryTimeout   = 5 * time.Minute
)

// Config is the provider config.
type Config struct {
	Driver      string   `json:"driver"`
	DSN         string   `json:"dsn"`
	Query       string   `json:"query"`
	IDColumn    string   `json:"id_column"`
	TitleColumn string   `json:"title_column,omitempty"`
	BodyColumn  string   `json:"body_column,omitempty"`
	URLColumn   string   `json:"url_column,omitempty"`
	URLTemplate string   `json:"url_template,omitempty"`
	Updated     string   `json:"updated_column,omitempty"`
	TeamColumns []string `json:"team_columns,omitempty"`
	Template    string   `json:"template,omitempty"`
	MaxRows     int      `json:"max_rows,omitempty"`
}

// Fetcher syncs query results.
type Fetcher struct {
	websource.Schema
	open func(driver, dsn string) (*sql.DB, error)
}

// New creates the fetcher.
func New() *Fetcher {
	return &Fetcher{open: sql.Open, Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:  Type,
		Label: "SQL query (PostgreSQL, MySQL)",
		Description: "Run a read-only SELECT and turn each row into a document with a Markdown template — " +
			"runbook tables, service inventory / CMDB, on-call rosters. Runs in a read-only transaction.",
		Fields: []websource.Field{
			{Key: "driver", Label: "Driver", Kind: websource.FieldSelect, Required: true, Options: []string{"postgres", "mysql"}, Default: "postgres"},
			{Key: "dsn", Label: "Connection string", Kind: websource.FieldSecret, Required: true,
				Help: "postgres://reader:pw@db:5432/cmdb?sslmode=require or reader:pw@tcp(db:3306)/cmdb?parseTime=true; use a read-only user"},
			{Key: "query", Label: "Query", Kind: websource.FieldText, Required: true, Placeholder: "SELECT id, name, owner, runbook FROM services"},
			{Key: "id_column", Label: "ID column", Kind: websource.FieldString, Required: true, Placeholder: "id"},
			{Key: "title_column", Label: "Title column", Kind: websource.FieldString, Placeholder: "name"},
			{Key: "body_column", Label: "Body column (Markdown)", Kind: websource.FieldString, Placeholder: "runbook"},
			{Key: "url_column", Label: "Link column", Kind: websource.FieldString},
			{Key: "url_template", Label: "Link template", Kind: websource.FieldString, Placeholder: "https://cmdb.example.com/svc/{{.id}}"},
			{Key: "updated_column", Label: "Updated-at column", Kind: websource.FieldString, Placeholder: "updated_at"},
			{Key: "team_columns", Label: "Team / owner columns", Kind: websource.FieldList, Placeholder: "owner_team"},
			{Key: "template", Label: "Markdown template (optional)", Kind: websource.FieldText, Help: "Go text/template over the row, e.g. ## {{.name}}\\n\\nOwner: {{.owner}}"},
			{Key: "max_rows", Label: "Max rows", Kind: websource.FieldNumber, Default: defaultMaxRows},
		},
	}}}
}

func decode(raw json.RawMessage) (Config, error) {
	cfg, err := websource.DecodeConfig[Config](raw, Type)
	cfg.Driver = strings.ToLower(strings.TrimSpace(cfg.Driver))
	if cfg.Driver == "postgresql" || cfg.Driver == "pgx" {
		cfg.Driver = "postgres"
	}
	cfg.Query = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(cfg.Query), ";"))
	if cfg.MaxRows <= 0 {
		cfg.MaxRows = defaultMaxRows
	}

	return cfg, err
}

// checkQuery accepts a single read statement. It is a guard against mistakes,
// not a security boundary: the read-only transaction and a read-only database
// user are.
func checkQuery(q string) error {
	lower := strings.ToLower(q)
	if !strings.HasPrefix(lower, "select") && !strings.HasPrefix(lower, "with") {
		return fmt.Errorf("query must be a SELECT (or WITH … SELECT)")
	}
	if strings.Contains(q, ";") {
		return fmt.Errorf("query must be a single statement")
	}

	return nil
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	if cfg.Driver != "postgres" && cfg.Driver != "mysql" {
		return fmt.Errorf("sql driver must be postgres or mysql")
	}
	if strings.TrimSpace(cfg.DSN) == "" {
		return fmt.Errorf("sql dsn is required")
	}
	if err := checkQuery(cfg.Query); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.IDColumn) == "" {
		return fmt.Errorf("sql id_column is required")
	}
	if _, err := websource.CompileTemplate(cfg.Template); err != nil {
		return err
	}
	if _, err := websource.CompileTemplate(cfg.URLTemplate); err != nil {
		return fmt.Errorf("url_template: %w", err)
	}

	return nil
}

func (f *Fetcher) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	merged, err := f.MergeObjects(current, update)
	if err != nil {
		return nil, err
	}

	return merged, f.Validate(merged)
}

func (f *Fetcher) ConfigView(raw json.RawMessage) any { return f.View(raw) }

func driverName(d string) string {
	if d == "postgres" {
		return "pgx"
	}

	return d
}

// query streams rows as column→value maps.
func (f *Fetcher) query(ctx context.Context, cfg Config, fn func(map[string]any) error) (truncated bool, err error) {
	db, err := f.open(driverName(cfg.Driver), cfg.DSN)
	if err != nil {
		return false, fmt.Errorf("open database; %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, fmt.Errorf("begin read-only transaction; %w", redactDSN(err, cfg.DSN))
	}
	defer func() { _ = tx.Rollback() }()
	if cfg.Driver == "postgres" {
		_, _ = tx.ExecContext(ctx, "SET LOCAL statement_timeout = '300s'")
	}

	rows, err := tx.QueryContext(ctx, cfg.Query)
	if err != nil {
		return false, fmt.Errorf("run query; %w", err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return false, err
	}

	n := 0
	for rows.Next() {
		if n >= cfg.MaxRows {
			return true, nil
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return false, fmt.Errorf("scan row; %w", err)
		}
		rec := make(map[string]any, len(cols))
		for i, c := range cols {
			rec[c] = normalizeValue(vals[i])
		}
		n++
		if err := fn(rec); err != nil {
			return false, err
		}
	}

	return false, rows.Err()
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	default:
		return x
	}
}

func redactDSN(err error, dsn string) error {
	if dsn == "" {
		return err
	}

	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), dsn, "<dsn>"))
}

func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	var out websource.PreviewResult
	if err := f.Validate(raw); err != nil {
		return out, err
	}
	cfg, _ := decode(raw)
	out.Limit = cfg.MaxRows
	truncated, err := f.query(ctx, cfg, func(rec map[string]any) error {
		out.Scanned++
		if _, ok := rec[cfg.IDColumn]; !ok {
			return fmt.Errorf("id_column %q is not in the result columns", cfg.IDColumn)
		}
		if websource.Stringify(rec[cfg.IDColumn]) != "" {
			out.ItemCount++
		}
		return nil
	})
	out.Truncated = truncated

	return out, err
}

// Fetch runs the query; the result set is the source of truth, so an
// untruncated run is complete and rows that disappeared are pruned.
func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, _ json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	tmpl, err := websource.CompileTemplate(cfg.Template)
	if err != nil {
		return nil, err
	}
	n := 0
	truncated, err := f.query(ctx, cfg, func(rec map[string]any) error {
		id := strings.TrimSpace(websource.Stringify(rec[cfg.IDColumn]))
		if id == "" {
			return nil
		}
		n++
		progress.Report(ctx, n, 0)

		return emit(renderRow(cfg, tmpl, id, rec))
	})
	if err != nil {
		return nil, err
	}

	return &websource.FetchResult{Complete: !truncated}, nil
}

func renderRow(cfg Config, tmpl *template.Template, id string, rec map[string]any) websource.RemotePage {
	page := websource.RemotePage{
		Slug:  websource.ListingSlug(id),
		Title: firstNonEmpty(websource.Stringify(rec[cfg.TitleColumn]), id),
	}
	if cfg.Updated != "" {
		page.UpdatedAt = websource.ParseTime(rec[cfg.Updated])
	}
	for _, c := range cfg.TeamColumns {
		page.Teams = append(page.Teams, websource.StringList(rec[c])...)
	}
	switch {
	case cfg.URLTemplate != "":
		page.URL, _ = websource.ExpandTemplate(cfg.URLTemplate, rec)
	case cfg.URLColumn != "":
		page.URL = websource.Stringify(rec[cfg.URLColumn])
	}
	if websource.ValidHTTPURL(page.URL) != nil {
		page.URL = ""
	}
	if tmpl != nil {
		md, err := websource.ExecTemplate(tmpl, rec)
		page.Markdown, page.Err = md, err

		return page
	}
	page.Markdown = websource.RenderRecord(rec, cfg.BodyColumn, cfg.TitleColumn)

	return page
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}

	return ""
}
