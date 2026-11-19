package snapshot_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// setupStore opens a fresh store and takes one snapshot of a
// one-file workspace, returning the store, the repo dir, and the
// snapshot commit ID.
func setupStore(t *testing.T) (*snapshot.Store, string, string) {
	t.Helper()
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello\n")
	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commit, err := s.Snapshot(ws, map[string]string{"run": "refs-test"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return s, dir, commit
}

func TestForkIsZeroCopy(t *testing.T) { // AC-F4-3
	s, dir, commit := setupStore(t)

	before := objectCount(t, dir)
	if err := s.Fork("lane-a", commit); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if after := objectCount(t, dir); after != before {
		t.Fatalf("object count changed by Fork: before %d, after %d (fork must be one update-ref, zero new objects)", before, after)
	}

	if got := git(t, dir, "rev-parse", "refs/vcscore/scopes/lane-a"); got != commit {
		t.Fatalf("refs/vcscore/scopes/lane-a = %s, want %s", got, commit)
	}

	refs, err := s.ListScopes()
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	want := snapshot.ScopeRef{Kind: snapshot.RefScope, Name: "lane-a", CommitID: commit}
	found := false
	for _, r := range refs {
		if r == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListScopes = %v, want it to contain %+v", refs, want)
	}
}

func TestRefsLayoutLaw(t *testing.T) { // AC-F4-5
	s, dir, commit := setupStore(t)
	if err := s.Fork("lane-a", commit); err != nil {
		t.Fatalf("Fork: %v", err)
	}

	out := git(t, dir, "for-each-ref", "--format=%(refname)")
	if out == "" {
		t.Fatal("for-each-ref returned no refs after Open + Snapshot + Fork")
	}
	classes := map[string]bool{
		"ground": true, "materialized": true, "scopes": true,
		"ops": true, "archive": true,
	}
	seen := map[string]bool{}
	for _, ref := range strings.Split(out, "\n") {
		parts := strings.Split(ref, "/")
		if len(parts) < 4 || parts[0] != "refs" || parts[1] != "vcscore" || !classes[parts[2]] {
			t.Fatalf("ref %q violates the layout law: every ref lives under refs/vcscore/{ground,materialized,scopes,ops,archive}/", ref)
		}
		seen[ref] = true
	}
	// Non-vacuous: the refs this test created are actually present.
	for _, want := range []string{
		"refs/vcscore/ops/latest",
		"refs/vcscore/ops/snapshot-" + commit[:12],
		"refs/vcscore/scopes/lane-a",
	} {
		if !seen[want] {
			t.Fatalf("expected ref %q missing; refs present: %v", want, seen)
		}
	}
}

func TestListScopesEnumeratesLayout(t *testing.T) { // AC-F4-5, the check as written
	s, dir, commit := setupStore(t)
	if err := s.Fork("lane-a", commit); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	// No Phase 1 API mints an archive ref; write one via plumbing.
	git(t, dir, "update-ref", "refs/vcscore/archive/retired-1", commit)

	got, err := s.ListScopes()
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	// Exactly the cross-layout set, each with its kind, sorted by
	// (kind, name) — no more, no fewer.
	want := []snapshot.ScopeRef{
		{Kind: snapshot.RefArchive, Name: "retired-1", CommitID: commit},
		{Kind: snapshot.RefOps, Name: "latest", CommitID: commit},
		{Kind: snapshot.RefOps, Name: "snapshot-" + commit[:12], CommitID: commit},
		{Kind: snapshot.RefScope, Name: "lane-a", CommitID: commit},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListScopes =\n%+v\nwant exactly\n%+v", got, want)
	}
}

func TestForkRejectsBadInputs(t *testing.T) {
	s, dir, commit := setupStore(t)
	blob := git(t, dir, "rev-parse", commit+":workspace/hello.txt")

	for _, tc := range []struct {
		name, scope, commitID string
	}{
		{"path escape scope", "../escape", commit},
		{"capitalized scope", "Has Caps", commit},
		{"blob as commit", "lane-a", blob},
	} {
		if err := s.Fork(tc.scope, tc.commitID); err == nil {
			t.Fatalf("Fork(%q, %q) succeeded, want error (%s)", tc.scope, tc.commitID, tc.name)
		}
	}
	if out := git(t, dir, "for-each-ref", "refs/vcscore/scopes"); out != "" {
		t.Fatalf("rejected Fork calls created scope refs:\n%s", out)
	}
}
