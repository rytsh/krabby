package credentials

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rakunlabs/bw"
)

func TestHostPath(t *testing.T) {
	tests := []struct {
		url      string
		wantHost string
		wantPath string
	}{
		{url: "git@gitlab.example.com:group/proj.git", wantHost: "gitlab.example.com", wantPath: "group/proj"},
		{url: "https://github.com/rakunlabs/ada.git", wantHost: "github.com", wantPath: "rakunlabs/ada"},
		{url: "ssh://git@gitlab.com:2222/group/sub/proj.git", wantHost: "gitlab.com", wantPath: "group/sub/proj"},
		{url: "https://gitlab.com", wantHost: "gitlab.com", wantPath: ""},
		{url: "gitlab.example.com", wantHost: "gitlab.example.com", wantPath: ""},
		{url: "github.com/rakunlabs", wantHost: "github.com", wantPath: "rakunlabs"},
		{url: "not-a-host", wantHost: "", wantPath: ""},
		{url: "", wantHost: "", wantPath: ""},
	}

	for _, tt := range tests {
		host, path := HostPath(tt.url)
		if host != tt.wantHost || path != tt.wantPath {
			t.Errorf("HostPath(%q) = (%q, %q), want (%q, %q)", tt.url, host, path, tt.wantHost, tt.wantPath)
		}
	}
}

func TestNormalizePattern(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "gitlab.example.com", want: "gitlab.example.com"},
		{in: "https://gitlab.example.com/", want: "gitlab.example.com"},
		{in: "https://github.com/rakunlabs", want: "github.com/rakunlabs"},
		{in: "git@github.com:rakunlabs", want: "github.com/rakunlabs"},
		{in: "github.com/rakunlabs/", want: "github.com/rakunlabs"},
	}

	for _, tt := range tests {
		if got := NormalizePattern(tt.in); got != tt.want {
			t.Errorf("NormalizePattern(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()

	dir := t.TempDir()

	db, err := bw.Open(filepath.Join(dir, "db"), bw.WithInMemory(true))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	store, err := New(db, filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	return store
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	sshKey := "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"

	// Host-wide ssh key for gitlab.
	if err := store.Set(ctx, &Credential{Pattern: "gitlab.example.com", Secret: sshKey}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// More specific token for one github org.
	if err := store.Set(ctx, &Credential{Pattern: "github.com/myorg", Secret: "glpat-123"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Kind inference.
	creds, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	for _, c := range creds {
		switch c.Pattern {
		case "gitlab.example.com":
			if c.Kind != KindSSH {
				t.Errorf("gitlab kind = %q, want ssh", c.Kind)
			}
		case "github.com/myorg":
			if c.Kind != KindToken {
				t.Errorf("github kind = %q, want token", c.Kind)
			}

			if c.Username != "oauth2" {
				t.Errorf("default username = %q, want oauth2", c.Username)
			}
		}
	}

	// SSH url on the gitlab host matches host pattern.
	auth, err := store.Resolve(ctx, "git@gitlab.example.com:group/proj.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if auth == nil || auth.SSHKeyPath == "" || auth.Token != "" {
		t.Errorf("gitlab resolve = %+v, want ssh key path", auth)
	}

	// Org-scoped token matches.
	auth, err = store.Resolve(ctx, "https://github.com/myorg/repo.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if auth == nil || auth.Token != "glpat-123" || auth.Username != "oauth2" {
		t.Errorf("github resolve = %+v, want token auth", auth)
	}

	// Other org on github: no match.
	auth, err = store.Resolve(ctx, "https://github.com/other/repo.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if auth != nil {
		t.Errorf("unexpected match for other org: %+v", auth)
	}

	// Most specific pattern wins.
	if err := store.Set(ctx, &Credential{Pattern: "github.com", Secret: "host-wide-token"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	auth, err = store.Resolve(ctx, "https://github.com/myorg/repo.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if auth == nil || auth.Token != "glpat-123" {
		t.Errorf("specific pattern should win, got %+v", auth)
	}

	auth, err = store.Resolve(ctx, "https://github.com/other/repo.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if auth == nil || auth.Token != "host-wide-token" {
		t.Errorf("host-wide pattern should match other org, got %+v", auth)
	}

	// Delete removes the credential.
	if err := store.Delete(ctx, "github.com/myorg"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	auth, err = store.Resolve(ctx, "https://github.com/myorg/repo.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if auth == nil || auth.Token != "host-wide-token" {
		t.Errorf("after delete, host-wide should match, got %+v", auth)
	}
}

func TestSetValidation(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.Set(ctx, &Credential{Pattern: "", Secret: "x"}); err == nil {
		t.Error("empty pattern accepted")
	}

	if err := store.Set(ctx, &Credential{Pattern: "gitlab.com", Secret: ""}); err == nil {
		t.Error("empty secret accepted")
	}

	if err := store.Set(ctx, &Credential{Pattern: "gitlab.com", Secret: "x", Kind: "bogus"}); err == nil {
		t.Error("bogus kind accepted")
	}
}
