package store_test

import (
	"fmt"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/record"
)

// TestPathRecordsThroughBoundsToPrefix pins the bounded prefix
// read behind cut resolution (Codex P2 on PR #2): the ordinal
// bound lives in the query, so records beyond the bound are
// never enumerated or fetched. (The whole-path PathRecords
// enumerates every ID and fetches each; a GC pass collecting an
// out-of-prefix record between those steps made a fully
// retained cut's resolution fail with ErrNotFound.)
func TestPathRecordsThroughBoundsToPrefix(t *testing.T) {
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()

	var ids []string
	prev := rootID
	for i := 0; i < 4; i++ {
		r, err := s.AppendFact("runs/a", draftCiting(t, "test.fact", fmt.Sprintf(`{"n":%d}`, i), []string{prev}))
		if err != nil {
			t.Fatalf("AppendFact %d: %v", i, err)
		}
		ids = append(ids, r.RecordID)
		prev = r.RecordID
	}

	assertPrefix := func(throughOrdinal int64, wantIDs []string) {
		t.Helper()
		recs, err := s.PathRecordsThrough("runs/a", throughOrdinal)
		if err != nil {
			t.Fatalf("PathRecordsThrough(runs/a, %d): %v", throughOrdinal, err)
		}
		if len(recs) != len(wantIDs) {
			t.Fatalf("PathRecordsThrough(runs/a, %d) returned %d records, want %d", throughOrdinal, len(recs), len(wantIDs))
		}
		for i, rec := range recs {
			if rec.ID != wantIDs[i] {
				t.Fatalf("PathRecordsThrough(runs/a, %d)[%d] = %s, want %s (ordinal order)", throughOrdinal, i, rec.ID, wantIDs[i])
			}
			if rec.OwnerOrdinal != int64(i) {
				t.Fatalf("PathRecordsThrough(runs/a, %d)[%d] ordinal = %d, want %d", throughOrdinal, i, rec.OwnerOrdinal, i)
			}
			if rec.OwnerPath != "runs/a" {
				t.Fatalf("PathRecordsThrough(runs/a, %d)[%d] owner path = %q, want runs/a", throughOrdinal, i, rec.OwnerPath)
			}
		}
	}

	// The bound selects exactly the addressed prefix, in order.
	assertPrefix(0, ids[:1])
	assertPrefix(1, ids[:2])
	assertPrefix(2, ids[:3])
	// A bound at or past the tail returns the whole path,
	// identical to PathRecords.
	assertPrefix(3, ids)
	assertPrefix(99, ids)
	all, err := s.PathRecords("runs/a")
	if err != nil {
		t.Fatalf("PathRecords: %v", err)
	}
	if len(all) != len(ids) {
		t.Fatalf("PathRecords returned %d records, want %d", len(all), len(ids))
	}

	// A path with no records reads as empty at any bound.
	empty, err := s.PathRecordsThrough("runs/none", 5)
	if err != nil {
		t.Fatalf("PathRecordsThrough(runs/none, 5): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("PathRecordsThrough(runs/none, 5) returned %d records, want 0", len(empty))
	}

	// The bound is per owner path: records on another path at
	// the same ordinals are not selected.
	other, err := s.AppendFact("runs/b", draftCiting(t, "test.fact", `{"n":"other"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("AppendFact runs/b: %v", err)
	}
	recs, err := s.PathRecordsThrough("runs/a", 99)
	if err != nil {
		t.Fatalf("PathRecordsThrough(runs/a, 99): %v", err)
	}
	for _, rec := range recs {
		if rec.ID == other.RecordID {
			t.Fatal("PathRecordsThrough(runs/a, 99) selected a record from runs/b")
		}
	}
}
