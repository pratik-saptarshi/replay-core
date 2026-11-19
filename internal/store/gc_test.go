package store_test

import (
	"errors"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

func TestGCSurvival(t *testing.T) { // AC-F2-7
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()

	// Fixture: main path runs/main with a published cut mid-chain.
	// Ordinals: r1=0, r2=1, cut=2, milestone=3, s1=4, s2=5, s3=6,
	// r3=7, r4=8 (frontier tail). The cut runs through r2.
	r1, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"r1"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append r1: %v", err)
	}
	r2, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"r2"}`, []string{r1.RecordID}))
	if err != nil {
		t.Fatalf("append r2: %v", err)
	}
	cutID, err := slice.PublishCut(s, "runs/main", r2.RecordID)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	milestone, err := s.AppendMilestone("runs/main", "task-gc", "snap-gc", []string{r2.RecordID})
	if err != nil {
		t.Fatalf("AppendMilestone: %v", err)
	}

	// Side branch: 3 records citing a main-path record (r1) but
	// cited by nothing — unreachable from any retention root: the
	// frontier tail's ancestry runs r4→r3→r2→r1, never through the
	// branch, and no cut or milestone reaches it either.
	s1, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"s1"}`, []string{r1.RecordID}))
	if err != nil {
		t.Fatalf("append s1: %v", err)
	}
	s2, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"s2"}`, []string{s1.RecordID}))
	if err != nil {
		t.Fatalf("append s2: %v", err)
	}
	s3, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"s3"}`, []string{s2.RecordID}))
	if err != nil {
		t.Fatalf("append s3: %v", err)
	}

	r3, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"r3"}`, []string{r2.RecordID}))
	if err != nil {
		t.Fatalf("append r3: %v", err)
	}
	r4, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"r4"}`, []string{r3.RecordID}))
	if err != nil {
		t.Fatalf("append r4: %v", err)
	}

	// Resolve the cut and record the slice digest before GC.
	before, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut before GC: %v", err)
	}
	digestBefore, err := before.Digest()
	if err != nil {
		t.Fatalf("slice Digest before GC: %v", err)
	}

	report, err := s.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if report.Collected != 3 {
		t.Fatalf("Collected = %d, want 3 (the side branch)", report.Collected)
	}
	if report.Kept != 7 {
		t.Fatalf("Kept = %d, want 7 (root, r1, r2, cut, milestone, r3, r4)", report.Kept)
	}

	// Re-resolve the cut: the digest is identical to pre-GC and
	// every sliced record is still GetRecord-able.
	after, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut after GC: %v", err)
	}
	digestAfter, err := after.Digest()
	if err != nil {
		t.Fatalf("slice Digest after GC: %v", err)
	}
	if digestAfter != digestBefore {
		t.Fatalf("cut digest changed across GC: before %s, after %s", digestBefore, digestAfter)
	}
	for _, rec := range before.Records {
		if _, err := s.GetRecord(rec.ID); err != nil {
			t.Fatalf("sliced record %s not readable after GC: %v", rec.ID, err)
		}
	}

	// Each side-branch record is gone.
	for _, id := range []string{s1.RecordID, s2.RecordID, s3.RecordID} {
		if _, err := s.GetRecord(id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetRecord(%s) after GC: err = %v, want ErrNotFound", id, err)
		}
	}

	// The frontier tail and the milestone record survive.
	tail, err := s.Frontier("runs/main")
	if err != nil {
		t.Fatalf("Frontier after GC: %v", err)
	}
	if tail != r4.RecordID {
		t.Fatalf("frontier tail after GC = %s, want %s", tail, r4.RecordID)
	}
	if _, err := s.GetRecord(r4.RecordID); err != nil {
		t.Fatalf("frontier tail not readable after GC: %v", err)
	}
	if _, err := s.GetRecord(milestone.RecordID); err != nil {
		t.Fatalf("milestone not readable after GC: %v", err)
	}
}
