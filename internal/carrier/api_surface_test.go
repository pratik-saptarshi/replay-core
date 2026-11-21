package carrier_test

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
// internal/carrier (plan Task 2): every exported top-level
// identifier the package may declare, and nothing else. The
// allowlist is final for F5 — Tasks 3–5 add no exported names
// (drivers are unexported) — so an allowlist change needs its
// rationale recorded in the commit message, never extended
// silently.
var apiTopLevelAllowlist = map[string]bool{
	"DriverID":            true,
	"DriverKernelOverlay": true,
	"DriverFuseOverlay":   true,
	"DriverReflink":       true,
	"DriverCopy":          true,
	"ProbeResult":         true,
	"Selection":           true,
	"Select":              true,
	"Layer":               true,
	"Materialize":         true,
	"ErrForeignLayerDir":  true,
}

// apiLayerMethodAllowlist pins Layer's exported methods.
var apiLayerMethodAllowlist = map[string]bool{
	"Remove": true,
}

// apiSurfaceViolations audits one parsed source file for
// breaches of the pinned API surface: every exported top-level
// identifier is on the allowlist, and the only exported methods
// on an exported type are Layer's pinned methods (methods on
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
			if recv != "Layer" {
				violations = append(violations, "exported method "+recv+"."+d.Name.Name+" is outside the pinned surface (only Layer has exported methods)")
			} else if !apiLayerMethodAllowlist[d.Name.Name] {
				violations = append(violations, "exported method Layer."+d.Name.Name+" is not on the API allowlist")
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

// TestAPIGuardCarrierSurface is the standing machine check for
// the pinned F5 surface: the carrier package grows no exported
// identifier beyond the allowlist, and Layer grows no exported
// method beyond Remove.
//
// The guard first runs against synthetic control sources that
// MUST be flagged — a walker that cannot flag its own control
// cases fails here, so the guard cannot pass vacuously.
func TestAPIGuardCarrierSurface(t *testing.T) {
	// Negative control 1: a benign but unlisted export must be
	// flagged by the allowlist leg.
	unlisted := "package carrier\n\nfunc Frobnicate() {}\n"
	if got := auditAPISource(t, "control.go", unlisted); len(got) == 0 {
		t.Fatal("negative control not flagged: walker accepted unlisted export Frobnicate — the allowlist leg has no teeth")
	}

	// Negative control 2: an unlisted exported method on Layer
	// must be flagged by the method leg.
	method := "package carrier\n\ntype Layer struct{}\n\nfunc (l *Layer) Frobnicate() {}\n"
	if got := auditAPISource(t, "control2.go", method); len(got) == 0 {
		t.Fatal("negative control not flagged: walker accepted unlisted method Layer.Frobnicate — the method leg has no teeth")
	}

	// Negative control 3: an exported method on any other
	// exported type is outside the pinned surface.
	other := "package carrier\n\ntype Selection struct{}\n\nfunc (s Selection) Frobnicate() {}\n"
	if got := auditAPISource(t, "control3.go", other); len(got) == 0 {
		t.Fatal("negative control not flagged: walker accepted exported method Selection.Frobnicate — the receiver leg has no teeth")
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
