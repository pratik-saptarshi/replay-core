package snapshot_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// The .git-entry suite (Codex P1 on PR #2): the walk skipped
// directories named .git, but in a linked worktree — or a
// workspace containing a checked-out submodule — .git is a
// regular file holding a "gitdir:" pointer. The file reached
// staging, update-index refused "workspace/.git", and such
// workspaces could not be snapshotted at all. Any entry named
// .git is now skipped whatever its filesystem type.

func TestSnapshotSkipsGitFile(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello\n")
	writeFile(t, filepath.Join(ws, ".git"), "gitdir: /elsewhere/.git/worktrees/ws\n")

	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit, err := s.Snapshot(ws, map[string]string{"run": "gitfile"})
	if err != nil {
		t.Fatalf("Snapshot over a workspace with a .git file: %v", err)
	}
	tree := git(t, dir, "ls-tree", "-r", "--format=%(path)", commit+":workspace")
	if strings.Contains(tree, ".git") {
		t.Fatalf(".git leaked into the workspace tree:\n%s", tree)
	}
	if !strings.Contains(tree, "hello.txt") {
		t.Fatalf("workspace tree is missing hello.txt:\n%s", tree)
	}
	manifest := git(t, dir, "cat-file", "-p", commit+":meta/manifest.json")
	if strings.Contains(manifest, ".git") {
		t.Fatalf(".git leaked into the manifest:\n%s", manifest)
	}
	cmd := exec.Command("git", "-C", dir, "cat-file", "-e", commit+":workspace/.git")
	if err := cmd.Run(); err == nil {
		t.Fatal("workspace/.git exists in the snapshot tree; want it absent")
	}
}

func TestSnapshotSkipsGitDirectory(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello\n")
	writeFile(t, filepath.Join(ws, ".git", "HEAD"), "ref: refs/heads/main\n")

	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit, err := s.Snapshot(ws, map[string]string{"run": "gitdir"})
	if err != nil {
		t.Fatalf("Snapshot over a workspace with a .git directory: %v", err)
	}
	tree := git(t, dir, "ls-tree", "-r", "--format=%(path)", commit+":workspace")
	if strings.Contains(tree, ".git") {
		t.Fatalf(".git leaked into the workspace tree:\n%s", tree)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git", "HEAD")); err != nil {
		t.Fatalf("the workspace's own .git was disturbed: %v", err)
	}
}
