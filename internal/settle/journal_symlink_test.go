package settle

// Symlink-swap recovery tests (PR #7 wave 2): a path that is a
// symlink on one side of a settlement and a directory on the
// other produces the mirror image of the shape-swap family —
// the settlement's own writes replace an ancestor of a unit
// path, and any later pass that resolves the descendant
// through the replacement leaves the tree it was confined to.
// Wave 1 closed that traversal class in Apply's union builder
// (the pass now decides lexically, from its own replaced set);
// these fixtures pin the same class in the journal's rollback
// and in the materializer's directory pruning.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// newSymlinkSwapFixture mirrors newJournalFixture, but the
// parent tree holds old as a SYMLINK to a directory outside
// the tree (the target dir, holding its own inner.txt) — the
// fork side of a symlink→directory shape swap. The target dir
// is returned alongside the fixture: it is the outside tree
// every assertion in this file watches.
func newSymlinkSwapFixture(t *testing.T) (*journalFixture, string) {
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

	targetDir := filepath.Join(scratch, "target")
	journalWriteFile(t, filepath.Join(targetDir, "inner.txt"), "target inner\n", 0o644)

	parentDir := filepath.Join(scratch, "parent")
	journalWriteFile(t, filepath.Join(parentDir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "mod.txt"), "original\n", 0o644)
	if err := os.Symlink(targetDir, filepath.Join(parentDir, "old")); err != nil {
		t.Fatalf("symlink parent old: %v", err)
	}
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
	}, targetDir
}

// newDirToSymlinkFixture mirrors newDirSwapFixture, but the
// shape swap runs the other way: the parent tree holds old as
// a real DIRECTORY (old/inner.txt, and an empty old/sub), and
// an outside target directory — holding its own inner.txt and
// its own empty sub — waits for the run to point a symlink at
// it. A third directory, gone, stands empty in the parent and
// in no tree: the pruning control, legitimately pruned by any
// correct settle.
func newDirToSymlinkFixture(t *testing.T) (*journalFixture, string) {
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

	targetDir := filepath.Join(scratch, "target")
	journalWriteFile(t, filepath.Join(targetDir, "inner.txt"), "target inner\n", 0o644)
	if err := os.MkdirAll(filepath.Join(targetDir, "sub"), 0o755); err != nil {
		t.Fatalf("create target sub: %v", err)
	}

	parentDir := filepath.Join(scratch, "parent")
	journalWriteFile(t, filepath.Join(parentDir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "mod.txt"), "original\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "old", "inner.txt"), "inner\n", 0o644)
	if err := os.MkdirAll(filepath.Join(parentDir, "old", "sub"), 0o755); err != nil {
		t.Fatalf("create parent old/sub: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(parentDir, "gone"), 0o755); err != nil {
		t.Fatalf("create parent gone: %v", err)
	}
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
	}, targetDir
}

// TestSelectDirToSymlinkSwapPruneStaysInTree (PR #7 wave 2,
// Codex P2): the materializer's directory pruning must not
// resolve through a symlink the same materialization
// installed. The run replaces the directory old with a symlink
// to an outside target. The pre-materialization inventory
// still lists old/sub as a directory of the parent; after the
// write lands, resolving old/sub follows the freshly installed
// link and names the TARGET's sub — and the prune, which
// removes any still-directory it finds there, deletes the
// outside directory. The snapshot verification cannot see it
// (the parent's file map is exactly right) and the journal
// cannot restore it (no unit ever named it). The prune must
// exclude descendants of replaced ancestors lexically, the
// same decision the union builder makes — while still pruning
// a genuinely stale directory (gone) in the same settle.
func TestSelectDirToSymlinkSwapPruneStaysInTree(t *testing.T) {
	f, targetDir := newDirToSymlinkFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := os.Remove(filepath.Join(layer.Dir, "old", "inner.txt")); err != nil {
		t.Fatalf("remove layer inner.txt: %v", err)
	}
	if err := os.Remove(filepath.Join(layer.Dir, "old")); err != nil {
		t.Fatalf("remove layer old dir: %v", err)
	}
	if err := os.Symlink(targetDir, filepath.Join(layer.Dir, "old")); err != nil {
		t.Fatalf("symlink layer old: %v", err)
	}
	advanceRun(t, f, "run-a", layer.Dir)

	targetHash := hashTree(t, targetDir)

	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-w2p"); err != nil {
		t.Fatalf("Select: %v", err)
	}

	// The outside tree is byte-identical, including the empty
	// directory no file-map hash can see.
	if got := hashTree(t, targetDir); got != targetHash {
		t.Fatalf("target walk hash after Select = %s, want %s", got, targetHash)
	}
	if info, err := os.Stat(filepath.Join(targetDir, "sub")); err != nil || !info.IsDir() {
		t.Fatalf("target sub after Select: info = %v, err = %v; want the outside directory untouched (pruning escaped the tree)", info, err)
	}
	if b, err := os.ReadFile(filepath.Join(targetDir, "inner.txt")); err != nil || string(b) != "target inner\n" {
		t.Fatalf("target inner.txt after Select = %q, %v; want the unchanged outside file", b, err)
	}

	// The parent serves exactly the settled tree, and pruning
	// itself still works inside it: the stale empty directory
	// is gone.
	expected := filepath.Join(f.scratch, "expected")
	journalWriteFile(t, filepath.Join(expected, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "mod.txt"), "original\n", 0o644)
	if err := os.Symlink(targetDir, filepath.Join(expected, "old")); err != nil {
		t.Fatalf("symlink expected old: %v", err)
	}
	if got, want := hashTree(t, f.parentDir), hashTree(t, expected); got != want {
		t.Fatalf("parent walk hash after Select = %s, want %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(f.parentDir, "gone")); !os.IsNotExist(err) {
		t.Fatalf("parent gone after Select: err = %v, want pruned", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after Select = %d, want exactly 1", len(facts))
	}
}

// TestRecoverRepeatsSymlinkToDirRollback (PR #7 wave 2, Codex
// P1): rollback must never traverse a symlink it restored
// itself. The fork tree holds old as a symlink to an outside
// directory; the run settles a replacement directory at old
// holding old/inner.txt. The select is killed after the write
// mark (both units done: the write, and the delete of old the
// write's blocking removal consumed). A first Recover is then
// interrupted after both undos — the created inner.txt
// removed, the symlink at old restored from its preimage —
// with the journal not yet cleared. The repeated Recover runs
// the write unit's undo again: it finds no pre/old/inner.txt
// (capture is lexical, so nothing was ever recorded beneath
// the symlink) and reaches for the live old/inner.txt — which
// now resolves through the restored symlink into the outside
// tree, naming the target's own unchanged inner.txt. Removing
// it deletes a file settlement has no claim to, silently: the
// parent still converges and the journal still clears. The
// repeated rollback must instead decide lexically — an
// ancestor stands as a restored symlink, so the occupant the
// unit created is not reachable in this tree — and leave the
// outside tree byte-identical.
func TestRecoverRepeatsSymlinkToDirRollback(t *testing.T) {
	f, targetDir := newSymlinkSwapFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := os.Remove(filepath.Join(layer.Dir, "old")); err != nil {
		t.Fatalf("remove layer old symlink: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "old", "inner.txt"), "settled inner\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)

	preHash := hashTree(t, f.parentDir)
	targetHash := hashTree(t, targetDir)

	// Units: write old/inner.txt, delete old — both marked in
	// the write's single mark (done == 2).
	runCrashed(t, 2, func() {
		_, _ = Select(f.ctx, "parent", f.parentDir, "run-a", "op-w2")
	})

	// The interrupted first Recover: both undos performed with
	// rollback's own code, journal and staging left in place
	// (the construction TestRecoverRepeatsInterruptedFileToDirRollback
	// established — the crash hook never fires inside Recover).
	preDir := filepath.Join(stagingDirFor(f.root, "op-w2"), "pre")
	if err := undoWriteUnit(f.parentDir, preDir, "old/inner.txt"); err != nil {
		t.Fatalf("interrupted Recover, write undo: %v", err)
	}
	if err := undoDeleteUnit(f.parentDir, preDir, "old"); err != nil {
		t.Fatalf("interrupted Recover, delete undo: %v", err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after interrupted Recover = %s, want pre-select %s (the first rollback had converged the tree)", got, preHash)
	}
	if got := hashTree(t, targetDir); got != targetHash {
		t.Fatalf("target walk hash after interrupted Recover = %s, want %s (the first rollback stays in the tree)", got, targetHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); err != nil {
		t.Fatalf("journal after interrupted Recover: %v, want still present", err)
	}

	// The repeated Recover must converge the parent — and
	// leave the outside tree byte-identical.
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("repeated Recover: %v", err)
	}
	if got := hashTree(t, targetDir); got != targetHash {
		t.Fatalf("target walk hash after repeated Recover = %s, want %s (rollback deleted through the restored symlink)", got, targetHash)
	}
	if b, err := os.ReadFile(filepath.Join(targetDir, "inner.txt")); err != nil || string(b) != "target inner\n" {
		t.Fatalf("target inner.txt after repeated Recover = %q, %v; want the unchanged outside file", b, err)
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

	// Never wedged: the same select then succeeds whole, and
	// settling the replacement directory still never touches
	// the outside tree the fork's symlink pointed at.
	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-w2"); err != nil {
		t.Fatalf("fresh Select after repeated Recover: %v", err)
	}
	if got := hashTree(t, targetDir); got != targetHash {
		t.Fatalf("target walk hash after fresh Select = %s, want %s", got, targetHash)
	}
	expected := filepath.Join(f.scratch, "expected")
	journalWriteFile(t, filepath.Join(expected, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "mod.txt"), "original\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "old", "inner.txt"), "settled inner\n", 0o644)
	if got, want := hashTree(t, f.parentDir), hashTree(t, expected); got != want {
		t.Fatalf("parent walk hash after fresh Select = %s, want %s", got, want)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after fresh Select = %d, want exactly 1", len(facts))
	}
}
