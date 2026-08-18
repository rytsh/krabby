package apicatalog

import (
	"strings"
	"testing"
)

func TestResolvePath(t *testing.T) {
	tests := map[string]struct {
		template string
		params   map[string]string
		want     string
		wantErr  string
	}{
		"substitutes": {
			template: "/v1/invoices/{id}",
			params:   map[string]string{"id": "inv_42"},
			want:     "/v1/invoices/inv_42",
		},
		"multiple params": {
			template: "/orgs/{org}/repos/{repo}",
			params:   map[string]string{"org": "acme", "repo": "site"},
			want:     "/orgs/acme/repos/site",
		},
		// The security property: no parameter value may change the request
		// shape. A traversal, a query and a scheme-relative host must all end
		// up inert inside one path segment.
		"escapes traversal": {
			template: "/v1/files/{name}",
			params:   map[string]string{"name": "../../admin"},
			want:     "/v1/files/..%2F..%2Fadmin",
		},
		"escapes query and fragment": {
			template: "/v1/items/{id}",
			params:   map[string]string{"id": "1?admin=true#x"},
			want:     "/v1/items/1%3Fadmin=true%23x",
		},
		"missing is an error, not an empty segment": {
			template: "/v1/invoices/{id}/lines/{line}",
			params:   map[string]string{"id": "1"},
			wantErr:  "missing path parameter(s): line",
		},
		"no placeholders": {
			template: "/v1/health",
			params:   nil,
			want:     "/v1/health",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ResolvePath(tt.template, tt.params)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}

				return
			}
			if err != nil {
				t.Fatalf("ResolvePath() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ResolvePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppendQuery(t *testing.T) {
	got, err := AppendQuery("https://api.example.com/v1/items?limit=10", map[string]string{
		"cursor": "a b&c",
	})
	if err != nil {
		t.Fatalf("AppendQuery() error = %v", err)
	}
	// The hardcoded query must survive, and the value must be encoded.
	if !strings.Contains(got, "limit=10") || !strings.Contains(got, "cursor=a+b%26c") {
		t.Fatalf("AppendQuery() = %q", got)
	}

	same, err := AppendQuery("https://api.example.com/x", nil)
	if err != nil || same != "https://api.example.com/x" {
		t.Fatalf("no-params AppendQuery() = %q, %v", same, err)
	}
}

func TestMissingRequired(t *testing.T) {
	d := &Detail{Parameters: []Param{
		{Name: "id", In: "path", Required: true},
		{Name: "expand", In: "query"},
		{Name: "org", In: "query", Required: true},
		// A required header must not be demanded: the provider config supplies
		// auth headers the caller never names.
		{Name: "Authorization", In: "header", Required: true},
	}}

	missing := MissingRequired(d, CallRequest{Query: map[string]string{"org": "acme"}})
	if len(missing) != 1 || missing[0] != "path:id" {
		t.Fatalf("MissingRequired() = %v, want [path:id]", missing)
	}

	none := MissingRequired(d, CallRequest{
		PathParams: map[string]string{"id": "1"},
		Query:      map[string]string{"org": "acme"},
	})
	if len(none) != 0 {
		t.Fatalf("MissingRequired() = %v, want none", none)
	}
}

func TestNormalizeTimeout(t *testing.T) {
	if got := NormalizeTimeout(0); got != DefaultCallTimeout {
		t.Errorf("zero → %v, want default", got)
	}
	if got := NormalizeTimeout(10 * MaxCallTimeout); got != MaxCallTimeout {
		t.Errorf("over the cap → %v, want the cap", got)
	}
}

func TestTruncateBody(t *testing.T) {
	body, cut := TruncateBody(make([]byte, MaxCallBodyBytes+1))
	if !cut || len(body) != MaxCallBodyBytes {
		t.Fatalf("TruncateBody() len=%d cut=%v", len(body), cut)
	}

	body, cut = TruncateBody([]byte("small"))
	if cut || body != "small" {
		t.Fatalf("small body was cut")
	}
}
