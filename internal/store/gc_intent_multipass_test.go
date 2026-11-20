package store_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// TestGCIntentCollectedAcrossPasses pins the multi-pass intent
// rule (Codex P2 on PR #2): an intent row is dead when none of
// its receipted records survives, judged against the kept set —
// not when every receipt happens to fall in one pass's collected
// set. The old rule compared receipts against the current pass's
// deletions only, so a batch whose records were collected across
// two passes leaked its intent row permanently: after the pass
// that collected its last record, the earlier receipts were
// absent from that pass's set, the row survived, and no later
// pass could ever remove it.
func TestGCIntentCollectedAcrossPasses(t *testing.T) {
	s := openTemp(t)
	drafts := []store.RecordDraft{
		draft(t, "test.fact", `{"n":"a"}`),
		draft(t, "test.fact", `{"n":"b"}`),
	}
	first, err := s.AppendBatch("runs/gc", "intent-ab", drafts)
	if err != nil {
		t.Fatalf("append batch: %v", err)
	}
	aID, bID := first[0].RecordID, first[1].RecordID

	// Pass 1: B is the frontier tail and is kept; A, cited by
	// nothing, is collected. The intent must survive — B's
	// receipt is still live — and replaying it returns the
	// original receipts without appending.
	report, err := s.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage pass 1: %v", err)
	}
	if report.Collected != 1 || report.Kept != 2 {
		t.Fatalf("pass 1 report = %+v, want {Collected:1 Kept:2} (A collected; root and B kept)", report)
	}
	if _, err := s.GetRecord(aID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecord(A) after pass 1: err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetRecord(bID); err != nil {
		t.Fatalf("GetRecord(B) after pass 1: %v", err)
	}
	again, err := s.AppendBatch("runs/gc", "intent-ab", drafts)
	if err != nil {
		t.Fatalf("replay after pass 1: %v", err)
	}
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("intent did not survive pass 1: replay receipts = %v, want %v", again, first)
	}
	if recs, _ := s.PathRecords("runs/gc"); len(recs) != 1 {
		t.Fatalf("replay after pass 1 appended: %d records on runs/gc, want 1", len(recs))
	}

	// A new frontier: C cites nothing, so B becomes unreachable.
	cDrafts := []store.RecordDraft{draft(t, "test.fact", `{"n":"c"}`)}
	cFirst, err := s.AppendBatch("runs/gc", "intent-c", cDrafts)
	if err != nil {
		t.Fatalf("append C: %v", err)
	}

	// Pass 2 collects B — the intent's last surviving receipt.
	report, err = s.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage pass 2: %v", err)
	}
	if report.Collected != 1 || report.Kept != 2 {
		t.Fatalf("pass 2 report = %+v, want {Collected:1 Kept:2} (B collected; root and C kept)", report)
	}
	if _, err := s.GetRecord(bID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecord(B) after pass 2: err = %v, want ErrNotFound", err)
	}

	// Control: intent-c's receipt survives, so intent-c still
	// replays rather than re-appending.
	cAgain, err := s.AppendBatch("runs/gc", "intent-c", cDrafts)
	if err != nil {
		t.Fatalf("replay intent-c after pass 2: %v", err)
	}
	if !reflect.DeepEqual(cFirst, cAgain) {
		t.Fatalf("intent-c receipts changed across pass 2: %v vs %v", cAgain, cFirst)
	}
	if recs, _ := s.PathRecords("runs/gc"); len(recs) != 1 {
		t.Fatalf("intent-c replay appended: %d records on runs/gc, want 1", len(recs))
	}

	// The intent-ab row is gone: the same call that replayed
	// before now performs a fresh append. The records are
	// content-addressed, so the IDs recur — but at new ordinals,
	// and the path grows.
	fresh, err := s.AppendBatch("runs/gc", "intent-ab", drafts)
	if err != nil {
		t.Fatalf("append under intent-ab after pass 2: %v", err)
	}
	if fresh[0].RecordID != aID || fresh[1].RecordID != bID {
		t.Fatalf("fresh append IDs = %v, want the original content-addressed IDs %v", fresh, first)
	}
	if reflect.DeepEqual(first, fresh) {
		t.Fatalf("intent-ab survived pass 2: the call replayed the stale receipts %v instead of appending afresh", fresh)
	}
	if recs, _ := s.PathRecords("runs/gc"); len(recs) != 3 {
		t.Fatalf("fresh append under intent-ab: %d records on runs/gc, want 3 (C, A, B)", len(recs))
	}
}
