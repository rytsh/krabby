// Package credentials stores git credentials (SSH keys, HTTPS tokens) keyed by
// host or host/path-prefix patterns, resolved per repository URL.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"
)

// Credential kinds.
const (
	KindSSH   = "ssh"   // Secret is a private key (PEM); used for ssh urls.
	KindToken = "token" // Secret is an access token (PAT); used for https urls.
)

// Credential maps a pattern to git auth material.
type Credential struct {
	// Pattern is a host ("gitlab.example.com") or host/path prefix
	// ("github.com/rakunlabs"). The most specific match wins.
	Pattern string `bw:"pattern,pk" json:"pattern"`
	Kind    string `bw:"kind"       json:"kind"`
	// Username is used for https token auth (default "oauth2").
	Username string `bw:"username" json:"username,omitempty"`
	// Secret is the PEM private key or the token. Never serialized to JSON.
	Secret    string    `bw:"secret" json:"-"`
	UpdatedAt time.Time `bw:"updated_at" json:"updated_at,omitzero"`
}

// Store persists credentials and materializes SSH keys as files for git.
type Store struct {
	bucket  *bw.Bucket[Credential]
	keysDir string
}

// New opens the credentials bucket. keysDir holds materialized SSH key files.
func New(db *bw.DB, keysDir string) (*Store, error) {
	bucket, err := bw.RegisterBucket[Credential](db, "credentials")
	if err != nil {
		return nil, fmt.Errorf("register credentials bucket; %w", err)
	}

	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir keys dir; %w", err)
	}

	return &Store{bucket: bucket, keysDir: keysDir}, nil
}

// Set validates and stores a credential. Kind is inferred from the secret when
// empty. For SSH kinds the key file is (re)written.
func (s *Store) Set(ctx context.Context, cred *Credential) error {
	cred.Pattern = NormalizePattern(cred.Pattern)
	if cred.Pattern == "" {
		return errors.New("pattern is required")
	}

	if cred.Secret == "" {
		return errors.New("secret is required")
	}

	if cred.Kind == "" {
		cred.Kind = inferKind(cred.Secret)
	}

	if cred.Kind != KindSSH && cred.Kind != KindToken {
		return fmt.Errorf("invalid kind %q (want %q or %q)", cred.Kind, KindSSH, KindToken)
	}

	if cred.Kind == KindToken && cred.Username == "" {
		cred.Username = "oauth2"
	}

	cred.UpdatedAt = time.Now()

	if err := s.bucket.Insert(ctx, cred); err != nil {
		return fmt.Errorf("store credential %s; %w", cred.Pattern, err)
	}

	if cred.Kind == KindSSH {
		if _, err := s.writeKeyFile(cred); err != nil {
			return err
		}
	}

	return nil
}

// List returns all credentials. Secret is excluded from JSON marshaling but
// present in memory; callers must not log it.
func (s *Store) List(ctx context.Context) ([]*Credential, error) {
	q, err := query.Parse("_limit=10000")
	if err != nil {
		return nil, fmt.Errorf("parse query; %w", err)
	}

	creds, err := s.bucket.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list credentials; %w", err)
	}

	if creds == nil {
		creds = []*Credential{}
	}

	return creds, nil
}

// Delete removes a credential and its key file.
func (s *Store) Delete(ctx context.Context, pattern string) error {
	pattern = NormalizePattern(pattern)

	if err := s.bucket.Delete(ctx, pattern); err != nil && !errors.Is(err, bw.ErrNotFound) {
		return fmt.Errorf("delete credential %s; %w", pattern, err)
	}

	_ = os.Remove(s.keyFilePath(pattern))

	return nil
}

// Auth is resolved git authentication material for a repository URL.
type Auth struct {
	// SSHKeyPath is a private key file for ssh urls.
	SSHKeyPath string
	// Username/Token are for https urls.
	Username string
	Token    string
}

// Resolve returns auth for repoURL, or nil when no pattern matches.
func (s *Store) Resolve(ctx context.Context, repoURL string) (*Auth, error) {
	host, path := HostPath(repoURL)
	if host == "" {
		return nil, nil
	}

	full := host
	if path != "" {
		full = host + "/" + path
	}

	creds, err := s.List(ctx)
	if err != nil {
		return nil, err
	}

	var best *Credential

	for _, cred := range creds {
		p := cred.Pattern
		if full == p || strings.HasPrefix(full, p+"/") {
			if best == nil || len(p) > len(best.Pattern) {
				best = cred
			}
		}
	}

	if best == nil {
		return nil, nil
	}

	if best.Kind == KindToken {
		return &Auth{Username: best.Username, Token: best.Secret}, nil
	}

	keyPath, err := s.writeKeyFile(best)
	if err != nil {
		return nil, err
	}

	return &Auth{SSHKeyPath: keyPath}, nil
}

func (s *Store) keyFilePath(pattern string) string {
	name := strings.NewReplacer("/", "_", ":", "_").Replace(pattern)

	return filepath.Join(s.keysDir, name+".pem")
}

// writeKeyFile materializes the SSH key with 0600 perms and returns its path.
func (s *Store) writeKeyFile(cred *Credential) (string, error) {
	path := s.keyFilePath(cred.Pattern)

	secret := cred.Secret
	if !strings.HasSuffix(secret, "\n") {
		secret += "\n" // ssh requires a trailing newline on PEM files
	}

	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		return "", fmt.Errorf("write key file %s; %w", path, err)
	}

	return path, nil
}

func inferKind(secret string) string {
	if strings.Contains(secret, "-----BEGIN") {
		return KindSSH
	}

	return KindToken
}

// NormalizePattern strips scheme/user prefixes and .git/trailing slashes so
// "https://gitlab.com/group" and "gitlab.com/group/" store identically.
func NormalizePattern(pattern string) string {
	p := strings.TrimSpace(pattern)

	if host, path := HostPath(p); host != "" {
		if path != "" {
			return host + "/" + path
		}

		return host
	}

	return strings.Trim(p, "/")
}

// HostPath extracts (host, path) from git URL forms:
// scp-like git@host:path, scheme://[user@]host[:port]/path, or bare host/path.
func HostPath(url string) (string, string) {
	s := strings.TrimSpace(url)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")

	if s == "" {
		return "", ""
	}

	// scheme://[user@]host[:port]/path
	if _, rest, ok := strings.Cut(s, "://"); ok {
		if at := strings.LastIndex(rest, "@"); at != -1 {
			rest = rest[at+1:]
		}

		host, path, _ := strings.Cut(rest, "/")
		host, _, _ = strings.Cut(host, ":") // drop port

		return host, strings.Trim(path, "/")
	}

	// scp-like git@host:path
	if at := strings.Index(s, "@"); at != -1 {
		rest := s[at+1:]

		host, path, _ := strings.Cut(rest, ":")

		return host, strings.Trim(path, "/")
	}

	// bare host[/path] - require a dot in host to avoid matching plain words
	host, path, _ := strings.Cut(s, "/")
	if !strings.Contains(host, ".") {
		return "", ""
	}

	return host, strings.Trim(path, "/")
}
