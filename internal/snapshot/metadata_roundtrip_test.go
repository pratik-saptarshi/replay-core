package snapshot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// The generated-metadata round-trip suite (Codex P2 on PR #2):
// the snapshot writer marshals meta/manifest.json and
// meta/snapshot.json with canonical.MarshalKernel, which
// preserves raw bytes, but the readers canonical-decode both
// documents and the decoder rejects invalid UTF-8. A workspace
// file whose name carries invalid UTF-8 bytes — legal on Unix —
// therefore produced a manifest that published cleanly and then
// failed Open and every later Snapshot, permanently, even after
// the file was removed; invalid UTF-8 in the caller's metadata
// map did the same to meta/snapshot.json. Snapshot now
// round-trips both generated documents through their readers
// before any ref is published, and refuses without moving the
// chain when they do not parse.

// badName is a filename carrying an invalid UTF-8 byte.
const badName = "bad-\xff"

func TestSnapshotRejectsInvalidUTF8Filename(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, badName), "bad name\n")
	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := s.Snapshot(ws, nil); err == nil {
		t.Fatal("Snapshot over a workspace with an invalid-UTF-8 filename succeeded; " +
			"the manifest it publishes cannot be decoded by its own reader")
	} else if !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("Snapshot error = %q, want it to name the manifest document", err)
	}

	// Nothing was published: no refs exist under refs/vcscore/ops/.
	if got := git(t, dir, "for-each-ref", "refs/vcscore/ops/"); got != "" {
		t.Fatalf("refs exist after the refused snapshot:\n%s", got)
	}

	// With the file removed the same store snapshots cleanly,
	// and the result is the chain's first commit — the refused
	// call created nothing.
	if err := os.Remove(filepath.Join(ws, badName)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "good.txt"), "good\n")
	commit, err := s.Snapshot(ws, nil)
	if err != nil {
		t.Fatalf("Snapshot after removing the bad file: %v", err)
	}
	if got := git(t, dir, "cat-file", "-p", commit); strings.Contains(got, "parent ") {
		t.Fatalf("first successful snapshot has a parent — the refused call created a commit:\n%s", got)
	}
}

func TestSnapshotInvalidUTF8FilenameLeavesHeadPut(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello\n")
	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first, err := s.Snapshot(ws, map[string]string{"run": "round-trip"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	writeFile(t, filepath.Join(ws, badName), "bad name\n")
	if _, err := s.Snapshot(ws, nil); err == nil {
		t.Fatal("Snapshot with an invalid-UTF-8 filename succeeded on an existing chain")
	}
	if got := git(t, dir, "rev-parse", "refs/vcscore/ops/latest"); got != first {
		t.Fatalf("chain head moved after the refused snapshot: %s -> %s", first, got)
	}

	// The store still reopens on the unmoved head and chains
	// onto it once the file is gone.
	if err := os.Remove(filepath.Join(ws, badName)); err != nil {
		t.Fatal(err)
	}
	s2, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	second, err := s2.Snapshot(ws, nil)
	if err != nil {
		t.Fatalf("Snapshot after reopen: %v", err)
	}
	if got := git(t, dir, "cat-file", "-p", second); !strings.Contains(got, "parent "+first) {
		t.Fatalf("snapshot after reopen does not chain onto %s:\n%s", first, got)
	}
}

func TestSnapshotRejectsInvalidUTF8Metadata(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello\n")
	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first, err := s.Snapshot(ws, map[string]string{"run": "round-trip"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	for name, meta := range map[string]map[string]string{
		"value": {"note": "bad-\xff"},
		"key":   {"bad-\xff": "note"},
	} {
		if _, err := s.Snapshot(ws, meta); err == nil {
			t.Fatalf("Snapshot with invalid UTF-8 in a metadata %s succeeded; "+
				"the meta/snapshot.json it publishes cannot be decoded by its own reader", name)
		} else if !strings.Contains(err.Error(), "snapshot") {
			t.Fatalf("Snapshot error = %q, want it to name the snapshot metadata document", err)
		}
	}
	if got := git(t, dir, "rev-parse", "refs/vcscore/ops/latest"); got != first {
		t.Fatalf("chain head moved after the refused snapshots: %s -> %s", first, got)
	}
}
