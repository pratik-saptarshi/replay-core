package snapshot_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// objectCount returns the store repo's total object count
// (loose + packed) from git count-objects -v.
func objectCount(t *testing.T, dir string) int {
	t.Helper()
	out := git(t, dir, "count-objects", "-v")
	total := 0
	for _, line := range strings.Split(out, "\n") {
		for _, key := range []string{"count:", "in-pack:"} {
			if strings.HasPrefix(line, key) {
				n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, key)))
				if err != nil {
					t.Fatalf("count-objects line %q: %v", line, err)
				}
				total += n
			}
		}
	}
	return total
}

// workspaceTree returns the blob ID of every entry under the
// commit's workspace/ subtree, keyed by workspace-relative path.
func workspaceTree(t *testing.T, dir, commit string) map[string]string {
	t.Helper()
	out := git(t, dir, "ls-tree", "-r", commit+":workspace")
	entries := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		// line: "<mode> blob <sha>\t<path>"
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("ls-tree line %q has no path separator", line)
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			t.Fatalf("ls-tree line %q: want 3 fields before the path", line)
		}
		entries[path] = fields[2]
	}
	return entries
}

// manifestEntry returns the blob ID and mtime that the manifest
// of the given commit records for rel.
func manifestEntry(t *testing.T, dir, commit, rel string) (string, int64) {
	t.Helper()
	raw := git(t, dir, "cat-file", "-p", commit+":meta/manifest.json")
	// The manifest is canonical-kernel bytes: the law's prefix tag
	// on the first line, then the canonical JSON document.
	doc := raw
	if i := strings.Index(doc, "\n"); i >= 0 {
		doc = doc[i+1:]
	}
	var manifest map[string]struct {
		Blob  string `json:"blob"`
		Kind  string `json:"kind"`
		Mtime int64  `json:"mtime"`
		Size  int64  `json:"size"`
	}
	if err := json.Unmarshal([]byte(doc), &manifest); err != nil {
		t.Fatalf("meta/manifest.json of %s does not parse: %v\n%s", commit, err, raw)
	}
	e, ok := manifest[rel]
	if !ok {
		t.Fatalf("manifest of %s missing %q: %s", commit, rel, raw)
	}
	return e.Blob, e.Mtime
}

// TestSnapshotIncrementalReuseLaw is the discriminating test for
// AC-F4-2's reuse law: an entry whose (size, mtime) pair matches
// the previous snapshot's manifest reuses its blob SHA without the
// file being read or re-hashed. The object-count bound cannot
// discriminate reuse from rehashing (git is content-addressed, so
// rehashing unchanged bytes writes no new objects), so this test
// instead changes a file's bytes while preserving its size and
// mtime exactly: a rehashing build picks up the new bytes and
// fails; a build obeying the reuse law carries the OLD blob into
// the new snapshot and passes.
func TestSnapshotIncrementalReuseLaw(t *testing.T) { // AC-F4-2
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "stable.txt"), "stable\n")
	target := filepath.Join(ws, "target.txt")
	const original = "AAAAAAAAAAAAAAAA\n"
	writeFile(t, target, original)

	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit1, err := s.Snapshot(ws, nil)
	if err != nil {
		t.Fatalf("Snapshot 1: %v", err)
	}
	oldBlob, oldMtime := manifestEntry(t, dir, commit1, "target.txt")

	// Replace the file's bytes with different content of exactly
	// the same size, then restore the manifest-recorded mtime, so
	// the (size, mtime) reuse key is unchanged.
	const replacement = "BBBBBBBBBBBBBBBB\n"
	if len(replacement) != len(original) {
		t.Fatalf("fixture: replacement is %d bytes, original is %d", len(replacement), len(original))
	}
	if err := os.WriteFile(target, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(0, oldMtime).UTC()
	if err := os.Chtimes(target, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(original)) || info.ModTime().UnixNano() != oldMtime {
		t.Fatalf("fixture: target now has size %d, mtime %d; want size %d, mtime %d",
			info.Size(), info.ModTime().UnixNano(), len(original), oldMtime)
	}

	commit2, err := s.Snapshot(ws, nil)
	if err != nil {
		t.Fatalf("Snapshot 2: %v", err)
	}

	// The reuse law: snapshot 2's workspace tree and manifest must
	// carry the OLD blob for target.txt, even though the file's
	// bytes on disk are now the replacement's.
	if got := workspaceTree(t, dir, commit2)["target.txt"]; got != oldBlob {
		t.Fatalf("snapshot 2 workspace tree blob for target.txt = %s, want the reused %s", got, oldBlob)
	}
	if got, _ := manifestEntry(t, dir, commit2, "target.txt"); got != oldBlob {
		t.Fatalf("snapshot 2 manifest blob for target.txt = %s, want the reused %s", got, oldBlob)
	}
	if got := git(t, dir, "cat-file", "-p", commit2+":workspace/target.txt"); got != strings.TrimSpace(original) {
		t.Fatalf("snapshot 2 target.txt content = %q, want the original %q (the on-disk replacement must not be picked up)", got, original)
	}
}

func TestSnapshotIncrementalObjectBound(t *testing.T) { // AC-F4-2
	// Workspace of 1,000 small files.
	ws := t.TempDir()
	for i := 0; i < 1000; i++ {
		writeFile(t, filepath.Join(ws, fmt.Sprintf("f%04d.txt", i)),
			fmt.Sprintf("content of file %d\n", i))
	}

	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit1, err := s.Snapshot(ws, nil)
	if err != nil {
		t.Fatalf("Snapshot 1: %v", err)
	}
	before := objectCount(t, dir)

	// Modify exactly 1 file.
	const changed = "f0500.txt"
	if err := os.WriteFile(filepath.Join(ws, changed), []byte("changed content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit2, err := s.Snapshot(ws, nil)
	if err != nil {
		t.Fatalf("Snapshot 2: %v", err)
	}
	after := objectCount(t, dir)

	// The incremental build is O(changed): the second snapshot may
	// add only the changed blob, the rewritten trees, the meta
	// blobs, and the commit — with slack, at most 20 new objects.
	if delta := after - before; delta > 20 {
		t.Fatalf("snapshot 2 added %d objects (%d -> %d), want <= 20", delta, before, after)
	}

	// The two snapshots' workspace/ trees differ only in the
	// modified file's entry.
	tree1 := workspaceTree(t, dir, commit1)
	tree2 := workspaceTree(t, dir, commit2)
	if len(tree1) != 1000 || len(tree2) != 1000 {
		t.Fatalf("workspace trees hold %d and %d entries, want 1000 each", len(tree1), len(tree2))
	}
	var diffs []string
	for path, blob1 := range tree1 {
		blob2, ok := tree2[path]
		if !ok {
			t.Fatalf("snapshot 2 workspace tree is missing %q", path)
		}
		if blob1 != blob2 {
			diffs = append(diffs, path)
		}
	}
	if len(diffs) != 1 || diffs[0] != changed {
		t.Fatalf("workspace trees differ at %v, want exactly [%s]", diffs, changed)
	}
}
