package settle

// Shape-changing delta tests (plan Task 8, fix round): a path
// that is a file on one side of a settlement and a directory on
// the other produces ancestor-related unit pairs — a write unit
// whose path descends through a delete unit's path, or a write
// unit whose landing destroys delete units beneath it. The
// Task 8 review reproduced two recovery wedges in this family
// (preimage stat ENOTDIR on the file→directory swap; forward
// delete failure and directory-preimage restore on the
// directory→file swap); these fixtures pin both directions
// through crash-and-recover, plus the verified-SELECT journal
// completion the Step 2 fixtures did not pair.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// newDirSwapFixture mirrors newJournalFixture, but the parent
// tree holds old.txt as a DIRECTORY (old.txt/inner.txt) — the
// fork side of a directory→file shape swap.
func newDirSwapFixture(t *testing.T) *journalFixture {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	scratch, err := os.MkdirTemp(wd, ".settle-journal-fixture-")
	if err != nil {
		t.Fatalf("create btrfs scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })

	parentDir := filepath.Join(scratch, "parent")
	journalWriteFile(t, filepath.Join(parentDir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "mod.txt"), "original\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "old.txt", "inner.txt"), "inner\n", 0o644)
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=settle-test", "-c", "user.email=settle-test@localhost", "commit", "-q", "-m", "parent"},
	} {
		full := append([]string{"-C", parentDir}, args...)
		cmd := exec.Command("git", full...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	tracePath := filepath.Join(scratch, "trace.db")
	traceStore, err := store.Open(tracePath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = traceStore.Close() })
	snapsPath := filepath.Join(scratch, "snaps.git")
	snaps, err := snapshot.Open(snapsPath)
	if err != nil {
		t.Fatalf("snapshot.Open: %v", err)
	}
	parent0, err := snaps.Snapshot(parentDir, map[string]string{"run": "settle-journal-fixture"})
	if err != nil {
		t.Fatalf("Snapshot parent: %v", err)
	}
	if err := snaps.Fork("parent", parent0); err != nil {
		t.Fatalf("Fork parent scope: %v", err)
	}
	root := filepath.Join(scratch, "settle-root")
	return &journalFixture{
		ctx:       &Context{Trace: traceStore, Snaps: snaps, Root: root},
		trace:     traceStore,
		snaps:     snaps,
		root:      root,
		parentDir: parentDir,
		parent0:   parent0,
		scratch:   scratch,
		tracePath: tracePath,
		snapsPath: snapsPath,
	}
}

// crashPanic is the sentinel runCrashed panics with at the
// armed unit boundary.
type crashPanic struct{ done int }

// runCrashed runs fn with unitCrashHook armed to panic once
// done reaches limit, simulating a kill at that unit boundary:
// every journal write is durable before the hook fires, so the
// on-disk state fn leaves behind is exactly a crashed verb's.
// It reports whether the crash fired; a verb that completes
// without firing it is a test failure.
func runCrashed(t *testing.T, limit int, fn func()) {
	t.Helper()
	unitCrashHook = func(done int) {
		if done >= limit {
			panic(crashPanic{done: done})
		}
	}
	defer func() { unitCrashHook = nil }()
	crashed := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(crashPanic); ok {
					crashed = true
					return
				}
				panic(r)
			}
		}()
		fn()
	}()
	if !crashed {
		t.Fatalf("verb completed without crashing at unit boundary %d", limit)
	}
}

// advanceRun snapshots dir as runScope's new head.
func advanceRun(t *testing.T, f *journalFixture, runScope, dir string) {
	t.Helper()
	commit, err := f.snaps.Snapshot(dir, map[string]string{"run": "settle-swap-fixture"})
	if err != nil {
		t.Fatalf("Snapshot run layer: %v", err)
	}
	if err := f.snaps.AdvanceScope(runScope, f.parent0, commit); err != nil {
		t.Fatalf("AdvanceScope run: %v", err)
	}
}

// TestSelectCrashRecoveryFileToDirSwap (Task 8 fix round,
// finding F-1): the fork tree holds old.txt as a file; the run
// replaces it with a directory holding inner.txt. The select is
// killed after the write mark — both units are marked done in
// that one mark (the write cleared its blocking ancestor, the
// file old.txt, and marked its delete unit with it). Recover
// must converge the parent to exactly pre-select: the write
// unit's preimage stat passes through the delete unit's file
// preimage at pre/old.txt (ENOTDIR — no preimage exists beneath
// a file), the created directory is removed, the file is
// restored, no fact is minted, and the same select then
// succeeds whole.
func TestSelectCrashRecoveryFileToDirSwap(t *testing.T) {
	f := newJournalFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := os.Remove(filepath.Join(layer.Dir, "old.txt")); err != nil {
		t.Fatalf("remove layer old.txt: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "old.txt", "inner.txt"), "inner\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)

	preHash := hashTree(t, f.parentDir)

	// Units: write old.txt/inner.txt, delete old.txt — both are
	// marked in the write's single mark (done == 2).
	runCrashed(t, 2, func() {
		_, _ = Select(f.ctx, "parent", f.parentDir, "run-a", "op-f1")
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after Recover = %s, want pre-select %s", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rolled-back select left %d facts, want 0", len(facts))
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after rollback; want %s unmoved", head, err, f.parent0)
	}

	// The same select, retried whole, succeeds.
	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-f1"); err != nil {
		t.Fatalf("fresh Select after rollback: %v", err)
	}
	expected := filepath.Join(f.scratch, "expected")
	journalWriteFile(t, filepath.Join(expected, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "mod.txt"), "original\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "old.txt", "inner.txt"), "inner\n", 0o644)
	if got, want := hashTree(t, f.parentDir), hashTree(t, expected); got != want {
		t.Fatalf("parent walk hash after fresh Select = %s, want %s", got, want)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after fresh Select = %d, want exactly 1", len(facts))
	}
}

// setupDirToFileRun forks runScope from the dir-swap fixture's
// parent and advances the run so old.txt is a file there: the
// run delta against the fork is write old.txt, delete
// old.txt/inner.txt.
func setupDirToFileRun(t *testing.T, f *journalFixture, runScope string) {
	t.Helper()
	layer, err := Fork(f.ctx, "parent", runScope)
	if err != nil {
		t.Fatalf("Fork %q: %v", runScope, err)
	}
	if err := os.Remove(filepath.Join(layer.Dir, "old.txt", "inner.txt")); err != nil {
		t.Fatalf("remove layer inner.txt: %v", err)
	}
	if err := os.Remove(filepath.Join(layer.Dir, "old.txt")); err != nil {
		t.Fatalf("remove layer old.txt dir: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "old.txt"), "now a file\n", 0o644)
	advanceRun(t, f, runScope, layer.Dir)
}

func expectedDirToFileDir(t *testing.T, f *journalFixture) string {
	t.Helper()
	dir := filepath.Join(f.scratch, "expected")
	journalWriteFile(t, filepath.Join(dir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(dir, "mod.txt"), "original\n", 0o644)
	journalWriteFile(t, filepath.Join(dir, "old.txt"), "now a file\n", 0o644)
	return dir
}

// TestSelectDirToFileSwapCompletes (Task 8 fix round, finding
// F-2a): the directory→file swap settles forward. The write
// unit for old.txt clears the directory standing at its target
// — destroying old.txt/inner.txt as a side effect — so the
// delete unit for old.txt/inner.txt must complete (its target
// is gone, which is all the unit asks), not fail ENOTDIR and
// strand the journal in applying.
func TestSelectDirToFileSwapCompletes(t *testing.T) {
	f := newDirSwapFixture(t)
	setupDirToFileRun(t, f, "run-a")

	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-f2"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got, want := hashTree(t, f.parentDir), hashTree(t, expectedDirToFileDir(t, f)); got != want {
		t.Fatalf("parent walk hash after Select = %s, want %s", got, want)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Select: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after Select = %d, want exactly 1", len(facts))
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head == f.parent0 {
		t.Fatalf("parent head = %s, %v after Select; want advanced past %s", head, err, f.parent0)
	}
}

// TestSelectCrashRecoveryDirToFileSwap (Task 8 fix round,
// finding F-2b): the directory→file select is killed after the
// write mark. The write unit's preimage path pre/old.txt is a
// DIRECTORY — it exists to hold the delete unit's preimage of
// old.txt/inner.txt; the write unit itself has no file preimage
// (a directory stood at its target, and a directory's contents
// are their own units). Recover must converge the parent to
// exactly pre-select — the written file removed, inner.txt
// restored from its own preimage — never wedge on reading the
// directory as a file, and a second Recover is a no-op.
func TestSelectCrashRecoveryDirToFileSwap(t *testing.T) {
	f := newDirSwapFixture(t)
	setupDirToFileRun(t, f, "run-a")
	preHash := hashTree(t, f.parentDir)

	runCrashed(t, 1, func() {
		_, _ = Select(f.ctx, "parent", f.parentDir, "run-a", "op-f2")
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after Recover = %s, want pre-select %s", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rolled-back select left %d facts, want 0", len(facts))
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after rollback; want %s unmoved", head, err, f.parent0)
	}

	// Never wedged: a second Recover is a no-op, and the same
	// select then succeeds whole.
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-f2"); err != nil {
		t.Fatalf("fresh Select after rollback: %v", err)
	}
	if got, want := hashTree(t, f.parentDir), hashTree(t, expectedDirToFileDir(t, f)); got != want {
		t.Fatalf("parent walk hash after fresh Select = %s, want %s", got, want)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after fresh Select = %d, want exactly 1", len(facts))
	}
}

// TestRecoverCompletesVerifiedSelectJournal (Task 8 fix round,
// Minor coverage): a hand-built verified SELECT journal
// completes the commit point the way the apply one does — the
// parent scope advances old_head → result_commit, exactly one
// select fact is minted from the recorded body alone (its
// run_head is the journal's result_commit), and a second
// Recover is a no-op. Step 2's fixtures paired select with
// rollback and apply with completion only; this pins the
// select completion branch.
func TestRecoverCompletesVerifiedSelectJournal(t *testing.T) {
	f := newJournalFixture(t)

	// The result tree the crashed verb had materialized and
	// verified before it died: snapshot it as the result commit.
	resultDir := filepath.Join(f.scratch, "result")
	journalWriteFile(t, filepath.Join(resultDir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(resultDir, "mod.txt"), "settled\n", 0o644)
	resultCommit, err := f.snaps.Snapshot(resultDir, map[string]string{"run": "settle-journal-fixture"})
	if err != nil {
		t.Fatalf("Snapshot result: %v", err)
	}
	// The parent directory already serves the result tree (the
	// verb verified it); old.txt was deleted by a unit.
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "settled\n", 0o644)
	if err := os.Remove(filepath.Join(f.parentDir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	postHash := hashTree(t, f.parentDir)

	j := &journal{
		Op:           "op-vs",
		Verb:         verbSelect,
		ParentScope:  "parent",
		RunScope:     "run-a",
		State:        journalStateVerified,
		OldHead:      f.parent0,
		ResultCommit: resultCommit,
		ParentDir:    f.parentDir,
		Fact:         store.SettleSelectBody("op-vs", "parent", "run-a", f.parent0, resultCommit),
		Units: []journalUnit{
			{Path: "mod.txt", Kind: unitWrite, Done: true},
			{Path: "old.txt", Kind: unitDelete, Done: true},
		},
	}
	if err := writeJournal(f.root, j); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != resultCommit {
		t.Fatalf("parent head = %s, %v after Recover; want result commit %s", head, err, resultCommit)
	}
	if got := hashTree(t, f.parentDir); got != postHash {
		t.Fatalf("parent walk hash changed across completion: %s -> %s", postHash, got)
	}
	facts := f.journalFacts(t, "run-a")
	if len(facts) != 1 {
		t.Fatalf("settle facts on run-a = %d, want exactly 1", len(facts))
	}
	if ref, _ := stringField(facts[0].Record, "schema_ref"); ref != store.SettleSelectSchemaRef {
		t.Fatalf("fact schema_ref = %q, want %q", ref, store.SettleSelectSchemaRef)
	}
	var body canonical.Value
	for _, p := range facts[0].Record.Pairs {
		if p.Key == "body" {
			body = p.Value
		}
	}
	for key, want := range map[string]string{
		"op": "op-vs", "parent_scope": "parent", "run_scope": "run-a",
		"parent_head": f.parent0, "run_head": resultCommit,
	} {
		if got, _ := stringField(body, key); got != want {
			t.Fatalf("minted fact body[%q] = %q, want %q (the recorded body, minted as recorded)", key, got, want)
		}
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after completion: err = %v, want gone", err)
	}

	// Idempotent: a second Recover finds no journal, changes
	// nothing, and mints no second fact.
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after second Recover = %d, want exactly 1", len(facts))
	}
}

// TestRecoverRepeatsInterruptedFileToDirRollback (PR #7
// review, Codex P2): rollback must be repeatable across an
// interruption of recovery itself. A first Recover of the
// file→directory swap undoes the write unit (removing the
// created old.txt/inner.txt) and then the delete unit
// (restoring the file old.txt from its preimage); if the
// process dies there — both undos done, journal not yet
// cleared — the repeated Recover runs the same undos against
// the restored tree. The repeated write-unit undo reaches
// removeOccupant("old.txt/inner.txt"), whose stat now fails
// ENOTDIR (old.txt is a file again): that is absence — the
// state the undo asks for already holds — not a wedge. On the
// unfixed code the repeated Recover fails permanently, and
// Select/Apply, which call Recover at entry, refuse forever.
//
// The crash hook fires only at forward unit marks, never
// inside Recover, so the interrupted state is built by running
// rollbackJournal's own undo steps directly (undoWriteUnit,
// then undoDeleteUnit — rollback's exact order) and stopping
// before clearJournalState: the journal bytes, the preimage
// staging, and the parent tree are then exactly what a Recover
// killed at that line leaves behind.
func TestRecoverRepeatsInterruptedFileToDirRollback(t *testing.T) {
	f := newJournalFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := os.Remove(filepath.Join(layer.Dir, "old.txt")); err != nil {
		t.Fatalf("remove layer old.txt: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "old.txt", "inner.txt"), "inner\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)

	preHash := hashTree(t, f.parentDir)

	// The select is killed after the write mark: both units
	// (write old.txt/inner.txt, delete old.txt) are done.
	runCrashed(t, 2, func() {
		_, _ = Select(f.ctx, "parent", f.parentDir, "run-a", "op-r2")
	})

	// The interrupted first Recover: both undos performed with
	// rollback's own code, journal and staging left in place.
	preDir := filepath.Join(stagingDirFor(f.root, "op-r2"), "pre")
	if err := undoWriteUnit(f.parentDir, preDir, journalUnit{Path: "old.txt/inner.txt", Kind: unitWrite, Done: true, Started: true, TempCreated: true}, "op-r2", nil); err != nil {
		t.Fatalf("interrupted Recover, write undo: %v", err)
	}
	if err := undoDeleteUnit(f.parentDir, preDir, journalUnit{Path: "old.txt", Kind: unitDelete, Done: true, Started: true}, "op-r2", nil); err != nil {
		t.Fatalf("interrupted Recover, delete undo: %v", err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after interrupted Recover = %s, want pre-select %s (the first rollback had converged the tree)", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); err != nil {
		t.Fatalf("journal after interrupted Recover: %v, want still present", err)
	}

	// The repeated Recover must converge, not wedge.
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("repeated Recover: %v", err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after repeated Recover = %s, want pre-select %s", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after repeated Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rolled-back select left %d facts, want 0", len(facts))
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after repeated Recover; want %s unmoved", head, err, f.parent0)
	}

	// Never wedged at the verb level either: the same select
	// (whose entry calls Recover) succeeds whole.
	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-r2"); err != nil {
		t.Fatalf("fresh Select after repeated Recover: %v", err)
	}
	expected := filepath.Join(f.scratch, "expected")
	journalWriteFile(t, filepath.Join(expected, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "mod.txt"), "original\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "old.txt", "inner.txt"), "inner\n", 0o644)
	if got, want := hashTree(t, f.parentDir), hashTree(t, expected); got != want {
		t.Fatalf("parent walk hash after fresh Select = %s, want %s", got, want)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after fresh Select = %d, want exactly 1", len(facts))
	}
}
