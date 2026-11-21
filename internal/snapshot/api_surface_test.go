package snapshot_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// apiTopLevelAllowlist is the pinned exported surface of
// internal/snapshot (plan Task 6): every exported top-level
// identifier the package may declare — Phase 1's surface plus
// this task's scope/tree additions — and nothing else. An
// allowlist change needs its rationale recorded in the commit
// message, never extended silently.
var apiTopLevelAllowlist = map[string]bool{
	"Store":             true,
	"Open":              true,
	"SnapshotAt":        true,
	"RefKind":           true,
	"RefScope":          true,
	"RefOps":            true,
	"RefArchive":        true,
	"ScopeRef":          true,
	"WorkspaceEntry":    true,
	"ErrGitUnavailable": true,
	"ErrScopeExists":    true,
	"ErrScopeNotFound":  true,
	"ErrScopeHeadMoved": true,
}

// apiStoreMethodAllowlist pins Store's exported methods.
var apiStoreMethodAllowlist = map[string]bool{
	"Snapshot":         true,
	"Fork":             true,
	"ListScopes":       true,
	"ScopeHead":        true,
	"AdvanceScope":     true,
	"ArchiveScope":     true,
	"WorkspaceTreeID":  true,
	"WorkspaceFiles":   true,
	"ExtractWorkspace": true,
}

// apiSurfaceViolations audits one parsed source file for
// breaches of the pinned API surface: every exported top-level
// identifier is on the allowlist, and the only exported methods
// on an exported type are Store's pinned methods (methods on
// unexported types are not reachable API and are out of scope).
func apiSurfaceViolations(f *ast.File) []string {
	var violations []string

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				// Top-level function.
				if ast.IsExported(d.Name.Name) && !apiTopLevelAllowlist[d.Name.Name] {
					violations = append(violations, "exported func "+d.Name.Name+" is not on the API allowlist")
				}
				continue
			}
			// Method: only methods on exported receiver types
			// are reachable API.
			recv := receiverTypeName(d)
			if recv == "" || !ast.IsExported(recv) {
				continue
			}
			if !ast.IsExported(d.Name.Name) {
				continue
			}
			if recv != "Store" {
				violations = append(violations, "exported method "+recv+"."+d.Name.Name+" is outside the pinned surface (only Store has exported methods)")
			} else if !apiStoreMethodAllowlist[d.Name.Name] {
				violations = append(violations, "exported method Store."+d.Name.Name+" is not on the API allowlist")
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if ast.IsExported(sp.Name.Name) && !apiTopLevelAllowlist[sp.Name.Name] {
						violations = append(violations, "exported type "+sp.Name.Name+" is not on the API allowlist")
					}
				case *ast.ValueSpec:
					for _, name := range sp.Names {
						if ast.IsExported(name.Name) && !apiTopLevelAllowlist[name.Name] {
							violations = append(violations, "exported var/const "+name.Name+" is not on the API allowlist")
						}
					}
				}
			}
		}
	}
	return violations
}

// receiverTypeName returns the base type name of a method's
// receiver, unwrapping a pointer; "" if it cannot be determined.
func receiverTypeName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	expr := d.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// auditAPISource parses one source string and returns its
// surface violations.
func auditAPISource(t *testing.T, name, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return apiSurfaceViolations(f)
}

// TestAPIGuardSnapshotSurface is the standing machine check for
// the pinned F4 surface: the snapshot package grows no exported
// identifier beyond the allowlist, and Store grows no exported
// method beyond the pinned set.
//
// The guard first runs against synthetic control sources that
// MUST be flagged — a walker that cannot flag its own control
// cases fails here, so the guard cannot pass vacuously.
func TestAPIGuardSnapshotSurface(t *testing.T) {
	// Negative control 1: a benign but unlisted export must be
	// flagged by the allowlist leg.
	unlisted := "package snapshot\n\nfunc Frobnicate() {}\n"
	if got := auditAPISource(t, "control.go", unlisted); len(got) == 0 {
		t.Fatal("negative control not flagged: walker accepted unlisted export Frobnicate — the allowlist leg has no teeth")
	}

	// Negative control 2: an unlisted exported method on Store
	// must be flagged by the method leg.
	method := "package snapshot\n\ntype Store struct{}\n\nfunc (s *Store) Frobnicate() {}\n"
	if got := auditAPISource(t, "control2.go", method); len(got) == 0 {
		t.Fatal("negative control not flagged: walker accepted unlisted method Store.Frobnicate — the method leg has no teeth")
	}

	// Negative control 3: an exported method on any other
	// exported type is outside the pinned surface.
	other := "package snapshot\n\ntype ScopeRef struct{}\n\nfunc (r ScopeRef) Frobnicate() {}\n"
	if got := auditAPISource(t, "control3.go", other); len(got) == 0 {
		t.Fatal("negative control not flagged: walker accepted exported method ScopeRef.Frobnicate — the receiver leg has no teeth")
	}

	// The real package: every non-test source in this directory.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate package sources")
	}
	dir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		checked++
		for _, v := range auditAPISource(t, name, string(src)) {
			t.Errorf("%s: %s", name, v)
		}
	}
	if checked == 0 {
		t.Fatal("no package sources audited — the guard checked nothing")
	}
}
