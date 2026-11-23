package settle

// Spare-on-absent tests (PR #11 Sourcery re-review round 2,
// panel ruling on F3, replacing the identity fix's
// flag-force judgment call): a per-kind temp-created record
// carrying the flag but NO identity — the form journals
// written before temp identities were recorded carry —
// proves nothing about the occupant at the derived name.
// The flag says a temp was created; it cannot say the file
// standing there now is that temp. No identity proof, no
// deletion: at BOTH removal sites (removeUnitTemps and
// restorePreimage's recorded-temp collision retry) a
// flag-only record spares the occupant, exactly as an
// unrecorded name is spared.
//
// Sparing creates one hazard the fixtures below pin shut:
// a spared GENUINE write temp stands at the write temp's
// derived name, and the same unit's restore — which must
// write the preimage back through a temp of its own —
// would collide with it at that name, and a retry that
// also spares would wedge recovery on the unit forever.
// The restore therefore derives its temp name distinctly
// (the restore derivation), removeUnitTemps cleans that
// name under the restore's record as well, and a spared
// write temp can never block a restore.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// TestRecoverSparesForeignOccupantUnderFlagOnlyRecord
// (removeUnitTemps site): a flag-only journal whose unit's
// write landed, and a foreign file occupying the write
// temp's derived name. The record cannot prove the
// occupant is the settlement's temp — under the flag-force
// reading this fixture runs against first, Recover deleted
// it on the flag alone. Recover must spare it
// byte-identical, restore the destination from its
// preimage around it, and clear the journal.
func TestRecoverSparesForeignOccupantUnderFlagOnlyRecord(t *testing.T) {
	f := newJournalFixture(t)

	dst := filepath.Join(f.parentDir, "mod.txt")
	occupant := filepath.Join(f.parentDir, derivedTempName(dst, "op-spare", false))
	journalWriteFile(t, occupant, "precious\n", 0o644)
	// The unit's write landed before the kill.
	journalWriteFile(t, dst, "run mod\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-spare", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	writeRawJournal(t, f, "op-spare", []canonical.Value{
		rawUnitWithTemp("mod.txt", unitWrite, false, true, false),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if b, err := os.ReadFile(occupant); err != nil || string(b) != "precious\n" {
		t.Fatalf("foreign occupant at the write temp's name after Recover = %q, %v; want byte-identical %q (a flag-only record proves no ownership — no identity proof, no deletion)", b, err, "precious\n")
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "original\n" {
		t.Fatalf("mod.txt after Recover = %q, %v; want the restored preimage %q (the undo completes around the spared occupant)", got, err, "original\n")
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rollback minted %d facts, want 0", len(facts))
	}
}

// TestRecoverRefusesRestoreOverFlagOnlyOccupantAtRestoreName
// (restorePreimage site): a flag-only journal for a delete
// unit whose restore must write the preimage back, and a
// foreign file occupying the RESTORE temp's derived name.
// The restore's exclusive creation collides there; the
// recorded-temp retry is gated on the same record, and a
// flag-only record proves no ownership — so the retry
// spares the occupant and the collision keeps the
// descriptive refusal the unrecorded collision has: the
// occupant intact, the journal in place as evidence. (The
// restore's name is its own derivation: an occupant at the
// WRITE temp's name would not even be met by the restore —
// see TestRecoverCompletesBesideLegacyRestoreTemp.)
func TestRecoverRefusesRestoreOverFlagOnlyOccupantAtRestoreName(t *testing.T) {
	f := newJournalFixture(t)

	// The verb's delete landed.
	if err := os.Remove(filepath.Join(f.parentDir, "mod.txt")); err != nil {
		t.Fatal(err)
	}
	occupant := filepath.Join(f.parentDir, derivedRestoreTempName(filepath.Join(f.parentDir, "mod.txt"), "op-rspare"))
	journalWriteFile(t, occupant, "precious\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-rspare", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	writeRawJournal(t, f, "op-rspare", []canonical.Value{
		rawUnitWithTemp("mod.txt", unitDelete, true, true, false),
	})

	err := Recover(f.ctx, "parent")
	if err == nil {
		t.Fatal("Recover restored over a foreign occupant at the restore temp's name on a flag-only record; want a descriptive refusal")
	}
	if !strings.Contains(err.Error(), occupant) {
		t.Fatalf("Recover refusal = %v, want it to name the occupied restore temp path %s", err, occupant)
	}
	if b, rerr := os.ReadFile(occupant); rerr != nil || string(b) != "precious\n" {
		t.Fatalf("foreign occupant after refused Recover = %q, %v; want byte-identical %q (a flag-only record is no warrant to delete)", b, rerr, "precious\n")
	}
	if _, serr := os.Stat(journalPath(f.root, "parent")); serr != nil {
		t.Fatalf("journal after refused Recover: %v, want still present as evidence", serr)
	}
}

// TestRecoverFlagOnlyGenuineTempSparesAndRestoreCompletes
// is the no-wedge pin: a flag-only journal, the unit's
// GENUINE temp standing at the write temp's derived name
// (partial content, the kill inside its write), and a
// restore that must actually WRITE — the destination
// serves the preimage's bytes at the wrong mode, so the
// already-serving shortcut does not fire and the restore
// has to land the preimage through a temp. Sparing the
// write temp (no identity proof) must not wedge the
// restore: the restore derives its own temp name, lands,
// and Recover completes fully — temp spared byte-identical,
// destination restored with the preimage's mode, journal
// cleared. Against the flag-force code this fixture fails
// on the spared temp (deleted); against a naive spare-only
// flip — spare, but the restore still deriving the write
// name — it fails on the wedge (the restore collides with
// the spared temp and the retry spares it too, refusing
// forever).
func TestRecoverFlagOnlyGenuineTempSparesAndRestoreCompletes(t *testing.T) {
	f := newJournalFixture(t)

	dst := filepath.Join(f.parentDir, "mod.txt")
	tempPath := filepath.Join(f.parentDir, derivedTempName(dst, "op-nwedge", false))
	journalWriteFile(t, tempPath, "run mo", 0o644)
	// The destination serves the preimage's bytes at the
	// wrong mode: the restore cannot take its shortcut.
	if err := os.Chmod(dst, 0o644); err != nil {
		t.Fatal(err)
	}
	preDir := filepath.Join(f.root, "staging", "op-nwedge", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o755)

	writeRawJournal(t, f, "op-nwedge", []canonical.Value{
		rawUnitWithTemp("mod.txt", unitWrite, false, true, false),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v (a spared genuine write temp must never wedge the unit's restore)", err)
	}
	if b, err := os.ReadFile(tempPath); err != nil || string(b) != "run mo" {
		t.Fatalf("genuine write temp after Recover = %q, %v; want left standing byte-identical %q (spared: no identity proof)", b, err, "run mo")
	}
	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mod.txt mode after Recover = %v, want the preimage's 0755 (the restore landed, not shortcut)", info.Mode().Perm())
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "original\n" {
		t.Fatalf("mod.txt after Recover = %q, %v; want the restored preimage %q", got, err, "original\n")
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone (completed fully, no wedge)", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rollback minted %d facts, want 0", len(facts))
	}
}

// TestRecoverCompletesBesideLegacyRestoreTemp: the
// flag-only killed-restore state of the previous
// generation — a first recovery's restore (which derived
// its temp from the WRITE name then) was killed with its
// temp standing at that name, the record flag-only, the
// destination still absent. The restore now derives its
// own name, so Recover completes beside the legacy temp:
// the destination is restored, the journal clears, and the
// legacy temp stands as debris — spared, since the
// flag-only record proves no ownership of it either.
func TestRecoverCompletesBesideLegacyRestoreTemp(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	// The verb's delete landed.
	if err := os.Remove(filepath.Join(f.parentDir, "mod.txt")); err != nil {
		t.Fatalf("remove mod.txt: %v", err)
	}
	// The killed legacy restore's temp at the WRITE-derived
	// name, carrying the preimage bytes it was about to
	// rename into place.
	tempPath := filepath.Join(f.parentDir, derivedTempName(filepath.Join(f.parentDir, "mod.txt"), "op-legacy-restore", false))
	journalWriteFile(t, tempPath, "original\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-legacy-restore", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	writeRawJournal(t, f, "op-legacy-restore", []canonical.Value{
		rawUnitWithTemp("mod.txt", unitDelete, true, true, false),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v (the restore derives its own name now; the legacy temp cannot wedge it)", err)
	}
	if got, err := os.ReadFile(filepath.Join(f.parentDir, "mod.txt")); err != nil || string(got) != "original\n" {
		t.Fatalf("mod.txt after Recover = %q, %v; want the restored preimage", got, err)
	}
	if b, err := os.ReadFile(tempPath); err != nil || string(b) != "original\n" {
		t.Fatalf("legacy restore temp after Recover = %q, %v; want left standing byte-identical (spared debris of the accepted class)", b, err)
	}
	if err := os.Remove(tempPath); err != nil {
		t.Fatal(err)
	}
	assertRolledBackToPre(t, f, "op-legacy-restore", preHash)
}

// TestRecoverCleansRecordedRestoreTempOfWriteUnit: a write
// unit's undo was itself killed mid-restore — the first
// Recover removed the unit's write temp, began restoring
// its preimage, recorded the restore temp (flag and the
// preimage's identity: the record re-binds to the temp
// that may now stand), and died with the restore temp
// standing at the restore-derived name and the destination
// already removed. removeUnitTemps cleans the restore name
// under that record — verified against the recorded
// identity — and the retried restore completes.
func TestRecoverCleansRecordedRestoreTempOfWriteUnit(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	dst := filepath.Join(f.parentDir, "mod.txt")
	// The killed undo removed the destination's occupant
	// and died with its restore temp standing.
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(f.parentDir, derivedRestoreTempName(dst, "op-rstclean"))
	journalWriteFile(t, tempPath, "original\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-rstclean", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	writeRawJournal(t, f, "op-rstclean", []canonical.Value{
		rawUnitWithTempIdentity("mod.txt", unitWrite, true, true, false, tempFileIdentityOf("original\n")),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("recorded restore temp after Recover: err = %v, want cleaned under its record (verified against the recorded identity)", err)
	}
	assertRolledBackToPre(t, f, "op-rstclean", preHash)
}
