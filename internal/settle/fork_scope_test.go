package settle

// Scope-name validation tests (PR #7 review, Codex P1 on
// settle.go): Fork constructs filesystem paths from the run
// scope — the base extraction directory and the temporary
// layer root it unconditionally RemoveAlls — BEFORE the
// snapshot store's Fork ever validates the scope name. A
// run scope carrying traversal components therefore steers
// those paths outside the settle Root: with Root at
// <scratch>/a/b/settle and scope "../../../victim", the base
// extraction targets <scratch>/a/victim while the temporary
// layer root resolves to <root>/victim, and when the former
// is absent or empty the execution reaches the RemoveAll and
// deletes the latter — outside the settlement root — before
// the invalid scope is eventually rejected. The scope-name
// law (the snapshot store's: lowercase alphanumerics and
// hyphens, starting with an alphanumeric — a single ref path
// component) must be enforced before any path is constructed
// from the name, in Fork and in every other verb that builds
// a filesystem path from a scope name (the sidecar path in
// Select/Apply/Discard, the journal path in Recover).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestForkRejectsTraversalRunScopeBeforeFilesystemEffects:
// the finding's shape, built against a deep settle Root so
// both resolutions of the traversal scope land inside the
// scratch dir. The victim directory sits where Fork's
// temporary-layer RemoveAll would resolve; its sentinel must
// survive, and the refused Fork must have created nothing at
// all under the settle Root besides the victim the test
// itself placed there.
func TestForkRejectsTraversalRunScopeBeforeFilesystemEffects(t *testing.T) {
	f := newJournalFixture(t)
	deepRoot := filepath.Join(f.scratch, "a", "b", "settle")
	ctx := &Context{Trace: f.trace, Snaps: f.snaps, Root: deepRoot}

	victim := filepath.Join(deepRoot, "victim")
	journalWriteFile(t, filepath.Join(victim, "sentinel.txt"), "sentinel\n", 0o644)

	if _, err := Fork(ctx, "parent", "../../../victim"); err == nil {
		t.Fatal("Fork with a traversal run scope succeeded, want a refusal")
	}

	if got, err := os.ReadFile(filepath.Join(victim, "sentinel.txt")); err != nil || string(got) != "sentinel\n" {
		t.Fatalf("victim sentinel after refused Fork = %q, %v; want it intact (the traversal reached a RemoveAll outside the settle root)", got, err)
	}
	entries, err := os.ReadDir(deepRoot)
	if err != nil {
		t.Fatalf("read settle root after refused Fork: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "victim" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("settle root after refused Fork = %v, want only the test's own victim directory (validation must precede every filesystem effect)", names)
	}
	if _, err := os.Stat(filepath.Join(f.scratch, "a", "victim")); !os.IsNotExist(err) {
		t.Fatalf("base extraction target after refused Fork: err = %v, want never created", err)
	}
}

// TestForkRejectsInvalidRunScopeNames: names the scope-name
// law rejects — uppercase, underscore, a leading hyphen, an
// embedded separator — are refused before the settle Root is
// even created.
func TestForkRejectsInvalidRunScopeNames(t *testing.T) {
	for _, name := range []string{"Run_A", "run_a", "-run", "run/a", "run..a"} {
		f := newJournalFixture(t)
		if _, err := Fork(f.ctx, "parent", name); err == nil {
			t.Fatalf("Fork with run scope %q succeeded, want a refusal", name)
		}
		if _, err := os.Stat(f.root); !os.IsNotExist(err) {
			t.Fatalf("Fork with run scope %q created the settle root (err = %v); validation must precede every filesystem effect", name, err)
		}
	}
}

// TestVerbsRejectInvalidScopeNamesBeforePaths is the census
// leg: Fork is not the only verb that builds a filesystem
// path from a scope name before the snapshot store validates
// it. Select and Apply read the fork sidecar at a path built
// from the run scope; Discard does the same; Recover reads
// the journal at a path built from the parent scope. Each
// must refuse an invalid name as invalid — not resolve it
// into a path and report the absence it finds there.
func TestVerbsRejectInvalidScopeNamesBeforePaths(t *testing.T) {
	f := newJournalFixture(t)
	const bad = "../../../victim"

	if _, err := Select(f.ctx, "parent", f.parentDir, bad, "op-1"); err == nil || errors.Is(err, ErrNoForkProvenance) || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("Select with traversal run scope: err = %v, want an invalid-scope refusal before the sidecar path is built", err)
	}
	if _, err := Apply(f.ctx, "parent", f.parentDir, bad, "op-1"); err == nil || errors.Is(err, ErrNoForkProvenance) || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("Apply with traversal run scope: err = %v, want an invalid-scope refusal before the sidecar path is built", err)
	}
	if _, err := Discard(f.ctx, bad, "op-1"); err == nil || errors.Is(err, ErrNoForkProvenance) || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("Discard with traversal run scope: err = %v, want an invalid-scope refusal before the sidecar path is built", err)
	}
	if err := Recover(f.ctx, bad); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("Recover with traversal parent scope: err = %v, want an invalid-scope refusal before the journal path is built", err)
	}
	if _, err := Fork(f.ctx, bad, "run-a"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("Fork with traversal parent scope: err = %v, want an invalid-scope refusal", err)
	}
}
