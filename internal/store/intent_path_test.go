package store_test

import (
	"errors"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The cross-path intent suite (issue #3, minimal fix): an intent
// ID belongs to the owner path that first used it, globally.
// Reusing it on a different path used to fall into the ordinary
// replay/conflict logic against the first path's row — the same
// drafts returned the FIRST path's receipts with a nil error and
// appended nothing on the requested path. It is now rejected
// with ErrIntentPathMismatch before the digest is ever compared;
// per-path intent scoping remains an open Phase 2 design
// question (issue #3).

func TestAppendIntentRejectsCrossPathReuse(t *testing.T) {
	s := openTemp(t)
	drafts := []store.RecordDraft{draft(t, "test.fact", `{"n":1}`), draft(t, "test.fact", `{"n":2}`)}
	if _, err := s.AppendBatch("runs/a", "intent-shared", drafts); err != nil {
		t.Fatalf("first use on runs/a: %v", err)
	}

	// Same intent, same drafts, different path: not a replay.
	_, err := s.AppendBatch("runs/b", "intent-shared", drafts)
	if !errors.Is(err, store.ErrIntentPathMismatch) {
		t.Fatalf("same drafts cross-path: err = %v, want ErrIntentPathMismatch", err)
	}
	if recs, _ := s.PathRecords("runs/b"); len(recs) != 0 {
		t.Fatalf("cross-path reuse appended %d records on runs/b", len(recs))
	}

	// Same intent, different drafts, different path: the path
	// check precedes the digest check, so this is the path
	// error, not IntentConflictError.
	other := []store.RecordDraft{draft(t, "test.fact", `{"n":3}`)}
	_, err = s.AppendBatch("runs/b", "intent-shared", other)
	if !errors.Is(err, store.ErrIntentPathMismatch) {
		t.Fatalf("different drafts cross-path: err = %v, want ErrIntentPathMismatch", err)
	}
	var conflict *store.IntentConflictError
	if errors.As(err, &conflict) {
		t.Fatalf("cross-path reuse surfaced as IntentConflictError: %v", err)
	}
	if recs, _ := s.PathRecords("runs/b"); len(recs) != 0 {
		t.Fatalf("cross-path conflict appended %d records on runs/b", len(recs))
	}

	// The owning path's replay is unaffected.
	again, err := s.AppendBatch("runs/a", "intent-shared", drafts)
	if err != nil {
		t.Fatalf("same-path replay after cross-path rejections: %v", err)
	}
	if len(again) != 2 {
		t.Fatalf("same-path replay returned %d receipts, want 2", len(again))
	}
}
