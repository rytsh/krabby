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

// allowedLockForUses names the non-test functions permitted to reach for a raw,
// unlocked mutex. Everything else must go through lockKey.
//
//   - lockKey itself is the sanctioned wrapper.
//   - collectionLock only forwards the mutex under a suffixed key; its callers
//     go through mutateCollection, which locks it properly.
//   - serviceLock is collectionLock's API-catalog counterpart, with the same
//     shape: it forwards a mutex under a suffixed key, and its callers go
//     through mutateService.
//   - ensureDocsTextKey needs TryLock so a concurrent backfill of the same key
//     can be skipped instead of waited on.
var allowedLockForUses = map[string]bool{
	"lockKey":           true,
	"collectionLock":    true,
	"serviceLock":       true,
	"ensureDocsTextKey": true,
}

// The per-key mutexes are held across whole pipeline stages, so obtaining one
// and unlocking it without having locked it is easy to write and catastrophic
// to run: Go answers an unlock of an unlocked mutex with a fatal error, which
// no recover can contain, so the process dies and every in-flight request with
// it. That happened once, in SetRepoOverrides.
//
// lockKey exists so the unlock cannot be obtained without having taken the
// lock. This test keeps the escape hatch (lockFor) from quietly spreading back
// through the package.
func TestLockForIsNotUsedOutsideItsSanctionedCallers(t *testing.T) {
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

		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}

				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "lockFor" {
					return true
				}

				if allowedLockForUses[fn.Name.Name] {
					return true
				}

				t.Errorf(
					"%s: %s calls lockFor, which returns an UNLOCKED mutex; use `defer m.lockKey(key)()` instead, "+
						"or add the function to allowedLockForUses with a reason if it genuinely needs TryLock",
					fset.Position(call.Pos()), fn.Name.Name,
				)

				return true
			})
		}
	}
}

// lockKey must hand back the mutex's own Unlock and nothing else; a wrapper
// that could be invoked twice would reintroduce the same fatal.
func TestLockKeyUnlockIsNotReentrant(t *testing.T) {
	m, _ := newLockTestManager(t)

	unlock := m.lockKey("owner/repo")
	unlock()

	// Re-locking must work, proving the first unlock released exactly once.
	unlock2 := m.lockKey("owner/repo")
	unlock2()

	if !waitLockFree(t, m, "owner/repo") {
		t.Fatal("lock not released")
	}
}
