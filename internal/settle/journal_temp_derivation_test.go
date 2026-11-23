package settle

// Per-derivation temp ownership tests (PR #11, Codex P2 on
// the Sourcery round-2 head): the restore temp's derivation
// is distinct from the write temp's, but through the F3 fix
// the two names shared ONE file-kind ownership record — the
// record re-bound when a restore created its temp, and
// before any restore existed it held the WRITE temp's
// identity. removeUnitTemps applied that identity to BOTH
// names, so a write-only record authorized deleting the
// occupant at the RESTORE-derived name whenever the
// occupant's bytes matched the write content or any strict
// prefix of it (the empty file included): a pre-existing
// file with no unit and no preimage, removed on a warrant
// issued for a different name, with no concurrent callers.
//
// The record is therefore per derivation: the write record
// vouches only for the write-derived name, and the restore
// temp carries its own record — flag and identity, recorded
// by rollback's restore hook and only by it. A unit with no
// restore record has no temp removed at the restore name,
// exactly as an unrecorded name's occupant is spared.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// plantWriteOnlyRecordWithRestoreOccupant builds the
// Codex shape: a write of mod.txt killed after its temp was
// created and partially written but before the rename — the
// destination still serves its original bytes, the unit is
// started but not done, the journal carries the write
// record WITH the write content's identity, the write temp
// itself stands with partial content — and a foreign file
// occupies the RESTORE-derived name, a name no restore has
// ever created a temp at for this unit. It returns the
// fixture and the paths the assertions judge.
func plantWriteOnlyRecordWithRestoreOccupant(t *testing.T, op, occupantContent string) (f *journalFixture, dst, writeTemp, restoreOccupant string) {
	t.Helper()
	f = newJournalFixture(t)

	dst = filepath.Join(f.parentDir, "mod.txt")
	writeTemp = filepath.Join(f.parentDir, derivedTempName(dst, op, false))
	journalWriteFile(t, writeTemp, "run mo", 0o644)
	restoreOccupant = filepath.Join(f.parentDir, derivedRestoreTempName(dst, op))
	journalWriteFile(t, restoreOccupant, occupantContent, 0o644)
	preDir := filepath.Join(f.root, "staging", op, "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	stageTreeFile(t, f, op, "mod.txt", "run mod\n")

	writeRawJournal(t, f, op, []canonical.Value{
		rawUnitWithTempIdentity("mod.txt", unitWrite, false, true, false, tempFileIdentityOf("run mod\n")),
	})
	return f, dst, writeTemp, restoreOccupant
}

// assertWriteOnlyRecordSparedRestoreOccupant runs Recover
// over the planted shape and pins the disposition: the
// foreign occupant at the restore-derived name survives
// byte-identical, the unit's own write temp is cleaned
// under its own record, and the undo otherwise completes.
func assertWriteOnlyRecordSparedRestoreOccupant(t *testing.T, op, occupantContent string) {
	t.Helper()
	f, dst, writeTemp, restoreOccupant := plantWriteOnlyRecordWithRestoreOccupant(t, op, occupantContent)

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if b, err := os.ReadFile(restoreOccupant); err != nil || string(b) != occupantContent {
		t.Fatalf("foreign occupant at the restore-derived name after Recover = %q, %v; want byte-identical %q (a write-only record vouches for the write temp's name only — no restore ever created a temp at this name)", b, err, occupantContent)
	}
	if _, err := os.Stat(writeTemp); !os.IsNotExist(err) {
		t.Fatalf("unit's own write temp after Recover: err = %v, want removed under its own record (verified against the recorded identity)", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "original\n" {
		t.Fatalf("mod.txt after Recover = %q, %v; want the preimage %q (the undo completes around the spared occupant)", got, err, "original\n")
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rollback minted %d facts, want 0", len(facts))
	}
}

// TestRecoverSparesRestoreNameOccupantMatchingWritePrefix:
// the occupant's content is a strict prefix of the write
// content — the shape the Codex reproduction deleted
// through the identity check's prefix leg, judged against
// the staged write source under the write record.
func TestRecoverSparesRestoreNameOccupantMatchingWritePrefix(t *testing.T) {
	assertWriteOnlyRecordSparedRestoreOccupant(t, "op-dprefix", "run")
}

// TestRecoverSparesRestoreNameOccupantMatchingWriteContent:
// the occupant's content EQUALS the write content, so its
// recomputed identity is the recorded identity itself —
// the exact-match leg must not authorize a name the
// record's derivation never created either.
func TestRecoverSparesRestoreNameOccupantMatchingWriteContent(t *testing.T) {
	assertWriteOnlyRecordSparedRestoreOccupant(t, "op-dequal", "run mod\n")
}

// TestRecoverSparesEmptyRestoreNameOccupantUnderWriteRecord:
// the empty file — a strict prefix of every content — is
// the widest form of the same shape.
func TestRecoverSparesEmptyRestoreNameOccupantUnderWriteRecord(t *testing.T) {
	assertWriteOnlyRecordSparedRestoreOccupant(t, "op-dempty", "")
}
