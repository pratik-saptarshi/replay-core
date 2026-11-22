package settle

// Journal and recovery tests (plan Task 8). This file is in
// package settle (not settle_test) because the crash child in
// crash_test.go needs the unexported unitCrashHook, and the
// hand-built journal fixtures are built to the journal format
// directly. The fixture mirrors settle_test.go's: a real git
// working tree as parentDir and the settle Context.Root on a
// btrfs scratch dir under the working directory (amendment F1),
// so Fork's carrier selection lands on the copy carrier
// deterministically. The stores live in the same scratch dir —
// not t.TempDir() — because the crash child reopens them by
// path from a second process.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

type journalFixture struct {
	ctx       *Context
	trace     *store.Store
	snaps     *snapshot.Store
	root      string // the settle Context.Root
	parentDir string
	parent0   string // the parent scope's initial head
	scratch   string
	tracePath string
	snapsPath string
}

func journalWriteFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func newJournalFixture(t *testing.T) *journalFixture {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	scratch, err := os.MkdirTemp(wd, ".settle-journal-fixture-")
	if err != nil {
		t.Fatalf("create btrfs scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })

	parentDir := filepath.Join(scratch, "parent")
	journalWriteFile(t, filepath.Join(parentDir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "mod.txt"), "original\n", 0o644)
	journalWriteFile(t, filepath.Join(parentDir, "old.txt"), "old\n", 0o644)
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=settle-test", "-c", "user.email=settle-test@localhost", "commit", "-q", "-m", "parent"},
	} {
		full := append([]string{"-C", parentDir}, args...)
		cmd := exec.Command("git", full...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	tracePath := filepath.Join(scratch, "trace.db")
	traceStore, err := store.Open(tracePath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = traceStore.Close() })
	snapsPath := filepath.Join(scratch, "snaps.git")
	snaps, err := snapshot.Open(snapsPath)
	if err != nil {
		t.Fatalf("snapshot.Open: %v", err)
	}
	parent0, err := snaps.Snapshot(parentDir, map[string]string{"run": "settle-journal-fixture"})
	if err != nil {
		t.Fatalf("Snapshot parent: %v", err)
	}
	if err := snaps.Fork("parent", parent0); err != nil {
		t.Fatalf("Fork parent scope: %v", err)
	}
	root := filepath.Join(scratch, "settle-root")
	return &journalFixture{
		ctx:       &Context{Trace: traceStore, Snaps: snaps, Root: root},
		trace:     traceStore,
		snaps:     snaps,
		root:      root,
		parentDir: parentDir,
		parent0:   parent0,
		scratch:   scratch,
		tracePath: tracePath,
		snapsPath: snapsPath,
	}
}

// hashTree hashes a directory's non-directory entries — relative
// path, kind, mode, and bytes (or symlink target) — into one
// digest: the walk hash the recovery tests compare byte-exactly.
// Entries named .git are skipped, mirroring the snapshot walk.
func hashTree(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	var rels []string
	entries := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != dir && d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entries[rel] = "symlink:" + target
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entries[rel] = fmt.Sprintf("file:%o:%x", info.Mode().Perm(), sha256.Sum256(b))
		default:
			entries[rel] = "other"
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("hashTree(%s): %v", dir, err)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		fmt.Fprintf(h, "%s %s\n", rel, entries[rel])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// journalFacts returns the settlement facts recorded on
// runScope's trace path, in path order.
func (f *journalFixture) journalFacts(t *testing.T, runScope string) []store.StoredRecord {
	t.Helper()
	recs, err := f.trace.PathRecords(runScope)
	if err != nil {
		t.Fatalf("PathRecords(%q): %v", runScope, err)
	}
	var out []store.StoredRecord
	for _, rec := range recs {
		ref, _ := stringField(rec.Record, "schema_ref")
		switch ref {
		case store.SettleSelectSchemaRef, store.SettleApplySchemaRef, store.SettleDiscardSchemaRef:
			out = append(out, rec)
		}
	}
	return out
}

// TestRecoverRejectsCorruptJournal (plan Task 8, Step 1; input
// shape — Review Focus: recovery trusting a corrupt journal): a
// journal that does not decode, and one that decodes but names
// an unknown state, are each a descriptive refusal — no writes
// anywhere, and the journal file is left in place as evidence.
// With no journal at all, Recover is a no-op.
func TestRecoverRejectsCorruptJournal(t *testing.T) {
	f := newJournalFixture(t)

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover with no journal = %v, want nil", err)
	}

	journalsDir := filepath.Join(f.root, "journals")
	if err := os.MkdirAll(journalsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jpath := filepath.Join(journalsDir, "parent.json")
	before := hashTree(t, f.parentDir)

	if err := os.WriteFile(jpath, []byte("{this is not canonical json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Recover(f.ctx, "parent"); err == nil {
		t.Fatal("Recover with a garbage journal succeeded, want a descriptive refusal")
	}
	if _, err := os.Stat(jpath); err != nil {
		t.Fatalf("journal after refused Recover: %v, want it left in place as evidence", err)
	}
	if got := hashTree(t, f.parentDir); got != before {
		t.Fatalf("parent walk hash changed across refused Recover: %s -> %s", before, got)
	}

	// A journal that decodes but names an unknown state is
	// refused the same way: shape validation, never a guess.
	v := canonical.ObjectOf(
		canonical.Pair{Key: "fact", Value: store.SettleSelectBody("op-x", "parent", "run-a", f.parent0, f.parent0)},
		canonical.Pair{Key: "old_head", Value: canonical.StringOf(f.parent0)},
		canonical.Pair{Key: "op", Value: canonical.StringOf("op-x")},
		canonical.Pair{Key: "parent_dir", Value: canonical.StringOf(f.parentDir)},
		canonical.Pair{Key: "parent_scope", Value: canonical.StringOf("parent")},
		canonical.Pair{Key: "result_commit", Value: canonical.StringOf(f.parent0)},
		canonical.Pair{Key: "run_scope", Value: canonical.StringOf("run-a")},
		canonical.Pair{Key: "state", Value: canonical.StringOf("exploded")},
		canonical.Pair{Key: "units", Value: canonical.ArrayOf()},
		canonical.Pair{Key: "verb", Value: canonical.StringOf("select")},
	)
	b, err := canonical.MarshalKernel(v)
	if err != nil {
		t.Fatalf("marshal unknown-state journal: %v", err)
	}
	if err := os.WriteFile(jpath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Recover(f.ctx, "parent"); err == nil {
		t.Fatal("Recover with an unknown-state journal succeeded, want a descriptive refusal")
	}
	if _, err := os.Stat(jpath); err != nil {
		t.Fatalf("journal after refused Recover: %v, want it left in place as evidence", err)
	}
	if got := hashTree(t, f.parentDir); got != before {
		t.Fatalf("parent walk hash changed across refused Recover: %s -> %s", before, got)
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after refused Recovers; want %s unmoved", head, err, f.parent0)
	}
}

// TestJournalRoundTrip: a journal written by writeJournal reads
// back field-for-field through readJournal's validation —
// including the recorded fact body and the unit marks.
func TestJournalRoundTrip(t *testing.T) {
	f := newJournalFixture(t)
	want := &journal{
		Op:           "op-rt",
		Verb:         verbApply,
		ParentScope:  "parent",
		RunScope:     "run-a",
		State:        journalStateApplying,
		OldHead:      f.parent0,
		ResultCommit: f.parent0,
		ParentDir:    f.parentDir,
		Fact:         store.SettleApplyBody("op-rt", "parent", "run-a", f.parent0, f.parent0, []string{"a.txt", "b/c.txt"}),
		Units: []journalUnit{
			{Path: "a.txt", Kind: unitWrite, Done: true},
			{Path: "b/c.txt", Kind: unitWrite, Done: false},
			{Path: "old.txt", Kind: unitDelete, Done: true},
		},
	}
	if err := writeJournal(f.root, want); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
	got, err := readJournal(f.root, "parent")
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if got == nil {
		t.Fatal("readJournal returned no journal for a written journal")
	}
	if got.Op != want.Op || got.Verb != want.Verb || got.ParentScope != want.ParentScope ||
		got.RunScope != want.RunScope || got.State != want.State || got.OldHead != want.OldHead ||
		got.ResultCommit != want.ResultCommit || got.ParentDir != want.ParentDir {
		t.Fatalf("round-tripped journal = %+v, want %+v", got, want)
	}
	if len(got.Units) != len(want.Units) {
		t.Fatalf("round-tripped units = %v, want %v", got.Units, want.Units)
	}
	for i := range want.Units {
		if got.Units[i] != want.Units[i] {
			t.Fatalf("unit %d = %+v, want %+v", i, got.Units[i], want.Units[i])
		}
	}
	b, err := canonical.MarshalKernel(got.Fact)
	if err != nil {
		t.Fatalf("marshal round-tripped fact: %v", err)
	}
	wb, err := canonical.MarshalKernel(want.Fact)
	if err != nil {
		t.Fatalf("marshal fact: %v", err)
	}
	if string(b) != string(wb) {
		t.Fatalf("round-tripped fact body = %s, want %s", b, wb)
	}
}

// TestRecoverRollsBackPlannedJournal (plan Task 8, Step 2): a
// hand-built planned journal with two done units — one write
// that overwrote an existing file (preimage captured), one
// write that created a file (no preimage) — rolls the parent
// back byte-exactly: the overwritten file's bytes return, the
// created file is gone, no fact is minted, the scope head never
// moved, and journal + staging are cleared.
func TestRecoverRollsBackPlannedJournal(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	// The interrupted verb's partial work: mod.txt overwritten,
	// added.txt created.
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "changed\n", 0o644)
	journalWriteFile(t, filepath.Join(f.parentDir, "added.txt"), "created\n", 0o644)
	// Its preimage tree: mod.txt's original bytes; added.txt has
	// no preimage (the unit created it).
	preDir := filepath.Join(f.root, "staging", "op-rb", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	j := &journal{
		Op:           "op-rb",
		Verb:         verbSelect,
		ParentScope:  "parent",
		RunScope:     "run-a",
		State:        journalStatePlanned,
		OldHead:      f.parent0,
		ResultCommit: f.parent0,
		ParentDir:    f.parentDir,
		Fact:         store.SettleSelectBody("op-rb", "parent", "run-a", f.parent0, f.parent0),
		Units: []journalUnit{
			{Path: "mod.txt", Kind: unitWrite, Done: true},
			{Path: "added.txt", Kind: unitWrite, Done: true},
			{Path: "keep.txt", Kind: unitWrite, Done: false},
		},
	}
	if err := writeJournal(f.root, j); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after rollback = %s, want pre-apply %s", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after rollback: err = %v, want gone", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "staging", "op-rb")); !os.IsNotExist(err) {
		t.Fatalf("staging after rollback: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rollback minted %d facts, want 0", len(facts))
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after rollback; want %s unmoved", head, err, f.parent0)
	}
}

// TestRecoverCompletesRecordedJournal (plan Task 8, Step 2): a
// hand-built verified journal completes the commit point
// idempotently — the parent scope advances old_head →
// result_commit, exactly one fact is minted from the recorded
// body alone (its fields asserted exactly), journal + staging
// are cleared, and a second Recover is a no-op.
func TestRecoverCompletesRecordedJournal(t *testing.T) {
	f := newJournalFixture(t)

	// The result tree the crashed verb had materialized and
	// verified before it died: snapshot it as the result commit.
	resultDir := filepath.Join(f.scratch, "result")
	journalWriteFile(t, filepath.Join(resultDir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(resultDir, "mod.txt"), "settled\n", 0o644)
	resultCommit, err := f.snaps.Snapshot(resultDir, map[string]string{"run": "settle-journal-fixture"})
	if err != nil {
		t.Fatalf("Snapshot result: %v", err)
	}
	// The parent directory already serves the result tree (the
	// verb verified it); old.txt was deleted by a unit.
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "settled\n", 0o644)
	if err := os.Remove(filepath.Join(f.parentDir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	postHash := hashTree(t, f.parentDir)

	settled := []string{"mod.txt", "old.txt"}
	j := &journal{
		Op:           "op-vc",
		Verb:         verbApply,
		ParentScope:  "parent",
		RunScope:     "run-a",
		State:        journalStateVerified,
		OldHead:      f.parent0,
		ResultCommit: resultCommit,
		ParentDir:    f.parentDir,
		Fact:         store.SettleApplyBody("op-vc", "parent", "run-a", f.parent0, resultCommit, settled),
		Units: []journalUnit{
			{Path: "mod.txt", Kind: unitWrite, Done: true},
			{Path: "old.txt", Kind: unitDelete, Done: true},
		},
	}
	if err := writeJournal(f.root, j); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != resultCommit {
		t.Fatalf("parent head = %s, %v after Recover; want result commit %s", head, err, resultCommit)
	}
	if got := hashTree(t, f.parentDir); got != postHash {
		t.Fatalf("parent walk hash changed across completion: %s -> %s", postHash, got)
	}
	facts := f.journalFacts(t, "run-a")
	if len(facts) != 1 {
		t.Fatalf("settle facts on run-a = %d, want exactly 1", len(facts))
	}
	if ref, _ := stringField(facts[0].Record, "schema_ref"); ref != store.SettleApplySchemaRef {
		t.Fatalf("fact schema_ref = %q, want %q", ref, store.SettleApplySchemaRef)
	}
	var body canonical.Value
	for _, p := range facts[0].Record.Pairs {
		if p.Key == "body" {
			body = p.Value
		}
	}
	for key, want := range map[string]string{
		"op": "op-vc", "parent_scope": "parent", "run_scope": "run-a",
		"fork_commit": f.parent0, "result_commit": resultCommit,
	} {
		if got, _ := stringField(body, key); got != want {
			t.Fatalf("minted fact body[%q] = %q, want %q (the recorded body, minted as recorded)", key, got, want)
		}
	}
	if got := stringArrayField(body, "settled_paths"); len(got) != 2 || got[0] != "mod.txt" || got[1] != "old.txt" {
		t.Fatalf("minted fact settled_paths = %v, want %v", got, settled)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after completion: err = %v, want gone", err)
	}

	// Idempotent: a second Recover finds no journal, changes
	// nothing, and mints no second fact.
	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
		t.Fatalf("settle facts after second Recover = %d, want exactly 1", len(facts))
	}
}
