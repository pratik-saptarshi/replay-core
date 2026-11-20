package snapshot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// The empty-workspace suite (Codex P2 on PR #2): when the
// workspace is empty — or holds only empty directories — the
// walk stages nothing, and the snapshot's root tree came out
// holding only meta/, violating the root-tree law (exactly
// {workspace, meta}) and breaking consumers that resolve the
// workspace subtree. The workspace/ subtree now exists in every
// snapshot, as git's empty tree when there are no entries. The
// walk law stands: empty directories leave no entries UNDER
// workspace/; only the subtree itself is guaranteed.

const emptyTreeID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

func rootNames(t *testing.T, dir, commit string) map[string]bool {
	t.Helper()
	root := git(t, dir, "ls-tree", "--format=%(path)", commit)
	names := map[string]bool{}
	for _, line := range strings.Split(root, "\n") {
		names[line] = true
	}
	return names
}

func TestSnapshotEmptyWorkspaceKeepsSubtree(t *testing.T) {
	ws := t.TempDir() // genuinely empty
	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit, err := s.Snapshot(ws, map[string]string{"run": "empty"})
	if err != nil {
		t.Fatalf("Snapshot of an empty workspace: %v", err)
	}
	if names := rootNames(t, dir, commit); len(names) != 2 || !names["workspace"] || !names["meta"] {
		t.Fatalf("root tree entries = %v, want exactly {workspace, meta}", names)
	}
	if got := git(t, dir, "rev-parse", commit+":workspace"); got != emptyTreeID {
		t.Fatalf("workspace subtree = %s, want git's empty tree %s", got, emptyTreeID)
	}
	manifest := git(t, dir, "cat-file", "-p", commit+":meta/manifest.json")
	if !strings.Contains(manifest, "{}") {
		t.Fatalf("manifest of an empty workspace = %s, want an empty object", manifest)
	}

	// The chain continues from an empty-workspace head: a
	// reopened store adopts it and snapshots again.
	s2, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("reopen over an empty-workspace head: %v", err)
	}
	commit2, err := s2.Snapshot(ws, map[string]string{"run": "empty-2"})
	if err != nil {
		t.Fatalf("second Snapshot of an empty workspace: %v", err)
	}
	if got := git(t, dir, "cat-file", "-p", commit2); !strings.Contains(got, "parent "+commit) {
		t.Fatalf("second commit has no parent %s:\n%s", commit, got)
	}
}

func TestSnapshotOnlyEmptyDirsKeepsSubtree(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "sub", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit, err := s.Snapshot(ws, map[string]string{"run": "empty-dirs"})
	if err != nil {
		t.Fatalf("Snapshot of a workspace with only empty directories: %v", err)
	}
	if names := rootNames(t, dir, commit); len(names) != 2 || !names["workspace"] || !names["meta"] {
		t.Fatalf("root tree entries = %v, want exactly {workspace, meta}", names)
	}
	if got := git(t, dir, "rev-parse", commit+":workspace"); got != emptyTreeID {
		t.Fatalf("workspace subtree = %s, want git's empty tree %s", got, emptyTreeID)
	}
	// No entries under workspace/: the empty directories are
	// represented nowhere, per the walk law.
	tree := git(t, dir, "ls-tree", "-r", "--format=%(path)", commit)
	if strings.Contains(tree, "sub") {
		t.Fatalf("empty directories leaked into the tree:\n%s", tree)
	}
}
