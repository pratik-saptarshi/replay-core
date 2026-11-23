package settle

// Parent-path ancestor tests (PR #11 Sourcery re-review,
// finding F2): rollback's root checks judge the journaled
// parent path's FINAL component (parentRootReplaced) and,
// since the identity fix, the directory it names — but no
// check judged the path's ANCESTOR components. The recorded
// path was resolved — absolute and symlink-free — when the
// journal was written, so a symlink standing at any
// component of it at recovery time is necessarily a
// post-record substitution: an ancestor directory moved
// aside and a symlink placed at its name. Every path
// rollback computes then resolves through the link into
// whatever tree it names today — a tree that can be
// repointed at will — while the component guards, which
// start their walks INSIDE the parent, never see it.
//
// The check is a component-wise Lstat walk of the recorded
// path from the filesystem root, following nothing. It
// cannot misfire on the legitimate shape — a caller whose
// own spelling of the parent passed through a symlink —
// because resolution at the verb's entry means the journal
// only ever records the resolved form, whose components
// were real directories when recorded.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyScratchTree duplicates the fixture's scratch tree at
// dst — files byte-for-byte with their modes, symlinks as
// symlinks — the copy the ancestor-swap fixtures point
// their substituted symlink at.
func copyScratchTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, info.Mode().Perm())
		default:
			return nil
		}
	})
	if err != nil {
		t.Fatalf("copy scratch tree %s -> %s: %v", src, dst, err)
	}
}

// crashSelectForAncestorTests leaves a crashed Select's
// state on f: the run's new bytes landed in the parent, the
// journal and preimages durable.
func crashSelectForAncestorTests(t *testing.T, f *journalFixture, op string) {
	t.Helper()
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "run mod\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)
	runCrashed(t, 1, func() {
		_, _ = Select(f.ctx, "parent", f.parentDir, "run-a", op)
	})
	if b, err := os.ReadFile(filepath.Join(f.parentDir, "mod.txt")); err != nil || string(b) != "run mod\n" {
		t.Fatalf("parent mod.txt after crash = %q, %v; want the landed bytes", b, err)
	}
}

// TestRecoverRefusesAncestorSymlinkToCopy: the fixture's
// scratch directory — a proper ancestor of the journaled
// parent — is moved aside and replaced by a symlink to a
// COPY of the whole scratch tree. Rollback must refuse at
// the ancestor check, naming the symlinked component, and
// leave the copy, the original, and the journal untouched.
func TestRecoverRefusesAncestorSymlinkToCopy(t *testing.T) {
	f := newJournalFixture(t)
	crashSelectForAncestorTests(t, f, "op-anc")

	copyDir := f.scratch + "-copy"
	origDir := f.scratch + "-orig"
	t.Cleanup(func() {
		_ = os.RemoveAll(copyDir)
		_ = os.RemoveAll(origDir)
	})
	copyScratchTree(t, f.scratch, copyDir)
	copyHash := hashTree(t, filepath.Join(copyDir, "parent"))
	if err := os.Rename(f.scratch, origDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(copyDir, f.scratch); err != nil {
		t.Fatal(err)
	}

	err := Recover(f.ctx, "parent")
	if err == nil {
		t.Fatal("Recover through an ancestor symlink succeeded; want a descriptive refusal")
	}
	if !strings.Contains(err.Error(), "symlink") || !strings.Contains(err.Error(), f.scratch) {
		t.Fatalf("Recover refusal = %v, want it to name the symlinked ancestor component %s", err, f.scratch)
	}
	if got := hashTree(t, filepath.Join(copyDir, "parent")); got != copyHash {
		t.Fatalf("copy parent walk hash after refused Recover = %s, want %s (rollback resolved through the ancestor symlink into the copy)", got, copyHash)
	}
	if b, rerr := os.ReadFile(filepath.Join(origDir, "parent", "mod.txt")); rerr != nil || string(b) != "run mod\n" {
		t.Fatalf("original parent mod.txt = %q, %v; want its landed bytes untouched", b, rerr)
	}
	if _, serr := os.Stat(journalPath(f.root, "parent")); serr != nil {
		t.Fatalf("journal after refused Recover: %v, want still present as evidence", serr)
	}
}

// TestRecoverRefusesAncestorSymlinkToOriginal: the ancestor
// is replaced by a symlink pointing back at the ORIGINAL
// directory itself — the recorded path still resolves to
// the very tree the settlement wrote through, so the form
// checks pass and the identity check passes, and only the
// ancestor walk can see the substitution. The recorded
// path's meaning must not depend on a link that can be
// repointed: rollback refuses, and the journal stays.
func TestRecoverRefusesAncestorSymlinkToOriginal(t *testing.T) {
	f := newJournalFixture(t)
	crashSelectForAncestorTests(t, f, "op-anc")

	origDir := f.scratch + "-orig"
	t.Cleanup(func() { _ = os.RemoveAll(origDir) })
	if err := os.Rename(f.scratch, origDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(origDir, f.scratch); err != nil {
		t.Fatal(err)
	}

	err := Recover(f.ctx, "parent")
	if err == nil {
		t.Fatal("Recover through an ancestor symlink to the original tree succeeded; want a descriptive refusal (the recorded path was symlink-free when written)")
	}
	if !strings.Contains(err.Error(), "symlink") || !strings.Contains(err.Error(), f.scratch) {
		t.Fatalf("Recover refusal = %v, want it to name the symlinked ancestor component %s", err, f.scratch)
	}
	if b, rerr := os.ReadFile(filepath.Join(origDir, "parent", "mod.txt")); rerr != nil || string(b) != "run mod\n" {
		t.Fatalf("original parent mod.txt = %q, %v; want its landed bytes untouched (rollback ran through the link)", b, rerr)
	}
	if _, serr := os.Stat(journalPath(f.root, "parent")); serr != nil {
		t.Fatalf("journal after refused Recover: %v, want still present as evidence", serr)
	}
}

// TestSelectThroughSymlinkedAncestorRecovers is the
// control: the CALLER spells the parent through a symlinked
// ancestor at write time. Resolution at the verb's entry
// records the resolved form in the journal, so the
// settlement and its recovery proceed end-to-end exactly as
// for a directly spelled parent — the ancestor walk judges
// the recorded path, whose components are real directories,
// never the caller's spelling.
func TestSelectThroughSymlinkedAncestorRecovers(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	linkParent := filepath.Join(f.scratch, "linkparent")
	if err := os.Symlink(f.parentDir, linkParent); err != nil {
		t.Fatal(err)
	}

	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "run mod\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)

	runCrashed(t, 1, func() {
		_, _ = Select(f.ctx, "parent", linkParent, "run-a", "op-link")
	})

	j, err := readJournal(f.root, "parent")
	if err != nil || j == nil {
		t.Fatalf("read crashed journal: journal = %v, err = %v", j, err)
	}
	if j.ParentDir != f.parentDir {
		t.Fatalf("journal parent_dir = %q, want the resolved form %q (the caller's symlinked spelling must never be recorded)", j.ParentDir, f.parentDir)
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover of a settlement written through a symlinked spelling: %v", err)
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
}
