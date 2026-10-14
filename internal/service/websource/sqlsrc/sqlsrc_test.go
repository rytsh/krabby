package sqlsrc

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/websource"
)

// fakeDriver serves one fixed result set and records whether the transaction
// was opened read-only.
type fakeDriver struct{ readOnly *bool }

func (d fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{d}, nil }

type fakeConn struct{ d fakeDriver }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not used") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return fakeTx{}, nil }
func (c *fakeConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	*c.d.readOnly = opts.ReadOnly
	return fakeTx{}, nil
}
func (c *fakeConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &fakeRows{data: [][]driver.Value{
		{int64(1), "billing", "Payments", []byte("Restart with `systemctl restart billing`")},
		{int64(2), "search", "Discovery", nil},
	}}, nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeRows struct {
	data [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return []string{"id", "name", "owner", "runbook"} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.i])
	r.i++
	return nil
}

func TestSQLRowsToPages(t *testing.T) {
	readOnly := false
	sql.Register("fake-krabby", fakeDriver{readOnly: &readOnly})
	f := New()
	f.open = func(_, dsn string) (*sql.DB, error) { return sql.Open("fake-krabby", dsn) }

	raw, err := f.MergeConfig(nil, json.RawMessage(`{"driver":"postgres","dsn":"postgres://r@db/x","query":"SELECT id, name, owner, runbook FROM services;",
"id_column":"id","title_column":"name","body_column":"runbook","team_columns":["owner"],"url_template":"https://cmdb.example.com/svc/{{.name}}"}`))
	if err != nil {
		t.Fatal(err)
	}
	var pages []websource.RemotePage
	res, err := f.Fetch(context.Background(), &websource.Collection{Config: raw}, nil, nil, func(p websource.RemotePage) error {
		pages = append(pages, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !readOnly || !res.Complete || len(pages) != 2 {
		t.Fatalf("readOnly=%v complete=%v pages=%d", readOnly, res.Complete, len(pages))
	}
	p := pages[0]
	if p.Title != "billing" || p.URL != "https://cmdb.example.com/svc/billing" || p.Teams[0] != "Payments" ||
		!strings.Contains(p.Markdown, "systemctl restart billing") || !strings.Contains(p.Markdown, "**owner**: Payments") {
		t.Fatalf("page = %+v", p)
	}
	view, _ := json.Marshal(f.ConfigView(raw))
	if strings.Contains(string(view), "postgres://") {
		t.Fatalf("dsn leaked: %s", view)
	}
}

func TestQueryGuard(t *testing.T) {
	for _, q := range []string{"DELETE FROM x", "SELECT 1; DROP TABLE x", "update x set y=1"} {
		if err := checkQuery(q); err == nil {
			t.Errorf("accepted %q", q)
		}
	}
	if err := checkQuery("WITH a AS (SELECT 1) SELECT * FROM a"); err != nil {
		t.Fatal(err)
	}
}
