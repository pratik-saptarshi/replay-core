package settle

// Relative parent directory tests (PR #7 wave 2, Codex P2):
// Select and Apply accept the parent directory as the caller
// spells it, and a caller may spell it relatively — ".", or
// "workspace". The materializer performs the settlement
// happily through the relative path, but the journal records
// ParentDir verbatim, and Recover — whose fixed signature
// receives no directory and must act on the recorded one, from
// whatever process and working directory comes next — reads it
// back through decodeJournal, which requires an absolute path.
// A journal left behind by a crashed relative-path verb was
// therefore unreadable by the only mechanism that could
// resolve it: every later Select and Apply on that parent
// failed at its entry Recover, permanently. The materializer
// now normalizes the directory to an absolute path before the
// journal is written or the parent is touched (decodeJournal's
// requirement stays, as the backstop for journals written by
// anything else); these fixtures pin the whole arc for both
// materializing verbs.

import (
	"os"
	"path/filepath"
	"testing"
)

// chdir moves the test process into dir and restores the
// original working directory at cleanup. The settle tests do
// not run in parallel; the relative-path verbs below resolve
// against the process working directory by definition.
func chdir(t *testing.T, dir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

// TestSelectRelativeParentDirCrashRecovers: a Select spelled
// with a relative parent directory crashes at a unit boundary;
// the journal on disk must record the ABSOLUTE directory, and
// Recover — invoked from a different working directory, with
// no directory argument to give — must converge the parent
// byte-exactly. The same verb, spelled relatively again, then
// succeeds whole.
func TestSelectRelativeParentDirCrashRecovers(t *testing.T) {
	f := newJournalFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "settled\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)
	preHash := hashTree(t, f.parentDir)

	chdir(t, f.scratch)
	runCrashed(t, 1, func() {
		_, _ = Select(f.ctx, "parent", "parent", "run-a", "op-rel")
	})

	// The journal records the absolute parent directory: that
	// is what makes it recoverable by a process that never
	// shared this working directory.
	j, err := readJournal(f.root, "parent")
	if err != nil {
		t.Fatalf("readJournal after relative-path crash: %v (the journal a crashed verb leaves must be readable by Recover)", err)
	}
	if j == nil {
		t.Fatal("readJournal after relative-path crash: no journal, want the crashed select's")
	}
	if j.ParentDir != f.parentDir {
		t.Fatalf("journal parent_dir = %q, want the absolute %q", j.ParentDir, f.parentDir)
	}

	// Recover from a different working directory converges.
	if err := os.Chdir(filepath.Dir(f.scratch)); err != nil {
		t.Fatalf("Chdir away: %v", err)
	}
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover from a different working directory: %v", err)
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

	// And the relative spelling still works for the verb
	// itself: the same select, from the scratch directory
	// again, succeeds whole.
	if err := os.Chdir(f.scratch); err != nil {
		t.Fatalf("Chdir back: %v", err)
	}
	if _, err := Select(f.ctx, "parent", "parent", "run-a", "op-rel"); err != nil {
		t.Fatalf("fresh relative-path Select after Recover: %v", err)
	}
	expected := filepath.Join(f.scratch, "expected")
	journalWriteFile(t, filepath.Join(expected, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "mod.txt"), "settled\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "old.txt"), "old\n", 0o644)
	if got, want := hashTree(t, f.parentDir), hashTree(t, expected); got != want {
		t.Fatalf("parent walk hash after fresh Select = %s, want %s", got, want)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after fresh Select = %d, want exactly 1", len(facts))
	}
}

// TestApplyRelativeParentDirCrashRecovers: the same arc
// through Apply — the normalization lives in the materializer
// both verbs commit through, and this pins that Apply's
// journal is the absolute-path one too.
func TestApplyRelativeParentDirCrashRecovers(t *testing.T) {
	f := newJournalFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "settled\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)
	// The parent side's disjoint post-fork change.
	journalWriteFile(t, filepath.Join(f.parentDir, "keep.txt"), "parent keep\n", 0o644)
	preHash := hashTree(t, f.parentDir)

	chdir(t, f.scratch)
	runCrashed(t, 1, func() {
		_, _ = Apply(f.ctx, "parent", "parent", "run-a", "op-relapp")
	})

	j, err := readJournal(f.root, "parent")
	if err != nil {
		t.Fatalf("readJournal after relative-path crash: %v (the journal a crashed verb leaves must be readable by Recover)", err)
	}
	if j == nil {
		t.Fatal("readJournal after relative-path crash: no journal, want the crashed apply's")
	}
	if j.ParentDir != f.parentDir {
		t.Fatalf("journal parent_dir = %q, want the absolute %q", j.ParentDir, f.parentDir)
	}

	if err := os.Chdir(filepath.Dir(f.scratch)); err != nil {
		t.Fatalf("Chdir away: %v", err)
	}
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover from a different working directory: %v", err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after Recover = %s, want pre-apply %s", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rolled-back apply left %d facts, want 0", len(facts))
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after Recover; want %s unmoved", head, err, f.parent0)
	}

	// The same apply, spelled relatively again, succeeds whole
	// and lands the union of the two disjoint deltas.
	if err := os.Chdir(f.scratch); err != nil {
		t.Fatalf("Chdir back: %v", err)
	}
	if _, err := Apply(f.ctx, "parent", "parent", "run-a", "op-relapp"); err != nil {
		t.Fatalf("fresh relative-path Apply after Recover: %v", err)
	}
	expected := filepath.Join(f.scratch, "expected")
	journalWriteFile(t, filepath.Join(expected, "keep.txt"), "parent keep\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "mod.txt"), "settled\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "old.txt"), "old\n", 0o644)
	if got, want := hashTree(t, f.parentDir), hashTree(t, expected); got != want {
		t.Fatalf("parent walk hash after fresh Apply = %s, want %s", got, want)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after fresh Apply = %d, want exactly 1", len(facts))
	}
}
