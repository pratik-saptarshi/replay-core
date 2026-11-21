package settle

// Pending-settlement Discard tests (PR #7 review, Codex P1 on
// settle.go): a Select or Apply that crashes with a VERIFIED
// journal but before minting its fact leaves no settlement
// fact on the run scope — so Discard's consume-once scan sees
// nothing, and the unfixed Discard proceeds without ever
// resolving the parent journal: it archives the run scope and
// mints a discard fact, yet the journal survives, and a later
// Recover still completes the earlier settlement and mints
// its select/apply fact on top of the discard (the fact-exists
// check keys on the journal's own schema and op, so the
// discard fact does not stop it). In the pre-CAS window
// recovery even advances the parent scope to output Discard
// has already reported discarded. Discard must resolve the
// parent journal identified by the fork sidecar — completing
// or rolling back the pending settlement — and recheck
// consumption before performing any discard effect.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// TestDiscardResolvesVerifiedJournalBeforeDiscarding: the
// pending settlement is a verified select journal — the tree
// materialized, the ref unmoved, no fact minted. Discard must
// complete that settlement through Recover (parent advances,
// exactly one select fact is minted) and then refuse as
// consumed: no archive, no discard fact, no second fact ever.
func TestDiscardResolvesVerifiedJournalBeforeDiscarding(t *testing.T) {
	f := newJournalFixture(t)
	if _, err := Fork(f.ctx, "parent", "run-a"); err != nil {
		t.Fatalf("Fork: %v", err)
	}

	// The result tree the crashed select had materialized and
	// verified: snapshot it as the result commit, advance the
	// run scope to it, and serve it from the parent directory.
	resultDir := filepath.Join(f.scratch, "result")
	journalWriteFile(t, filepath.Join(resultDir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(resultDir, "mod.txt"), "settled\n", 0o644)
	resultCommit, err := f.snaps.Snapshot(resultDir, map[string]string{"run": "settle-discard-pending"})
	if err != nil {
		t.Fatalf("Snapshot result: %v", err)
	}
	if err := f.snaps.AdvanceScope("run-a", f.parent0, resultCommit); err != nil {
		t.Fatalf("AdvanceScope run: %v", err)
	}
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "settled\n", 0o644)
	if err := os.Remove(filepath.Join(f.parentDir, "old.txt")); err != nil {
		t.Fatal(err)
	}

	j := &journal{
		Op:           "op-pending",
		Verb:         verbSelect,
		ParentScope:  "parent",
		RunScope:     "run-a",
		State:        journalStateVerified,
		OldHead:      f.parent0,
		ResultCommit: resultCommit,
		ParentDir:    f.parentDir,
		Fact:         store.SettleSelectBody("op-pending", "parent", "run-a", f.parent0, resultCommit),
		Units: []journalUnit{
			{Path: "mod.txt", Kind: unitWrite, Done: true},
			{Path: "old.txt", Kind: unitDelete, Done: true},
		},
	}
	if err := writeJournal(f.root, j); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}

	if _, err := Discard(f.ctx, "run-a", "op-discard"); !errors.Is(err, ErrScopeConsumed) {
		t.Fatalf("Discard with a verified settlement pending: err = %v, want ErrScopeConsumed (the pending settlement resolves first and consumes the scope)", err)
	}

	// The pending settlement completed exactly once.
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != resultCommit {
		t.Fatalf("parent head = %s, %v; want the pending settlement's result commit %s", head, err, resultCommit)
	}
	facts := f.journalFacts(t, "run-a")
	if len(facts) != 1 {
		t.Fatalf("settle facts on run-a = %d, want exactly 1 (the completed select, no discard fact)", len(facts))
	}
	if ref, _ := stringField(facts[0].Record, "schema_ref"); ref != store.SettleSelectSchemaRef {
		t.Fatalf("fact schema_ref = %q, want %q", ref, store.SettleSelectSchemaRef)
	}
	// No discard effect happened: the run scope was not
	// archived, and the journal is gone (resolved, not left
	// for a later Recover to mint a second fact from).
	if head, err := f.snaps.ScopeHead("run-a"); err != nil || head != resultCommit {
		t.Fatalf("run head = %s, %v; want %s unarchived", head, err, resultCommit)
	}
	refs, err := f.snaps.ListScopes()
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	for _, r := range refs {
		if r.Kind == snapshot.RefArchive && r.Name == "run-a" {
			t.Fatalf("run-a was archived by the refused Discard: %+v", r)
		}
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Discard: err = %v, want gone (resolved, not stranded)", err)
	}
	// And a later Recover is a no-op: no second fact can ever
	// land on top of the discard that was refused.
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("later Recover: %v", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after later Recover = %d, want exactly 1", len(facts))
	}
}

// TestDiscardRollsBackApplyingJournalBeforeDiscarding: the
// pending settlement is an applying journal — the parent is
// mid-materialization, between trees. Discard must roll it
// back through Recover first (parent exactly pre-verb), then
// proceed: the run scope archives and exactly one discard
// fact is minted, with the parent left at its pre-verb state
// rather than diverged from its scope head with no fact
// recording the divergence.
func TestDiscardRollsBackApplyingJournalBeforeDiscarding(t *testing.T) {
	f := newJournalFixture(t)
	if _, err := Fork(f.ctx, "parent", "run-a"); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	preHash := hashTree(t, f.parentDir)

	// The crashed select's landed unit and its preimage.
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "changed\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-pending", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	j := &journal{
		Op:           "op-pending",
		Verb:         verbSelect,
		ParentScope:  "parent",
		RunScope:     "run-a",
		State:        journalStateApplying,
		OldHead:      f.parent0,
		ResultCommit: f.parent0,
		ParentDir:    f.parentDir,
		Fact:         store.SettleSelectBody("op-pending", "parent", "run-a", f.parent0, f.parent0),
		Units: []journalUnit{
			{Path: "mod.txt", Kind: unitWrite, Done: true},
		},
	}
	if err := writeJournal(f.root, j); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}

	receipt, err := Discard(f.ctx, "run-a", "op-discard")
	if err != nil {
		t.Fatalf("Discard with an applying settlement pending: %v", err)
	}

	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after Discard = %s, want pre-verb %s (the pending settlement must be rolled back before discarding, not left diverged)", got, preHash)
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after Discard; want %s unmoved", head, err, f.parent0)
	}
	facts := f.journalFacts(t, "run-a")
	if len(facts) != 1 || facts[0].ID != receipt.RecordID {
		t.Fatalf("settle facts on run-a = %v, want exactly the discard receipt %s", facts, receipt.RecordID)
	}
	if ref, _ := stringField(facts[0].Record, "schema_ref"); ref != store.SettleDiscardSchemaRef {
		t.Fatalf("fact schema_ref = %q, want %q", ref, store.SettleDiscardSchemaRef)
	}
	refs, err := f.snaps.ListScopes()
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	foundArchive := false
	for _, r := range refs {
		if r.Kind == snapshot.RefArchive && r.Name == "run-a" {
			foundArchive = true
		}
	}
	if !foundArchive {
		t.Fatalf("ListScopes = %v, want archive/run-a (the discard itself still lands)", refs)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Discard: err = %v, want gone (resolved, not stranded)", err)
	}
}
