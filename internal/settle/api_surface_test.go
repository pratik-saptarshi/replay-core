package settle_test

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
// internal/settle (plan Task 9): every exported top-level
// identifier the package may declare — the verb surface and
// its errors — and nothing else. The pinned surface carries
// no exported methods on any exported type (Context is a pure
// record; the verbs are functions), so the method leg of the
// guard admits none. An allowlist change needs its rationale
// recorded in the commit message, never extended silently.
var apiTopLevelAllowlist = map[string]bool{
	"Context":             true,
	"Fork":                true,
	"Select":              true,
	"Apply":               true,
	"Discard":             true,
	"Recover":             true,
	"ErrParentAdvanced":   true,
	"ErrScopeConsumed":    true,
	"ErrNoForkProvenance": true,
	"ErrOverlap":          true,
}

// apiSurfaceViolations audits one parsed source file for
// breaches of the pinned API surface: every exported top-level
// identifier is on the allowlist, and no exported method is
// declared on any exported type (methods on unexported types
// are not reachable API and are out of scope).
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
			// are reachable API — and the pinned settle surface
			// has none.
			recv := receiverTypeName(d)
			if recv == "" || !ast.IsExported(recv) {
				continue
			}
			if ast.IsExported(d.Name.Name) {
				violations = append(violations, "exported method "+recv+"."+d.Name.Name+" is outside the pinned surface (settle pins no exported methods)")
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

// TestAPIGuardSettleSurface is the standing machine check for
// the pinned F6 surface: the settle package grows no exported
// identifier beyond the allowlist, and no exported type grows
// an exported method.
//
// The guard first runs against synthetic control sources that
// MUST be flagged — a walker that cannot flag its own control
// cases fails here, so the guard cannot pass vacuously.
func TestAPIGuardSettleSurface(t *testing.T) {
	// Negative control 1: a benign but unlisted export must be
	// flagged by the allowlist leg.
	unlisted := "package settle\n\nfunc Frobnicate() {}\n"
	if got := auditAPISource(t, "control.go", unlisted); len(got) == 0 {
		t.Fatal("negative control not flagged: walker accepted unlisted export Frobnicate — the allowlist leg has no teeth")
	}

	// Negative control 2: an exported method on Context — the
	// package's one exported type — must be flagged by the
	// method leg; the pinned surface has no methods.
	method := "package settle\n\ntype Context struct{}\n\nfunc (c *Context) Frobnicate() {}\n"
	if got := auditAPISource(t, "control2.go", method); len(got) == 0 {
		t.Fatal("negative control not flagged: walker accepted exported method Context.Frobnicate — the method leg has no teeth")
	}

	// Negative control 3: an unlisted exported var must be
	// flagged by the allowlist leg.
	variable := "package settle\n\nvar ErrFrobnicate = errors.New(\"x\")\n"
	if got := auditAPISource(t, "control3.go", variable); len(got) == 0 {
		t.Fatal("negative control not flagged: walker accepted unlisted var ErrFrobnicate — the allowlist leg has no teeth")
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
