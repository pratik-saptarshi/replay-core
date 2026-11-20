package store_test

import (
	"errors"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The duplicate-append suite (issue #4): three duplicate shapes
// used to fail an append with a raw SQLite UNIQUE constraint
// string — a draft citing the same record twice (record_edges
// primary key), the same draft twice in one batch (records
// primary key), and a fresh intent carrying content already in
// the store under another intent (records primary key). Each is
// now detected before it reaches the constraint and reported
// with a named sentinel, and the failed append is atomic: no
// records, and no intent row — a corrected retry under the same
// intent ID must succeed.

func pathCount(t *testing.T, s *store.Store, path string) int {
	t.Helper()
	recs, err := s.PathRecords(path)
	if err != nil {
		t.Fatalf("PathRecords(%q): %v", path, err)
	}
	return len(recs)
}

func TestAppendRejectsDuplicateCauseInOneDraft(t *testing.T) {
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()
	d := draftCiting(t, "test.fact", `{"n":1}`, []string{rootID, rootID})
	_, err := s.AppendBatch("runs/dup", "intent-dup-cause", []store.RecordDraft{d})
	if !errors.Is(err, store.ErrDuplicateCause) {
		t.Fatalf("err = %v, want ErrDuplicateCause", err)
	}
	if n := pathCount(t, s, "runs/dup"); n != 0 {
		t.Fatalf("path has %d records after rejected append, want 0", n)
	}
}

func TestAppendRejectsDuplicateDraftInBatch(t *testing.T) {
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()
	d := draftCiting(t, "test.fact", `{"n":1}`, []string{rootID})
	_, err := s.AppendBatch("runs/dup", "intent-dup-draft", []store.RecordDraft{d, d})
	if !errors.Is(err, store.ErrDuplicateRecord) {
		t.Fatalf("err = %v, want ErrDuplicateRecord", err)
	}
	if n := pathCount(t, s, "runs/dup"); n != 0 {
		t.Fatalf("path has %d records after rejected append, want 0", n)
	}
	// No intent row was written for the failed append: the same
	// intent ID accepts a corrected batch instead of replaying
	// or conflicting.
	other := draftCiting(t, "test.fact", `{"n":2}`, []string{rootID})
	receipts, err := s.AppendBatch("runs/dup", "intent-dup-draft", []store.RecordDraft{d, other})
	if err != nil {
		t.Fatalf("corrected retry under the same intent: %v", err)
	}
	if len(receipts) != 2 {
		t.Fatalf("corrected retry returned %d receipts, want 2", len(receipts))
	}
	if n := pathCount(t, s, "runs/dup"); n != 2 {
		t.Fatalf("path has %d records after corrected retry, want 2", n)
	}
}

func TestAppendRejectsContentAlreadyStoredUnderAnotherIntent(t *testing.T) {
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()
	d := draftCiting(t, "test.fact", `{"n":1}`, []string{rootID})
	if _, err := s.AppendBatch("runs/dup", "intent-first", []store.RecordDraft{d}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	_, err := s.AppendBatch("runs/dup", "intent-second", []store.RecordDraft{d})
	if !errors.Is(err, store.ErrDuplicateRecord) {
		t.Fatalf("err = %v, want ErrDuplicateRecord", err)
	}
	if n := pathCount(t, s, "runs/dup"); n != 1 {
		t.Fatalf("path has %d records after rejected append, want 1", n)
	}
	// The first intent's replay is untouched by the rejection.
	receipts, err := s.AppendBatch("runs/dup", "intent-first", []store.RecordDraft{d})
	if err != nil {
		t.Fatalf("replay of the first intent: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("replay returned %d receipts, want 1", len(receipts))
	}
}
