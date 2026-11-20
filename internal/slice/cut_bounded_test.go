package slice_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// TestResolveCutSurvivesCollectionBeyondTheCut is the scenario
// half of the bounded-prefix fix (Codex P2 on PR #2): a published
// cut addresses an ordinal prefix, so resolving it may only load
// that prefix. The pre-fix ResolveCut loaded the whole path via
// PathRecords and filtered afterward — PathRecords enumerates
// every ID on the path and then fetches each one, so a GC pass
// collecting an out-of-prefix record between the enumeration and
// the fetch made the fetch fail and the fully retained cut
// resolve to ErrNotFound. The enumerate/fetch interleaving
// cannot be forced from outside the store without a hook, so
// this test pins the end state the structural fix guarantees —
// GC really collects beyond the cut, and the cut still resolves
// to exactly its pre-GC slice — while the store-level unit test
// (TestPathRecordsThroughBoundsToPrefix) pins the bounded read
// itself.
func TestResolveCutSurvivesCollectionBeyondTheCut(t *testing.T) {
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()

	a := appendFact(t, s, "runs/x", `{"n":"a"}`, []string{rootID})
	cutID, err := slice.PublishCut(s, "runs/x", a)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	before, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut before GC: %v", err)
	}
	beforeDigest, err := before.Digest()
	if err != nil {
		t.Fatalf("Digest before GC: %v", err)
	}

	// Two independent facts land beyond the cut. B is citable by
	// nothing and is not the frontier, so GC collects it; C is
	// the frontier and stays.
	b := appendFact(t, s, "runs/x", `{"n":"b"}`, []string{rootID})
	c := appendFact(t, s, "runs/x", `{"n":"c"}`, []string{rootID})

	report, err := s.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if report.Collected != 1 {
		t.Fatalf("GC collected %d records, want exactly 1 (the out-of-prefix B)", report.Collected)
	}
	if _, err := s.GetRecord(b); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecord(B) err = %v, want ErrNotFound (B was collected)", err)
	}
	if _, err := s.GetRecord(c); err != nil {
		t.Fatalf("GetRecord(C): %v (the frontier must survive GC)", err)
	}

	after, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut after GC: %v", err)
	}
	afterDigest, err := after.Digest()
	if err != nil {
		t.Fatalf("Digest after GC: %v", err)
	}
	if afterDigest != beforeDigest {
		t.Fatalf("cut digest changed across GC: before %s, after %s", beforeDigest, afterDigest)
	}
	if got, want := after.Order(), before.Order(); !reflect.DeepEqual(got, want) {
		t.Fatalf("cut order changed across GC: before %v, after %v", want, got)
	}
}
