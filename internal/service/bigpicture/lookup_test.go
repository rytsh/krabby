package bigpicture

import (
	"strings"
	"testing"
)

func TestValidateLookup(t *testing.T) {
	granted := map[string]map[string]bool{"config": {"get_config": true}}
	catalog := map[string][]string{"config": {"consul", "vault"}}
	material := "### repo:acme/orders config.go\nconst path = \"admin/orders\"\nadministrator"
	cases := []struct {
		call LookupCall
		ok   bool
	}{
		{LookupCall{Server: "config", Tool: "get_config", Arguments: map[string]any{"path": "admin/orders", "source": "consul"}}, true},
		{LookupCall{Server: "config", Tool: "get_config", Arguments: map[string]any{"path": "secret/prod/db"}}, false},
		{LookupCall{Server: "config", Tool: "get_config", Arguments: map[string]any{"path": "admin"}}, true},
		{LookupCall{Server: "config", Tool: "get_config", Arguments: map[string]any{"path": "dmin/orders"}}, false},
		{LookupCall{Server: "config", Tool: "delete_config", Arguments: map[string]any{"path": "admin/orders"}}, false},
		{LookupCall{Server: "other", Tool: "get_config", Arguments: map[string]any{"path": "admin/orders"}}, false},
		{LookupCall{Server: "config", Tool: "get_config", Arguments: map[string]any{"path": map[string]any{"x": "admin/orders"}}}, false},
	}
	for _, c := range cases {
		if reason := ValidateLookup(c.call, granted, catalog, material); (reason == "") != c.ok {
			t.Errorf("%v: reason %q, want ok=%t", c.call.Arguments, reason, c.ok)
		}
	}
}

func TestRedactSecrets(t *testing.T) {
	in := `kafka.topic: order-created
db.password: hunter2
"apiKey": "abc123", "host": "db.internal"
url: postgres://svc:pw@db.internal/orders`
	out := RedactSecrets(in)
	for _, leaked := range []string{"hunter2", "abc123", "svc:pw"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("secret %q leaked: %s", leaked, out)
		}
	}
	for _, kept := range []string{"order-created", "db.password", "db.internal", "apiKey"} {
		if !strings.Contains(out, kept) {
			t.Fatalf("%q removed: %s", kept, out)
		}
	}
}
