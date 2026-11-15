package apicatalog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rakunlabs/bw"
)

func newStore(t *testing.T) (*Store, context.Context) {
	t.Helper()

	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := New(db)
	if err != nil {
		t.Fatal(err)
	}

	return store, context.Background()
}

func mustService(t *testing.T, s *Store, name, group string) {
	t.Helper()

	if err := s.UpsertService(context.Background(), &Service{Name: name, Group: group, Kind: KindOpenAPI}); err != nil {
		t.Fatal(err)
	}
}

func mustOperation(t *testing.T, s *Store, service, method, path, operationID, summary string, tags ...string) *Operation {
	t.Helper()

	slug := OpSlug(method, path)
	op := &Operation{
		ID:          OperationID(service, slug),
		Service:     service,
		OpSlug:      slug,
		OperationID: operationID,
		Method:      method,
		Path:        path,
		Summary:     summary,
		Tags:        tags,
	}
	if err := s.UpsertOperation(context.Background(), op); err != nil {
		t.Fatal(err)
	}

	return op
}

func serviceNames(services []*Service) string {
	names := make([]string, len(services))
	for i, svc := range services {
		names[i] = svc.Name
	}

	return strings.Join(names, ",")
}

func operationPaths(ops []*Operation) string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.Method + " " + op.Path
	}

	return strings.Join(out, ",")
}

func TestMissingRecordsAreNilNotErrors(t *testing.T) {
	s, ctx := newStore(t)

	if svc, err := s.GetService(ctx, "nope"); svc != nil || err != nil {
		t.Errorf("GetService = %v, %v", svc, err)
	}
	if grp, err := s.GetGroup(ctx, "nope"); grp != nil || err != nil {
		t.Errorf("GetGroup = %v, %v", grp, err)
	}
	if op, err := s.GetOperation(ctx, "nope/nope"); op != nil || err != nil {
		t.Errorf("GetOperation = %v, %v", op, err)
	}

	// Deleting something already gone is the state the caller wanted.
	for name, err := range map[string]error{
		"service":   s.DeleteService(ctx, "nope"),
		"group":     s.DeleteGroup(ctx, "nope"),
		"operation": s.DeleteOperation(ctx, "nope/nope"),
	} {
		if err != nil {
			t.Errorf("deleting a missing %s: %v", name, err)
		}
	}
}

// Listings are handed to JSON encoders; an empty catalog must encode as [],
// not null.
func TestEmptyListingsAreEmptySlices(t *testing.T) {
	s, ctx := newStore(t)

	services, err := s.ListServices(ctx)
	if err != nil || services == nil || len(services) != 0 {
		t.Errorf("ListServices = %v, %v", services, err)
	}

	ops, err := s.Operations(ctx, "billing")
	if err != nil || ops == nil || len(ops) != 0 {
		t.Errorf("Operations = %v, %v", ops, err)
	}

	paged, total, err := s.ServicesPaged(ctx, "", "", 0, 10)
	if err != nil || paged == nil || total != 0 {
		t.Errorf("ServicesPaged = %v, %d, %v", paged, total, err)
	}
}

func TestListServicesIsSortedByName(t *testing.T) {
	s, ctx := newStore(t)
	for _, name := range []string{"payments", "auth", "billing"} {
		mustService(t, s, name, "")
	}

	services, err := s.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := serviceNames(services); got != "auth,billing,payments" {
		t.Errorf("order = %s", got)
	}
}

func TestServicesPagedFiltersAndPages(t *testing.T) {
	s, ctx := newStore(t)
	mustService(t, s, "billing", "finance")
	mustService(t, s, "billing-legacy", "finance")
	mustService(t, s, "payments", "finance")
	mustService(t, s, "auth", "")
	mustService(t, s, "search", "platform")

	for name, tt := range map[string]struct {
		group, search string
		offset, limit int
		want          string
		total         int
	}{
		"everything":                   {"", "", 0, 10, "auth,billing,billing-legacy,payments,search", 5},
		"one group":                    {"finance", "", 0, 10, "billing,billing-legacy,payments", 3},
		"group is case-insensitive":    {" Finance ", "", 0, 10, "billing,billing-legacy,payments", 3},
		"name search":                  {"", "bill", 0, 10, "billing,billing-legacy", 2},
		"search is case-insensitive":   {"", "BILL", 0, 10, "billing,billing-legacy", 2},
		"group and search":             {"finance", "pay", 0, 10, "payments", 1},
		"first page":                   {"", "", 0, 2, "auth,billing", 5},
		"second page":                  {"", "", 2, 2, "billing-legacy,payments", 5},
		"last partial page":            {"", "", 4, 2, "search", 5},
		"offset past the end":          {"", "", 9, 2, "", 5},
		"negative offset is the start": {"", "", -3, 2, "auth,billing", 5},
		"zero limit still counts":      {"", "", 0, 0, "", 5},
		"no match":                     {"", "zzz", 0, 10, "", 0},
	} {
		got, total, err := s.ServicesPaged(ctx, tt.group, tt.search, tt.offset, tt.limit)
		if err != nil {
			t.Errorf("%s: %v", name, err)

			continue
		}
		if serviceNames(got) != tt.want || total != tt.total {
			t.Errorf("%s: got %q (total %d), want %q (total %d)", name, serviceNames(got), total, tt.want, tt.total)
		}
	}
}

// "default" is the display name of the unset group, so asking for it has to
// find the services that never named one.
func TestDefaultGroupSelectsServicesWithoutOne(t *testing.T) {
	s, ctx := newStore(t)
	mustService(t, s, "auth", "")
	mustService(t, s, "billing", "finance")

	got, total, err := s.ServicesPaged(ctx, GroupDefault, "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if serviceNames(got) != "auth" || total != 1 {
		t.Errorf("default group = %q (total %d), want only the ungrouped service", serviceNames(got), total)
	}
}

func TestGroupsMergeCountsWithDescriptions(t *testing.T) {
	s, ctx := newStore(t)
	mustService(t, s, "billing", "finance")
	mustService(t, s, "payments", "finance")
	mustService(t, s, "auth", "")

	if _, err := s.UpsertGroup(ctx, "Finance", "  Invoicing and payments.  "); err != nil {
		t.Fatal(err)
	}
	// Described before it has any service.
	if _, err := s.UpsertGroup(ctx, "planned", "Not built yet."); err != nil {
		t.Fatal(err)
	}

	groups, err := s.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}

	want := []GroupSummary{
		{Name: "default", ServiceCount: 1},
		{Name: "finance", Description: "Invoicing and payments.", ServiceCount: 2},
		{Name: "planned", Description: "Not built yet.", ServiceCount: 0},
	}
	if len(groups) != len(want) {
		t.Fatalf("groups = %+v, want %+v", groups, want)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Errorf("group %d = %+v, want %+v", i, groups[i], want[i])
		}
	}
}

func TestDescribingTheDefaultGroup(t *testing.T) {
	s, ctx := newStore(t)
	mustService(t, s, "auth", "")

	if _, err := s.UpsertGroup(ctx, "default", "Everything not filed elsewhere."); err != nil {
		t.Fatal(err)
	}

	groups, err := s.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0] != (GroupSummary{Name: "default", Description: "Everything not filed elsewhere.", ServiceCount: 1}) {
		t.Errorf("groups = %+v, want one default group carrying both the count and the description", groups)
	}
}

func TestUpsertGroupKeepsCreatedAt(t *testing.T) {
	s, ctx := newStore(t)

	first, err := s.UpsertGroup(ctx, "finance", "v1")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)

	second, err := s.UpsertGroup(ctx, "finance", "v2")
	if err != nil {
		t.Fatal(err)
	}

	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt moved from %v to %v on update", first.CreatedAt, second.CreatedAt)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("UpdatedAt did not advance: %v then %v", first.UpdatedAt, second.UpdatedAt)
	}

	stored, err := s.GetGroup(ctx, "FINANCE")
	if err != nil || stored == nil || stored.Description != "v2" {
		t.Errorf("GetGroup = %+v, %v", stored, err)
	}
}

func TestUpsertGroupRejectsUnsafeNames(t *testing.T) {
	s, ctx := newStore(t)

	for _, name := range []string{"a/b", "two words", "..", "-dash"} {
		if _, err := s.UpsertGroup(ctx, name, "x"); err == nil {
			t.Errorf("group name %q was accepted", name)
		}
	}
}

// Removing a description must never orphan the services it described.
func TestDeleteGroupKeepsItsServices(t *testing.T) {
	s, ctx := newStore(t)
	mustService(t, s, "billing", "finance")
	if _, err := s.UpsertGroup(ctx, "finance", "Invoicing."); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteGroup(ctx, "finance"); err != nil {
		t.Fatal(err)
	}

	svc, err := s.GetService(ctx, "billing")
	if err != nil || svc == nil || svc.Group != "finance" {
		t.Fatalf("service after its group's description was deleted: %+v, %v", svc, err)
	}

	groups, err := s.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0] != (GroupSummary{Name: "finance", ServiceCount: 1}) {
		t.Errorf("groups = %+v, want finance still listed, without a description", groups)
	}
}

func TestOperationsAreScopedToTheirService(t *testing.T) {
	s, ctx := newStore(t)
	mustOperation(t, s, "billing", "GET", "/v1/invoices", "listInvoices", "List invoices")
	mustOperation(t, s, "billing", "POST", "/v1/invoices", "createInvoice", "Create an invoice")
	mustOperation(t, s, "auth", "POST", "/v1/token", "issueToken", "Issue a token")

	ops, err := s.Operations(ctx, "billing")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 {
		t.Fatalf("billing has %d operations, want 2", len(ops))
	}
	for i, op := range ops {
		if op.Service != "billing" {
			t.Errorf("operation %s leaked in from another service", op.ID)
		}
		if i > 0 && ops[i-1].OpSlug > op.OpSlug {
			t.Errorf("operations not sorted by slug: %s before %s", ops[i-1].OpSlug, op.OpSlug)
		}
	}
}

func TestDeleteServiceRemovesItsOperationsOnly(t *testing.T) {
	s, ctx := newStore(t)
	mustService(t, s, "billing", "")
	mustService(t, s, "auth", "")
	gone := mustOperation(t, s, "billing", "GET", "/v1/invoices", "listInvoices", "")
	kept := mustOperation(t, s, "auth", "POST", "/v1/token", "issueToken", "")

	if err := s.DeleteService(ctx, "billing"); err != nil {
		t.Fatal(err)
	}

	if svc, _ := s.GetService(ctx, "billing"); svc != nil {
		t.Error("the service record survived its deletion")
	}
	if op, _ := s.GetOperation(ctx, gone.ID); op != nil {
		t.Error("an operation outlived its service")
	}
	if op, _ := s.GetOperation(ctx, kept.ID); op == nil {
		t.Error("deleting one service removed another service's operation")
	}
	if svc, _ := s.GetService(ctx, "auth"); svc == nil {
		t.Error("deleting one service removed another")
	}
}

func TestOperationsPagedFilters(t *testing.T) {
	s, ctx := newStore(t)
	mustOperation(t, s, "billing", "GET", "/v1/invoices", "listInvoices", "List invoices", "Invoices")
	mustOperation(t, s, "billing", "POST", "/v1/invoices", "createInvoice", "Create an invoice", "Invoices", "Write")
	mustOperation(t, s, "billing", "GET", "/v1/customers", "listCustomers", "List customers", "Customers")
	mustOperation(t, s, "billing", "DELETE", "/v1/customers/{id}", "removeCustomer", "Remove a customer", "Customers", "Write")
	mustOperation(t, s, "auth", "GET", "/v1/invoices", "shadow", "Same path, other service", "Invoices")

	for name, tt := range map[string]struct {
		search, tag, method string
		offset, limit       int
		want                string
		total               int
	}{
		// Sorted by path, then method.
		"everything": {"", "", "", 0, 10,
			"GET /v1/customers,DELETE /v1/customers/{id},GET /v1/invoices,POST /v1/invoices", 4},
		"by tag":                     {"", "Invoices", "", 0, 10, "GET /v1/invoices,POST /v1/invoices", 2},
		"tag is case-insensitive":    {"", " invoices ", "", 0, 10, "GET /v1/invoices,POST /v1/invoices", 2},
		"by method":                  {"", "", "get", 0, 10, "GET /v1/customers,GET /v1/invoices", 2},
		"tag and method":             {"", "write", "POST", 0, 10, "POST /v1/invoices", 1},
		"search matches the path":    {"customers", "", "", 0, 10, "GET /v1/customers,DELETE /v1/customers/{id}", 2},
		"search matches the summary": {"remove a", "", "", 0, 10, "DELETE /v1/customers/{id}", 1},
		"search matches the id":      {"createinvoice", "", "", 0, 10, "POST /v1/invoices", 1},
		"search and tag":             {"list", "Customers", "", 0, 10, "GET /v1/customers", 1},
		"paged":                      {"", "", "", 1, 2, "DELETE /v1/customers/{id},GET /v1/invoices", 4},
		"offset past the end":        {"", "", "", 10, 2, "", 4},
		"no match":                   {"refund", "", "", 0, 10, "", 0},
	} {
		got, total, err := s.OperationsPaged(ctx, "billing", tt.search, tt.tag, tt.method, tt.offset, tt.limit)
		if err != nil {
			t.Errorf("%s: %v", name, err)

			continue
		}
		if operationPaths(got) != tt.want || total != tt.total {
			t.Errorf("%s:\n got %q (total %d)\nwant %q (total %d)", name, operationPaths(got), total, tt.want, tt.total)
		}
	}
}

// A reader has one of three spellings in hand: what listings print, what the
// spec document shows, and what file paths use.
func TestFindOperationAcceptsEveryHandle(t *testing.T) {
	s, ctx := newStore(t)
	want := mustOperation(t, s, "billing", "GET", "/v1/invoices/{id}", "getInvoice", "Fetch one invoice")
	mustOperation(t, s, "auth", "GET", "/v1/invoices/{id}", "getInvoice", "Same id, other service")

	for name, handle := range map[string]string{
		"operation id":        "getInvoice",
		"padded operation id": "  getInvoice  ",
		"slug":                want.OpSlug,
	} {
		got, err := s.FindOperation(ctx, "billing", handle)
		if err != nil {
			t.Errorf("%s: %v", name, err)

			continue
		}
		if got == nil || got.ID != want.ID {
			t.Errorf("%s: FindOperation(%q) = %+v, want %s", name, handle, got, want.ID)
		}
	}

	for name, handle := range map[string]string{"empty": "", "blank": "   ", "unknown": "deleteEverything"} {
		if got, err := s.FindOperation(ctx, "billing", handle); got != nil || err != nil {
			t.Errorf("%s: FindOperation(%q) = %+v, %v; want nil", name, handle, got, err)
		}
	}
}

// An operation without an operationId is identified as "METHOD /path", and
// that is the handle a reader copies out of a listing.
func TestFindOperationByMethodAndPath(t *testing.T) {
	s, ctx := newStore(t)
	want := mustOperation(t, s, "billing", "GET", "/v1/health", "GET /v1/health", "")

	got, err := s.FindOperation(ctx, "billing", "GET /v1/health")
	if err != nil || got == nil || got.ID != want.ID {
		t.Fatalf("FindOperation = %+v, %v", got, err)
	}
}

func TestTagsAreDistinctSortedAndKeepTheirCasing(t *testing.T) {
	s, ctx := newStore(t)
	mustOperation(t, s, "billing", "GET", "/a", "a", "", "Invoices", " write ")
	mustOperation(t, s, "billing", "GET", "/b", "b", "", "invoices", "Customers", "")
	mustOperation(t, s, "auth", "GET", "/c", "c", "", "Tokens")

	tags, err := s.Tags(ctx, "billing")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tags, ","); got != "Customers,Invoices,write" {
		t.Errorf("tags = %q, want distinct, sorted, first spelling kept, other services excluded", got)
	}

	none, err := s.Tags(ctx, "empty")
	if err != nil || len(none) != 0 {
		t.Errorf("Tags of a service with no operations = %v, %v", none, err)
	}
}

func TestUpsertOperationDerivesNormalizedTags(t *testing.T) {
	s, ctx := newStore(t)
	op := mustOperation(t, s, "billing", "GET", "/a", "a", "", " Invoices ", "INVOICES", "", "Write")

	stored, err := s.GetOperation(ctx, op.ID)
	if err != nil || stored == nil {
		t.Fatal(err)
	}
	if got := strings.Join(stored.TagsNorm, ","); got != "invoices,write" {
		t.Errorf("TagsNorm = %q, want lowercased and de-duplicated", got)
	}
	// The tags as written are what a reader sees.
	if got := strings.Join(stored.Tags, "|"); got != " Invoices |INVOICES||Write" {
		t.Errorf("Tags = %q, want them stored as given", got)
	}

	// Replacing the tags replaces the derived form too, including to nothing.
	op.Tags = nil
	if err := s.UpsertOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if stored, _ = s.GetOperation(ctx, op.ID); len(stored.TagsNorm) != 0 {
		t.Errorf("TagsNorm = %q after the tags were removed", stored.TagsNorm)
	}
	if got, total, _ := s.OperationsPaged(ctx, "billing", "", "invoices", "", 0, 10); total != 0 {
		t.Errorf("a removed tag still matches: %s", operationPaths(got))
	}
}

func TestUpsertReplacesRatherThanDuplicates(t *testing.T) {
	s, ctx := newStore(t)
	mustOperation(t, s, "billing", "GET", "/v1/invoices", "listInvoices", "old summary")
	mustOperation(t, s, "billing", "GET", "/v1/invoices", "listInvoices", "new summary")

	ops, err := s.Operations(ctx, "billing")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Summary != "new summary" {
		t.Errorf("operations = %+v, want the one record updated in place", ops)
	}
}
