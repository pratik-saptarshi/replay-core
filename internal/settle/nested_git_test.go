package settle

// Nested-repository replacement tests (PR #7 wave 7, Codex P1
// on b96ae2c): a directory replacement whose target directory
// holds an entry the workspace inventory excludes — an entry
// named .git, at any depth, of any filesystem type — deletes
// content no snapshot records, no journal unit names, and no
// preimage captures. Verification compares file maps the
// inventory defines, so it accepts the result; rollback
// restores only what preimages hold, so the metadata is gone
// for good. The settlement must refuse such a replacement
// before any mutation instead.
//
// The exclusion is name-based, not content-based: the snapshot
// walk (internal/snapshot) and walkDirFiles both skip an entry
// whose NAME is .git, whatever its type or contents. The
// fixtures therefore plant a hand-built metadata tree — a few
// plausible files — rather than a real repository: the walks
// never read a byte of it, which is exactly the defect.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// nestedGitFiles is the hand-built metadata tree planted at
// parent/sub/.git by plantNestedGitDir: relative path → bytes.
var nestedGitFiles = map[string]string{
	"HEAD":                "ref: refs/heads/main\n",
	"config":              "[core]\n\trepositoryformatversion = 0\n",
	"refs/heads/main":     "5f3c9a1b2d4e6f708192a3b4c5d6e7f8091a2b3c\n",
	"objects/ab/cdef0123": "object bytes\n",
	"objects/pack/idx":    "pack index bytes\n",
}

// nestedGitFixtureWith builds a journalFixture whose parent
// tree plant() populates. Unlike the older fixtures the parent
// is not git-initialized: nothing in the settlement flow reads
// the parent's own repository state (snapshots walk the tree
// into the store's repo), and the nested metadata below is
// deliberately not a real repository.
func nestedGitFixtureWith(t *testing.T, plant func(t *testing.T, parentDir string)) *journalFixture {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	scratch, err := os.MkdirTemp(wd, ".settle-nested-git-fixture-")
	if err != nil {
		t.Fatalf("create btrfs scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })

	parentDir := filepath.Join(scratch, "parent")
	journalWriteFile(t, filepath.Join(parentDir, "keep.txt"), "keep\n", 0o644)
	plant(t, parentDir)

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
	parent0, err := snaps.Snapshot(parentDir, map[string]string{"run": "settle-nested-git-fixture"})
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

// plantNestedGitDir plants sub/tracked.txt and the sub/.git
// metadata directory tree.
func plantNestedGitDir(t *testing.T, parentDir string) {
	t.Helper()
	journalWriteFile(t, filepath.Join(parentDir, "sub", "tracked.txt"), "tracked\n", 0o644)
	for rel, content := range nestedGitFiles {
		journalWriteFile(t, filepath.Join(parentDir, "sub", ".git", filepath.FromSlash(rel)), content, 0o644)
	}
}

// plantNestedGitFile plants the worktree form: sub/.git is a
// regular file holding a gitdir pointer.
func plantNestedGitFile(t *testing.T, parentDir string) {
	t.Helper()
	journalWriteFile(t, filepath.Join(parentDir, "sub", "tracked.txt"), "tracked\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "sub", ".git"), "gitdir: ../elsewhere/.git\n", 0o644)
}

// plantDeepNestedGit plants the excluded entry one directory
// deeper: sub/inner/.git, beneath tracked content at both
// levels.
func plantDeepNestedGit(t *testing.T, parentDir string) {
	t.Helper()
	journalWriteFile(t, filepath.Join(parentDir, "sub", "tracked.txt"), "tracked\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "sub", "inner", "deep.txt"), "deep\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "sub", "inner", ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "sub", "inner", ".git", "refs", "heads", "main"), "5f3c9a1b2d4e6f708192a3b4c5d6e7f8091a2b3c\n", 0o644)
}

// plantPlainSub plants sub with only inventoried content —
// the control shape: no excluded entry anywhere beneath it.
func plantPlainSub(t *testing.T, parentDir string) {
	t.Helper()
	journalWriteFile(t, filepath.Join(parentDir, "sub", "tracked.txt"), "tracked\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "sub", "deep", "more.txt"), "more\n", 0o644)
}

// setupSubToFileRun forks runScope and advances the run so sub
// is a regular file there: the run delta against the fork is
// write sub, plus a delete unit for every inventoried entry
// that stood beneath sub.
func setupSubToFileRun(t *testing.T, f *journalFixture, runScope string) {
	t.Helper()
	layer, err := Fork(f.ctx, "parent", runScope)
	if err != nil {
		t.Fatalf("Fork %q: %v", runScope, err)
	}
	if err := os.RemoveAll(filepath.Join(layer.Dir, "sub")); err != nil {
		t.Fatalf("remove layer sub: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "sub"), "now a file\n", 0o644)
	advanceRun(t, f, runScope, layer.Dir)
}

// setupSubToSymlinkRun forks runScope and advances the run so
// sub is a symlink there.
func setupSubToSymlinkRun(t *testing.T, f *journalFixture, runScope string) {
	t.Helper()
	layer, err := Fork(f.ctx, "parent", runScope)
	if err != nil {
		t.Fatalf("Fork %q: %v", runScope, err)
	}
	if err := os.RemoveAll(filepath.Join(layer.Dir, "sub")); err != nil {
		t.Fatalf("remove layer sub: %v", err)
	}
	if err := os.Symlink("keep.txt", filepath.Join(layer.Dir, "sub")); err != nil {
		t.Fatalf("symlink layer sub: %v", err)
	}
	advanceRun(t, f, runScope, layer.Dir)
}

// assertNestedGitIntact fails the test unless every planted
// metadata file still stands in the parent with its planted
// bytes. It is checked BEFORE the refusal assertion in the
// tests below: on the unfixed head its failure is the concrete
// evidence of the loss (the metadata is simply gone).
func assertNestedGitIntact(t *testing.T, f *journalFixture) {
	t.Helper()
	for rel, want := range nestedGitFiles {
		b, err := os.ReadFile(filepath.Join(f.parentDir, "sub", ".git", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("nested .git entry %q unreadable after the verb: %v — excluded metadata was destroyed", rel, err)
		}
		if string(b) != want {
			t.Fatalf("nested .git entry %q = %q, want planted %q", rel, b, want)
		}
	}
}

// assertRefusalLeavesNoTrace pins the refusal's shape: a
// descriptive error naming the excluded entry, no journal, no
// fact, an unmoved parent head.
func assertRefusalLeavesNoTrace(t *testing.T, f *journalFixture, runScope string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("verb succeeded, want a refusal: the replacement deletes inventory-excluded content no preimage captures")
	}
	if !strings.Contains(err.Error(), ".git") {
		t.Fatalf("refusal error does not name the excluded entry: %v", err)
	}
	if _, serr := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(serr) {
		t.Fatalf("journal after refusal: err = %v, want none written", serr)
	}
	if facts := f.journalFacts(t, runScope); len(facts) != 0 {
		t.Fatalf("refused verb left %d facts, want 0", len(facts))
	}
	if head, herr := f.snaps.ScopeHead("parent"); herr != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after refusal; want %s unmoved", head, herr, f.parent0)
	}
}

// TestSelectRefusesDirToFileOverNestedGit (PR #7 wave 7): the
// directory→file replacement of a directory holding a nested
// repository's metadata directory is refused before any
// mutation — the metadata, the tracked content, and the
// parent's walk hash all stand — and the refusal is
// retry-safe: with the nested repository moved out, the same
// select under the same op ID proceeds and settles.
func TestSelectRefusesDirToFileOverNestedGit(t *testing.T) {
	f := nestedGitFixtureWith(t, plantNestedGitDir)
	setupSubToFileRun(t, f, "run-a")
	preHash := hashTree(t, f.parentDir)

	_, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-w7")
	assertNestedGitIntact(t, f)
	assertRefusalLeavesNoTrace(t, f, "run-a", err)
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after refusal = %s, want pre-verb %s", got, preHash)
	}

	// Retry-safe: move the nested repository out and the same
	// operation proceeds.
	moved := filepath.Join(f.scratch, "moved-git")
	if err := os.Rename(filepath.Join(f.parentDir, "sub", ".git"), moved); err != nil {
		t.Fatalf("move nested .git out: %v", err)
	}
	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-w7"); err != nil {
		t.Fatalf("Select after moving the nested repository out: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(f.parentDir, "sub"))
	if err != nil || string(b) != "now a file\n" {
		t.Fatalf("parent sub after retry = %q, %v; want the run's file", b, err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after retry = %d, want exactly 1", len(facts))
	}
}

// TestSelectRefusesDirToSymlinkOverNestedGit (PR #7 wave 7):
// replaceWithSymlink removes the standing directory exactly
// as the file writer does, so the symlink form of the
// replacement is refused the same way.
func TestSelectRefusesDirToSymlinkOverNestedGit(t *testing.T) {
	f := nestedGitFixtureWith(t, plantNestedGitDir)
	setupSubToSymlinkRun(t, f, "run-a")
	preHash := hashTree(t, f.parentDir)

	_, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-w7s")
	assertNestedGitIntact(t, f)
	assertRefusalLeavesNoTrace(t, f, "run-a", err)
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after refusal = %s, want pre-verb %s", got, preHash)
	}
}

// TestSelectRefusesDirToFileOverGitFile (PR #7 wave 7): the
// excluded entry need not be a directory — a worktree's .git
// FILE beneath the replaced directory is inventory-excluded
// too, and its silent deletion is refused identically.
func TestSelectRefusesDirToFileOverGitFile(t *testing.T) {
	f := nestedGitFixtureWith(t, plantNestedGitFile)
	setupSubToFileRun(t, f, "run-a")

	_, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-w7f")
	b, rerr := os.ReadFile(filepath.Join(f.parentDir, "sub", ".git"))
	if rerr != nil || string(b) != "gitdir: ../elsewhere/.git\n" {
		t.Fatalf("sub/.git file after the verb = %q, %v — the excluded entry was destroyed", b, rerr)
	}
	assertRefusalLeavesNoTrace(t, f, "run-a", err)
}

// TestSelectRefusesDirToFileOverDeepNestedGit (PR #7 wave 7):
// the excluded entry may sit at any depth beneath the
// directory being replaced — here inside sub/inner — and the
// refusal still fires.
func TestSelectRefusesDirToFileOverDeepNestedGit(t *testing.T) {
	f := nestedGitFixtureWith(t, plantDeepNestedGit)
	setupSubToFileRun(t, f, "run-a")

	_, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-w7d")
	b, rerr := os.ReadFile(filepath.Join(f.parentDir, "sub", "inner", ".git", "HEAD"))
	if rerr != nil || string(b) != "ref: refs/heads/main\n" {
		t.Fatalf("sub/inner/.git/HEAD after the verb = %q, %v — the deeply excluded entry was destroyed", b, rerr)
	}
	assertRefusalLeavesNoTrace(t, f, "run-a", err)
}

// TestApplyRefusesDirToFileOverNestedGit (PR #7 wave 7): the
// refusal lives in the shared materializer, so Apply — whose
// union tree reaches the same write unit through the disjoint
// composition — refuses identically, before the parent is
// touched.
func TestApplyRefusesDirToFileOverNestedGit(t *testing.T) {
	f := nestedGitFixtureWith(t, plantNestedGitDir)
	setupSubToFileRun(t, f, "run-a")
	preHash := hashTree(t, f.parentDir)

	_, err := Apply(f.ctx, "parent", f.parentDir, "run-a", "op-w7a")
	assertNestedGitIntact(t, f)
	assertRefusalLeavesNoTrace(t, f, "run-a", err)
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after refusal = %s, want pre-verb %s", got, preHash)
	}
}

// TestSelectDirToFileWithoutExcludedContent (PR #7 wave 7,
// control): a directory→file replacement whose directory holds
// only inventoried content still settles — the refusal is
// keyed to the excluded content, not to the replacement shape.
func TestSelectDirToFileWithoutExcludedContent(t *testing.T) {
	f := nestedGitFixtureWith(t, plantPlainSub)
	setupSubToFileRun(t, f, "run-a")

	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-w7c"); err != nil {
		t.Fatalf("Select over a fully inventoried directory: %v", err)
	}
	expected := filepath.Join(f.scratch, "expected")
	journalWriteFile(t, filepath.Join(expected, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(expected, "sub"), "now a file\n", 0o644)
	if got, want := hashTree(t, f.parentDir), hashTree(t, expected); got != want {
		t.Fatalf("parent walk hash after Select = %s, want %s", got, want)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after Select = %d, want exactly 1", len(facts))
	}
}
