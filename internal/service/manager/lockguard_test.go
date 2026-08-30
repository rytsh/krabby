package manager

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowedRawLockUses names the non-test functions permitted to touch the keyed
// lock table directly instead of going through lockKey/tryLockKey.
//
// Both wrappers exist so a caller cannot obtain a release without having
// completed an acquisition. Reaching into m.locks re-opens that door.
var allowedRawLockUses = map[string]bool{
	"lockKey":    true,
	"tryLockKey": true,
}

// The per-key mutexes are held across whole pipeline stages, so obtaining one
// and unlocking it without having locked it is easy to write and catastrophic
// to run: Go answers an unlock of an unlocked mutex with a fatal error, which
// no recover can contain, so the process dies and every in-flight request with
// it. That happened once, in SetRepoOverrides.
//
// lockKey and tryLockKey are the only ways to get a release, and each one is
// bound to an acquisition that already succeeded. This test keeps direct access
// to the lock table from quietly spreading back through the package.
func TestKeyedLockTableIsNotAccessedDirectly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// The lock table's own implementation obviously uses it.
		if name == "keyedlocks.go" {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || allowedRawLockUses[fn.Name.Name] {
				continue
			}

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "locks" {
					return true
				}

				if ident, ok := sel.X.(*ast.Ident); !ok || ident.Name != "m" {
					return true
				}

				t.Errorf(
					"%s: %s touches m.locks directly; use `defer m.lockKey(key)()` or m.tryLockKey(key) "+
						"so the release cannot be obtained without the acquisition",
					fset.Position(sel.Pos()), fn.Name.Name,
				)

				return true
			})
		}
	}
}

// The release handed back by lockKey is idempotent: calling it twice is a no-op
// rather than the fatal that unlocking an unlocked mutex would produce. That
// matters because releases are passed around as values and stored in defers.
func TestLockKeyReleaseIsIdempotent(t *testing.T) {
	m, _ := newLockTestManager(t)

	unlock := m.lockKey("owner/repo")
	unlock()
	unlock() // must not fatal, and must not release a later acquisition

	unlock2 := m.lockKey("owner/repo")

	// The stale release must not have unlocked the new acquisition.
	if _, ok := m.tryLockKey("owner/repo"); ok {
		t.Fatal("a repeated release freed a later acquisition")
	}

	unlock2()

	if !waitLockFree(t, m, "owner/repo") {
		t.Fatal("lock not released")
	}
}

// The lock table used to grow by one entry per key ever seen and never shrink,
// so every repository, web source and API service — including deleted ones —
// left a mutex behind for the lifetime of the process.
func TestKeyedLocksReclaimsUnusedKeys(t *testing.T) {
	var k keyedLocks

	for i := range 100 {
		key := "repo/" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		k.acquire(key)()
	}

	if got := k.size(); got != 0 {
		t.Errorf("lock table retained %d entries after every lock was released, want 0", got)
	}
}

// A key held by one caller must survive another caller's release of it, or two
// goroutines would end up on different mutexes for the same key and the lock
// would stop excluding anything.
func TestKeyedLocksKeepsKeyWhileContended(t *testing.T) {
	var k keyedLocks

	release := k.acquire("shared")

	if _, ok := k.tryAcquire("shared"); ok {
		t.Fatal("tryAcquire succeeded on a held key")
	}
	if got := k.size(); got != 1 {
		t.Fatalf("size = %d while the key is held, want 1", got)
	}

	release()

	if got := k.size(); got != 0 {
		t.Errorf("size = %d after release, want 0", got)
	}
}
