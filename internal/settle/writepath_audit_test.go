package settle_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/carrier"
	"github.com/pratik-saptarshi/replay-core/internal/settle"
)

// The write-path audit (plan Task 9, Step 2; AC-F6-7's audit
// leg — the only path to a real tree). Its claim is scoped to
// the F5/F6 packages, internal/settle and internal/carrier,
// and says so: the snapshot package's writes are F4's surface
// (guarded by its own Task 6 guard) and the trace store's
// writes are F2's; the verbs' ref and fact effects go through
// those stores' APIs and are not file writes at all.
//
// The audit is an AST walk in the Phase 1 Task 12 spirit:
// every call site that can write outside the process — the os
// write/remove/rename family, directory and symlink creation,
// the FICLONE-bearing syscall.Syscall, syscall.Mount/Unmount,
// io.Copy into a file, and file Write/WriteString methods —
// must appear inside an allowlisted function, each entry
// carrying its rationale. A new write site fails this test
// until it is allowlisted with a rationale: the audit cannot
// prove a negative about future code, so it makes the
// negative expensive to break silently, which is what a
// guard is for.

// writeFamilyCalls is the audited selector-call set, keyed by
// imported package base name.
var writeFamilyCalls = map[string]map[string]bool{
	"os": {
		"WriteFile": true, "Create": true, "CreateTemp": true,
		"OpenFile": true, "Remove": true, "RemoveAll": true,
		"Rename": true, "Mkdir": true, "MkdirAll": true,
		"MkdirTemp": true, "Symlink": true, "Link": true,
		"Truncate": true, "Chmod": true, "Chown": true,
		"Lchown": true, "Chtimes": true,
	},
	"io":      {"Copy": true, "CopyN": true},
	"syscall": {"Mount": true, "Unmount": true, "Syscall": true},
}

// writeMethodCalls is the audited method-call set: writes
// through an already-open file handle.
var writeMethodCalls = map[string]bool{"Write": true, "WriteString": true}

// writeAllowlist pins every function in internal/settle and
// internal/carrier that may contain an external-write call
// site, keyed "pkg.Func" (methods: "pkg.Recv.Func"), with the
// rationale for its writes. The audited function set and this
// list must agree exactly: an unlisted write function fails,
// and a stale entry whose function no longer writes fails
// too, so the list cannot quietly outlive the code.
var writeAllowlist = map[string]string{
	// internal/settle — the journal materializer and rollback.
	"settle.materialize":       "the journaled materializer (Task 8): applies a settlement's unit list to the parent directory — the one path through which Select's and Apply's writes land",
	"settle.capturePreimage":   "journal preparation: copies preimages into the journal's staging area under the settle Root before any unit lands",
	"settle.restorePreimage":   "journal rollback: restores one unit's staged preimage into the parent directory",
	"settle.removeOccupant":    "journal rollback helper: removes whatever currently occupies a restore target in the parent directory",
	"settle.writeJournal":      "the journal writer: canonical JSON to a temp file in the journal directory under the settle Root, fsynced and renamed into place",
	"settle.clearJournalState": "journal cleanup after a commit or a completed rollback: removes the journal file and its staging area under the settle Root",
	// internal/settle — the staging builders and file writers
	// the materializer drives.
	"settle.copyFileAtomic":     "the materializer's per-file write into the parent directory at a verb's commit point: temp file in the target directory, then rename over the destination",
	"settle.replaceWithSymlink": "the materializer's per-symlink write into the parent directory: temp symlink, then rename over the destination",
	"settle.clearBlockingDirs":  "the materializer's helper: removes a real directory blocking a file or symlink write during materialization",
	"settle.buildUnionTree":     "Apply's staging builder: assembles the union tree in staging under the settle Root, never in the parent directory",
	"settle.overlayDelta":       "Apply's staging builder: overlays the run delta onto the staged union tree under the settle Root",
	// internal/settle — the journal and sidecar writers' kin:
	// the verbs' own operational state under the settle Root.
	"settle.writeSidecar": "the fork provenance writer: canonical JSON to a temp file in Root/forks, fsynced and renamed over the sidecar path",
	"settle.Fork":         "the fork verb's operational writes under the settle Root: it stages the fork base extraction and renames it into place, removing its own staging on failure; it never writes the parent directory",
	"settle.Discard":      "the discard verb removes the run's fork sidecar — its own operational state under the settle Root — once the scope is archived",
	// internal/carrier — the layer lifecycle.
	"carrier.Materialize":           "the layer lifecycle: builds the staging tree under the layer root it owns, renames the view into place, and wipes stale layers whose identity marker it issued",
	"carrier.buildViewLayer":        "the view-driver layer builder: assembles upper/work/view under the layer root for mount-backed drivers",
	"carrier.Layer.Remove":          "removes the layer root the carrier created",
	"carrier.releaseLayerState":     "releases a superseded layer's state (its identity marker) before the root is rebuilt",
	"carrier.removeStagingRemnants": "removes the carrier's own staging remnants under a layer root after an interrupted build",
	"carrier.writeMarker":           "writes the layer identity marker inside the layer root",
	// internal/carrier — the drivers' materialize internals.
	"carrier.copyTree":                      "the copy driver's materialize internal: recreates the base tree inside the staging view",
	"carrier.copyFile":                      "the copy driver's per-file write inside the staging view",
	"carrier.cloneTree":                     "the reflink driver's materialize internal: recreates the base tree inside the staging view by clone",
	"carrier.realCloneFile":                 "the reflink driver's per-file clone (FICLONE via syscall.Syscall) inside the staging view; a failed clone removes its partial destination",
	"carrier.overlayDriver.materializeView": "the kernel-overlay driver's materialize internal: creates upper/work/view under the layer root and mounts the overlay",
	"carrier.overlayDriver.recoverRemnant":  "crash-remnant recovery: wipes a carrier-owned overlay remnant under the layer root after proving ownership by its layout",
	"carrier.discardDirs":                   "overlay remnant helper: removes the remnant's directories under the layer root",
	"carrier.unmount":                       "the overlay unmount helper (syscall.Unmount, detaching on retry)",
	// internal/carrier — the probes. A probe that cannot
	// write cannot report truth (AC-F5-1, AC-F5-2); each
	// probe's scratch writes are confined to a fresh scratch
	// subtree of the probed root and removed before it
	// returns, and no probe touches a caller's tree.
	"carrier.probeCopy":          "the copy probe: one scratch temp file in the probed root, removed before it returns",
	"carrier.probeKernelOverlay": "the kernel-overlay probe: scratch dirs, one probe file, and the scratch mount, all inside a fresh scratch subtree of the probed root, unmounted and removed before it returns",
	"carrier.probeReflink":       "the reflink probe: scratch files and one FICLONE inside a fresh scratch subtree of the probed root, removed before it returns",
}

// writeSite is one enumerated external-write call site.
type writeSite struct {
	pkg  string // "settle" or "carrier"
	file string // base name of the source file
	fn   string // allowlist key of the enclosing function
	call string // e.g. "os.WriteFile", ".Write()"
	pos  string // file:line, for failure messages
}

// auditWritePaths walks the settle and carrier packages under
// root (the internal/ directory) and returns every audited
// call site plus the violations: sites in functions the
// allowlist does not name. Call sites outside any function
// are attributed to "<file scope>", which is never
// allowlisted, so they always violate.
func auditWritePaths(root string) ([]writeSite, []string, error) {
	var sites []writeSite
	var violations []string
	for _, pkg := range []string{"settle", "carrier"} {
		dir := filepath.Join(root, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return nil, nil, fmt.Errorf("parse %s: %w", path, err)
			}
			fileSites, fileViolations := auditWriteFile(fset, f, pkg, name)
			sites = append(sites, fileSites...)
			violations = append(violations, fileViolations...)
		}
	}
	return sites, violations, nil
}

// auditWriteFile audits one parsed source file.
func auditWriteFile(fset *token.FileSet, f *ast.File, pkg, file string) ([]writeSite, []string) {
	// Import local name -> package base name. A dot import of
	// an audited package would hide selector calls from this
	// walk, so it is itself a violation.
	imports := map[string]string{}
	var violations []string
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, "\"")
		base := filepath.Base(p)
		local := base
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == "." {
			if _, audited := writeFamilyCalls[base]; audited {
				violations = append(violations, fmt.Sprintf("%s: dot import of %s defeats the write-path audit", file, base))
			}
			continue
		}
		imports[local] = base
	}

	// Enclosing-function ranges, for attributing each call
	// site to its innermost enclosing FuncDecl.
	type fnRange struct {
		key        string
		start, end token.Pos
	}
	var ranges []fnRange
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		key := pkg + "." + fd.Name.Name
		if recv := receiverTypeName(fd); recv != "" {
			key = pkg + "." + recv + "." + fd.Name.Name
		}
		ranges = append(ranges, fnRange{key, fd.Pos(), fd.End()})
	}
	enclosing := func(pos token.Pos) string {
		best := ""
		var bestStart token.Pos
		for _, r := range ranges {
			if pos >= r.start && pos <= r.end && r.start >= bestStart {
				best, bestStart = r.key, r.start
			}
		}
		if best == "" {
			return pkg + ".<file scope>"
		}
		return best
	}

	var sites []writeSite
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		var callName string
		if id, isIdent := sel.X.(*ast.Ident); isIdent {
			if base, isImport := imports[id.Name]; isImport {
				if writeFamilyCalls[base][sel.Sel.Name] {
					callName = base + "." + sel.Sel.Name
				}
			}
		}
		if callName == "" && writeMethodCalls[sel.Sel.Name] {
			// A method call: audited unless its receiver is an
			// imported package name (a package-level func the
			// family leg already judged).
			if id, isIdent := sel.X.(*ast.Ident); isIdent {
				if _, isImport := imports[id.Name]; isImport {
					return true
				}
			}
			callName = "." + sel.Sel.Name + "()"
		}
		if callName == "" {
			return true
		}
		fn := enclosing(call.Pos())
		pos := fmt.Sprintf("%s:%d", file, fset.Position(call.Pos()).Line)
		sites = append(sites, writeSite{pkg: pkg, file: file, fn: fn, call: callName, pos: pos})
		if _, ok := writeAllowlist[fn]; !ok {
			violations = append(violations, fmt.Sprintf("%s: %s calls %s, which is not on the write-path allowlist", pos, fn, callName))
		}
		return true
	})
	return sites, violations
}

// writeControlTree builds a synthetic two-package tree for the
// audit's negative controls.
func writeControlTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, pkg := range []string{"settle", "carrier"} {
		if err := os.MkdirAll(filepath.Join(root, pkg), 0o755); err != nil {
			t.Fatalf("control tree: %v", err)
		}
	}
	rogue := "package settle\n\nimport \"os\"\n\nfunc rogueWrite() error { return os.WriteFile(\"x\", nil, 0o644) }\n"
	if err := os.WriteFile(filepath.Join(root, "settle", "rogue.go"), []byte(rogue), 0o644); err != nil {
		t.Fatalf("control tree: %v", err)
	}
	listed := "package carrier\n\nimport \"os\"\n\nfunc writeMarker() error { return os.WriteFile(\"x\", nil, 0o644) }\n"
	if err := os.WriteFile(filepath.Join(root, "carrier", "listed.go"), []byte(listed), 0o644); err != nil {
		t.Fatalf("control tree: %v", err)
	}
	return root
}

// TestWritePathAudit is the standing machine check that the
// F5/F6 packages write outside the process only from
// allowlisted functions, plus the behavioral leg of AC-F6-7's
// audit: a fixture parent directory's hash changes ONLY
// across a successful Select or Apply — Fork, carrier
// Materialize/Remove, a refused verb's probe snapshots,
// Discard, and Recover with no journal all leave it untouched.
func TestWritePathAudit(t *testing.T) {
	// Negative controls first: the walker must flag a write in
	// an unlisted function and accept the identical call in an
	// allowlisted one — a walker that cannot tell them apart
	// has no teeth, whatever it says about the real tree.
	controlRoot := writeControlTree(t)
	_, controlViolations, err := auditWritePaths(controlRoot)
	if err != nil {
		t.Fatalf("audit control tree: %v", err)
	}
	if len(controlViolations) != 1 || !strings.Contains(controlViolations[0], "rogueWrite") {
		t.Fatalf("control violations = %v, want exactly the rogueWrite site flagged", controlViolations)
	}

	// The real tree.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate package sources")
	}
	internalDir := filepath.Dir(filepath.Dir(thisFile))
	sites, violations, err := auditWritePaths(internalDir)
	if err != nil {
		t.Fatalf("audit write paths: %v", err)
	}
	for _, v := range violations {
		t.Error(v)
	}
	if len(sites) < 40 {
		t.Fatalf("audit enumerated only %d write sites — the walk is not seeing the tree", len(sites))
	}
	observed := map[string]bool{}
	for _, s := range sites {
		observed[s.fn] = true
	}
	var stale []string
	for fn := range writeAllowlist {
		if !observed[fn] {
			stale = append(stale, fn)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("write-path allowlist entries with no remaining write site (stale): %v", stale)
	}

	// Behavioral leg: hash-watch the fixture parent directory.
	f := newFixture(t)
	hash := func() string { return walkHash(t, f.parentDir) }
	parentHead := func() string {
		t.Helper()
		head, err := f.snaps.ScopeHead("parent")
		if err != nil {
			t.Fatalf("ScopeHead(parent): %v", err)
		}
		return head
	}
	h0 := hash()

	// Fork writes only under the settle Root.
	layerA, err := settle.Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork run-a: %v", err)
	}
	if got := hash(); got != h0 {
		t.Fatal("Fork changed the parent directory's hash — Fork may only read it")
	}

	// Carrier Materialize/Remove on an unrelated base tree
	// under the scratch dir: writes confined to the layer root.
	scratch := filepath.Dir(f.root)
	baseDir := filepath.Join(scratch, "probe-base")
	writeTestFile(t, filepath.Join(baseDir, "base.txt"), "base\n", 0o644)
	probeLayer, err := carrier.Materialize(carrier.Selection{Driver: carrier.DriverCopy},
		baseDir, "probe-base-1", filepath.Join(scratch, "probe-layers", "probe-1"))
	if err != nil {
		t.Fatalf("carrier Materialize: %v", err)
	}
	if err := probeLayer.Remove(); err != nil {
		t.Fatalf("carrier Remove: %v", err)
	}
	if got := hash(); got != h0 {
		t.Fatal("carrier Materialize/Remove changed the parent directory's hash")
	}

	// A refused Select probe-snapshots the parent directory
	// and runs Recover at entry: still no writes to it.
	writeTestFile(t, filepath.Join(layerA.Dir, "run-a.txt"), "run a\n", 0o644)
	f.advanceRun(t, layerA, "run-a", f.parent0)
	writeTestFile(t, filepath.Join(f.parentDir, "sneak.txt"), "sneak\n", 0o644)
	advanceParent(t, f)
	h1 := hash()
	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "run-a", "op-refused"); !errors.Is(err, settle.ErrParentAdvanced) {
		t.Fatalf("Select onto advanced parent: err = %v, want ErrParentAdvanced", err)
	}
	if got := hash(); got != h1 {
		t.Fatal("a refused Select changed the parent directory's hash")
	}

	// Recover with no journal is a no-op over the parent tree.
	if err := settle.Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover with no journal: %v", err)
	}
	if got := hash(); got != h1 {
		t.Fatal("Recover with no journal changed the parent directory's hash")
	}

	// A successful Select is the first operation allowed to
	// change the hash.
	layerB, err := settle.Fork(f.ctx, "parent", "run-b")
	if err != nil {
		t.Fatalf("Fork run-b: %v", err)
	}
	writeTestFile(t, filepath.Join(layerB.Dir, "run-b.txt"), "run b\n", 0o644)
	f.advanceRun(t, layerB, "run-b", parentHead())
	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "run-b", "op-select-b"); err != nil {
		t.Fatalf("Select run-b: %v", err)
	}
	h2 := hash()
	if h2 == h1 {
		t.Fatal("successful Select did not change the parent directory's hash")
	}

	// Discard writes nothing in the parent tree.
	if _, err := settle.Fork(f.ctx, "parent", "run-c"); err != nil {
		t.Fatalf("Fork run-c: %v", err)
	}
	if _, err := settle.Discard(f.ctx, "run-c", "op-discard-c"); err != nil {
		t.Fatalf("Discard run-c: %v", err)
	}
	if got := hash(); got != h2 {
		t.Fatal("Discard changed the parent directory's hash")
	}

	// A successful Apply changes it again, landing the union.
	layerD, err := settle.Fork(f.ctx, "parent", "run-d")
	if err != nil {
		t.Fatalf("Fork run-d: %v", err)
	}
	writeTestFile(t, filepath.Join(layerD.Dir, "run-d.txt"), "run d\n", 0o644)
	f.advanceRun(t, layerD, "run-d", parentHead())
	writeTestFile(t, filepath.Join(f.parentDir, "parent-d.txt"), "parent d\n", 0o644)
	advanceParent(t, f)
	h3 := hash()
	if _, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-d", "op-apply-d"); err != nil {
		t.Fatalf("Apply run-d: %v", err)
	}
	if got := hash(); got == h3 {
		t.Fatal("successful Apply did not change the parent directory's hash")
	}
}
