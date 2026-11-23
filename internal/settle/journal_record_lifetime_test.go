package settle

// Record-lifetime and parent-root tests (PR #11 review
// round): two ways the temp-ownership record and the
// journal's parent directory could outlive the state they
// vouch for.
//
// Record lifetime: a unit's per-kind temp-created record
// is set when its temp is created and, in the code these
// fixtures run against first, never cleared. But a record
// may only ever vouch for a temp that may still stand.
// Once a write unit is marked done, its temp has been
// renamed onto the destination — it cannot still stand —
// and once a unit's undo completes in rollback, its
// restore temp has likewise been renamed into place. A
// record that survives either completion vouches for
// whatever later comes to occupy the derived name, and a
// later rollback's cleanup deletes it: a file the
// settlement does not own, lost on a stale warrant. The
// fixtures below pin the clearing at both completions —
// the done-mark in the materializer's write loop, and the
// undo completion in rollback itself.
//
// Parent root: rollback's guards judge the components
// INSIDE the parent directory, but the journaled parent
// path itself was resolved — absolute, symlink-free — when
// the settlement wrote it. If that path's final component
// stands as a symlink when rollback runs, the directory
// was replaced after the crash (moved aside and linked is
// a filesystem state; no concurrent actor is needed), and
// every undo path resolves through the link into another
// tree. Rollback must refuse at its entry, before any undo
// work, and leave the journal in place as evidence.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// TestFilesEqualComparesAcrossChunks pins filesEqual's
// semantics across the streaming rewrite (PR #11 review
// round): the comparison now reads in bounded chunks
// instead of loading both files whole — it sits on the
// recovery path, where a file can be as large as the tree
// being settled — so equality must hold across chunk
// boundaries, a difference past the first chunk must still
// be seen, and the size fast-path and the unreadable-file
// error must behave exactly as the whole-file comparison
// did. (The whole-file original returns the same answers
// on every case here; this fixture is the semantics pin
// the rewrite must not move, not a red.)
func TestFilesEqualComparesAcrossChunks(t *testing.T) {
	dir := t.TempDir()
	content := make([]byte, 300_000)
	for i := range content {
		content[i] = byte(i*31 + i>>8)
	}
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if same, err := filesEqual(a, b); err != nil || !same {
		t.Fatalf("filesEqual(equal multi-chunk files) = %v, %v; want true, nil", same, err)
	}

	// A difference in a later chunk, at the same size.
	altered := append([]byte(nil), content...)
	altered[200_000] ^= 0xff
	if err := os.WriteFile(b, altered, 0o644); err != nil {
		t.Fatal(err)
	}
	if same, err := filesEqual(a, b); err != nil || same {
		t.Fatalf("filesEqual(same size, late difference) = %v, %v; want false, nil", same, err)
	}

	// Different sizes: the fast-path answers.
	if err := os.WriteFile(b, content[:len(content)-1], 0o644); err != nil {
		t.Fatal(err)
	}
	if same, err := filesEqual(a, b); err != nil || same {
		t.Fatalf("filesEqual(different sizes) = %v, %v; want false, nil", same, err)
	}

	// A missing input is an error, as the whole-file read
	// was.
	if _, err := filesEqual(a, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("filesEqual(missing input) error = nil, want an error")
	}
}

// TestRecoverSparesOccupantAtCompletedUnitsTempName: a
// two-unit Select is crashed after the first unit's
// completion mark persisted and the second unit's rename
// landed (its window). A foreign file is then planted at
// the COMPLETED unit's derived temp name. The completed
// unit's temp was renamed away before its mark persisted,
// so nothing of the settlement's stands at that name; its
// record, cleared in the same journal write that marked
// it done, is no longer in the journal the crash left.
// Recover must leave the occupant byte-identical while
// converging the parent and clearing the journal. Both
// units are creations (no preimage), so the completed
// unit's undo removes its landed file directly and never
// needs the derived name itself; the with-preimage shape
// — where the undo's restore must write through the name
// and an occupant there is a descriptive refusal — is the
// fold-in fixtures' pinned cell, not this one's.
func TestRecoverSparesOccupantAtCompletedUnitsTempName(t *testing.T) {
	f := newJournalFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "new1.txt"), "new one\n", 0o644)
	journalWriteFile(t, filepath.Join(layer.Dir, "new2.txt"), "new two\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)
	preHash := hashTree(t, f.parentDir)

	// Kill at the second unit's window: the first unit's
	// completion mark is durable, the second unit's rename
	// has landed but its mark has not.
	runCrashed(t, 2, func() {
		_, _ = Select(f.ctx, "parent", f.parentDir, "run-a", "op-stale")
	})

	j, err := readJournal(f.root, "parent")
	if err != nil || j == nil {
		t.Fatalf("read crashed journal: journal = %v, err = %v", j, err)
	}
	var donePath string
	for _, u := range j.Units {
		if u.Kind == unitWrite && u.Done {
			if donePath != "" {
				t.Fatalf("crashed journal units = %+v, want exactly one completed write unit", j.Units)
			}
			donePath = u.Path
		}
	}
	if donePath == "" {
		t.Fatalf("crashed journal units = %+v, want exactly one completed write unit", j.Units)
	}
	occupant := filepath.Join(f.parentDir, derivedTempName(filepath.Join(f.parentDir, donePath), "op-stale", false))
	journalWriteFile(t, occupant, "precious\n", 0o644)

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if b, err := os.ReadFile(occupant); err != nil || string(b) != "precious\n" {
		t.Fatalf("occupant at a COMPLETED unit's derived temp name after Recover = %q, %v; want byte-identical %q (the unit's temp-created record outlived the unit and vouched for a temp that no longer stands)", b, err, "precious\n")
	}
	if err := os.Remove(occupant); err != nil {
		t.Fatal(err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after Recover = %s, want pre-select %s", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rolled-back select left %d facts, want 0", len(facts))
	}
}

// TestRecoverSparesOccupantAtRestoredUnitsTempName: the
// rollback-side completion. A first Recover restores a
// write unit whose preimage is a regular file — the
// restore writes through a derived-name temp, recording
// the file kind for it — and then dies on a later unit's
// refusal, leaving the journal in place. The restore's
// rename landed, so the unit's restore temp no longer
// stands; the record the restore set must have stopped
// vouching when the unit's undo completed. A foreign file
// planted at the derived name before a second Recover
// must survive byte-identical while the settlement
// converges around it.
func TestRecoverSparesOccupantAtRestoredUnitsTempName(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	// The verb's landed state: mod.txt was overwritten by a
	// symlink (the write unit below), old.txt was deleted
	// (the delete unit below).
	if err := os.Remove(filepath.Join(f.parentDir, "mod.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep.txt", filepath.Join(f.parentDir, "mod.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.parentDir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	// mod.txt's captured preimage. old.txt's is deliberately
	// not staged yet: its absence is what kills the first
	// Recover after the write unit's undo completes.
	preDir := filepath.Join(f.root, "staging", "op-restore-stale", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	// The write unit carries the LINK kind's record only —
	// its writer created a symlink temp — so the file kind
	// is exactly the kind the first Recover's restore is
	// about to create and record.
	writeRawJournal(t, f, "op-restore-stale", []canonical.Value{
		rawUnitWithTemp("mod.txt", unitWrite, true, true, true),
		rawUnit("old.txt", unitDelete, true, true),
	})

	// The first Recover: the write unit's restore lands
	// (recording the file kind on the way), then the delete
	// unit's missing preimage refuses and the journal stays.
	err := Recover(f.ctx, "parent")
	if err == nil || !strings.Contains(err.Error(), "has no preimage") {
		t.Fatalf("first Recover = %v, want the delete unit's missing-preimage refusal", err)
	}
	if b, rerr := os.ReadFile(filepath.Join(f.parentDir, "mod.txt")); rerr != nil || string(b) != "original\n" {
		t.Fatalf("mod.txt after first Recover = %q, %v; want the restored preimage (the restore this fixture's record came from must have landed)", b, rerr)
	}
	if _, serr := os.Stat(journalPath(f.root, "parent")); serr != nil {
		t.Fatalf("journal after first Recover: %v, want still present", serr)
	}

	// The occupant, planted at the file kind's derived name
	// the first Recover's restore wrote through — and the
	// delete unit's preimage, staged now so the second
	// Recover can converge.
	occupant := filepath.Join(f.parentDir, derivedTempName(filepath.Join(f.parentDir, "mod.txt"), "op-restore-stale", false))
	journalWriteFile(t, occupant, "precious\n", 0o644)
	journalWriteFile(t, filepath.Join(preDir, "old.txt"), "old\n", 0o644)

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if b, err := os.ReadFile(occupant); err != nil || string(b) != "precious\n" {
		t.Fatalf("occupant at a RESTORED unit's derived temp name after second Recover = %q, %v; want byte-identical %q (the restore's temp-created record outlived the restore and vouched for a temp that no longer stands)", b, err, "precious\n")
	}
	if err := os.Remove(occupant); err != nil {
		t.Fatal(err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after second Recover = %s, want pre-verb %s", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after second Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rolled-back settlement left %d facts, want 0", len(facts))
	}
}

// TestRecoverRefusesReplacedParentRoot: the journaled
// parent directory was moved aside after the crash and a
// symlink to an outside tree stands at its path. The
// settlement's unit carries its temp-created record, so
// the unfixed rollback's temp cleanup resolves the derived
// name through the link and deletes the outside file
// standing there — and its restores would write into the
// outside tree besides. Rollback must refuse at its entry,
// name the directory and the link, and leave both the
// outside tree and the journal untouched.
func TestRecoverRefusesReplacedParentRoot(t *testing.T) {
	f := newJournalFixture(t)

	outsideDir := filepath.Join(f.scratch, "outside")
	victim := filepath.Join(outsideDir, derivedTempName(filepath.Join(f.parentDir, "mod.txt"), "op-replaced", false))
	journalWriteFile(t, victim, "victim\n", 0o644)
	outsideHash := hashTree(t, outsideDir)

	preDir := filepath.Join(f.root, "staging", "op-replaced", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	writeRawJournal(t, f, "op-replaced", []canonical.Value{
		rawUnitWithTemp("mod.txt", unitWrite, false, true, false),
	})

	// The replacement: the journaled directory moved aside,
	// a symlink to the outside tree standing at its path.
	if err := os.Rename(f.parentDir, filepath.Join(f.scratch, "parent-real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, f.parentDir); err != nil {
		t.Fatal(err)
	}

	err := Recover(f.ctx, "parent")
	if err == nil {
		t.Fatal("Recover through a replaced parent root succeeded; want a descriptive refusal")
	}
	if !strings.Contains(err.Error(), f.parentDir) || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Recover refusal = %v, want it to name the parent directory %s and the symlink standing there", err, f.parentDir)
	}
	if b, rerr := os.ReadFile(victim); rerr != nil || string(b) != "victim\n" {
		t.Fatalf("outside victim after Recover = %q, %v; want byte-identical %q (rollback resolved through the replaced parent root)", b, rerr, "victim\n")
	}
	if got := hashTree(t, outsideDir); got != outsideHash {
		t.Fatalf("outside walk hash after Recover = %s, want %s (rollback touched the tree the link names)", got, outsideHash)
	}
	if _, serr := os.Stat(journalPath(f.root, "parent")); serr != nil {
		t.Fatalf("journal after refused Recover: %v, want still present as evidence", serr)
	}
}
