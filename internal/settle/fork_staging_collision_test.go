package settle

// Fork/settlement staging collision test (PR #7 wave 5, Codex
// P1 on settle.go): two namespaces used to share the staging/
// directory — settlement staging keyed by caller-supplied
// operation ID (staging/<op>) and Fork's temporary layer root
// (staging/fork-<runScope>). A crashed settlement whose op ID
// was "fork-run-b" kept its rollback preimages in exactly the
// directory Fork removes unconditionally at entry when forking
// run scope "run-b", so the Fork destroyed the preimages while
// the journal survived — and Recover then treated a modified
// file as a creation and deleted it instead of restoring its
// original bytes.
//
// The fix is structural: Fork's temporary layer roots live in
// their own namespace under the settle Root (fork-staging/),
// which no operation ID can name — an op ID is a single path
// component under staging/, and the fork namespace is not
// under staging/ at all.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestForkKeepsPendingSettlementStaging: a real Fork of a new
// run scope must not touch a pending settlement's staging,
// even when the settlement's op ID spells the name Fork's
// temporary root used to occupy; Recover afterwards restores
// the parent byte-exactly from the surviving preimages.
func TestForkKeepsPendingSettlementStaging(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)
	const op = "fork-run-b"

	// Parent scope "parent"'s crashed settlement, sharing the
	// colliding op ID (helper from journal_cross_scope_test.go:
	// mod.txt already overwritten, its preimage durable under
	// staging/<op>, an applying journal recording the done
	// write unit).
	leaveApplyingJournalA(t, f, op)

	// A real Fork of run scope "run-b" from the same parent.
	if _, err := Fork(f.ctx, "parent", "run-b"); err != nil {
		t.Fatalf("Fork run-b: %v", err)
	}

	// The pending settlement's preimage survived the Fork.
	if _, err := os.Stat(filepath.Join(f.root, "staging", op, "pre", "mod.txt")); err != nil {
		t.Fatalf("pending settlement's preimage after Fork: %v, want it intact", err)
	}

	// And Recover restores the parent byte-exactly.
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover parent: %v", err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after Recover = %s, want pre-apply %s (original bytes were not restored)", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
}

// TestForkRemovesItsOwnStaleTempRoot: a remnant of a crashed
// Fork in Fork's own temp namespace is removed outright on
// re-entry — never adopted, never a wedge.
func TestForkRemovesItsOwnStaleTempRoot(t *testing.T) {
	f := newJournalFixture(t)

	// Debris of a crashed Fork of "run-b": a stale temp root
	// carrying a marker no fresh layer would contain.
	stale := filepath.Join(f.root, "fork-staging", "run-b")
	journalWriteFile(t, filepath.Join(stale, "stale-marker.txt"), "stale\n", 0o644)

	layer, err := Fork(f.ctx, "parent", "run-b")
	if err != nil {
		t.Fatalf("Fork run-b over a stale temp root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layer.Dir, "stale-marker.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale marker in handed-out layer: err = %v, want it gone with the remnant", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("fork temp root after Fork: err = %v, want it renamed into place", err)
	}
}
