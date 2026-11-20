package slice_test

import (
	"errors"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The abandon-target suite (panel W3-4): Abandon minted its
// record without ever fetching the target, so abandoning a
// nonexistent ID returned a success receipt for a silent no-op
// and abandoning a record on another path also succeeded.
// MintCut has always fetched and path-checked its through-fact;
// the abandon target is now validated the same way, before
// anything is minted.

func TestAbandonRefusesNonexistentTarget(t *testing.T) {
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()
	fact := appendFact(t, s, "runs/ab", `{"n":"f1"}`, []string{rootID})

	ghost := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := slice.Abandon(s, "runs/ab", ghost, []string{fact}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Abandon of a nonexistent target: err = %v, want ErrNotFound", err)
	}
	recs, err := s.PathRecords("runs/ab")
	if err != nil {
		t.Fatalf("PathRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("path holds %d records after refused abandon, want 1 (no abandon record minted)", len(recs))
	}
}

func TestAbandonRefusesCrossPathTarget(t *testing.T) {
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()
	foreign := appendFact(t, s, "runs/other", `{"n":"f1"}`, []string{rootID})

	if _, err := slice.Abandon(s, "runs/ab", foreign, nil); err == nil {
		t.Fatal("Abandon of a target on another path succeeded; want a descriptive error")
	}
	recs, err := s.PathRecords("runs/ab")
	if err != nil {
		t.Fatalf("PathRecords: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("path holds %d records after refused abandon, want 0 (no abandon record minted)", len(recs))
	}
}
