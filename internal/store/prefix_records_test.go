package store_test

import (
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The prefix-membership suite (panel W3-2): CollectGarbage
// seeded a published cut's retained prefix from path_entries,
// while ResolveCut built the same prefix from the records
// table's placement (via GetRecord). The two sets are computed
// from different tables and diverged under a path_entries-only
// tamper: GC collected a record the cut still resolved, and the
// published cut silently shrank. Prefix membership is now
// derived from the records table — the placement truth every
// read reports — on both sides, so a tampered path_entries row
// changes neither retention nor resolution.

// seedCutPrefixFixture builds a path whose in-prefix fact A is
// retained only by a published cut's prefix: A (ordinal 0) and B
// (ordinal 1) cite only the root; the cut (ordinal 2) runs
// through B; D (ordinal 3) is unreachable once E (ordinal 4,
// citing only the root) takes the frontier. It closes the store
// and returns the file path and the A / B / cut IDs.
func seedCutPrefixFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rootID := record.RootWitnessRecordID()
	a, err := s.AppendFact("runs/px", draftCiting(t, "test.fact", `{"n":"a"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append A: %v", err)
	}
	b, err := s.AppendFact("runs/px", draftCiting(t, "test.fact", `{"n":"b"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append B: %v", err)
	}
	cutID, err := slice.PublishCut(s, "runs/px", b.RecordID)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	if _, err := s.AppendFact("runs/px", draftCiting(t, "test.fact", `{"n":"d"}`, []string{rootID})); err != nil {
		t.Fatalf("append D: %v", err)
	}
	if _, err := s.AppendFact("runs/px", draftCiting(t, "test.fact", `{"n":"e"}`, []string{rootID})); err != nil {
		t.Fatalf("append E: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, a.RecordID, b.RecordID, cutID
}

func TestGCPrefixSeedIgnoresPathEntriesTamper(t *testing.T) {
	rootID := record.RootWitnessRecordID()

	// Control: the untampered fixture collects only D.
	controlPath, ca, cb, ccut := seedCutPrefixFixture(t)
	control := reopenStore(t, controlPath)
	controlReport, err := control.CollectGarbage()
	if err != nil {
		t.Fatalf("control CollectGarbage: %v", err)
	}
	if controlReport != (store.GarbageReport{Collected: 1, Kept: 5}) {
		t.Fatalf("control report = %+v, want {Collected:1 Kept:5}", controlReport)
	}

	// Tampered: A's path_entries ordinal is moved out of the
	// cut's prefix (0 to 99); the records table is untouched.
	// GC's kept set and the cut's resolution must be identical
	// to the untampered run.
	path, a, b, cut := seedCutPrefixFixture(t)
	if a != ca || b != cb || cut != ccut {
		t.Fatalf("fixture is not deterministic: (%s, %s, %s) vs (%s, %s, %s)", a, b, cut, ca, cb, ccut)
	}
	rawExec(t, path, "UPDATE path_entries SET owner_ordinal = 99 WHERE record_id = ?", a)
	s := reopenStore(t, path)

	before, err := slice.ResolveCut(s, cut, nil)
	if err != nil {
		t.Fatalf("ResolveCut before GC: %v", err)
	}
	if !equalIDs(sliceIDs(before), []string{rootID, a, b}) {
		t.Fatalf("cut slice before GC = %v, want [root, A, B]", sliceIDs(before))
	}
	digestBefore, err := before.Digest()
	if err != nil {
		t.Fatalf("slice Digest before GC: %v", err)
	}

	report, err := s.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage over tampered path_entries: %v", err)
	}
	if report != controlReport {
		t.Fatalf("report = %+v, want identical to untampered %+v", report, controlReport)
	}
	if _, err := s.GetRecord(a); err != nil {
		t.Fatalf("in-prefix fact A collected after its path_entries ordinal was moved: %v", err)
	}

	after, err := slice.ResolveCut(s, cut, nil)
	if err != nil {
		t.Fatalf("ResolveCut after GC: %v", err)
	}
	if !equalIDs(sliceIDs(after), []string{rootID, a, b}) {
		t.Fatalf("cut slice after GC = %v, want [root, A, B]", sliceIDs(after))
	}
	digestAfter, err := after.Digest()
	if err != nil {
		t.Fatalf("slice Digest after GC: %v", err)
	}
	if digestAfter != digestBefore {
		t.Fatalf("cut digest changed across GC: %s -> %s", digestBefore, digestAfter)
	}
}
