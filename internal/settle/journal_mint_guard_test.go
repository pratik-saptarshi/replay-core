package settle

// Mint-guard tests (PR #11 Sourcery re-review round 2,
// panel ruling on F4): Recover's verified branch completes
// a crashed verb's commit point — it mints the journal's
// recorded fact iff no fact carries the journal's op. The
// op-specific check diverges from the verbs' consume-once
// law, which is keyed on ANY settlement fact on the run
// scope: if the scope already carries ANOTHER operation's
// fact (the durable end-state of two overlapping Select
// calls, whose lifetimes spec §9 excludes but whose journal
// can still be planted or left behind), recovery minted a
// SECOND settlement fact for a scope only one call
// settled. The guard closes the divergence as
// defense-in-depth — §9 is unchanged, no concurrency is
// supported or detected: when the journal's own fact is
// absent but any settlement fact stands on the run scope,
// Recover refuses descriptively and leaves the journal in
// place as evidence, rather than mint and clear.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// TestRecoverRefusesToMintSecondFactForConsumedScope is
// the constructed end-state: a winner Select completed
// whole (its fact standing, the parent head at the run
// head), and a second operation's verified journal —
// old_head at the fork head, result_commit at the shared
// result head — is then resolved by Recover. Recover must
// refuse to mint: exactly one fact remains (the winner's),
// the journal stays in place as evidence, and the head
// does not move.
func TestRecoverRefusesToMintSecondFactForConsumedScope(t *testing.T) {
	f := newJournalFixture(t)

	// The winner settles whole.
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "run mod\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)
	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-winner"); err != nil {
		t.Fatalf("winner Select: %v", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("facts after winner Select = %d, want exactly 1", len(facts))
	}
	winnerHead, err := f.snaps.ScopeHead("parent")
	if err != nil {
		t.Fatalf("ScopeHead(parent): %v", err)
	}

	// The loser's verified journal: the durable state the
	// overlapping-calls interleaving leaves behind.
	loser := &journal{
		Op:           "op-loser",
		Verb:         verbSelect,
		ParentScope:  "parent",
		RunScope:     "run-a",
		State:        journalStateVerified,
		OldHead:      f.parent0,
		ResultCommit: winnerHead,
		ParentDir:    f.parentDir,
		Fact:         store.SettleSelectBody("op-loser", "parent", "run-a", f.parent0, winnerHead),
		Units: []journalUnit{
			{Path: "mod.txt", Kind: unitWrite, Done: true},
		},
	}
	if err := writeJournal(f.root, loser); err != nil {
		t.Fatalf("writeJournal loser: %v", err)
	}

	err = Recover(f.ctx, "parent")
	if err == nil {
		t.Fatal("Recover minted a second settlement fact for a consumed scope; want a descriptive refusal")
	}
	if !strings.Contains(err.Error(), "run-a") {
		t.Fatalf("Recover refusal = %v, want it to name the consumed run scope", err)
	}
	facts := f.journalFacts(t, "run-a")
	if len(facts) != 1 {
		t.Fatalf("facts after refused Recover = %d, want exactly 1 (the winner's)", len(facts))
	}
	for _, p := range facts[0].Record.Pairs {
		if p.Key == "body" {
			if op, _ := stringField(p.Value, "op"); op != "op-winner" {
				t.Fatalf("standing fact's op = %q, want the winner's %q", op, "op-winner")
			}
		}
	}
	if _, serr := os.Stat(journalPath(f.root, "parent")); serr != nil {
		t.Fatalf("journal after refused Recover: %v, want still present as evidence", serr)
	}
	if head, herr := f.snaps.ScopeHead("parent"); herr != nil || head != winnerHead {
		t.Fatalf("parent head = %s, %v after refused Recover; want %s unmoved", head, herr, winnerHead)
	}
}

// plantVerifiedSelectJournal writes a verified select
// journal for op against a snapshotted result tree and
// returns the result commit: the winner-crashed-at-the-
// commit-point state the two floor fixtures below resolve.
func plantVerifiedSelectJournal(t *testing.T, f *journalFixture, op string) string {
	t.Helper()
	resultDir := filepath.Join(f.scratch, "result-"+op)
	journalWriteFile(t, filepath.Join(resultDir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(resultDir, "mod.txt"), "settled\n", 0o644)
	resultCommit, err := f.snaps.Snapshot(resultDir, map[string]string{"run": "settle-mint-guard"})
	if err != nil {
		t.Fatalf("Snapshot result: %v", err)
	}
	j := &journal{
		Op:           op,
		Verb:         verbSelect,
		ParentScope:  "parent",
		RunScope:     "run-a",
		State:        journalStateVerified,
		OldHead:      f.parent0,
		ResultCommit: resultCommit,
		ParentDir:    f.parentDir,
		Fact:         store.SettleSelectBody(op, "parent", "run-a", f.parent0, resultCommit),
		Units: []journalUnit{
			{Path: "mod.txt", Kind: unitWrite, Done: true},
		},
	}
	if err := writeJournal(f.root, j); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
	return resultCommit
}

// TestRecoverMintsForVerifiedJournalWithNoFactsAnywhere is
// the legitimate winner-recovery floor the guard must not
// misfire on: the winner crashed after its CAS advanced
// the parent head to the result commit but before it
// minted — head already at result_commit, no settlement
// fact anywhere. Recover still mints the journal's fact
// and completes.
func TestRecoverMintsForVerifiedJournalWithNoFactsAnywhere(t *testing.T) {
	f := newJournalFixture(t)
	resultCommit := plantVerifiedSelectJournal(t, f, "op-winner")

	// The CAS landed before the crash.
	if err := f.snaps.AdvanceScope("parent", f.parent0, resultCommit); err != nil {
		t.Fatalf("AdvanceScope: %v", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("facts before Recover = %d, want 0", len(facts))
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v (the mint guard must not misfire on a legitimate post-CAS recovery)", err)
	}
	facts := f.journalFacts(t, "run-a")
	if len(facts) != 1 {
		t.Fatalf("facts after Recover = %d, want exactly 1 (the winner's, minted by recovery)", len(facts))
	}
	for _, p := range facts[0].Record.Pairs {
		if p.Key == "body" {
			if op, _ := stringField(p.Value, "op"); op != "op-winner" {
				t.Fatalf("minted fact's op = %q, want %q", op, "op-winner")
			}
		}
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != resultCommit {
		t.Fatalf("parent head = %s, %v after Recover; want %s", head, err, resultCommit)
	}
}

// TestRecoverSkipsMintWhenOwnFactAlreadyStands is the
// other floor: the winner crashed after minting its fact
// but before clearing its journal. Recover finds the
// journal's own fact already present, skips the mint, and
// completes — exactly one fact, journal cleared.
func TestRecoverSkipsMintWhenOwnFactAlreadyStands(t *testing.T) {
	f := newJournalFixture(t)
	resultCommit := plantVerifiedSelectJournal(t, f, "op-own")

	if err := f.snaps.AdvanceScope("parent", f.parent0, resultCommit); err != nil {
		t.Fatalf("AdvanceScope: %v", err)
	}
	// The mint landed before the crash.
	if _, err := f.trace.MintSettleSelect("run-a", "op-own", "parent", "run-a", f.parent0, resultCommit); err != nil {
		t.Fatalf("MintSettleSelect: %v", err)
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("facts after Recover = %d, want exactly 1 (no second mint)", len(facts))
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
}
