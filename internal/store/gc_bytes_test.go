package store_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The GC classification suite (issue #5): CollectGarbage's root
// classification — which records are cuts, which are milestones —
// must come from each record's canonical bytes, never from the
// records.schema_ref column. The column is a denormalized copy
// of the record's own schema_ref field; the panel flipped one
// cell with direct SQL and GC deleted a live milestone and the
// fact only it retained. Both flip directions are pinned here:
// a tampered column must leave GC's report and the kept set
// identical to an untampered run.

// seedMilestoneFixture builds a store whose only retention path
// to fact x is milestone m's causal closure: x (ordinal 0) is
// cited by m (ordinal 1), and the frontier tail (ordinal 2)
// cites only the root. It closes the store and returns the file
// path and the x / milestone IDs.
func seedMilestoneFixture(t *testing.T) (string, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rootID := record.RootWitnessRecordID()
	x, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"x"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append x: %v", err)
	}
	m, err := s.AppendMilestone("runs/main", "task-tamper", "snap-tamper", []string{x.RecordID})
	if err != nil {
		t.Fatalf("AppendMilestone: %v", err)
	}
	if _, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"tail"}`, []string{rootID})); err != nil {
		t.Fatalf("append tail: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, x.RecordID, m.RecordID
}

func TestGCClassifiesMilestoneFromBytesNotColumn(t *testing.T) {
	// Control: the untampered fixture collects nothing — the
	// milestone retains itself and, through its closure, x.
	controlPath, cx, cm := seedMilestoneFixture(t)
	control := reopenStore(t, controlPath)
	controlReport, err := control.CollectGarbage()
	if err != nil {
		t.Fatalf("control CollectGarbage: %v", err)
	}
	if controlReport != (store.GarbageReport{Collected: 0, Kept: 4}) {
		t.Fatalf("control report = %+v, want {Collected:0 Kept:4}", controlReport)
	}

	// Tampered: the milestone's schema_ref COLUMN now claims an
	// ordinary ref. Its canonical bytes still say milestone, and
	// GC must classify by the bytes: the report is identical and
	// both records survive.
	path, x, m := seedMilestoneFixture(t)
	if x != cx || m != cm {
		t.Fatalf("fixture is not deterministic: (%s, %s) vs (%s, %s)", x, m, cx, cm)
	}
	rawExec(t, path, "UPDATE records SET schema_ref = ? WHERE record_id = ?", "test.fact", m)
	s := reopenStore(t, path)
	report, err := s.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage over tampered column: %v", err)
	}
	if report != controlReport {
		t.Fatalf("report = %+v, want identical to untampered %+v", report, controlReport)
	}
	for _, id := range []string{x, m} {
		if _, err := s.GetRecord(id); err != nil {
			t.Fatalf("record %s collected after its column was flipped: %v", id, err)
		}
	}
}

// seedOrdinaryFixture builds a store with one genuinely
// unreachable fact u (ordinal 0, cited by nothing, not the
// frontier): a cites only the root (ordinal 1) and the frontier
// tail cites a (ordinal 2). It closes the store and returns the
// file path and u's ID.
func seedOrdinaryFixture(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rootID := record.RootWitnessRecordID()
	u, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"u"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append u: %v", err)
	}
	a, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"a"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append a: %v", err)
	}
	if _, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"tail"}`, []string{a.RecordID})); err != nil {
		t.Fatalf("append tail: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, u.RecordID
}

func TestGCClassifiesForgedCutColumnFromBytes(t *testing.T) {
	// Control: the untampered fixture collects exactly u.
	controlPath, cu := seedOrdinaryFixture(t)
	control := reopenStore(t, controlPath)
	controlReport, err := control.CollectGarbage()
	if err != nil {
		t.Fatalf("control CollectGarbage: %v", err)
	}
	if controlReport != (store.GarbageReport{Collected: 1, Kept: 3}) {
		t.Fatalf("control report = %+v, want {Collected:1 Kept:3}", controlReport)
	}

	// Tampered: u's schema_ref COLUMN now claims the cut ref,
	// but u's body is an ordinary fact body — no owner_path, no
	// through_owner_ordinal. GC must classify by the bytes:
	// complete normally, retain nothing extra, and collect u
	// exactly as the untampered run did. (Classifying by the
	// column instead aborts the pass trying to parse u as a
	// cut.)
	path, u := seedOrdinaryFixture(t)
	if u != cu {
		t.Fatalf("fixture is not deterministic: %s vs %s", u, cu)
	}
	rawExec(t, path, "UPDATE records SET schema_ref = ? WHERE record_id = ?", store.CutSchemaRef, u)
	s := reopenStore(t, path)
	report, err := s.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage over forged cut column: %v", err)
	}
	if report != controlReport {
		t.Fatalf("report = %+v, want identical to untampered %+v", report, controlReport)
	}
	if _, err := s.GetRecord(u); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecord(u) after GC: err = %v, want ErrNotFound (no retention widening)", err)
	}
}
