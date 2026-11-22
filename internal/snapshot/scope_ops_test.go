package snapshot_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// TestScopeHeadAdvanceArchive (plan Task 6, Step 1): the scope
// surface settlement stands on — head reads that name a missing
// scope, a compare-and-swap advance that refuses a stale old
// value without moving the head, and archiving that moves the
// ref to the archive class instead of deleting it.
func TestScopeHeadAdvanceArchive(t *testing.T) {
	s, _, commit := setupStore(t)

	if _, err := s.ScopeHead("missing"); !errors.Is(err, snapshot.ErrScopeNotFound) {
		t.Fatalf("ScopeHead(missing) err = %v, want ErrScopeNotFound", err)
	}

	if err := s.Fork("lane-a", commit); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	head, err := s.ScopeHead("lane-a")
	if err != nil {
		t.Fatalf("ScopeHead(lane-a): %v", err)
	}
	if head != commit {
		t.Fatalf("ScopeHead(lane-a) = %s, want %s", head, commit)
	}

	// A second commit to advance to.
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello v2\n")
	commit2, err := s.Snapshot(ws, map[string]string{"run": "scope-ops-2"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// AdvanceScope with a wrong old value: refused, head unmoved.
	if err := s.AdvanceScope("lane-a", commit2, commit2); !errors.Is(err, snapshot.ErrScopeHeadMoved) {
		t.Fatalf("AdvanceScope(wrong old) err = %v, want ErrScopeHeadMoved", err)
	}
	if head, err := s.ScopeHead("lane-a"); err != nil || head != commit {
		t.Fatalf("after refused advance, ScopeHead = %s, %v; want %s", head, err, commit)
	}

	// AdvanceScope with the right old value: the head moves.
	if err := s.AdvanceScope("lane-a", commit, commit2); err != nil {
		t.Fatalf("AdvanceScope: %v", err)
	}
	if head, err := s.ScopeHead("lane-a"); err != nil || head != commit2 {
		t.Fatalf("after advance, ScopeHead = %s, %v; want %s", head, err, commit2)
	}

	// AdvanceScope on a missing scope names the absence.
	if err := s.AdvanceScope("missing", commit, commit2); !errors.Is(err, snapshot.ErrScopeNotFound) {
		t.Fatalf("AdvanceScope(missing) err = %v, want ErrScopeNotFound", err)
	}

	// ArchiveScope: the ref moves to the archive class at the
	// same commit; the scope ref is gone.
	if err := s.ArchiveScope("lane-a"); err != nil {
		t.Fatalf("ArchiveScope: %v", err)
	}
	if _, err := s.ScopeHead("lane-a"); !errors.Is(err, snapshot.ErrScopeNotFound) {
		t.Fatalf("ScopeHead after archive err = %v, want ErrScopeNotFound", err)
	}
	refs, err := s.ListScopes()
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	foundArchive := false
	for _, r := range refs {
		if r.Kind == snapshot.RefScope && r.Name == "lane-a" {
			t.Fatalf("ListScopes still shows scope lane-a after archive: %+v", r)
		}
		if r.Kind == snapshot.RefArchive && r.Name == "lane-a" {
			if r.CommitID != commit2 {
				t.Fatalf("archive/lane-a at %s, want %s", r.CommitID, commit2)
			}
			foundArchive = true
		}
	}
	if !foundArchive {
		t.Fatalf("ListScopes = %v, want archive/lane-a at %s", refs, commit2)
	}

	// Archiving again: the scope no longer exists.
	if err := s.ArchiveScope("lane-a"); !errors.Is(err, snapshot.ErrScopeNotFound) {
		t.Fatalf("second ArchiveScope err = %v, want ErrScopeNotFound", err)
	}
}
