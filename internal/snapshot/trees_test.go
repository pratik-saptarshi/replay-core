package snapshot_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// lsTreeGroundTruth parses `git ls-tree -r <commit>:workspace`
// into the WorkspaceFiles map the read must reproduce exactly:
// blob IDs, git tree modes, and symlink flags from git itself,
// not from the implementation under test.
func lsTreeGroundTruth(t *testing.T, dir, commit string) map[string]snapshot.WorkspaceEntry {
	t.Helper()
	out := git(t, dir, "ls-tree", "-r", commit+":workspace")
	want := map[string]snapshot.WorkspaceEntry{}
	if out == "" {
		return want
	}
	for _, line := range strings.Split(out, "\n") {
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("ls-tree line %q has no tab", line)
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			t.Fatalf("ls-tree line %q: want mode/type/oid", line)
		}
		want[path] = snapshot.WorkspaceEntry{
			Blob:    fields[2],
			Mode:    fields[0],
			Symlink: fields[0] == "120000",
		}
	}
	return want
}

// TestWorkspaceFilesAndExtract (plan Task 6, Step 1): the tree
// surface settlement computes over — WorkspaceFiles must equal
// the ls-tree ground truth (modes included: a mode-only change
// is a real change, never invisible), WorkspaceTreeID must name
// the commit's workspace subtree, and ExtractWorkspace must
// reproduce the tree byte-for-byte with symlinks as symlinks and
// the executable's mode intact.
func TestWorkspaceFilesAndExtract(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello\n")
	writeFile(t, filepath.Join(ws, "sub", "nested.txt"), "nested\n")
	if err := os.WriteFile(filepath.Join(ws, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(ws, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit, err := s.Snapshot(ws, map[string]string{"run": "trees-test"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	files, err := s.WorkspaceFiles(commit)
	if err != nil {
		t.Fatalf("WorkspaceFiles: %v", err)
	}
	if want := lsTreeGroundTruth(t, dir, commit); !reflect.DeepEqual(files, want) {
		t.Fatalf("WorkspaceFiles =\n%v\nwant ls-tree ground truth\n%v", files, want)
	}
	// The executable's mode is carried, not flattened.
	if got := files["run.sh"].Mode; got != "100755" {
		t.Fatalf("WorkspaceFiles[run.sh].Mode = %q, want 100755", got)
	}

	treeLine := git(t, dir, "ls-tree", commit, "workspace")
	fields := strings.Fields(strings.SplitN(treeLine, "\t", 2)[0])
	if len(fields) != 3 {
		t.Fatalf("ls-tree root line %q: want mode/type/oid", treeLine)
	}
	treeID, err := s.WorkspaceTreeID(commit)
	if err != nil {
		t.Fatalf("WorkspaceTreeID: %v", err)
	}
	if treeID != fields[2] {
		t.Fatalf("WorkspaceTreeID = %s, want %s (ls-tree)", treeID, fields[2])
	}

	// ExtractWorkspace into a fresh dir reproduces the tree.
	out := filepath.Join(t.TempDir(), "extracted")
	if err := s.ExtractWorkspace(commit, out); err != nil {
		t.Fatalf("ExtractWorkspace: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "hello.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("extracted hello.txt = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "sub", "nested.txt")); err != nil || string(b) != "nested\n" {
		t.Fatalf("extracted sub/nested.txt = %q, %v", b, err)
	}
	info, err := os.Lstat(filepath.Join(out, "link.txt"))
	if err != nil {
		t.Fatalf("lstat extracted link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("extracted link.txt mode = %v, want a symlink", info.Mode())
	}
	if target, err := os.Readlink(filepath.Join(out, "link.txt")); err != nil || target != "hello.txt" {
		t.Fatalf("extracted link target = %q, %v; want hello.txt", target, err)
	}
	info, err = os.Stat(filepath.Join(out, "run.sh"))
	if err != nil {
		t.Fatalf("stat extracted run.sh: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("extracted run.sh mode = %o, want 755 (mode preserved, not flattened)", info.Mode().Perm())
	}

	// ExtractWorkspace refuses a non-empty target.
	if err := s.ExtractWorkspace(commit, out); err == nil {
		t.Fatal("ExtractWorkspace into a non-empty dir succeeded, want a refusal")
	}

	// An empty-workspace commit: empty map, extract yields an
	// empty dir.
	emptyCommit, err := s.Snapshot(t.TempDir(), map[string]string{"run": "trees-empty"})
	if err != nil {
		t.Fatalf("Snapshot empty: %v", err)
	}
	files, err = s.WorkspaceFiles(emptyCommit)
	if err != nil {
		t.Fatalf("WorkspaceFiles(empty): %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("WorkspaceFiles(empty) = %v, want empty", files)
	}
	emptyOut := filepath.Join(t.TempDir(), "extracted-empty")
	if err := s.ExtractWorkspace(emptyCommit, emptyOut); err != nil {
		t.Fatalf("ExtractWorkspace(empty): %v", err)
	}
	entries, err := os.ReadDir(emptyOut)
	if err != nil {
		t.Fatalf("ReadDir extracted empty: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("extracted empty workspace has %d entries, want 0", len(entries))
	}
}
