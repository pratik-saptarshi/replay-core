package store_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/store"
)

func TestAppendIntentReplayReturnsPriorReceipt(t *testing.T) { // AC-F2-2, first leg
	s := openTemp(t)
	drafts := []store.RecordDraft{draft(t, "test.fact", `{"n":1}`), draft(t, "test.fact", `{"n":2}`)}
	first, err := s.AppendBatch("runs/a", "intent-1", drafts)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.AppendBatch("runs/a", "intent-1", drafts)
	if err != nil {
		t.Fatalf("replay errored: %v", err)
	}
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("receipts differ: %v vs %v", first, again)
	}
	if recs, _ := s.PathRecords("runs/a"); len(recs) != 2 {
		t.Fatalf("store grew on replay: %d records", len(recs))
	}
}

func TestAppendIntentConflictAppendsNothing(t *testing.T) { // AC-F2-2, second leg
	s := openTemp(t)
	_, _ = s.AppendBatch("runs/a", "intent-1", []store.RecordDraft{draft(t, "test.fact", `{"n":1}`)})
	_, err := s.AppendBatch("runs/a", "intent-1", []store.RecordDraft{draft(t, "test.fact", `{"n":2}`)})
	var conflict *store.IntentConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want IntentConflictError", err)
	}
	if recs, _ := s.PathRecords("runs/a"); len(recs) != 1 {
		t.Fatalf("conflict appended: %d records", len(recs))
	}
}

func TestAppendBatchRejectsDegenerateInputs(t *testing.T) { // Review Focus 3
	s := openTemp(t)
	d := draft(t, "test.fact", `{"n":1}`)
	if _, err := s.AppendBatch("runs/a", "i", nil); !errors.Is(err, store.ErrEmptyBatch) {
		t.Fatalf("empty batch err = %v", err)
	}
	if _, err := s.AppendBatch("runs/a", "", []store.RecordDraft{d}); err == nil {
		t.Fatal("empty intent accepted")
	}
	if _, err := s.AppendBatch("", "i", []store.RecordDraft{d}); err == nil {
		t.Fatal("empty owner path accepted")
	}
	if _, err := s.AppendBatch("replaycore.system", "i", []store.RecordDraft{d}); err == nil {
		t.Fatal("reserved path accepted")
	}
}
