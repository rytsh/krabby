package manager

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/apicatalog"
	"github.com/rytsh/krabby/internal/service/queue"
)

// fakeAPIProvider is a scriptable apicatalog.Provider: each Fetch pops the next
// scripted result, so a test can describe a sequence of syncs directly.
type fakeAPIProvider struct {
	runs []fakeAPIRun
	seen int
}

type fakeAPIRun struct {
	ops       []apicatalog.RemoteOperation
	complete  bool
	unchanged bool
	info      apicatalog.ServiceInfo
}

func (f *fakeAPIProvider) Validate(json.RawMessage) error { return nil }

func (f *fakeAPIProvider) MergeConfig(_, update json.RawMessage) (json.RawMessage, error) {
	if len(update) == 0 {
		return json.RawMessage(`{}`), nil
	}

	return update, nil
}

func (f *fakeAPIProvider) ConfigView(json.RawMessage) any { return map[string]any{} }

func (f *fakeAPIProvider) Fetch(_ context.Context, _ *apicatalog.Service, _ json.RawMessage, emit apicatalog.Emit) (*apicatalog.FetchResult, error) {
	run := fakeAPIRun{complete: true}
	if f.seen < len(f.runs) {
		run = f.runs[f.seen]
	}
	f.seen++

	for _, op := range run.ops {
		if err := emit(op); err != nil {
			return nil, err
		}
	}

	return &apicatalog.FetchResult{
		Complete:  run.complete,
		Unchanged: run.unchanged,
		Info:      run.info,
		State:     json.RawMessage(`{"v":1}`),
	}, nil
}

// newAPIManager builds a Manager with an in-memory catalog store and no docs
// indexes, which is enough to exercise the sync's record and filesystem
// behaviour without paying for an embedder.
func newAPIManager(t *testing.T, provider apicatalog.Provider) (*Manager, *apicatalog.Store) {
	t.Helper()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatalf("bw.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := apicatalog.New(db)
	if err != nil {
		t.Fatalf("apicatalog.New: %v", err)
	}

	m := &Manager{
		queue:        queue.New(context.Background(), 1),
		locks:        map[string]*sync.Mutex{},
		activity:     map[string]map[string]struct{}{},
		progress:     map[string]map[string]Progress{},
		apisRootDir:  t.TempDir(),
		apiStore:     store,
		apiProviders: map[string]apicatalog.Provider{"fake": provider},
		docs:         &docsBundle{},
	}
	t.Cleanup(m.queue.Close)

	return m, store
}

func remoteOp(method, path, summary string) apicatalog.RemoteOperation {
	return apicatalog.RemoteOperation{
		OpSlug:      apicatalog.OpSlug(method, path),
		OperationID: method + " " + path,
		Method:      method,
		Path:        path,
		Summary:     summary,
		Detail:      json.RawMessage(`{"method":"` + method + `"}`),
		Markdown:    "# " + method + " " + path + "\n\n" + summary + "\n",
	}
}

func seedService(t *testing.T, store *apicatalog.Store, name, group string) {
	t.Helper()

	svc := &apicatalog.Service{
		Name:   name,
		Group:  group,
		Kind:   "fake",
		Status: apicatalog.StatusPending,
		Config: json.RawMessage(`{}`),
	}
	if err := store.UpsertService(context.Background(), svc); err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

func TestRefreshAPIServiceWritesOperations(t *testing.T) {
	ctx := context.Background()

	provider := &fakeAPIProvider{runs: []fakeAPIRun{{
		complete: true,
		info:     apicatalog.ServiceInfo{Title: "Billing", Version: "1.0", ResolvedURL: "https://b/api"},
		ops: []apicatalog.RemoteOperation{
			remoteOp("POST", "/v1/invoices", "Create an invoice"),
			remoteOp("GET", "/v1/invoices/{id}", "Fetch one invoice"),
		},
	}}}

	m, store := newAPIManager(t, provider)
	seedService(t, store, "billing", "finance")

	if err := m.RefreshAPIService(ctx, "billing"); err != nil {
		t.Fatalf("RefreshAPIService() error = %v", err)
	}

	ops, total, err := store.OperationsPaged(ctx, "billing", "", "", "", 0, 50)
	if err != nil {
		t.Fatalf("OperationsPaged() error = %v", err)
	}
	if total != 2 || len(ops) != 2 {
		t.Fatalf("stored %d operations (total %d), want 2", len(ops), total)
	}

	// The markdown projection must exist on disk, because that is what the RAG
	// index reads.
	for _, op := range ops {
		file := filepath.Join(m.apisDir("billing"), op.OpSlug+".md")
		if _, err := os.Stat(file); err != nil {
			t.Errorf("markdown projection missing for %s: %v", op.OperationID, err)
		}
	}

	svc, err := store.GetService(ctx, "billing")
	if err != nil {
		t.Fatal(err)
	}
	if svc.Status != apicatalog.StatusReady {
		t.Errorf("status = %q, want ready (last error: %q)", svc.Status, svc.LastError)
	}
	if svc.OperationCount != 2 {
		t.Errorf("OperationCount = %d, want 2", svc.OperationCount)
	}
	if svc.Title != "Billing" || svc.ResolvedBaseURL != "https://b/api" {
		t.Errorf("document metadata not copied onto the service: %+v", svc)
	}
}

// TestRefreshAPIServicePrunesOnlyWhenComplete locks in the deletion rule: an
// operation that was not emitted may only be removed when the provider
// guaranteed it enumerated the whole document.
func TestRefreshAPIServicePrunesOnlyWhenComplete(t *testing.T) {
	ctx := context.Background()

	provider := &fakeAPIProvider{runs: []fakeAPIRun{
		{complete: true, ops: []apicatalog.RemoteOperation{
			remoteOp("POST", "/v1/invoices", "Create"),
			remoteOp("GET", "/v1/invoices", "List"),
		}},
		// An incomplete sweep that saw only one operation must not delete the
		// other: "not seen" means "not enumerated", not "gone".
		{complete: false, ops: []apicatalog.RemoteOperation{
			remoteOp("POST", "/v1/invoices", "Create"),
		}},
		// A complete sweep that saw only one may.
		{complete: true, ops: []apicatalog.RemoteOperation{
			remoteOp("POST", "/v1/invoices", "Create"),
		}},
	}}

	m, store := newAPIManager(t, provider)
	seedService(t, store, "billing", "")

	if err := m.RefreshAPIService(ctx, "billing"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, total, _ := store.OperationsPaged(ctx, "billing", "", "", "", 0, 50); total != 2 {
		t.Fatalf("after first sync total = %d, want 2", total)
	}

	if err := m.RefreshAPIService(ctx, "billing"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if _, total, _ := store.OperationsPaged(ctx, "billing", "", "", "", 0, 50); total != 2 {
		t.Fatalf("an incomplete sweep pruned an operation: total = %d, want 2", total)
	}

	if err := m.RefreshAPIService(ctx, "billing"); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if _, total, _ := store.OperationsPaged(ctx, "billing", "", "", "", 0, 50); total != 1 {
		t.Fatalf("a complete sweep did not prune: total = %d, want 1", total)
	}
}

// TestRefreshAPIServiceUnchangedKeepsCatalog is the regression test for the
// conditional-fetch path: a provider that reports the document has not moved
// emits nothing, and treating that as a complete sweep would delete every
// operation and report the API as empty.
func TestRefreshAPIServiceUnchangedKeepsCatalog(t *testing.T) {
	ctx := context.Background()

	provider := &fakeAPIProvider{runs: []fakeAPIRun{
		{complete: true, ops: []apicatalog.RemoteOperation{
			remoteOp("POST", "/v1/invoices", "Create"),
		}},
		{unchanged: true},
	}}

	m, store := newAPIManager(t, provider)
	seedService(t, store, "billing", "")

	if err := m.RefreshAPIService(ctx, "billing"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := m.RefreshAPIService(ctx, "billing"); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	_, total, err := store.OperationsPaged(ctx, "billing", "", "", "", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("an unchanged sync wiped the catalog: total = %d, want 1", total)
	}

	svc, _ := store.GetService(ctx, "billing")
	if svc.Status != apicatalog.StatusReady {
		t.Errorf("status after an unchanged sync = %q, want ready", svc.Status)
	}
}

// TestUnsafeSlugIsDroppedNotFatal covers a hostile document: an operation whose
// slug would escape the service directory is skipped, and — critically — is not
// marked seen, so a later prune cannot act on its name either.
func TestUnsafeSlugIsDroppedNotFatal(t *testing.T) {
	ctx := context.Background()

	bad := remoteOp("GET", "/ok", "Fine")
	bad.OpSlug = "../../etc/passwd"

	provider := &fakeAPIProvider{runs: []fakeAPIRun{{
		complete: true,
		ops:      []apicatalog.RemoteOperation{bad, remoteOp("GET", "/good", "Good")},
	}}}

	m, store := newAPIManager(t, provider)
	seedService(t, store, "billing", "")

	if err := m.RefreshAPIService(ctx, "billing"); err != nil {
		t.Fatalf("RefreshAPIService() error = %v", err)
	}

	_, total, err := store.OperationsPaged(ctx, "billing", "", "", "", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("stored %d operations, want only the safe one", total)
	}
}

func TestAPIGroups(t *testing.T) {
	ctx := context.Background()

	m, store := newAPIManager(t, &fakeAPIProvider{})
	seedService(t, store, "billing", "finance")
	seedService(t, store, "ledger", "finance")
	seedService(t, store, "misc", "")

	if _, err := m.UpsertAPIGroup(ctx, "finance", "Money movement APIs."); err != nil {
		t.Fatalf("UpsertAPIGroup() error = %v", err)
	}
	// A described-but-empty group must still be listed, so a user can write the
	// description before adding the services.
	if _, err := m.UpsertAPIGroup(ctx, "future", "Planned."); err != nil {
		t.Fatalf("UpsertAPIGroup() error = %v", err)
	}

	groups, err := m.APIGroups(ctx)
	if err != nil {
		t.Fatalf("APIGroups() error = %v", err)
	}

	got := map[string]apicatalog.GroupSummary{}
	for _, g := range groups {
		got[g.Name] = g
	}

	if g := got["finance"]; g.ServiceCount != 2 || g.Description != "Money movement APIs." {
		t.Errorf("finance = %+v, want 2 services and a description", g)
	}
	if g := got[apicatalog.GroupDefault]; g.ServiceCount != 1 {
		t.Errorf("ungrouped services were not folded into %q: %+v", apicatalog.GroupDefault, g)
	}
	if g, ok := got["future"]; !ok || g.ServiceCount != 0 {
		t.Errorf("described-but-empty group missing: %+v", g)
	}
}

// TestUpdateAPIServiceKeepsUnmentionedFields is the partial-update rule: a
// request that changes one field must not wipe the others.
func TestUpdateAPIServiceKeepsUnmentionedFields(t *testing.T) {
	ctx := context.Background()

	m, store := newAPIManager(t, &fakeAPIProvider{})
	seedService(t, store, "billing", "finance")

	if err := m.mutateService(ctx, "billing", func(svc *apicatalog.Service) error {
		svc.BaseURL = "https://internal"
		svc.Specs = []string{"0 2 * * *"}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var update apicatalog.ServiceUpdate
	if err := json.Unmarshal([]byte(`{"description":"Invoicing."}`), &update); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateAPIService(ctx, "billing", update, nil); err != nil {
		t.Fatalf("UpdateAPIService() error = %v", err)
	}

	svc, err := store.GetService(ctx, "billing")
	if err != nil {
		t.Fatal(err)
	}
	if svc.Description != "Invoicing." {
		t.Errorf("description = %q, want the update applied", svc.Description)
	}
	if svc.BaseURL != "https://internal" {
		t.Errorf("base_url = %q, want the stored override preserved", svc.BaseURL)
	}
	if len(svc.Specs) != 1 || svc.Specs[0] != "0 2 * * *" {
		t.Errorf("specs = %v, want the stored schedule preserved", svc.Specs)
	}
	if svc.Group != "finance" {
		t.Errorf("group = %q, want it preserved", svc.Group)
	}
}

// TestServiceUpdateNullClears covers the other half of the rule: an explicit
// null must clear, where an omitted field keeps.
func TestServiceUpdateNullClears(t *testing.T) {
	svc := &apicatalog.Service{
		Description: "old",
		BaseURL:     "https://internal",
		SpecPatch:   json.RawMessage(`{"a":1}`),
	}

	var update apicatalog.ServiceUpdate
	if err := json.Unmarshal([]byte(`{"base_url":null}`), &update); err != nil {
		t.Fatal(err)
	}

	rerender, err := update.Apply(svc)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !rerender {
		t.Errorf("clearing the base URL did not request a re-render")
	}
	if svc.BaseURL != "" {
		t.Errorf("base_url = %q, want cleared by an explicit null", svc.BaseURL)
	}
	if svc.Description != "old" {
		t.Errorf("description = %q, want kept when omitted", svc.Description)
	}
	if string(svc.SpecPatch) != `{"a":1}` {
		t.Errorf("spec_patch = %s, want kept when omitted", svc.SpecPatch)
	}
}

func TestServiceUpdateRejectsBadInput(t *testing.T) {
	tests := []struct {
		name   string
		update string
	}{
		{name: "base url scheme", update: `{"base_url":"ftp://x"}`},
		{name: "spec patch not an object", update: `{"spec_patch":[1,2]}`},
		{name: "group name", update: `{"group":"Not Valid"}`},
		{name: "refresh interval", update: `{"refresh_interval":"soon"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var update apicatalog.ServiceUpdate
			if err := json.Unmarshal([]byte(tt.update), &update); err != nil {
				t.Fatal(err)
			}
			if _, err := update.Apply(&apicatalog.Service{}); err == nil {
				t.Fatalf("Apply() error = nil, want a validation failure")
			}
		})
	}
}
