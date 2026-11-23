package settle

// Temp-identity tests (PR #11 Sourcery re-review, finding
// F3): the per-kind temp-created record proves a temp of
// that kind was created for the unit — but the record can
// outlive the temp it vouches for. If the temp is removed
// or renamed by another actor, or the writer's ownership
// callback reports an error after the journal rename has
// already landed the record (writeJournal renames before
// its final directory sync; the writer then aborts and
// removes its temp, leaving the durable record standing
// over nothing), a different file can later occupy the
// derived name — and rollback, gating on the flag alone,
// deletes it: a file the settlement did not create, lost on
// a warrant whose object no longer exists.
//
// The record therefore carries the temp's identity,
// recorded in the same journal write as the flag itself —
// the flag cannot stand without it. For a file temp the
// identity is the digest and size of the content the temp
// was created to carry (the writer holds those bytes before
// creation; recording them costs no second read); for a
// link temp it is the link's target. Cleanup verifies the
// occupant against the recorded identity before removing
// it: an exact content match, or — for the kill that lands
// inside the temp's single content write — a strict prefix
// of the very content the identity names, checked against
// the settlement's own staged source. A mismatch is left
// standing and the undo proceeds around it; the restore
// path's recorded-temp collision applies the same
// verification, and a foreign occupant there keeps the
// descriptive refusal the unrecorded collision has.
//
// Journals written before identities were recorded carry
// the flag alone; their records prove nothing about the
// occupant, and cleanup spares it — no identity proof, no
// deletion (PR #11 Sourcery re-review round 2, ruling on
// F3; the spare fixtures live in journal_temp_spare_test.go
// and the converted killed-write fixtures in
// journal_temp_test.go) — while every journal the writer
// produces now binds the identity.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// tempFileIdentityOf computes a file temp's recorded
// identity in the test's own terms: the digest-and-size
// form the write path records for the content a temp is
// created to carry.
func tempFileIdentityOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:]) + ":" + strconv.Itoa(len(content))
}

// rawUnitWithTempIdentity builds one journal unit carrying
// the per-kind temp-created record AND that kind's temp
// identity, in the canonical on-disk form the writer now
// produces: the record and the identity land in one journal
// write, so a unit in this form is what a kill after the
// record leaves behind.
func rawUnitWithTempIdentity(path, kind string, done, started, link bool, identity string) canonical.Value {
	pairs := []canonical.Pair{
		{Key: "done", Value: canonical.BoolOf(done)},
		{Key: "kind", Value: canonical.StringOf(kind)},
		{Key: "path", Value: canonical.StringOf(path)},
		{Key: "started", Value: canonical.BoolOf(started)},
		{Key: "temp_created_file", Value: canonical.BoolOf(!link)},
		{Key: "temp_created_link", Value: canonical.BoolOf(link)},
	}
	if link {
		pairs = append(pairs, canonical.Pair{Key: "temp_link_identity", Value: canonical.StringOf(identity)})
	} else {
		pairs = append(pairs, canonical.Pair{Key: "temp_file_identity", Value: canonical.StringOf(identity)})
	}
	return canonical.ObjectOf(pairs...)
}

// stageTreeFile stages the result-tree file a write unit
// lands from — the settlement's own source bytes for the
// unit, which staging holds until the journal clears.
func stageTreeFile(t *testing.T, f *journalFixture, op, rel, content string) {
	t.Helper()
	journalWriteFile(t, filepath.Join(f.root, "staging", op, "tree", filepath.FromSlash(rel)), content, 0o644)
}

// TestRecoverSparesForeignOccupantAfterTempRenamedAway:
// the settlement's recorded temp was renamed away by
// another actor before recovery, and a foreign file now
// occupies the derived name. The record stands, but it
// vouches for the renamed temp — not for this occupant.
// Recover must leave the occupant byte-identical, converge
// the rollback around it, and leave the renamed temp itself
// standing too (no cleanup pass names it; rollback does not
// sweep).
func TestRecoverSparesForeignOccupantAfterTempRenamedAway(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	dst := filepath.Join(f.parentDir, "mod.txt")
	tmpName := filepath.Join(f.parentDir, derivedTempName(dst, "op-tident", false))
	renamed := filepath.Join(f.parentDir, "moved-temp")
	journalWriteFile(t, renamed, "run mod\n", 0o644)
	journalWriteFile(t, tmpName, "precious\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-tident", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	stageTreeFile(t, f, "op-tident", "mod.txt", "run mod\n")

	writeRawJournal(t, f, "op-tident", []canonical.Value{
		rawUnitWithTempIdentity("mod.txt", unitWrite, false, true, false, tempFileIdentityOf("run mod\n")),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if b, err := os.ReadFile(tmpName); err != nil || string(b) != "precious\n" {
		t.Fatalf("foreign occupant at the derived name after Recover = %q, %v; want byte-identical %q (the record vouched for a temp that had been renamed away, not for this file)", b, err, "precious\n")
	}
	if b, err := os.ReadFile(renamed); err != nil || string(b) != "run mod\n" {
		t.Fatalf("renamed-away temp after Recover = %q, %v; want left standing byte-identical %q (rollback removes only what it can name and verify)", b, err, "run mod\n")
	}
	if err := os.Remove(tmpName); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	assertRolledBackToPre(t, f, "op-tident", preHash)
}

// TestRecoverSparesOccupantAfterRecordOutlivedTemp pins
// the callback-error sub-shape: the ownership record's
// journal rename landed, the write then reported an error,
// and the writer aborted and removed its own temp — the
// durable journal holds the flag and the identity with NO
// temp standing. A file that later occupies the derived
// name is foreign to the settlement, and Recover must spare
// it exactly as in the renamed-away shape: the record
// outlived its temp, so the flag alone is not proof.
func TestRecoverSparesOccupantAfterRecordOutlivedTemp(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	dst := filepath.Join(f.parentDir, "mod.txt")
	tmpName := filepath.Join(f.parentDir, derivedTempName(dst, "op-cberr", false))
	journalWriteFile(t, tmpName, "later file\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-cberr", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	stageTreeFile(t, f, "op-cberr", "mod.txt", "run mod\n")

	writeRawJournal(t, f, "op-cberr", []canonical.Value{
		rawUnitWithTempIdentity("mod.txt", unitWrite, false, true, false, tempFileIdentityOf("run mod\n")),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if b, err := os.ReadFile(tmpName); err != nil || string(b) != "later file\n" {
		t.Fatalf("occupant at the derived name after Recover = %q, %v; want byte-identical %q (the record outlived the temp it vouched for)", b, err, "later file\n")
	}
	if err := os.Remove(tmpName); err != nil {
		t.Fatal(err)
	}
	assertRolledBackToPre(t, f, "op-cberr", preHash)
}

// TestRecoverSparesInPlaceSwappedTempContent: the recorded
// temp still stands at its name, but its bytes were swapped
// in place — same name, same size, different content. The
// occupant is not the temp the record names, and Recover
// must leave it byte-identical while converging around it.
func TestRecoverSparesInPlaceSwappedTempContent(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	dst := filepath.Join(f.parentDir, "mod.txt")
	tmpName := filepath.Join(f.parentDir, derivedTempName(dst, "op-swap", false))
	journalWriteFile(t, tmpName, "RUN MOD\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-swap", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	stageTreeFile(t, f, "op-swap", "mod.txt", "run mod\n")

	writeRawJournal(t, f, "op-swap", []canonical.Value{
		rawUnitWithTempIdentity("mod.txt", unitWrite, false, true, false, tempFileIdentityOf("run mod\n")),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if b, err := os.ReadFile(tmpName); err != nil || string(b) != "RUN MOD\n" {
		t.Fatalf("in-place swapped temp after Recover = %q, %v; want byte-identical %q (the name and the record survived; the content is not the recorded temp's)", b, err, "RUN MOD\n")
	}
	if err := os.Remove(tmpName); err != nil {
		t.Fatal(err)
	}
	assertRolledBackToPre(t, f, "op-swap", preHash)
}

// TestRecoverSparesReplacedLinkTempTarget: a symlink
// write's recorded link temp was replaced by a link to a
// different target. The record vouches for the link it
// created — the one naming the recorded target — and
// Recover must leave the replacement link standing.
func TestRecoverSparesReplacedLinkTempTarget(t *testing.T) {
	f := newJournalFixture(t)

	dst := filepath.Join(f.parentDir, "link.txt")
	tmpName := filepath.Join(f.parentDir, derivedTempName(dst, "op-tlink", true))
	if err := os.Symlink("other.txt", tmpName); err != nil {
		t.Fatal(err)
	}

	writeRawJournal(t, f, "op-tlink", []canonical.Value{
		rawUnitWithTempIdentity("link.txt", unitWrite, false, true, true, "keep.txt"),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	target, err := os.Readlink(tmpName)
	if err != nil || target != "other.txt" {
		t.Fatalf("replaced link temp after Recover = %q, %v; want a link to %q left standing (the record vouches for a link to keep.txt, not this one)", target, err, "other.txt")
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
}

// TestRecoverCleansGenuineTempWithRecordedIdentity: the
// recorded temp stands at its name with the very content it
// was created to carry (a kill between its content write
// and its rename). Verification passes on the recorded
// identity and Recover removes it as it converges.
func TestRecoverCleansGenuineTempWithRecordedIdentity(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	dst := filepath.Join(f.parentDir, "mod.txt")
	tmpName := filepath.Join(f.parentDir, derivedTempName(dst, "op-genuine", false))
	journalWriteFile(t, tmpName, "run mod\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-genuine", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	stageTreeFile(t, f, "op-genuine", "mod.txt", "run mod\n")

	writeRawJournal(t, f, "op-genuine", []canonical.Value{
		rawUnitWithTempIdentity("mod.txt", unitWrite, false, true, false, tempFileIdentityOf("run mod\n")),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, err := os.Stat(tmpName); !os.IsNotExist(err) {
		t.Fatalf("genuine recorded temp after Recover: err = %v, want removed (its content matches the recorded identity)", err)
	}
	assertRolledBackToPre(t, f, "op-genuine", preHash)
}

// TestRecoverCleansPartialTempWithRecordedIdentity: the
// kill landed inside the temp's single content write, so
// the temp stands holding a strict prefix of the content
// its record names. The prefix is checked against the
// settlement's own staged source for the unit — the only
// bytes the identity can name — and the temp is removed as
// the settlement's own.
func TestRecoverCleansPartialTempWithRecordedIdentity(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	dst := filepath.Join(f.parentDir, "mod.txt")
	tmpName := filepath.Join(f.parentDir, derivedTempName(dst, "op-partial", false))
	journalWriteFile(t, tmpName, "run mo", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-partial", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	stageTreeFile(t, f, "op-partial", "mod.txt", "run mod\n")

	writeRawJournal(t, f, "op-partial", []canonical.Value{
		rawUnitWithTempIdentity("mod.txt", unitWrite, false, true, false, tempFileIdentityOf("run mod\n")),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, err := os.Stat(tmpName); !os.IsNotExist(err) {
		t.Fatalf("partial recorded temp after Recover: err = %v, want removed (a strict prefix of the recorded content, verified against the staged source)", err)
	}
	assertRolledBackToPre(t, f, "op-partial", preHash)
}

// TestRecoverRefusesRestoreOverForeignTempOccupant: the
// fold-in restore path under identity. A first Recover's
// restore recorded its temp (flag and identity, the
// preimage's content) and died; the temp was then removed
// and a foreign file placed at the derived name. The next
// Recover's restore collides at that name: the occupant is
// not the recorded temp, so the collision keeps the
// descriptive refusal the unrecorded collision has — the
// occupant intact, the journal in place as evidence.
func TestRecoverRefusesRestoreOverForeignTempOccupant(t *testing.T) {
	f := newJournalFixture(t)

	// The verb's delete landed.
	if err := os.Remove(filepath.Join(f.parentDir, "mod.txt")); err != nil {
		t.Fatal(err)
	}
	occupant := filepath.Join(f.parentDir, derivedRestoreTempName(filepath.Join(f.parentDir, "mod.txt"), "op-rident"))
	journalWriteFile(t, occupant, "precious\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-rident", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	writeRawJournal(t, f, "op-rident", []canonical.Value{
		rawUnitWithTempIdentity("mod.txt", unitDelete, true, true, false, tempFileIdentityOf("original\n")),
	})

	err := Recover(f.ctx, "parent")
	if err == nil {
		t.Fatal("Recover restored over a foreign occupant at the recorded restore temp's name; want a descriptive refusal")
	}
	if !strings.Contains(err.Error(), occupant) {
		t.Fatalf("Recover refusal = %v, want it to name the occupied temp path %s", err, occupant)
	}
	if b, rerr := os.ReadFile(occupant); rerr != nil || string(b) != "precious\n" {
		t.Fatalf("foreign occupant after refused Recover = %q, %v; want byte-identical %q (the restore removed a file its record does not vouch for)", b, rerr, "precious\n")
	}
	if _, serr := os.Stat(journalPath(f.root, "parent")); serr != nil {
		t.Fatalf("journal after refused Recover: %v, want still present as evidence", serr)
	}
}
