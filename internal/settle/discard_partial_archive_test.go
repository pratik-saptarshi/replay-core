package settle_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/settle"
	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// TestDiscardCompletesWithPartialArchival (PR #7 wave 8, Codex
// P2): the one window TestDiscardCompletesAfterCrashPastArchive
// could not close — a crash INSIDE ArchiveScope during a
// Discard, after the archive ref was created but before the
// scope ref was deleted. Both refs then name the run's head.
// On the unfixed code the state is a permanent wedge:
// ArchiveScope refuses its own partial work as an
// already-existing archive, and Discard — still finding the
// active scope — calls ArchiveScope again instead of entering
// its archived-head recovery, so cleanup and the discard fact
// stay blocked forever. The retry must complete the discard
// end to end: finish the archival, remove the layer, mint the
// fact citing the archived head, drop the sidecar.
func TestDiscardCompletesWithPartialArchival(t *testing.T) {
	f := newFixture(t)
	if _, err := settle.Fork(f.ctx, "parent", "run-a"); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	// The interrupted archival's surviving state, built through
	// the store's own surface: a completed archive (the crashed
	// call's landed create), then the scope ref re-created at
	// the same head (the delete that never ran) — both refs,
	// one head, exactly what the crash window leaves.
	if err := f.snaps.ArchiveScope("run-a"); err != nil {
		t.Fatalf("ArchiveScope (the crashed verb's landed create): %v", err)
	}
	if err := f.snaps.Fork("run-a", f.parent0); err != nil {
		t.Fatalf("re-create scope ref at the archived head: %v", err)
	}

	// Bounded retries, as an operator would once the transient
	// failure is gone: the discard must complete. (On the
	// unfixed code every attempt fails identically.)
	var receipt store.Receipt
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if receipt, err = settle.Discard(f.ctx, "run-a", "op-1"); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("Discard of a scope with a partial archival still failing after retries: %v", err)
	}

	refs, err := f.snaps.ListScopes()
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	foundArchive := false
	for _, r := range refs {
		if r.Kind == snapshot.RefScope && r.Name == "run-a" {
			t.Fatalf("scope run-a still present after completed Discard: %+v", r)
		}
		if r.Kind == snapshot.RefArchive && r.Name == "run-a" {
			if r.CommitID != f.parent0 {
				t.Fatalf("archive/run-a at %s, want fork commit %s", r.CommitID, f.parent0)
			}
			foundArchive = true
		}
	}
	if !foundArchive {
		t.Fatalf("ListScopes = %v, want archive/run-a at %s", refs, f.parent0)
	}
	if _, err := os.Stat(filepath.Join(f.root, "layers", "run-a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("layer root after completed Discard: err = %v, want gone", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "forks", "run-a.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar after completed Discard: err = %v, want gone", err)
	}
	facts := f.settleFacts(t, "run-a")
	if len(facts) != 1 || facts[0].ID != receipt.RecordID {
		t.Fatalf("settle facts on run-a = %v, want exactly the discard receipt %s", facts, receipt.RecordID)
	}
	body := factBody(t, facts[0])
	for key, want := range map[string]string{
		"op": "op-1", "run_scope": "run-a", "head": f.parent0,
		"archive_ref": "refs/vcscore/archive/run-a",
	} {
		if got, _ := stringField(body, key); got != want {
			t.Fatalf("discard fact body[%q] = %q, want %q (the archived head, cited as recorded)", key, got, want)
		}
	}
	// The completion is authoritative: a further retry reads
	// the fact and reports the scope consumed.
	if _, err := settle.Discard(f.ctx, "run-a", "op-2"); !errors.Is(err, settle.ErrScopeConsumed) {
		t.Fatalf("Discard after completed Discard: err = %v, want ErrScopeConsumed", err)
	}
}
