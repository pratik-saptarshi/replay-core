package snapshot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// TestSnapshotThroughSymlinkedWorkspaceRoot (panel P3): a
// workspace root reached through a symlink used to fail during
// snapshotting with a raw git error — the walk visited the link
// itself as the workspace's single entry and the tree build
// choked on it. The root is now resolved once at Snapshot time
// and the snapshot is taken at the real path.
func TestSnapshotThroughSymlinkedWorkspaceRoot(t *testing.T) {
	real := t.TempDir()
	writeFile(t, filepath.Join(real, "hello.txt"), "hello\n")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit, err := s.Snapshot(link, map[string]string{"run": "symlinked-root"})
	if err != nil {
		t.Fatalf("Snapshot through symlinked root: %v", err)
	}
	manifest := git(t, dir, "cat-file", "-p", commit+":meta/manifest.json")
	if !strings.Contains(manifest, "hello.txt") {
		t.Fatalf("manifest does not list hello.txt: %s", manifest)
	}
	if content := git(t, dir, "cat-file", "-p", commit+":workspace/hello.txt"); content != "hello" {
		t.Fatalf("workspace/hello.txt = %q, want %q", content, "hello")
	}
}
