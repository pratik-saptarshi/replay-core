package slice_test

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
// internal/slice (plan Task 12): every exported top-level identifier
// the package may declare, and nothing else. An allowlist change
// needs its rationale recorded in the commit message — never extend
// it silently.
var apiTopLevelAllowlist = map[string]bool{
	"Slice":                   true,
	"Cut":                     true,
	"PublishCut":              true,
	"ReadCut":                 true,
	"ResolveCut":              true,
	"Abandon":                 true,
	"ProjectEffectiveHistory": true,
	"EpochSlice":              true,
	"ErrUnknownMode":          true,
}

// apiSliceMethodAllowlist pins Slice's exported methods.
var apiSliceMethodAllowlist = map[string]bool{
	"Digest":   true,
	"Order":    true,
	"EffectAt": true,
}

// apiSurfaceViolations audits one parsed source file for breaches of
// the pinned API surface:
//
//  1. every exported top-level identifier is on the allowlist, and
//     the only exported methods on an exported type are Slice's
//     pinned methods (methods on unexported types are not reachable
//     API and are out of scope);
//  2. no exported identifier's name contains "effect"
//     (case-insensitive) except the two named exemptions below
//     (EffectAt and ProjectEffectiveHistory), and none contains
//     "cache" or "replay" — the E3-A ruling's negative claim
//     (AC-F3-7): positional access only, no cache store, no replay
//     entry point.
func apiSurfaceViolations(f *ast.File) []string {
	var violations []string
	var exported []string // every exported name seen, for rule 2

	note := func(name string) { exported = append(exported, name) }

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				// Top-level function.
				if ast.IsExported(d.Name.Name) {
					note(d.Name.Name)
					if !apiTopLevelAllowlist[d.Name.Name] {
						violations = append(violations, "exported func "+d.Name.Name+" is not on the API allowlist")
					}
				}
				continue
			}
			// Method: only methods on exported receiver types are
			// reachable API.
			recv := receiverTypeName(d)
			if recv == "" || !ast.IsExported(recv) {
				continue
			}
			if !ast.IsExported(d.Name.Name) {
				continue
			}
			note(d.Name.Name)
			if recv != "Slice" {
				violations = append(violations, "exported method "+recv+"."+d.Name.Name+" is outside the pinned surface (only Slice has exported methods)")
			} else if !apiSliceMethodAllowlist[d.Name.Name] {
				violations = append(violations, "exported method Slice."+d.Name.Name+" is not on the API allowlist")
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if ast.IsExported(sp.Name.Name) {
						note(sp.Name.Name)
						if !apiTopLevelAllowlist[sp.Name.Name] {
							violations = append(violations, "exported type "+sp.Name.Name+" is not on the API allowlist")
						}
					}
				case *ast.ValueSpec:
					for _, name := range sp.Names {
						if ast.IsExported(name.Name) {
							note(name.Name)
							if !apiTopLevelAllowlist[name.Name] {
								violations = append(violations, "exported var/const "+name.Name+" is not on the API allowlist")
							}
						}
					}
				}
			}
		}
	}

	for _, name := range exported {
		lower := strings.ToLower(name)
		// Rule 2's named exemptions: EffectAt (the pinned positional
		// accessor) and ProjectEffectiveHistory. "Effective" in the
		// latter is the spec §5 domain term for the effective-history
		// projection, not effect machinery, and the allowlist pins
		// the name deliberately — exempting it beside EffectAt is a
		// recorded controller ruling (Task 12), not a silent
		// loosening: every other "effect"-bearing name still trips.
		if strings.Contains(lower, "effect") && name != "EffectAt" && name != "ProjectEffectiveHistory" {
			violations = append(violations, "exported identifier "+name+" contains \"effect\" (only EffectAt and ProjectEffectiveHistory may)")
		}
		if strings.Contains(lower, "cache") {
			violations = append(violations, "exported identifier "+name+" contains \"cache\"")
		}
		if strings.Contains(lower, "replay") {
			violations = append(violations, "exported identifier "+name+" contains \"replay\"")
		}
	}
	return violations
}

// receiverTypeName returns the base type name of a method's receiver,
// unwrapping a pointer; "" if it cannot be determined.
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

// auditAPISource parses one source string and returns its surface
// violations.
func auditAPISource(t *testing.T, name, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return apiSurfaceViolations(f)
}

// TestAPIGuardEpochScope is the standing machine check for AC-F3-7's
// negative claim: the slice package grows no effect/cache/replay
// surface and no exported identifier beyond the pinned allowlist.
//
// The guard first runs against synthetic control sources that MUST
// be flagged — a walker that cannot flag its own control cases fails
// here, so the guard cannot pass vacuously.
func TestAPIGuardEpochScope(t *testing.T) {
	// Negative control 1 (plan-pinned): a banned-name export must be
	// flagged (it breaches both the allowlist and the name rules).
	control := "package slice\n\nfunc EffectCache() {}\n"
	if got := auditAPISource(t, "control.go", control); len(got) == 0 {
		t.Fatal("negative control not flagged: walker accepted func EffectCache() {} — the guard has no teeth")
	}

	// Negative control 2: a benign but unlisted export must be
	// flagged by the allowlist leg alone.
	unlisted := "package slice\n\nfunc Frobnicate() {}\n"
	if got := auditAPISource(t, "control2.go", unlisted); len(got) == 0 {
		t.Fatal("negative control not flagged: walker accepted unlisted export Frobnicate — the allowlist leg has no teeth")
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
