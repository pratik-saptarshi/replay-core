package settle

// In-flight window tests (PR #7 review, Codex P1 on journal.go):
// a verb's per-unit mutation — a write unit's rename, a delete
// unit's removal, a blocking ancestor's removal — lands BEFORE
// the journal write that records the unit's completion. A crash
// in that window leaves the parent tree mutated while the
// durable journal still says the unit is not done. Recovery
// that undoes only done units then skips the mutated unit,
// deletes the journal and the preimages, and the mutation
// becomes permanent with no settlement fact anywhere: e.g. a
// crash immediately after replacing mod.txt leaves its new
// bytes in place while the parent ref remains at the old
// commit.
//
// The materializer must therefore record recoverable in-flight
// intent BEFORE each mutation (a unit's "started" mark), and
// rollback must undo done OR started units. The hand-built
// fixtures below construct exactly the state a crash in the
// window leaves under that discipline — mutation landed (or,
// for the safety pin, not yet landed), journal state applying,
// the unit started but not done, its preimage captured — by
// writing the journal in its canonical on-disk form directly,
// so the fixtures compile and run against the unfixed code,
// where rollback skips the unit and the damage is observable.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// rawUnit builds one journal unit in its canonical on-disk
// form, including the in-flight "started" mark.
func rawUnit(path, kind string, done, started bool) canonical.Value {
	return canonical.ObjectOf(
		canonical.Pair{Key: "done", Value: canonical.BoolOf(done)},
		canonical.Pair{Key: "kind", Value: canonical.StringOf(kind)},
		canonical.Pair{Key: "path", Value: canonical.StringOf(path)},
		canonical.Pair{Key: "started", Value: canonical.BoolOf(started)},
	)
}

// writeRawJournal writes a select journal for op in state
// applying directly in canonical form (the shape writeJournal
// produces), bypassing the journal struct so the fixture does
// not depend on fields the unfixed code does not have. It
// also records the parent anchor writeJournal leaves beside
// every journal it writes, so the fixture's on-disk state is
// the full state a crash in the window leaves.
func writeRawJournal(t *testing.T, f *journalFixture, op string, units []canonical.Value) {
	t.Helper()
	v := canonical.ObjectOf(
		canonical.Pair{Key: "fact", Value: store.SettleSelectBody(op, "parent", "run-a", f.parent0, f.parent0)},
		canonical.Pair{Key: "old_head", Value: canonical.StringOf(f.parent0)},
		canonical.Pair{Key: "op", Value: canonical.StringOf(op)},
		canonical.Pair{Key: "parent_dir", Value: canonical.StringOf(f.parentDir)},
		canonical.Pair{Key: "parent_scope", Value: canonical.StringOf("parent")},
		canonical.Pair{Key: "result_commit", Value: canonical.StringOf(f.parent0)},
		canonical.Pair{Key: "run_scope", Value: canonical.StringOf("run-a")},
		canonical.Pair{Key: "state", Value: canonical.StringOf(journalStateApplying)},
		canonical.Pair{Key: "units", Value: canonical.ArrayOf(units...)},
		canonical.Pair{Key: "verb", Value: canonical.StringOf(verbSelect)},
	)
	b, err := canonical.MarshalKernel(v)
	if err != nil {
		t.Fatalf("marshal raw journal: %v", err)
	}
	jpath := journalPath(f.root, "parent")
	if err := os.MkdirAll(filepath.Dir(jpath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jpath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeParentAnchor(f.root, "parent", f.parentDir); err != nil {
		t.Fatalf("write parent anchor: %v", err)
	}
}

// assertRolledBackToPre asserts Recover converged the parent
// to exactly its pre-verb state: tree hash restored, journal
// and staging cleared, no fact minted, head unmoved.
func assertRolledBackToPre(t *testing.T, f *journalFixture, op, preHash string) {
	t.Helper()
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after Recover = %s, want pre-verb %s (the in-flight unit's mutation was not undone)", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "staging", op)); !os.IsNotExist(err) {
		t.Fatalf("staging after Recover: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rollback minted %d facts, want 0", len(facts))
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after Recover; want %s unmoved", head, err, f.parent0)
	}
}

// TestRecoverUndoesWriteInterruptedBeforeCompletionMark: the
// write of mod.txt landed (its new bytes are in the parent)
// but the completion mark never persisted. Recover must
// restore the preimage, not skip the unit and destroy the
// evidence.
func TestRecoverUndoesWriteInterruptedBeforeCompletionMark(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	// The mutation landed; the mark did not.
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "run mod\n", 0o644)
	// Its captured preimage.
	preDir := filepath.Join(f.root, "staging", "op-win", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	writeRawJournal(t, f, "op-win", []canonical.Value{
		rawUnit("mod.txt", unitWrite, false, true),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assertRolledBackToPre(t, f, "op-win", preHash)
}

// TestRecoverUndoesDeleteInterruptedBeforeCompletionMark: the
// removal of old.txt landed but its completion mark never
// persisted. Recover must restore the deleted file from its
// preimage.
func TestRecoverUndoesDeleteInterruptedBeforeCompletionMark(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	// The removal landed; the mark did not.
	if err := os.Remove(filepath.Join(f.parentDir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	preDir := filepath.Join(f.root, "staging", "op-win", "pre")
	journalWriteFile(t, filepath.Join(preDir, "old.txt"), "old\n", 0o644)

	writeRawJournal(t, f, "op-win", []canonical.Value{
		rawUnit("old.txt", unitDelete, false, true),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assertRolledBackToPre(t, f, "op-win", preHash)
}

// TestRecoverUndoesBlockingRemovalInterruptedBeforeWrite: the
// file→directory swap's write unit (old.txt/inner.txt) removes
// its blocking ancestor — the file old.txt — before it lands.
// A crash after that removal but before the write lands (and
// before any completion mark) leaves old.txt simply gone.
// Both units are started (the write's intent covers the
// blocking delete it performs), neither is done; Recover must
// restore the blocking file from the delete unit's preimage.
func TestRecoverUndoesBlockingRemovalInterruptedBeforeWrite(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	// The blocking removal landed; the write did not.
	if err := os.Remove(filepath.Join(f.parentDir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	preDir := filepath.Join(f.root, "staging", "op-win", "pre")
	journalWriteFile(t, filepath.Join(preDir, "old.txt"), "old\n", 0o644)

	writeRawJournal(t, f, "op-win", []canonical.Value{
		rawUnit("old.txt/inner.txt", unitWrite, false, true),
		rawUnit("old.txt", unitDelete, false, true),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assertRolledBackToPre(t, f, "op-win", preHash)
}

// TestRecoverStartedButUnlandedSwapIsIdentity is the safety
// pin for the in-flight discipline: a crash AFTER the intent
// record but BEFORE the mutation (the directory→file swap's
// write unit started, its subtree delete started, the parent
// still serving the original directory) must roll back to
// exactly the pre-verb tree — undoing a started unit whose
// mutation never landed is an identity, never a destruction
// of the original directory.
func TestRecoverStartedButUnlandedSwapIsIdentity(t *testing.T) {
	f := newDirSwapFixture(t)
	preHash := hashTree(t, f.parentDir)

	// Preimages as capture records them for the swap: the
	// write target old.txt was a directory, so its preimage
	// tree holds only the delete unit's file beneath it.
	preDir := filepath.Join(f.root, "staging", "op-win", "pre")
	journalWriteFile(t, filepath.Join(preDir, "old.txt", "inner.txt"), "inner\n", 0o644)

	writeRawJournal(t, f, "op-win", []canonical.Value{
		rawUnit("old.txt", unitWrite, false, true),
		rawUnit("old.txt/inner.txt", unitDelete, false, true),
	})

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assertRolledBackToPre(t, f, "op-win", preHash)
}

// TestSelectCrashBeforeCompletionMarkRecordsIntent runs a real
// Select killed in the window — after the first unit's
// mutation, before its completion mark is persisted (the
// crash hook now fires at that point as well as at marks).
// The journal left on disk must record the unit as started
// but not done, and Recover must then converge the parent to
// exactly pre-select, after which the same Select succeeds
// whole. On the unfixed code the hook fires only at the mark,
// so the journal the crash leaves says done — the window was
// never exercised.
func TestSelectCrashBeforeCompletionMarkRecordsIntent(t *testing.T) {
	f := newJournalFixture(t)
	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "run mod\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)
	preHash := hashTree(t, f.parentDir)

	runCrashed(t, 1, func() {
		_, _ = Select(f.ctx, "parent", f.parentDir, "run-a", "op-win")
	})

	// The journal the crash left must show the mutated unit
	// in flight: started, not done.
	b, err := os.ReadFile(journalPath(f.root, "parent"))
	if err != nil {
		t.Fatalf("read journal after crash: %v", err)
	}
	v, err := canonical.DecodeKernel(b)
	if err != nil {
		t.Fatalf("decode journal after crash: %v", err)
	}
	units, ok := arrayField(v, "units")
	if !ok || len(units) != 1 {
		t.Fatalf("journal units after crash = %v, want exactly the one write unit", units)
	}
	if started, ok := boolField(units[0], "started"); !ok || !started {
		t.Fatalf("crashed unit's started = %v, %v; want true (intent must be recorded before the mutation)", started, ok)
	}
	if done, ok := boolField(units[0], "done"); !ok || done {
		t.Fatalf("crashed unit's done = %v, %v; want false (the crash landed before the completion mark)", done, ok)
	}
	// And the mutation itself landed before the crash.
	if got, err := os.ReadFile(filepath.Join(f.parentDir, "mod.txt")); err != nil || string(got) != "run mod\n" {
		t.Fatalf("parent mod.txt after crash = %q, %v; want the landed mutation %q", got, err, "run mod\n")
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assertRolledBackToPre(t, f, "op-win", preHash)

	if _, err := Select(f.ctx, "parent", f.parentDir, "run-a", "op-win"); err != nil {
		t.Fatalf("fresh Select after rollback: %v", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after fresh Select = %d, want exactly 1", len(facts))
	}
}
