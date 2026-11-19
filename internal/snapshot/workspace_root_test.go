package snapshot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// The workspace-root suite (panel W3-5 and W3-8): the root was
// resolved with filepath.Abs + EvalSymlinks and nothing else,
// so an empty path resolved to the process working directory —
// Snapshot("") captured whatever directory the process ran in
// under a valid commit — and a regular file as root died deep in
// the git plumbing with a raw Invalid path error. The root is
// now refused, descriptively and before any commit, when it is
// empty or not a directory.

func TestSnapshotRejectsEmptyWorkspaceRoot(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello\n")
	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit, err := s.Snapshot(ws, map[string]string{"run": "root-checks"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if _, err := s.Snapshot("", nil); err == nil {
		t.Fatal("Snapshot(\"\") succeeded — it must not snapshot the process working directory")
	} else if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("Snapshot(\"\") error = %q, want it to name the empty root", err)
	}
	if got := git(t, dir, "rev-parse", "refs/vcscore/ops/latest"); got != commit {
		t.Fatalf("chain head moved after the refused snapshot: %s -> %s", commit, got)
	}
}

func TestSnapshotRejectsRegularFileWorkspaceRoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile.txt")
	writeFile(t, file, "not a directory\n")
	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	_, err = s.Snapshot(file, nil)
	if err == nil {
		t.Fatal("Snapshot over a regular file succeeded; want a descriptive refusal")
	}
	if !strings.Contains(err.Error(), "not a directory") || !strings.Contains(err.Error(), file) {
		t.Fatalf("Snapshot error = %q, want it to name %s as not a directory", err, file)
	}

	// No commit was created: the first successful snapshot is
	// the chain's first commit, with no parent.
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello\n")
	commit, err := s.Snapshot(ws, nil)
	if err != nil {
		t.Fatalf("Snapshot after the refused call: %v", err)
	}
	if got := git(t, dir, "cat-file", "-p", commit); strings.Contains(got, "parent ") {
		t.Fatalf("first successful snapshot has a parent — the refused call created a commit:\n%s", got)
	}
}

func TestSnapshotRejectsSymlinkToRegularFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile.txt")
	writeFile(t, file, "not a directory\n")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = s.Snapshot(link, nil)
	if err == nil {
		t.Fatal("Snapshot through a symlink to a regular file succeeded; want a descriptive refusal")
	}
	if !strings.Contains(err.Error(), "not a directory") || !strings.Contains(err.Error(), link) {
		t.Fatalf("Snapshot error = %q, want it to name %s (the path as given) as not a directory", err, link)
	}
}
