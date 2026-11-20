package slice_test

import (
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
)

func TestProjectionExcludesAbandonedSubtree(t *testing.T) { // AC-F3-4
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()
	// Fixture: main chain m1 -> m2 -> m3 (each cites the previous;
	// m1 cites the root); subtree off m1: s1 -> s2 (s1 cites m1,
	// s2 cites s1). All on one path.
	path := "runs/proj"
	m1 := appendFact(t, s, path, `{"n":"m1"}`, []string{rootID})
	m2 := appendFact(t, s, path, `{"n":"m2"}`, []string{m1})
	m3 := appendFact(t, s, path, `{"n":"m3"}`, []string{m2})
	s1 := appendFact(t, s, path, `{"n":"s1"}`, []string{m1})
	s2 := appendFact(t, s, path, `{"n":"s2"}`, []string{s1})

	// Abandon the s1 subtree, declared from the main chain's tail.
	receipt, err := slice.Abandon(s, path, s1, []string{m3})
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	sl, err := slice.ProjectEffectiveHistory(s, path)
	if err != nil {
		t.Fatalf("ProjectEffectiveHistory: %v", err)
	}
	got := map[string]bool{}
	for _, id := range sl.Order() {
		got[id] = true
	}
	if got[s1] {
		t.Errorf("projection contains abandoned root s1 (%s)", s1)
	}
	if got[s2] {
		t.Errorf("projection contains s2 (%s), a descendant of the abandoned root", s2)
	}
	for _, id := range []string{m1, m2, m3} {
		if !got[id] {
			t.Errorf("projection is missing %s, which no abandon touches", id)
		}
	}
	// The abandon fact itself is neither an abandoned root nor a
	// descendant of one: the declaration stays in effective history.
	if !got[receipt.RecordID] {
		t.Errorf("projection is missing the abandon fact %s itself", receipt.RecordID)
	}
	// A projection carries no cut ID, and its frontier is the path
	// tail — here the abandon fact, the last record appended to the
	// path.
	if sl.CutID != "" {
		t.Errorf("CutID = %q, want %q for a projection", sl.CutID, "")
	}
	if sl.Frontier != receipt.RecordID {
		t.Errorf("Frontier = %q, want the path tail %q", sl.Frontier, receipt.RecordID)
	}

	// Projection, never deletion (spec §5): the abandoned records
	// remain in the store, readable by ID and still on the path.
	if _, err := s.GetRecord(s1); err != nil {
		t.Errorf("GetRecord(s1) after projection: %v — abandoned bytes must remain readable", err)
	}
	if _, err := s.GetRecord(s2); err != nil {
		t.Errorf("GetRecord(s2) after projection: %v — abandoned bytes must remain readable", err)
	}
	pathRecs, err := s.PathRecords(path)
	if err != nil {
		t.Fatalf("PathRecords: %v", err)
	}
	if len(pathRecs) != 6 {
		t.Errorf("path holds %d records, want 6 (m1, m2, m3, s1, s2, abandon fact) — projection must not delete", len(pathRecs))
	}
}
