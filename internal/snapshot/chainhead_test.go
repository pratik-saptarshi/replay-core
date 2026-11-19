package snapshot_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// The chain-head validation suite (panel W3-6 and W3-7): Open
// adopts refs/vcscore/ops/latest as the chain head after
// validateChainHead, but the meta/snapshot.json leg was an
// existence check only — garbage text, a tree, a gitlink, and a
// symlink all passed — and the manifest leg stripped the first
// line without checking it was the canonical prefix and did no
// per-entry validation, so a manifest naming a nonexistent blob
// passed Open and then made every later Snapshot fail
// permanently through the reuse law. Both legs now validate
// content: the metadata must parse as the shape the writer
// emits, and the manifest must decode under the canonical law
// with every entry well-formed and every named blob present in
// the repository as a blob.

// seedSnapshotStore opens a fresh store and takes one real
// snapshot, returning the repo dir and the snapshot commit.
func seedSnapshotStore(t *testing.T) (string, string) {
	t.Helper()
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello\n")
	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit, err := s.Snapshot(ws, map[string]string{"run": "doctor"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return dir, commit
}

// doctorHead rebuilds base's root tree with meta/ assembled from
// the given mktree input lines (base's workspace subtree is kept
// verbatim), commits the result, and points
// refs/vcscore/ops/latest at the doctored commit.
func doctorHead(t *testing.T, dir, base string, metaLines []string) {
	t.Helper()
	workspaceTree := git(t, dir, "rev-parse", base+":workspace")
	metaTree := gitInput(t, dir, strings.Join(metaLines, "\n")+"\n", "mktree")
	rootTree := gitInput(t, dir,
		"040000 tree "+metaTree+"\tmeta\n"+
			"040000 tree "+workspaceTree+"\tworkspace\n",
		"mktree")
	doctored := git(t, dir,
		"-c", "user.name=replay-core-test", "-c", "user.email=replay-core-test@localhost",
		"commit-tree", rootTree, "-m", "doctored")
	git(t, dir, "update-ref", "refs/vcscore/ops/latest", doctored)
}

func TestOpenRejectsDoctoredSnapshotMeta(t *testing.T) {
	cases := map[string]func(t *testing.T, dir, base string) string{
		"garbage text": func(t *testing.T, dir, base string) string {
			blob := gitInput(t, dir, "garbage\nnot snapshot metadata\n", "hash-object", "-w", "--stdin")
			return "100644 blob " + blob + "\tsnapshot.json"
		},
		"a tree": func(t *testing.T, dir, base string) string {
			tree := git(t, dir, "rev-parse", base+":workspace")
			return "040000 tree " + tree + "\tsnapshot.json"
		},
		"a gitlink": func(t *testing.T, dir, base string) string {
			return "160000 commit " + base + "\tsnapshot.json"
		},
		"a symlink": func(t *testing.T, dir, base string) string {
			blob := gitInput(t, dir, "/some/target", "hash-object", "-w", "--stdin")
			return "120000 blob " + blob + "\tsnapshot.json"
		},
	}
	for name, snapshotLine := range cases {
		t.Run(name, func(t *testing.T) {
			dir, base := seedSnapshotStore(t)
			manifestBlob := git(t, dir, "rev-parse", base+":meta/manifest.json")
			doctorHead(t, dir, base, []string{
				"100644 blob " + manifestBlob + "\tmanifest.json",
				snapshotLine(t, dir, base),
			})
			_, err := snapshot.Open(dir)
			if err == nil {
				t.Fatalf("Open accepted a chain head whose meta/snapshot.json is %s; want a loud failure", name)
			}
			if !strings.Contains(err.Error(), "refs/vcscore/ops/latest") {
				t.Fatalf("Open error = %q, want it to name the chain-head ref", err)
			}
			if !strings.Contains(err.Error(), "snapshot.json") {
				t.Fatalf("Open error = %q, want it to name meta/snapshot.json as the problem", err)
			}
		})
	}
}

func TestOpenRejectsDoctoredManifest(t *testing.T) {
	cases := map[string]func(t *testing.T, dir, base string) string{
		"prefix plus null": func(t *testing.T, dir, base string) string {
			return gitInput(t, dir, "replaycore.kernel.canonical.v1\nnull", "hash-object", "-w", "--stdin")
		},
		"prefix-less JSON": func(t *testing.T, dir, base string) string {
			blob := git(t, dir, "rev-parse", base+":workspace/hello.txt")
			doc := `{"hello.txt":{"blob":"` + blob + `","kind":"file","mtime":1,"size":6}}`
			return gitInput(t, dir, doc, "hash-object", "-w", "--stdin")
		},
		"entry naming a nonexistent blob": func(t *testing.T, dir, base string) string {
			doc := "replaycore.kernel.canonical.v1\n" +
				`{"hello.txt":{"blob":"1111111111111111111111111111111111111111","kind":"file","mtime":1,"size":6}}`
			return gitInput(t, dir, doc, "hash-object", "-w", "--stdin")
		},
	}
	for name, manifestBlob := range cases {
		t.Run(name, func(t *testing.T) {
			dir, base := seedSnapshotStore(t)
			snapshotBlob := git(t, dir, "rev-parse", base+":meta/snapshot.json")
			doctorHead(t, dir, base, []string{
				"100644 blob " + manifestBlob(t, dir, base) + "\tmanifest.json",
				"100644 blob " + snapshotBlob + "\tsnapshot.json",
			})
			_, err := snapshot.Open(dir)
			if err == nil {
				t.Fatalf("Open accepted a chain head whose manifest is %s; want a loud failure", name)
			}
			if !strings.Contains(err.Error(), "refs/vcscore/ops/latest") {
				t.Fatalf("Open error = %q, want it to name the chain-head ref", err)
			}
			if !strings.Contains(err.Error(), "manifest") {
				t.Fatalf("Open error = %q, want it to name the manifest as the problem", err)
			}
		})
	}
}
