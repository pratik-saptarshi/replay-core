package settle

// Temp-file recovery tests (PR #7 wave 9, Codex P2 on
// a5a102b): a write unit lands through a temp file in the
// destination directory — created, written, renamed over the
// target. A kill after the temp is created but before the
// rename leaves the temp in the parent tree: the deferred
// removal never runs, and the journal records only the
// destination path, so rollback restores or removes the
// destination, clears the journal, and leaves the temp
// behind. Recovery then has not restored the pre-operation
// file map, and a later Apply can snapshot the leftover temp
// and preserve it as a parent change.
//
// The fix makes the temp's name a pure function of the
// destination path and the operation ID the journal already
// records — the file temp is ".synctree-" + the first 32
// hex chars of the SHA-256 of the destination and the op,
// NUL-separated, the symlink temp ".synctree-link-" + the
// same — so the name is knowable from durable state
// before any temp exists, and rollback removes the temps of
// done and started write units beside undoing the units
// themselves. (The post-merge fix wave added the ownership
// gate on that removal — issue #9: a unit's journal must
// carry the temp-created record the writer persists at
// creation, which is why the fixtures below build their
// units with rawUnitWithTemp — and folded the op ID into
// the derivation, so concurrent operations on one
// destination never share a temp name; the destination-only
// derivation it replaced is pinned as underivable debris by
// the legacy fixture below.) The fixtures below plant temps under those
// literal derived names (computed here, independently of the
// production helper) beside hand-built journals in the
// current on-disk format, so they compile and run against
// the unfixed code, where rollback leaves the temps and the
// damage is observable.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// derivedTempName computes the temp name the write path uses
// for a destination under an operation, in the test's own
// terms: the pin that the production derivation and the
// recovery derivation are one function of the recorded
// destination path and the journal's op.
func derivedTempName(dst, op string, link bool) string {
	sum := sha256.Sum256([]byte(dst + "\x00" + op))
	prefix := ".synctree-"
	if link {
		prefix = ".synctree-link-"
	}
	return prefix + hex.EncodeToString(sum[:])[:32]
}

// derivedRestoreTempName computes the temp name rollback's
// RESTORE path uses for a destination under an operation
// (PR #11 Sourcery re-review round 2, ruling on F3): a
// derivation distinct from the write path's, so a write
// temp spared at the write name — a flag-only record proves
// no ownership, and no identity proof means no deletion —
// can never block the same unit's restore, whose exclusive
// creation would otherwise collide with it and, the retry
// sparing it too, wedge recovery on the unit forever.
func derivedRestoreTempName(dst, op string) string {
	sum := sha256.Sum256([]byte(dst + "\x00" + op + "\x00restore"))
	return ".synctree-restore-" + hex.EncodeToString(sum[:])[:32]
}

// legacyDerivedTempName computes the temp name the
// destination-only derivation (the scheme the per-op
// derivation replaced) used for a destination: the pin for
// the previous generation's names, which the current
// derivation can no longer produce and rollback must
// therefore leave standing as debris.
func legacyDerivedTempName(dst string, link bool) string {
	sum := sha256.Sum256([]byte(dst))
	prefix := ".synctree-"
	if link {
		prefix = ".synctree-link-"
	}
	return prefix + hex.EncodeToString(sum[:])[:32]
}

// rawUnitWithTemp builds one journal unit carrying the
// per-kind temp-created record for ONE temp kind (link
// selects which): the ownership record the write path
// persists between that kind's temp creation and its
// content write, and the only warrant rollback accepts
// before removing that kind's derived-name temp
// (post-merge fix wave, issue #9). The killed writes in
// this file DID create their temps — the partial content
// planted below is what they wrote — so their hand-built
// journals carry the record the writer would have left,
// for exactly the kind it created, in the same canonical
// form. (The pre-fix debris fixture below deliberately
// carries none: its writer predates the record.)
func rawUnitWithTemp(path, kind string, done, started, link bool) canonical.Value {
	return canonical.ObjectOf(
		canonical.Pair{Key: "done", Value: canonical.BoolOf(done)},
		canonical.Pair{Key: "kind", Value: canonical.StringOf(kind)},
		canonical.Pair{Key: "path", Value: canonical.StringOf(path)},
		canonical.Pair{Key: "started", Value: canonical.BoolOf(started)},
		canonical.Pair{Key: "temp_created_file", Value: canonical.BoolOf(!link)},
		canonical.Pair{Key: "temp_created_link", Value: canonical.BoolOf(link)},
	)
}

// TestRecoverSparesFileTempLeftByKilledWrite: the write of
// mod.txt was killed after its temp file was created and
// partially written but before the rename — the destination
// still serves its original bytes, the unit is started but
// not done, its preimage is captured, and the temp stands in
// the parent tree. The journal is the flag-only form: the
// per-kind record stands with NO identity beside it, the
// form journals carried before temp identities were
// recorded. No identity proof, no deletion (PR #11 Sourcery
// re-review round 2, ruling on F3): the record alone cannot
// distinguish this genuine temp from a foreign occupant of
// the same name, so Recover SPARES it — the rollback
// converges around it, the journal clears, and the temp
// stands as visible debris of the accepted class, exactly
// as an unrecorded name's occupant is spared. (The
// identity-carrying form of this same window, where the
// temp IS removed on its verified identity, is pinned by
// TestRecoverCleansGenuineTempWithRecordedIdentity and
// TestRecoverCleansPartialTempWithRecordedIdentity.)
func TestRecoverSparesFileTempLeftByKilledWrite(t *testing.T) {
	f := newJournalFixture(t)

	// The temp the killed write left: partial content, the
	// rename never ran, the destination untouched.
	tempPath := filepath.Join(f.parentDir, derivedTempName(filepath.Join(f.parentDir, "mod.txt"), "op-temp", false))
	journalWriteFile(t, tempPath, "run mo", 0o644)
	// The unit's captured preimage.
	preDir := filepath.Join(f.root, "staging", "op-temp", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	writeRawJournal(t, f, "op-temp", []canonical.Value{
		rawUnitWithTemp("mod.txt", unitWrite, false, true, false),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if b, err := os.ReadFile(tempPath); err != nil || string(b) != "run mo" {
		t.Fatalf("flag-only unit's temp after Recover = %q, %v; want left standing byte-identical %q (no identity proof, no deletion)", b, err, "run mo")
	}
	if got, err := os.ReadFile(filepath.Join(f.parentDir, "mod.txt")); err != nil || string(got) != "original\n" {
		t.Fatalf("mod.txt after Recover = %q, %v; want its untouched bytes %q", got, err, "original\n")
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone (rollback converged around the spared temp)", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "staging", "op-temp")); !os.IsNotExist(err) {
		t.Fatalf("staging after Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rollback minted %d facts, want 0", len(facts))
	}

	// The subsequent-verb leg: the spared debris stands at
	// the name a fresh Select of the same operation must
	// create exclusively, so the retry refuses descriptively
	// at the debris' name — the accepted completability cell
	// for debris no record can prove ownership of — and the
	// refusal's own rollback converges with the debris still
	// standing. Refused, never adopted, never deleted on a
	// guess.
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "run mod\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)
	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-temp"); err == nil {
		t.Fatal("fresh Select over spared flag-only debris succeeded; want a descriptive refusal at the debris' name")
	} else if !strings.Contains(err.Error(), tempPath) {
		t.Fatalf("retry refusal = %v, want it to name the debris temp path %s", err, tempPath)
	}
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover after refused retry: %v", err)
	}
	if b, err := os.ReadFile(tempPath); err != nil || string(b) != "run mo" {
		t.Fatalf("spared debris after refused retry and Recover = %q, %v; want left standing byte-identical", b, err)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after final Recover: err = %v, want gone (converged, never wedged)", err)
	}
}

// TestRecoverSparesSymlinkTempLeftByKilledWrite: the same
// window on the symlink writer, under the same flag-only
// record — the temp link was created at its derived name
// but never renamed over the target. With no identity
// recorded beside the flag, Recover spares the link as it
// rolls the started unit back: the rollback converges and
// the link stands as debris.
func TestRecoverSparesSymlinkTempLeftByKilledWrite(t *testing.T) {
	f := newJournalFixture(t)

	// The temp link the killed write left, pointing at the
	// target the rename would have installed.
	tempPath := filepath.Join(f.parentDir, derivedTempName(filepath.Join(f.parentDir, "link.txt"), "op-temp", true))
	if err := os.Symlink("keep.txt", tempPath); err != nil {
		t.Fatal(err)
	}

	writeRawJournal(t, f, "op-temp", []canonical.Value{
		rawUnitWithTemp("link.txt", unitWrite, false, true, true),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	target, err := os.Readlink(tempPath)
	if err != nil || target != "keep.txt" {
		t.Fatalf("flag-only unit's link temp after Recover = %q, %v; want a link to %q left standing (no identity proof, no deletion)", target, err, "keep.txt")
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone (rollback converged around the spared temp)", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rollback minted %d facts, want 0", len(facts))
	}
}

// TestRecoverLeavesPreFixRandomTempDebris pins two things at
// once. Backward compatibility: a journal in the current
// on-disk format (the form every journal on this branch has
// ever had) with a started unit still decodes and resolves —
// the landed mutation is undone from its preimage and the
// journal clears. And the pre-fix debris disposition: a temp
// left by the pre-fix writer's RANDOM name cannot be derived
// from anything the journal records, so recovery leaves it —
// a visible file the operator can remove — rather than
// sweeping the directory by prefix, which could not tell
// debris from a genuine file of the same shape. (This branch
// has no production deployment; no shipped tree carries such
// debris.)
func TestRecoverLeavesPreFixRandomTempDebris(t *testing.T) {
	f := newJournalFixture(t)

	// The mutation landed; the mark did not.
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "run mod\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-temp", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	// A pre-fix writer's temp: random suffix, underivable.
	debris := filepath.Join(f.parentDir, ".synctree-123456789")
	journalWriteFile(t, debris, "partial", 0o644)

	writeRawJournal(t, f, "op-temp", []canonical.Value{
		rawUnit("mod.txt", unitWrite, false, true),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(f.parentDir, "mod.txt")); err != nil || string(got) != "original\n" {
		t.Fatalf("mod.txt after Recover = %q, %v; want the restored preimage", got, err)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
	if _, err := os.Stat(debris); err != nil {
		t.Fatalf("pre-fix random temp after Recover: err = %v, want it left in place (underivable, not swept)", err)
	}
}

// TestRecoverLeavesLegacyDerivedTempDebris: the same
// disposition for the previous generation's DERIVED names.
// The destination-only derivation named a temp from the
// destination alone; the per-op derivation that replaced it
// cannot produce that name from anything the journal
// records, so a temp standing under it is debris of the
// accepted class — left standing, never swept — even for a
// unit whose temp-created record makes rollback actively
// look for the unit's temp under the current derivation.
func TestRecoverLeavesLegacyDerivedTempDebris(t *testing.T) {
	f := newJournalFixture(t)

	// The mutation landed; the mark did not.
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "run mod\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-temp", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	// The previous generation's temp for the same
	// destination, under the destination-only derivation.
	debris := filepath.Join(f.parentDir, legacyDerivedTempName(filepath.Join(f.parentDir, "mod.txt"), false))
	journalWriteFile(t, debris, "partial", 0o644)

	writeRawJournal(t, f, "op-temp", []canonical.Value{
		rawUnitWithTemp("mod.txt", unitWrite, false, true, false),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(f.parentDir, "mod.txt")); err != nil || string(got) != "original\n" {
		t.Fatalf("mod.txt after Recover = %q, %v; want the restored preimage", got, err)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
	if b, err := os.ReadFile(debris); err != nil || string(b) != "partial" {
		t.Fatalf("legacy-derived temp after Recover = %q, %v; want left standing byte-identical (underivable under the current derivation, not swept)", b, err)
	}
}
