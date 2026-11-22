package settle

// Rollback temp-cleanup confinement and ownership tests
// (post-merge panel R1 + issue #9, both in removeUnitTemps;
// the restore-temp fixtures at the bottom pin the same
// ownership discipline on rollback's own restore path).
//
// R1 (ancestor confinement): removeUnitTemps computed a
// write unit's temp path through the LIVE tree and removed
// its occupant, with none of the lexical ancestor discipline
// its sibling rollback helpers carry. After a symlink-swap
// settlement is rolled back once — the created file removed,
// the symlink ancestor restored from its preimage, the
// journal not yet cleared — a repeated Recover's temp
// cleanup for the unit beneath the restored symlink resolves
// through the link and deletes a file in the OUTSIDE tree
// that happens to stand at the derived temp name. The parent
// converges and the journal clears, so the loss is silent.
//
// Issue #9 (ownership): wave 9 made temp names derivable
// from the destination path so rollback can compute them —
// but derivable is not owned. If a file the operation did
// not create already occupies the derived name, the write's
// exclusive creation refuses it and the verb fails with the
// unit marked started; the next Recover then deleted the
// pre-existing occupant unconditionally. It has no unit and
// no preimage: content lost, no crash required. Rollback may
// remove a derived-name temp only when the journal proves
// this settlement created it (the per-unit temp-created
// record the write path now persists between a temp's
// creation and its content write).
//
// The fixtures below construct both shapes deterministically
// so they run against the unfixed code, where the outside
// victim and the pre-existing occupant are deleted and the
// damage is observable.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// TestRecoverTempCleanupConfinedByRestoredSymlink (R1): the
// symlink-swap rollback of TestRecoverRepeatsSymlinkToDirRollback,
// extended with victims in the outside tree at BOTH derived
// temp names for the write unit beneath the swapped path.
// The interrupted first Recover performs both undos with the
// journal left in place; the repeated Recover's temp cleanup
// must decide lexically — an ancestor stands as a restored
// symlink, so the unit's temps are not reachable in this
// tree — and leave the outside tree byte-identical.
func TestRecoverTempCleanupConfinedByRestoredSymlink(t *testing.T) {
	f, targetDir := newSymlinkSwapFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := os.Remove(filepath.Join(layer.Dir, "old")); err != nil {
		t.Fatalf("remove layer old symlink: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "old", "inner.txt"), "settled inner\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)

	preHash := hashTree(t, f.parentDir)

	// Units: write old/inner.txt, delete old — both marked in
	// the write's single mark (done == 2).
	runCrashed(t, 2, func() {
		_, _ = Select(f.ctx, "parent", f.parentDir, "run-a", "op-r1")
	})

	// The interrupted first Recover: both undos performed with
	// rollback's own code, journal and staging left in place
	// (the construction TestRecoverRepeatsSymlinkToDirRollback
	// established — the crash hook never fires inside Recover).
	preDir := filepath.Join(stagingDirFor(f.root, "op-r1"), "pre")
	if err := undoWriteUnit(f.parentDir, preDir, journalUnit{Path: "old/inner.txt", Kind: unitWrite, Done: true, Started: true, TempCreated: true}, "op-r1", nil); err != nil {
		t.Fatalf("interrupted Recover, write undo: %v", err)
	}
	if err := undoDeleteUnit(f.parentDir, preDir, journalUnit{Path: "old", Kind: unitDelete, Done: true, Started: true}, "op-r1", nil); err != nil {
		t.Fatalf("interrupted Recover, delete undo: %v", err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after interrupted Recover = %s, want pre-select %s", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); err != nil {
		t.Fatalf("journal after interrupted Recover: %v, want still present", err)
	}

	// The victims: files in the OUTSIDE tree standing at the
	// two derived temp names for old/inner.txt — the names the
	// repeated Recover's temp cleanup computes for the unit,
	// resolved through the restored symlink at old.
	dst := filepath.Join(f.parentDir, "old", "inner.txt")
	victimFile := filepath.Join(targetDir, derivedTempName(dst, "op-r1", false))
	victimLink := filepath.Join(targetDir, derivedTempName(dst, "op-r1", true))
	journalWriteFile(t, victimFile, "victim file\n", 0o644)
	journalWriteFile(t, victimLink, "victim link\n", 0o644)
	targetHash := hashTree(t, targetDir)

	// The repeated Recover must converge the parent — and its
	// temp cleanup must never reach through the restored
	// symlink into the outside tree.
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("repeated Recover: %v", err)
	}
	for _, victim := range []struct {
		path, want string
	}{
		{victimFile, "victim file\n"},
		{victimLink, "victim link\n"},
	} {
		if b, err := os.ReadFile(victim.path); err != nil || string(b) != victim.want {
			t.Fatalf("outside victim %s after repeated Recover = %q, %v; want byte-identical %q (temp cleanup deleted through the restored symlink)", victim.path, b, err, victim.want)
		}
	}
	if got := hashTree(t, targetDir); got != targetHash {
		t.Fatalf("target walk hash after repeated Recover = %s, want %s (temp cleanup reached into the outside tree)", got, targetHash)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after repeated Recover = %s, want pre-select %s", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after repeated Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rolled-back select left %d facts, want 0", len(facts))
	}
}

// TestSelectRefusesPreExistingFileTempOccupant (issue #9,
// file kind): a file the operation did not create stands at
// the derived temp name for mod.txt. The write's exclusive
// creation refuses it — descriptively — and the verb fails
// with the unit marked started. Recover must then leave the
// occupant byte-identical: no unit names it, no preimage
// captured it, and the journal carries no record that this
// settlement ever created a temp at that name.
func TestSelectRefusesPreExistingFileTempOccupant(t *testing.T) {
	f := newJournalFixture(t)

	victim := filepath.Join(f.parentDir, derivedTempName(filepath.Join(f.parentDir, "mod.txt"), "op-own", false))
	journalWriteFile(t, victim, "precious\n", 0o644)
	preHash := hashTree(t, f.parentDir)

	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "run mod\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)

	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-own"); err == nil {
		t.Fatal("Select with a pre-existing file-temp occupant succeeded; want a descriptive refusal")
	} else if !strings.Contains(err.Error(), victim) {
		t.Fatalf("Select refusal = %v, want it to name the occupied temp path %s", err, victim)
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "precious\n" {
		t.Fatalf("pre-existing temp occupant after Recover = %q, %v; want byte-identical %q (rollback deleted a file the settlement never created)", b, err, "precious\n")
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after Recover = %s, want %s (rollback disturbed a tree the failed verb never wrote)", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("failed select left %d facts, want 0", len(facts))
	}
}

// TestSelectRefusesPreExistingLinkTempOccupant (issue #9,
// symlink kind): the same shape on the symlink writer — a
// foreign file stands at the derived LINK temp name for a
// symlink write. Symlink creation refuses it descriptively,
// and Recover leaves it byte-identical.
func TestSelectRefusesPreExistingLinkTempOccupant(t *testing.T) {
	f := newJournalFixture(t)

	victim := filepath.Join(f.parentDir, derivedTempName(filepath.Join(f.parentDir, "link.txt"), "op-own", true))
	journalWriteFile(t, victim, "precious link\n", 0o644)
	preHash := hashTree(t, f.parentDir)

	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := os.Symlink("keep.txt", filepath.Join(layer.Dir, "link.txt")); err != nil {
		t.Fatalf("create layer link.txt: %v", err)
	}
	advanceRun(t, f, "run-a", layer.Dir)

	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-own"); err == nil {
		t.Fatal("Select with a pre-existing link-temp occupant succeeded; want a descriptive refusal")
	} else if !strings.Contains(err.Error(), victim) {
		t.Fatalf("Select refusal = %v, want it to name the occupied temp path %s", err, victim)
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "precious link\n" {
		t.Fatalf("pre-existing link-temp occupant after Recover = %q, %v; want byte-identical %q (rollback deleted a file the settlement never created)", b, err, "precious link\n")
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after Recover = %s, want %s", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
}

// TestRecoverLeavesUnrecordedTempDebrisAndRetryRefuses pins
// the completability cell the journaled-ownership discipline
// deliberately accepts: a kill in the narrow window between
// a temp's creation and its ownership record leaves the
// operation's OWN temp standing with no record proving the
// settlement created it. Rollback must not delete it — the
// record's absence is treated as unowned, the same rule that
// protects a foreign occupant — so the journal converges
// with the debris left standing, and the retry's outcome is
// a descriptive refusal at the debris' name (its exclusive
// creation will not adopt it), never a silent overwrite and
// never a foreign deletion. Refused, never guessed.
func TestRecoverLeavesUnrecordedTempDebrisAndRetryRefuses(t *testing.T) {
	f := newJournalFixture(t)

	// The debris: the killed write's own temp, partially
	// written, the rename never run, the destination
	// untouched — and the journal carries the unit started
	// with NO temp-created record (the kill struck between
	// the creation and the record).
	debris := filepath.Join(f.parentDir, derivedTempName(filepath.Join(f.parentDir, "mod.txt"), "op-debris", false))
	journalWriteFile(t, debris, "run mo", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-debris", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	writeRawJournal(t, f, "op-debris", []canonical.Value{
		rawUnit("mod.txt", unitWrite, false, true),
	})
	preHash := hashTree(t, f.parentDir)

	// Recover converges the journal and leaves the unrecorded
	// debris standing: absence of the record means unowned.
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assertRolledBackToPre(t, f, "op-debris", preHash)
	if b, err := os.ReadFile(debris); err != nil || string(b) != "run mo" {
		t.Fatalf("unrecorded temp debris after Recover = %q, %v; want left standing byte-identical", b, err)
	}

	// The retry refuses descriptively at the debris' name:
	// its exclusive creation will not adopt the debris, and
	// nothing removes it on the retry's behalf either.
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "run mod\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)
	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-debris"); err == nil {
		t.Fatal("retry Select over unrecorded temp debris succeeded; want a descriptive refusal")
	} else if !strings.Contains(err.Error(), debris) {
		t.Fatalf("retry refusal = %v, want it to name the debris temp path %s", err, debris)
	}
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover after refused retry: %v", err)
	}
	if b, err := os.ReadFile(debris); err != nil || string(b) != "run mo" {
		t.Fatalf("unrecorded temp debris after refused retry and Recover = %q, %v; want left standing byte-identical", b, err)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after final Recover: err = %v, want gone (converged, never wedged)", err)
	}
}

// TestRecoverRetriesRestoreOverRecordedRestoreTemp pins the
// kill-inside-restore shape: a Recover rolling back a delete
// unit restores the preimage through a derived-name temp,
// and a kill between that temp's creation and its rename
// used to wedge every later Recover — no cleanup pass visits
// a delete unit's restore temp, and the next restore retried
// into the same name and failed on the collision. Rollback
// now persists the unit's temp-created record for its own
// restore temps (the record below), so the repeated restore
// may remove the recorded temp and retry creation once. The
// fixture is the state that kill leaves, built by hand the
// way the wave-9 fixtures build theirs: the verb's delete
// landed, the first restore's temp stands at the derived
// name under the journal's op, the record is in the journal,
// the destination is still absent, the preimage still
// staged.
func TestRecoverRetriesRestoreOverRecordedRestoreTemp(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	// The verb's delete landed.
	if err := os.Remove(filepath.Join(f.parentDir, "mod.txt")); err != nil {
		t.Fatalf("remove mod.txt: %v", err)
	}
	// The killed restore's temp, carrying the preimage bytes
	// it was about to rename into place.
	tempPath := filepath.Join(f.parentDir, derivedTempName(filepath.Join(f.parentDir, "mod.txt"), "op-restore", false))
	journalWriteFile(t, tempPath, "original\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-restore", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	writeRawJournal(t, f, "op-restore", []canonical.Value{
		rawUnitWithTemp("mod.txt", unitDelete, true, true),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assertRolledBackToPre(t, f, "op-restore", preHash)
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("recorded restore temp after Recover: err = %v, want consumed by the retried restore", err)
	}
}

// TestRecoverRefusesRestoreOverUnrecordedTemp: the same
// killed-restore shape with NO temp-created record for the
// unit. The temp standing at the derived name is unproven —
// it may be a genuine file that predates the settlement —
// so the restore's collision stays a descriptive refusal:
// the occupant survives byte-identical and the journal
// stays in place as evidence. Foreign until proven owned.
func TestRecoverRefusesRestoreOverUnrecordedTemp(t *testing.T) {
	f := newJournalFixture(t)

	// The verb's delete landed.
	if err := os.Remove(filepath.Join(f.parentDir, "mod.txt")); err != nil {
		t.Fatalf("remove mod.txt: %v", err)
	}
	occupant := filepath.Join(f.parentDir, derivedTempName(filepath.Join(f.parentDir, "mod.txt"), "op-restore", false))
	journalWriteFile(t, occupant, "precious\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-restore", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	writeRawJournal(t, f, "op-restore", []canonical.Value{
		rawUnit("mod.txt", unitDelete, true, true),
	})

	err := Recover(f.ctx, "parent")
	if err == nil {
		t.Fatal("Recover over an unrecorded restore-temp occupant succeeded; want a descriptive refusal")
	}
	if !strings.Contains(err.Error(), occupant) {
		t.Fatalf("Recover refusal = %v, want it to name the occupied temp path %s", err, occupant)
	}
	if b, rerr := os.ReadFile(occupant); rerr != nil || string(b) != "precious\n" {
		t.Fatalf("unrecorded temp occupant after refused Recover = %q, %v; want byte-identical %q (the restore removed a file the settlement never proved it created)", b, rerr, "precious\n")
	}
	if _, serr := os.Stat(journalPath(f.root, "parent")); serr != nil {
		t.Fatalf("journal after refused Recover: %v, want still present as evidence", serr)
	}
}
