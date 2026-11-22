package settle

// Symlinked parent root tests (PR #7 wave 6, Codex P2 on
// internal/settle/settle.go): Select and Apply accept the
// parent directory as the caller spells it, and a caller may
// spell it through a symlink — a spelling the snapshot side of
// the same verbs supports, because Snapshot resolves its
// workspace root before walking. Settle's own inventory walk
// (walkDirFiles) does not follow a symlinked root: it
// inventories the link itself as the parent's only entry, at
// ".", so the verbs planned every wanted file as a creation
// and the root link as an extra deletion — and preflight then
// failed capturing the link's preimage into the same pre/
// directory the file preimages had already populated, so
// settlement could not operate on a workspace path the
// snapshot API accepts. The parent root is now resolved once
// — at the verb entries and again in the materializer, the
// one path both verbs record the journal through — so the
// journal's parent_dir, the anchor's vouch, the inventory,
// and Recover's rollback target are the real tree's path,
// whatever spelling the caller used (see resolveParentDir).
// These fixtures pin the whole arc for both materializing
// verbs.

import (
	"os"
	"path/filepath"
	"testing"
)

// symlinkToParent returns a symlink naming the fixture's
// parent directory: the spelling these tests settle through.
func symlinkToParent(t *testing.T, f *journalFixture) string {
	t.Helper()
	link := filepath.Join(f.scratch, "parent-link")
	if err := os.Symlink(f.parentDir, link); err != nil {
		t.Fatalf("symlink to parent dir: %v", err)
	}
	return link
}

// writeExpectedParent builds the tree a settled parent must
// equal, with the given keep.txt content (the plain fixture's
// "keep\n", or the apply fixture's parent-side change).
func writeExpectedParent(t *testing.T, f *journalFixture, keepContent string) string {
	t.Helper()
	expected := filepath.Join(f.scratch, "expected")
	journalWriteFile(t, filepath.Join(expected, "keep.txt"), keepContent, 0o644)
	journalWriteFile(t, filepath.Join(expected, "mod.txt"), "settled\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "old.txt"), "old\n", 0o644)
	return expected
}

// TestSelectSymlinkedParentDir: a Select spelled through a
// symlink to the parent directory settles the real tree
// exactly as the same Select spelled directly.
func TestSelectSymlinkedParentDir(t *testing.T) {
	f := newJournalFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "settled\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)
	link := symlinkToParent(t, f)

	if _, err := Select(f.ctx, "parent", link, "run-a", "op-sym"); err != nil {
		t.Fatalf("Select through a symlinked parent directory: %v", err)
	}
	expected := writeExpectedParent(t, f, "keep\n")
	if got, want := hashTree(t, f.parentDir), hashTree(t, expected); got != want {
		t.Fatalf("parent walk hash after Select through symlink = %s, want %s", got, want)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after Select through symlink = %d, want exactly 1", len(facts))
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head == f.parent0 {
		t.Fatalf("parent head = %s, %v after Select; want it advanced past %s", head, err, f.parent0)
	}
}

// TestSelectSymlinkedParentDirCrashRecovers: a Select spelled
// through a symlink crashes at a unit boundary; the journal on
// disk must record the RESOLVED parent directory — Recover
// receives no directory and acts on the recorded one — and
// Recover must converge the real tree byte-exactly. The same
// verb, spelled through the symlink again, then succeeds
// whole.
func TestSelectSymlinkedParentDirCrashRecovers(t *testing.T) {
	f := newJournalFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "settled\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)
	link := symlinkToParent(t, f)
	preHash := hashTree(t, f.parentDir)

	runCrashed(t, 1, func() {
		_, _ = Select(f.ctx, "parent", link, "run-a", "op-symcrash")
	})

	j, err := readJournal(f.root, "parent")
	if err != nil {
		t.Fatalf("readJournal after symlinked-path crash: %v", err)
	}
	if j == nil {
		t.Fatal("readJournal after symlinked-path crash: no journal, want the crashed select's")
	}
	if j.ParentDir != f.parentDir {
		t.Fatalf("journal parent_dir = %q, want the resolved %q (the journal must name the real tree, not the caller's symlink spelling)", j.ParentDir, f.parentDir)
	}

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
		t.Fatalf("parent head = %s, %v after Recover; want %s unmoved", head, err, f.parent0)
	}

	if _, err := Select(f.ctx, "parent", link, "run-a", "op-symcrash"); err != nil {
		t.Fatalf("fresh Select through symlink after Recover: %v", err)
	}
	expected := writeExpectedParent(t, f, "keep\n")
	if got, want := hashTree(t, f.parentDir), hashTree(t, expected); got != want {
		t.Fatalf("parent walk hash after fresh Select = %s, want %s", got, want)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after fresh Select = %d, want exactly 1", len(facts))
	}
}

// TestApplySymlinkedParentDir: the same spelling through
// Apply — the parent side's disjoint post-fork change and the
// run's change land as their union on the real tree.
func TestApplySymlinkedParentDir(t *testing.T) {
	f := newJournalFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "settled\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)
	// The parent side's disjoint post-fork change.
	journalWriteFile(t, filepath.Join(f.parentDir, "keep.txt"), "parent keep\n", 0o644)
	link := symlinkToParent(t, f)

	if _, err := Apply(f.ctx, "parent", link, "run-a", "op-symapp"); err != nil {
		t.Fatalf("Apply through a symlinked parent directory: %v", err)
	}
	expected := writeExpectedParent(t, f, "parent keep\n")
	if got, want := hashTree(t, f.parentDir), hashTree(t, expected); got != want {
		t.Fatalf("parent walk hash after Apply through symlink = %s, want %s", got, want)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after Apply through symlink = %d, want exactly 1", len(facts))
	}
}
