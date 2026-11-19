package store_test

import (
	"errors"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// TestReservedSchemaRefsRejected (ADV-3): the three system schema
// refs — cut, milestone, abandon — may be minted only by their
// constructors, which derive the record body from validated
// records. AppendFact and AppendBatch must reject any caller draft
// carrying one, appending nothing: a forged cut claiming an
// arbitrary through_owner_ordinal otherwise resolves a slice that
// grows as the store grows, against AC-F3-2.
func TestReservedSchemaRefsRejected(t *testing.T) {
	reserved := []string{store.CutSchemaRef, store.MilestoneSchemaRef, store.AbandonSchemaRef}
	for _, ref := range reserved {
		t.Run("AppendFact/"+ref, func(t *testing.T) {
			s := openTemp(t)
			d := draft(t, ref, `{"n":1}`)
			if _, err := s.AppendFact("runs/a", d); !errors.Is(err, store.ErrReservedSchemaRef) {
				t.Fatalf("AppendFact with %s: err = %v, want ErrReservedSchemaRef", ref, err)
			}
			recs, err := s.PathRecords("runs/a")
			if err != nil {
				t.Fatalf("PathRecords: %v", err)
			}
			if len(recs) != 0 {
				t.Fatalf("AppendFact with %s appended %d records, want 0", ref, len(recs))
			}
		})
	}

	t.Run("AppendBatch is atomic against a reserved draft", func(t *testing.T) {
		s := openTemp(t)
		good := draft(t, "test.fact", `{"n":1}`)
		forged := draft(t, store.CutSchemaRef, `{"n":2}`)
		if _, err := s.AppendBatch("runs/a", "batch-1", []store.RecordDraft{good, forged}); !errors.Is(err, store.ErrReservedSchemaRef) {
			t.Fatalf("AppendBatch with a %s draft: err = %v, want ErrReservedSchemaRef", store.CutSchemaRef, err)
		}
		recs, err := s.PathRecords("runs/a")
		if err != nil {
			t.Fatalf("PathRecords: %v", err)
		}
		if len(recs) != 0 {
			t.Fatalf("AppendBatch appended %d records, want 0 (the valid draft must not land either)", len(recs))
		}
	})

	// The panel's forged cut, verbatim in shape: a caller-built
	// cut body claiming through_owner_ordinal 9999 for a real
	// through-fact. Rejected at the append boundary, so no cut
	// record exists for ReadCut to resolve.
	t.Run("the forged cut from the panel reproduction", func(t *testing.T) {
		s := openTemp(t)
		fact, err := s.AppendFact("runs/a", draft(t, "test.fact", `{"n":1}`))
		if err != nil {
			t.Fatalf("AppendFact fact: %v", err)
		}
		forged := draftCiting(t, store.CutSchemaRef,
			`{"owner_path":"runs/a","through_fact_id":"`+fact.RecordID+`","through_owner_ordinal":9999}`,
			[]string{fact.RecordID})
		forged.Mode = record.ModeDeclaration
		if _, err := s.AppendFact("runs/a", forged); !errors.Is(err, store.ErrReservedSchemaRef) {
			t.Fatalf("AppendFact forged cut: err = %v, want ErrReservedSchemaRef", err)
		}
		recs, err := s.PathRecords("runs/a")
		if err != nil {
			t.Fatalf("PathRecords: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("path holds %d records, want 1 (only the real fact)", len(recs))
		}
	})

	// The constructors keep working through the internal path:
	// AppendMilestone mints MilestoneSchemaRef itself.
	t.Run("AppendMilestone still mints", func(t *testing.T) {
		s := openTemp(t)
		if _, err := s.AppendMilestone("runs/a", "task-1", "", nil); err != nil {
			t.Fatalf("AppendMilestone: %v", err)
		}
		recs, err := s.PathRecords("runs/a")
		if err != nil {
			t.Fatalf("PathRecords: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("path holds %d records, want 1 (the milestone)", len(recs))
		}
	})
}
