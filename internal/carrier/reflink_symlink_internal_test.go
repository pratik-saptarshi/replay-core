//go:build linux

package carrier

// Symlinked base root, reflink half (PR #7 wave 6): the
// reflink driver's cloneTree shares the copy driver's
// WalkDir discipline, so a symlinked base root built an empty
// view there too — no clone calls at all, and a marker
// claiming the base over nothing. FICLONE is refused on this
// sandbox's filesystems, so the loop is exercised against the
// byte-copying stub, exactly as in reflink_internal_test.go:
// what is pinned is which tree the walk visits and which
// sources the clones are called on. After the root law
// (Materialize resolves the base root before dispatch), every
// clone source stands under the RESOLVED base — never under
// the caller's symlink spelling.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReflinkMaterializeSymlinkedBaseWithStubbedClones: a
// layer materialized from a symlinked base root clones the
// real tree's files, from resolved source paths.
func TestReflinkMaterializeSymlinkedBaseWithStubbedClones(t *testing.T) {
	base := t.TempDir()
	writeReflinkFixtureFile(t, filepath.Join(base, "top.txt"), "top level\n", 0o644)
	writeReflinkFixtureFile(t, filepath.Join(base, "sub", "run.sh"), "#!/bin/sh\necho run\n", 0o755)
	writeReflinkFixtureFile(t, filepath.Join(base, "sub", "deep", "notes.md"), "notes\n", 0o644)
	if err := os.Symlink("top.txt", filepath.Join(base, "top-link")); err != nil {
		t.Fatalf("symlink in base: %v", err)
	}
	link := filepath.Join(t.TempDir(), "base-link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatalf("symlink to base: %v", err)
	}

	var calls []string
	installCloneStub(t, stubCloneFunc(&calls, "", nil))

	root := filepath.Join(t.TempDir(), "layer")
	layer, err := Materialize(Selection{Driver: DriverReflink}, link, "tree-v1", root)
	if err != nil {
		t.Fatalf("Materialize through a symlinked base with stubbed clones: %v", err)
	}
	if len(calls) != 3 {
		t.Errorf("clone calls = %v, want exactly one per regular file of the base (3)", calls)
	}
	for _, src := range calls {
		if !strings.HasPrefix(src, base+string(os.PathSeparator)) {
			t.Errorf("clone called on %q, which is not under the resolved base %q", src, base)
		}
	}
	assertClonedTreeEqual(t, base, layer.Dir)
	if err := layer.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}
