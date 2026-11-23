package settle

// Orphan-anchor tests (PR #7 band-(a) repair of SEC-1): the
// parent anchor as first built vouched for a (scope,
// directory) pair — never for the journal itself. An anchor
// left behind by the system's own writer, with no journal
// beside it, therefore vouched for ANY journal naming the
// recorded directory, including a planted one. The orphan
// state is reachable three ways, the first with no crash at
// all: (i) writeJournal lands the anchor, then the journal
// write itself fails (ENOSPC, EMFILE, EIO) and the settlement
// aborts with the anchor in place; (ii) a crash between the
// anchor rename and the journal rename inside writeJournal;
// (iii) a crash inside clearJournalState between removing
// the journal and removing the anchor. The state persists
// until the next settlement on the scope writes a journal —
// and every verb calls Recover at entry, so the next
// legitimate verb call is what triggers the planted journal.
// The anchor file is plaintext, so the planter learns the
// exact directory to name from the anchor itself.
//
// The repair binds the vouch to the journal's identity: the
// anchor additionally records a one-way digest of the
// journal's canonical bytes, refreshed by writeJournal on
// every state transition, in two slots — the journal being
// written and the journal it replaces — so at every crash
// point inside writeJournal the journal actually on disk is
// vouched, while a journal the writer never produced matches
// neither slot and is refused even beside a genuine orphan
// anchor.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// orphanAnchor leaves f's root holding a parent anchor the
// system's writer recorded for the real parent directory —
// and no journal beside it. The state is produced the way a
// crash inside clearJournalState leaves it (the journal
// removed, the anchor not yet), which is byte-for-byte the
// state the other two orphan paths converge on: an anchor
// whose journal is gone.
func orphanAnchor(t *testing.T, f *journalFixture) {
	t.Helper()
	genuine := &journal{
		Op: "op-genuine", Verb: verbSelect, ParentScope: "parent", RunScope: "run-a",
		State: journalStatePlanned, OldHead: f.parent0, ResultCommit: f.parent0,
		ParentDir: f.parentDir,
		Fact:      store.SettleSelectBody("op-genuine", "parent", "run-a", f.parent0, f.parent0),
	}
	if err := writeJournal(f.root, genuine); err != nil {
		t.Fatalf("writeJournal genuine: %v", err)
	}
	if err := os.Remove(journalPath(f.root, "parent")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journalAnchorPath(f.root, "parent")); err != nil {
		t.Fatalf("anchor after journal removal: %v, want it left behind", err)
	}
}

// TestRecoverRefusesPlantedJournalBesideOrphanAnchor is the
// reviewer's bypass, exactly: an orphan anchor vouching for
// the real parent directory, then a planted applying journal
// naming that same directory, one done write unit for
// mod.txt, no staging anywhere (SEC-1's delete primitive).
// The directory check passes — only the journal-identity
// vouch can refuse it.
func TestRecoverRefusesPlantedJournalBesideOrphanAnchor(t *testing.T) {
	f := newJournalFixture(t)
	orphanAnchor(t, f)
	parentHash := hashTree(t, f.parentDir)
	jpath := journalPath(f.root, "parent")
	plantRawJournal(t, jpath, rawJournalSpec{
		op: "op-planted", verb: verbSelect, parentScope: "parent", runScope: "run-a",
		state: journalStateApplying, oldHead: f.parent0, resultCommit: f.parent0,
		parentDir: f.parentDir,
		fact:      store.SettleSelectBody("op-planted", "parent", "run-a", f.parent0, f.parent0),
		units:     []canonical.Value{rawUnit("mod.txt", unitWrite, true, true)},
	})

	err := Recover(f.ctx, "parent")
	assertAnchorRefusal(t, f, err, f.parentDir, parentHash, jpath)
}

// TestRecoverRefusesPlantedVerifiedJournalBesideOrphanAnchor
// is the verified-state twin: a planted verified journal
// beside the orphan anchor must not advance the scope or
// mint its fact — the vouch gate runs before any state is
// judged, so the completion branch is covered by the same
// refusal.
func TestRecoverRefusesPlantedVerifiedJournalBesideOrphanAnchor(t *testing.T) {
	f := newJournalFixture(t)
	orphanAnchor(t, f)
	resultDir := filepath.Join(f.scratch, "result")
	journalWriteFile(t, filepath.Join(resultDir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(resultDir, "mod.txt"), "settled\n", 0o644)
	resultCommit, err := f.snaps.Snapshot(resultDir, map[string]string{"run": "settle-anchor-orphan-forge"})
	if err != nil {
		t.Fatalf("Snapshot result: %v", err)
	}
	jpath := journalPath(f.root, "parent")
	plantRawJournal(t, jpath, rawJournalSpec{
		op: "op-planted-verified", verb: verbSelect, parentScope: "parent", runScope: "run-a",
		state: journalStateVerified, oldHead: f.parent0, resultCommit: resultCommit,
		parentDir: f.parentDir,
		fact:      store.SettleSelectBody("op-planted-verified", "parent", "run-a", f.parent0, resultCommit),
		units: []canonical.Value{
			rawUnit("mod.txt", unitWrite, true, true),
			rawUnit("old.txt", unitDelete, true, true),
		},
	})

	err = Recover(f.ctx, "parent")
	assertAnchorRefusal(t, f, err, "", "", jpath)
}

// TestRecoverIgnoresOrphanAnchorWithNoJournal: the anchor
// alone never acts. With no journal beside the orphan there
// is nothing to recover — the state a failed first journal
// write leaves (the anchor landed, the journal never did) —
// and Recover is the same no-op it is with no state at all.
func TestRecoverIgnoresOrphanAnchorWithNoJournal(t *testing.T) {
	f := newJournalFixture(t)
	orphanAnchor(t, f)
	before := hashTree(t, f.parentDir)
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover beside an orphan anchor with no journal = %v, want nil", err)
	}
	if got := hashTree(t, f.parentDir); got != before {
		t.Fatalf("parent walk hash changed across no-op Recover: %s -> %s", before, got)
	}
}

// interruptedRefresh builds a mid-settlement state and the
// two journal states of one transition. The journal on disk
// (j1) records a write unit started, its mutation landed in
// the parent, its preimage durable in staging — the state
// the in-flight discipline leaves. j2 is the journal the
// next writeJournal was landing when the process died: the
// same unit marked done. Only j2's bytes are returned; it is
// never planted at the journal path by this helper.
func interruptedRefresh(t *testing.T, f *journalFixture) (j1, j2 []byte) {
	t.Helper()
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "changed\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-live", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	live := &journal{
		Op: "op-live", Verb: verbSelect, ParentScope: "parent", RunScope: "run-a",
		State: journalStateApplying, OldHead: f.parent0, ResultCommit: f.parent0,
		ParentDir: f.parentDir,
		Fact:      store.SettleSelectBody("op-live", "parent", "run-a", f.parent0, f.parent0),
		Units:     []journalUnit{{Path: "mod.txt", Kind: unitWrite, Done: false, Started: true}},
	}
	if err := writeJournal(f.root, live); err != nil {
		t.Fatalf("writeJournal in-flight: %v", err)
	}
	var err error
	j1, err = os.ReadFile(journalPath(f.root, "parent"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := parentDirIdentity(f.parentDir)
	if err != nil {
		t.Fatalf("parent identity of fixture dir: %v", err)
	}
	j2 = plantRawJournal(t, filepath.Join(f.scratch, "next-state.json"), rawJournalSpec{
		op: "op-live", verb: verbSelect, parentScope: "parent", runScope: "run-a",
		state: journalStateApplying, oldHead: f.parent0, resultCommit: f.parent0,
		parentDir:      f.parentDir,
		parentIdentity: identity,
		fact:           store.SettleSelectBody("op-live", "parent", "run-a", f.parent0, f.parent0),
		units:          []canonical.Value{rawUnit("mod.txt", unitWrite, true, true)},
	})
	return j1, j2
}

// refreshAnchorVouch lands the anchor refresh writeJournal
// performs for a transition: the vouch for the journal being
// written (current) and the journal it replaces (prev).
func refreshAnchorVouch(t *testing.T, f *journalFixture, current, prev []byte) {
	t.Helper()
	if err := writeParentAnchor(f.root, parentAnchor{
		ParentScope:       "parent",
		ParentDir:         f.parentDir,
		JournalDigest:     journalBytesDigest(current),
		PrevJournalDigest: journalBytesDigest(prev),
	}); err != nil {
		t.Fatalf("refresh anchor vouch: %v", err)
	}
}

// TestRecoverActsOnPreviousJournalAfterInterruptedAnchorRefresh
// is the liveness half of the vouch: the process died after
// writeJournal refreshed the anchor for the next journal
// state but before that state's rename landed, so the
// journal on disk is the PREVIOUS state. It must still be
// vouched — through the second slot — and its settlement
// must roll back byte-exactly, not refuse.
func TestRecoverActsOnPreviousJournalAfterInterruptedAnchorRefresh(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)
	j1, j2 := interruptedRefresh(t, f)
	refreshAnchorVouch(t, f, j2, j1)

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover of the previous-state journal after an interrupted anchor refresh: %v", err)
	}
	assertRolledBackToPre(t, f, "op-live", preHash)
	if _, err := os.Stat(journalAnchorPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("anchor after completed Recover: err = %v, want gone with the journal", err)
	}
}

// TestRecoverActsOnLandedJournalAfterAnchorRefresh is the
// other landing of the same transition: the anchor refresh
// and the journal rename both landed before the crash, so
// the journal on disk is the new state, vouched through the
// first slot, and it recovers the same way.
func TestRecoverActsOnLandedJournalAfterAnchorRefresh(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)
	j1, j2 := interruptedRefresh(t, f)
	refreshAnchorVouch(t, f, j2, j1)
	if err := os.WriteFile(journalPath(f.root, "parent"), j2, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover of the landed journal after an anchor refresh: %v", err)
	}
	assertRolledBackToPre(t, f, "op-live", preHash)
}
