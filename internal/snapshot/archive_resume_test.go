package snapshot_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// TestArchiveScopeResumesPartialArchival (PR #7 wave 8, Codex
// P2): ArchiveScope creates the archive ref and then deletes
// the scope ref as separate operations. A process that exits
// between them — or a delete that fails, e.g. on a stale ref
// lock — leaves both refs naming the same head, and on the
// unfixed code that state is a permanent wedge: every retry
// fails at the existing-archive check, even after the transient
// failure is long gone. Both refs at one head is the archival's
// own half-done work, not a conflict: a retry must complete it
// by deleting the scope ref, and the completed archival must be
// indistinguishable from one that never crashed.
func TestArchiveScopeResumesPartialArchival(t *testing.T) {
	s, dir, commit := setupStore(t)
	if err := s.Fork("lane-a", commit); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	// The interrupted archival's surviving state, built with
	// plumbing: the create landed, the delete never ran.
	git(t, dir, "update-ref", "refs/vcscore/archive/lane-a", commit)
	if got := git(t, dir, "rev-parse", "refs/vcscore/scopes/lane-a"); got != commit {
		t.Fatalf("precondition: scopes/lane-a = %s, want %s", got, commit)
	}

	// Bounded retries, exactly what an operator does once the
	// transient failure clears: the archival must complete, not
	// merely fail differently. (On the unfixed code all three
	// attempts fail identically at the existing-archive check.)
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = s.ArchiveScope("lane-a"); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("ArchiveScope on a partial archival still failing after retries: %v", err)
	}

	// The end state is a completed archival's: scope ref gone,
	// archive ref at the head.
	if _, err := s.ScopeHead("lane-a"); !errors.Is(err, snapshot.ErrScopeNotFound) {
		t.Fatalf("ScopeHead after resumed archive err = %v, want ErrScopeNotFound", err)
	}
	if got := git(t, dir, "rev-parse", "refs/vcscore/archive/lane-a"); got != commit {
		t.Fatalf("archive/lane-a = %s after resumed archive, want %s", got, commit)
	}
	// And it is consumed exactly like a completed archival.
	if err := s.ArchiveScope("lane-a"); !errors.Is(err, snapshot.ErrScopeNotFound) {
		t.Fatalf("ArchiveScope after resumed archive err = %v, want ErrScopeNotFound", err)
	}
}

// TestArchiveScopeRefusesConflictingArchive (PR #7 wave 8):
// the resume law keys on the heads matching. An archive ref of
// the same name naming a DIFFERENT commit than the scope's head
// is not this scope's partial archival — it is a genuine
// conflict (the name was archived at another head), and
// ArchiveScope must refuse descriptively and move nothing.
func TestArchiveScopeRefusesConflictingArchive(t *testing.T) {
	s, dir, commit := setupStore(t)

	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "hello v2\n")
	other, err := s.Snapshot(ws, map[string]string{"run": "archive-resume-conflict"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if other == commit {
		t.Fatal("second snapshot returned the first commit ID; need two distinct targets")
	}

	if err := s.Fork("lane-a", commit); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	git(t, dir, "update-ref", "refs/vcscore/archive/lane-a", other)

	err = s.ArchiveScope("lane-a")
	if err == nil {
		t.Fatal("ArchiveScope with a conflicting archive succeeded; want a descriptive refusal")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("ArchiveScope conflict err = %v, want it to name the existing archive", err)
	}
	// Nothing moved: the scope still stands at its head and the
	// archive still names the conflicting commit.
	if got := git(t, dir, "rev-parse", "refs/vcscore/scopes/lane-a"); got != commit {
		t.Fatalf("scopes/lane-a = %s after refused archive, want unchanged %s", got, commit)
	}
	if got := git(t, dir, "rev-parse", "refs/vcscore/archive/lane-a"); got != other {
		t.Fatalf("archive/lane-a = %s after refused archive, want unchanged %s", got, other)
	}
}
