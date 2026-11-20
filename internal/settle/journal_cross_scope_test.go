package settle

// Cross-scope staging tests (PR #7 wave, Codex P1 on
// journal.go): staging is keyed only by operation ID, while
// journals are keyed by parent scope. A settlement on parent
// B reusing parent A's caller-supplied op ID therefore shares
// A's staging directory: B's materialization removes it at
// entry and B's completion clears it, destroying A's durable
// preimages while A's applying journal survives (entry
// recovery checks only the verb's own parent scope). Recover
// on A then treats a modified file as a creation — its
// preimage is missing — removes it, and clears A's journal
// without restoring the original bytes.
//
// The fix charged by the finding, in its second form: reject
// reuse of an op ID while another parent scope's journal owns
// the staging directory, and never let one scope's cleanup
// remove staging another scope's journal still owns.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// leaveApplyingJournalA constructs exactly the state a crashed
// settlement on parent scope "parent" leaves behind: mod.txt
// already overwritten in the parent tree, its preimage durable
// in staging under the shared op ID, and an applying journal
// recording the done write unit.
func leaveApplyingJournalA(t *testing.T, f *journalFixture, op string) {
	t.Helper()
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "changed\n", 0o644)
	preDir := filepath.Join(f.root, "staging", op, "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	j := &journal{
		Op:           op,
		Verb:         verbSelect,
		ParentScope:  "parent",
		RunScope:     "run-a",
		State:        journalStateApplying,
		OldHead:      f.parent0,
		ResultCommit: f.parent0,
		ParentDir:    f.parentDir,
		Fact:         store.SettleSelectBody(op, "parent", "run-a", f.parent0, f.parent0),
		Units: []journalUnit{
			{Path: "mod.txt", Kind: unitWrite, Done: true},
		},
	}
	if err := writeJournal(f.root, j); err != nil {
		t.Fatalf("writeJournal A: %v", err)
	}
}

// TestSelectSameOpIDOnOtherParentRefusedWhileJournalPending:
// a real Select on a second parent scope, reusing the pending
// settlement's op ID, is refused before any side effect; the
// pending settlement's preimages survive, and Recover on the
// first parent then restores its tree byte-exactly.
func TestSelectSameOpIDOnOtherParentRefusedWhileJournalPending(t *testing.T) {
	f := newJournalFixture(t)
	preHashA := hashTree(t, f.parentDir)
	const op = "shared-op"

	// Parent B: its own tree, scope, and run forked from it.
	parentBDir := filepath.Join(f.scratch, "parent-b")
	journalWriteFile(t, filepath.Join(parentBDir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(parentBDir, "mod.txt"), "b original\n", 0o644)
	parentB0, err := f.snaps.Snapshot(parentBDir, map[string]string{"run": "settle-cross-scope-fixture"})
	if err != nil {
		t.Fatalf("Snapshot parent B: %v", err)
	}
	if err := f.snaps.Fork("parent-b", parentB0); err != nil {
		t.Fatalf("Fork parent-b scope: %v", err)
	}
	layer, err := Fork(f.ctx, "parent-b", "run-b")
	if err != nil {
		t.Fatalf("Fork run-b: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "b settled\n", 0o644)
	runBHead, err := f.snaps.Snapshot(layer.Dir, map[string]string{"run": "settle-cross-scope-fixture"})
	if err != nil {
		t.Fatalf("Snapshot run-b layer: %v", err)
	}
	if err := f.snaps.AdvanceScope("run-b", parentB0, runBHead); err != nil {
		t.Fatalf("AdvanceScope run-b: %v", err)
	}
	preHashB := hashTree(t, parentBDir)

	// Parent A's crashed settlement, sharing the op ID.
	leaveApplyingJournalA(t, f, op)

	// B's Select reusing A's op ID must be refused.
	if _, err := Select(f.ctx, "parent-b", parentBDir, "run-b", op); err == nil {
		t.Fatalf("Select on parent-b reusing pending op %q succeeded, want a descriptive refusal", op)
	}
	if got := hashTree(t, parentBDir); got != preHashB {
		t.Fatalf("parent B tree changed across refused Select: %s -> %s", preHashB, got)
	}
	if head, err := f.snaps.ScopeHead("parent-b"); err != nil || head != parentB0 {
		t.Fatalf("parent-b head = %s, %v after refused Select; want %s unmoved", head, err, parentB0)
	}
	if facts := f.journalFacts(t, "run-b"); len(facts) != 0 {
		t.Fatalf("refused Select minted %d facts on run-b, want 0", len(facts))
	}

	// A's preimage survived B's attempt, and Recover restores
	// A's tree byte-exactly.
	if _, err := os.Stat(filepath.Join(f.root, "staging", op, "pre", "mod.txt")); err != nil {
		t.Fatalf("A's preimage after B's refused Select: %v, want it intact", err)
	}
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover parent A: %v", err)
	}
	if got := hashTree(t, f.parentDir); got != preHashA {
		t.Fatalf("parent A walk hash after Recover = %s, want pre-apply %s (original bytes were not restored)", got, preHashA)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal A after Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rollback of A minted %d facts, want 0", len(facts))
	}
}

// TestClearJournalStateKeepsStagingOwnedByOtherParent: one
// scope's journal cleanup must not remove the staging
// directory while another parent scope's journal with the
// same op ID still owns it.
func TestClearJournalStateKeepsStagingOwnedByOtherParent(t *testing.T) {
	f := newJournalFixture(t)
	const op = "shared-op"
	leaveApplyingJournalA(t, f, op)

	if err := clearJournalState(f.root, "parent-b", op); err != nil {
		t.Fatalf("clearJournalState for parent-b: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "staging", op, "pre", "mod.txt")); err != nil {
		t.Fatalf("A's preimage after parent-b cleanup: %v, want it intact", err)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); err != nil {
		t.Fatalf("A's journal after parent-b cleanup: %v, want it intact", err)
	}

	// A's own cleanup still removes both its journal and the
	// staging directory.
	if err := clearJournalState(f.root, "parent", op); err != nil {
		t.Fatalf("clearJournalState for parent A: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "staging", op)); !os.IsNotExist(err) {
		t.Fatalf("staging after A's own cleanup: err = %v, want gone", err)
	}
}
