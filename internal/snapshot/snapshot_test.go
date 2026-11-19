package snapshot_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// git runs a plumbing command against the store's bare repo and
// returns its trimmed stdout, failing the test on any error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(full, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotDualTree(t *testing.T) { // AC-F4-1
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello\n")
	writeFile(t, filepath.Join(ws, "sub", "nested.txt"), "nested\n")
	if err := os.MkdirAll(filepath.Join(ws, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit, err := s.Snapshot(ws, map[string]string{"run": "synthetic-1"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if got := git(t, dir, "cat-file", "-t", commit); got != "commit" {
		t.Fatalf("cat-file -t %s = %q, want commit", commit, got)
	}

	// The root tree holds exactly the workspace/ and meta/ subtrees.
	root := git(t, dir, "ls-tree", "--format=%(path)", commit)
	names := map[string]bool{}
	for _, line := range strings.Split(root, "\n") {
		names[line] = true
	}
	if len(names) != 2 || !names["workspace"] || !names["meta"] {
		t.Fatalf("root tree entries = %v, want exactly {workspace, meta}", names)
	}

	if got := git(t, dir, "cat-file", "-p", commit+":workspace/hello.txt"); got != "hello" {
		t.Fatalf("workspace/hello.txt blob = %q, want %q", got, "hello")
	}
	if got := git(t, dir, "cat-file", "-p", commit+":workspace/sub/nested.txt"); got != "nested" {
		t.Fatalf("workspace/sub/nested.txt blob = %q, want %q", got, "nested")
	}

	snapJSON := git(t, dir, "cat-file", "-p", commit+":meta/snapshot.json")
	if !strings.Contains(snapJSON, "synthetic-1") {
		t.Fatalf("meta/snapshot.json = %s, want it to contain the caller meta value %q", snapJSON, "synthetic-1")
	}

	manifestJSON := git(t, dir, "cat-file", "-p", commit+":meta/manifest.json")
	// The manifest is canonical-kernel bytes: the law's prefix tag
	// on the first line, then the canonical JSON document.
	manifestDoc := manifestJSON
	if i := strings.Index(manifestDoc, "\n"); i >= 0 {
		manifestDoc = manifestDoc[i+1:]
	}
	var manifest map[string]struct {
		Blob string `json:"blob"`
		Kind string `json:"kind"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal([]byte(manifestDoc), &manifest); err != nil {
		t.Fatalf("meta/manifest.json does not parse: %v\n%s", err, manifestJSON)
	}
	if len(manifest) != 2 {
		t.Fatalf("manifest lists %d entries, want 2: %s", len(manifest), manifestJSON)
	}
	for _, rel := range []string{"hello.txt", "sub/nested.txt"} {
		e, ok := manifest[rel]
		if !ok {
			t.Fatalf("manifest missing %q: %s", rel, manifestJSON)
		}
		if e.Kind != "file" || e.Blob == "" {
			t.Fatalf("manifest[%q] = %+v, want a file entry with a blob", rel, e)
		}
	}
	if manifest["hello.txt"].Size != int64(len("hello\n")) {
		t.Fatalf("manifest[hello.txt].Size = %d, want %d", manifest["hello.txt"].Size, len("hello\n"))
	}

	// The empty directory is represented nowhere: not in the tree,
	// not in the manifest (Git trees cannot represent empty dirs).
	tree := git(t, dir, "ls-tree", "-r", "--format=%(path)", commit)
	if strings.Contains(tree, "emptydir") {
		t.Fatalf("empty dir leaked into the tree:\n%s", tree)
	}
	if strings.Contains(manifestJSON, "emptydir") {
		t.Fatalf("empty dir leaked into the manifest:\n%s", manifestJSON)
	}

	// The chain continues: a second snapshot's parent is the first
	// commit, and a reopened store continues from it.
	commit2, err := s.Snapshot(ws, map[string]string{"run": "synthetic-2"})
	if err != nil {
		t.Fatalf("Snapshot 2: %v", err)
	}
	if got := git(t, dir, "cat-file", "-p", commit2); !strings.Contains(got, "parent "+commit) {
		t.Fatalf("second commit has no parent %s:\n%s", commit, got)
	}
	s2, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	commit3, err := s2.Snapshot(ws, map[string]string{"run": "synthetic-3"})
	if err != nil {
		t.Fatalf("Snapshot after reopen: %v", err)
	}
	if got := git(t, dir, "cat-file", "-p", commit3); !strings.Contains(got, "parent "+commit2) {
		t.Fatalf("post-reopen commit has no parent %s:\n%s", commit2, got)
	}
	if got := git(t, dir, "rev-parse", "refs/vcscore/ops/latest"); got != commit3 {
		t.Fatalf("refs/vcscore/ops/latest = %s, want %s", got, commit3)
	}
}

func TestSnapshotStoresSymlinksAsLinks(t *testing.T) { // Review Focus 4a
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	writeFile(t, target, "outside bytes\n")

	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "real.txt"), "real\n")
	if err := os.Symlink(target, filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit, err := s.Snapshot(ws, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	entry := git(t, dir, "ls-tree", commit+":workspace", "link.txt")
	// ls-tree line: "<mode> blob <sha>\t<path>"
	fields := strings.Fields(entry)
	if len(fields) < 1 || fields[0] != "120000" {
		t.Fatalf("link.txt tree entry = %q, want mode 120000", entry)
	}
	blob := git(t, dir, "cat-file", "-p", commit+":workspace/link.txt")
	if blob != target {
		t.Fatalf("symlink blob = %q, want the link target %q (never the target's bytes)", blob, target)
	}
}

func TestSnapshotFailsOnUnreadableFile(t *testing.T) { // Review Focus 4b
	// The suite runs as uid 0 in this sandbox, where permission bits
	// do not block reads — so the unreadable case is a file whose
	// absolute path exceeds the 4096-byte PATH_MAX: opening it by
	// its walked path fails even for root.
	base := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	// Nest level by level with relative names only, so no single
	// syscall ever resolves the (eventually over-long) absolute
	// path during fixture construction.
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	cur := base
	name := strings.Repeat("d", 100)
	for len(cur)+1+len("deep.txt") <= 4096 {
		if err := os.Mkdir(name, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(name); err != nil {
			t.Fatal(err)
		}
		cur = filepath.Join(cur, name)
	}
	// Create the file by its short relative name while chdir'd into
	// its directory; its absolute path is now over PATH_MAX.
	if err := os.WriteFile("deep.txt", []byte("deep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	deepPath := filepath.Join(cur, "deep.txt")
	if len(deepPath) <= 4096 {
		t.Fatalf("fixture path is %d bytes, want > 4096", len(deepPath))
	}

	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = s.Snapshot(base, nil)
	if err == nil {
		t.Fatal("Snapshot over a workspace with an unreadable file succeeded, want an error naming the path")
	}
	// The walk opens entries by their walked absolute paths, so it
	// fails at the first entry whose path crosses PATH_MAX — an
	// ancestor directory of deep.txt — and must name that path.
	if !strings.Contains(err.Error(), base) || !strings.Contains(err.Error(), "file name too long") {
		t.Fatalf("Snapshot error = %q, want it to name the path it could not open", err)
	}
}

func TestSnapshotRejectsSpecialFiles(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "real.txt"), "real\n")
	fifo := filepath.Join(ws, "pipe.fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = s.Snapshot(ws, nil)
	if err == nil {
		t.Fatal("Snapshot over a workspace containing a FIFO succeeded, want an error naming the path")
	}
	if !strings.Contains(err.Error(), "pipe.fifo") {
		t.Fatalf("Snapshot error = %q, want it to name the FIFO path", err)
	}
}
