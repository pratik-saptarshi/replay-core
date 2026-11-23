package settle

// The rollback deletion audit (post-merge fix wave, panel
// R1 + issue #9 process demand): the class that produced
// both defects — a rollback-path deletion that resolves a
// live-tree path without the lexical ancestor discipline,
// or deletes by derived name without journaled ownership —
// becomes a standing machine check, not another hand
// census. The audit enumerates every deletion site in
// journal.go (the os.Remove family and the package's own
// deletion helpers) and requires each enclosing function to
// appear in the classification table below with its
// discipline; the disciplines that are mechanically
// checkable (an ancestor-guard call in the body, the
// ownership record's presence) are checked against the AST.
// A new deletion site in an unclassified function fails
// this test, as does a classified function whose guard or
// gate disappears — the census is drawn by the invariant,
// and the machine, not a summary, keeps the count.

import (
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
)

// deletionClass is one classified function's discipline.
// guard names the ancestor-guard callee the function body
// must call before it deletes through a live-tree path (""
// where the function's deletions resolve no live-tree
// path). recordFields names the journaled per-kind
// temp-created ownership record fields the body must
// reference — the gate is checked per field, because the
// record itself is per kind: a cleanup that consults only
// one kind's record while deleting both kinds' names is
// the divergence the per-kind record exists to close.
// why states the discipline in words — the rationale a
// future editor must preserve or consciously amend here.
type deletionClass struct {
	guard        string
	recordFields []string
	why          string
}

// deletionClasses classifies every function in journal.go
// that contains a deletion site. The table and the code
// must agree exactly: an unlisted deletion function fails,
// and a stale entry whose function no longer deletes fails
// too, so the table cannot quietly outlive the code.
var deletionClasses = map[string]deletionClass{
	// The rollback path proper.
	"removeUnitTemps": {
		guard:        "blockedByAncestor",
		recordFields: []string{"TempCreatedFile", "TempCreatedLink"},
		why:          "derived-name temp cleanup: removes each kind's temp only under that kind's journaled temp-created record (a write unit creates one kind; the other kind's name is not its record's to vouch for), and only through ancestors that stand as real directories of this tree",
	},
	"removeCreatedOccupant": {
		guard: "blockedByAncestor",
		why:   "removes only the occupant a write unit created at its recorded path, and only when no ancestor stands replaced in the live tree",
	},
	"restorePreimage": {
		guard:        "symlinkAncestor",
		recordFields: []string{"TempCreatedFile"},
		why:          "restores a unit's preimage at its recorded path: refuses outright rather than remove or write through a symlink ancestor; its one derived-name removal — the restore temp a killed earlier Recover left — is ownership-gated on the unit's journaled file-kind temp-created record (a restore creates only file-kind temps, so the file kind is the only record that can vouch for it), and a collision without the record stays a refusal",
	},
	"removeOccupant": {
		why: "the deletion primitive itself: removes the single occupant at an exact path (a tree, for a directory); absence in targetGone's sense is success. It carries no discipline of its own — every caller must be classified above",
	},
	"rollbackJournal": {
		guard: "parentRootReplaced",
		why:   "rollback's conductor: before any undo it refuses a journal whose parent root's final component stands as a symlink (parentRootReplaced) — the journaled path was resolved when written, so a link there now means the root was replaced after the crash and every undo path would resolve into the tree the link names; its only deletion call is removeUnitTemps, whose guard and gate travel with it; the undo helpers it drives are classified in their own entries",
	},
	// The materializer's deletions: the verb's own journal
	// units and its own staging, never unrecorded content.
	"materialize": {
		why: "journal-unit deletions: removes blocking ancestors and extras that are themselves recorded units under captured preimages and started intent, prunes only Lstat-verified empty directories the result tree does not imply (lexically excluding replaced ancestors), and wipes its own staging remnant under the settle Root",
	},
	// Operational-state cleanup under the settle Root: the
	// settlement's own records and temp files, never
	// parent-tree content.
	"clearJournalState": {
		why: "removes the settlement's own journal, its parent anchor, and the op's staging directory, all under the settle Root, once the settlement is complete or undone",
	},
	"writeJournal": {
		why: "removes only its own journal temp file under the settle Root, on the failure paths of the write it was staging",
	},
	"writeParentAnchor": {
		why: "removes only its own anchor temp file under the settle Root, on the failure paths of the write it was staging",
	},
}

// deletionSite is one enumerated deletion call site in
// journal.go.
type deletionSite struct {
	fn   string // enclosing function name
	call string // e.g. "os.Remove", "removeOccupant"
	pos  string // line, for failure messages
}

// deletionCallees is the audited call set: the os removal
// family, plus the package's deletion helpers — a call to
// one of these IS a deletion, whoever makes it.
func isDeletionCall(call *ast.CallExpr, imports map[string]string) (string, bool) {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if id, ok := fun.X.(*ast.Ident); ok {
			if imports[id.Name] == "os" && (fun.Sel.Name == "Remove" || fun.Sel.Name == "RemoveAll") {
				return "os." + fun.Sel.Name, true
			}
		}
	case *ast.Ident:
		if fun.Name == "removeOccupant" || fun.Name == "removeUnitTemps" {
			return fun.Name, true
		}
	}
	return "", false
}

// auditJournalDeletions parses one journal.go-shaped source
// and returns its deletion sites plus the classification
// violations: sites in unclassified functions, and
// classified functions whose mechanically checkable
// discipline (guard call, ownership gate) is absent from
// the body.
func auditJournalDeletions(src []byte) ([]deletionSite, []string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "journal.go", src, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	imports := map[string]string{}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, "\"")
		base := filepath.Base(p)
		local := base
		if imp.Name != nil {
			local = imp.Name.Name
		}
		imports[local] = base
	}

	var sites []deletionSite
	var violations []string
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		name := fd.Name.Name
		class, classified := deletionClasses[name]
		callsGuard := map[string]bool{}
		referencedRecords := map[string]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if id, ok := node.Fun.(*ast.Ident); ok {
					callsGuard[id.Name] = true
				}
				if call, isDel := isDeletionCall(node, imports); isDel {
					sites = append(sites, deletionSite{
						fn:   name,
						call: call,
						pos:  fmt.Sprintf("journal.go:%d", fset.Position(node.Pos()).Line),
					})
					if !classified {
						violations = append(violations, fmt.Sprintf("journal.go:%d: %s calls %s, which is not in the rollback deletion classification", fset.Position(node.Pos()).Line, name, call))
					}
				}
			case *ast.SelectorExpr:
				if node.Sel.Name == "TempCreatedFile" || node.Sel.Name == "TempCreatedLink" {
					referencedRecords[node.Sel.Name] = true
				}
			}
			return true
		})
		if !classified {
			continue
		}
		if class.guard != "" && !callsGuard[class.guard] {
			violations = append(violations, fmt.Sprintf("%s is classified ancestor-guarded but never calls %s", name, class.guard))
		}
		for _, field := range class.recordFields {
			if !referencedRecords[field] {
				violations = append(violations, fmt.Sprintf("%s is classified ownership-gated but never references the %s record", name, field))
			}
		}
	}
	return sites, violations, nil
}

// TestRollbackDeletionAudit is the standing check: the real
// journal.go must classify clean, and the classifier must
// have teeth — the negative controls below feed it a rogue
// deletion and a guard-less, gate-less removeUnitTemps, and
// both must be flagged.
func TestRollbackDeletionAudit(t *testing.T) {
	// Negative control 1: an unclassified deletion site.
	rogue := `package settle

import "os"

func rogueDelete() error { return os.Remove("x") }
`
	_, rogueViolations, err := auditJournalDeletions([]byte(rogue))
	if err != nil {
		t.Fatalf("audit rogue control: %v", err)
	}
	if len(rogueViolations) != 1 || !strings.Contains(rogueViolations[0], "rogueDelete") {
		t.Fatalf("rogue control violations = %v, want exactly the rogueDelete site flagged", rogueViolations)
	}

	// Negative control 2: removeUnitTemps present and
	// deleting, but with neither its ancestor guard nor its
	// ownership gate — the guard and BOTH per-kind record
	// references must be flagged.
	unguarded := `package settle

func removeUnitTemps(parentDir, op string, u journalUnit) error {
	return removeOccupant(parentDir)
}

func removeOccupant(dst string) error { return nil }
`
	_, unguardedViolations, err := auditJournalDeletions([]byte(unguarded))
	if err != nil {
		t.Fatalf("audit unguarded control: %v", err)
	}
	joined := strings.Join(unguardedViolations, "\n")
	if !strings.Contains(joined, "blockedByAncestor") || !strings.Contains(joined, "TempCreatedFile") || !strings.Contains(joined, "TempCreatedLink") {
		t.Fatalf("unguarded control violations = %v, want the missing guard and both missing per-kind ownership gates flagged", unguardedViolations)
	}

	// Negative control 3: removeUnitTemps guarded and gated
	// on ONE kind's record only — the per-kind gate must
	// flag the kind it never consults, since a cleanup that
	// checks one record while deleting both kinds' names is
	// the scope divergence the per-kind record closes.
	oneKind := `package settle

func removeUnitTemps(parentDir, op string, u journalUnit) error {
	if !u.TempCreatedFile {
		return nil
	}
	if _, err := blockedByAncestor(parentDir, u.Path); err != nil {
		return err
	}
	return removeOccupant(parentDir)
}

func blockedByAncestor(parentDir, rel string) (bool, error) { return false, nil }

func removeOccupant(dst string) error { return nil }
`
	_, oneKindViolations, err := auditJournalDeletions([]byte(oneKind))
	if err != nil {
		t.Fatalf("audit one-kind control: %v", err)
	}
	if len(oneKindViolations) != 1 || !strings.Contains(oneKindViolations[0], "TempCreatedLink") {
		t.Fatalf("one-kind control violations = %v, want exactly the unconsulted TempCreatedLink gate flagged", oneKindViolations)
	}

	// Negative control 4: rollbackJournal conducting its
	// deletions without ever judging the parent root — the
	// missing parentRootReplaced call must be flagged,
	// since a conductor that skips the root check resolves
	// every undo through a replaced root into another tree.
	rootless := `package settle

func rollbackJournal(ctx *Context, j *journal) error {
	return removeUnitTemps(j.ParentDir, j.Op, j.Units[0])
}

func removeUnitTemps(parentDir, op string, u journalUnit) error { return nil }
`
	_, rootlessViolations, err := auditJournalDeletions([]byte(rootless))
	if err != nil {
		t.Fatalf("audit rootless control: %v", err)
	}
	joinedRootless := strings.Join(rootlessViolations, "\n")
	if !strings.Contains(joinedRootless, "rollbackJournal") || !strings.Contains(joinedRootless, "parentRootReplaced") {
		t.Fatalf("rootless control violations = %v, want rollbackJournal's missing parentRootReplaced check flagged", rootlessViolations)
	}

	// The real tree.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate package sources")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "journal.go"))
	if err != nil {
		t.Fatalf("read journal.go: %v", err)
	}
	sites, violations, err := auditJournalDeletions(src)
	if err != nil {
		t.Fatalf("audit journal.go: %v", err)
	}
	for _, v := range violations {
		t.Error(v)
	}
	if len(sites) < 12 {
		t.Fatalf("audit enumerated only %d deletion sites in journal.go — the walk is not seeing the tree", len(sites))
	}
	observed := map[string]bool{}
	for _, s := range sites {
		observed[s.fn] = true
	}
	var stale []string
	for fn := range deletionClasses {
		if !observed[fn] {
			stale = append(stale, fn)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("deletion classification entries with no remaining deletion site (stale): %v", stale)
	}
}
