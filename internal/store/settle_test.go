package store_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// bodyField returns the string stored under key in a record's
// body object, failing the test if the shape is not as expected.
func bodyField(t *testing.T, rec store.StoredRecord, key string) string {
	t.Helper()
	body, ok := recordField(rec.Record, "body")
	if !ok || body.Kind != canonical.KindObject {
		t.Fatalf("record %s has no body object", rec.ID)
	}
	v, ok := recordField(body, key)
	if !ok {
		t.Fatalf("record %s body has no field %q", rec.ID, key)
	}
	if v.Kind != canonical.KindString {
		t.Fatalf("record %s body field %q is not a string", rec.ID, key)
	}
	return v.Str
}

// recordField returns the value stored under key in a canonical
// object value.
func recordField(v canonical.Value, key string) (canonical.Value, bool) {
	if v.Kind != canonical.KindObject {
		return canonical.Value{}, false
	}
	for _, p := range v.Pairs {
		if p.Key == key {
			return p.Value, true
		}
	}
	return canonical.Value{}, false
}

// causedByOf returns a record's caused_by list as strings.
func causedByOf(t *testing.T, rec store.StoredRecord) []string {
	t.Helper()
	v, ok := recordField(rec.Record, "caused_by")
	if !ok || v.Kind != canonical.KindArray {
		t.Fatalf("record %s has no caused_by array", rec.ID)
	}
	out := make([]string, 0, len(v.Elems))
	for _, e := range v.Elems {
		if e.Kind != canonical.KindString {
			t.Fatalf("record %s caused_by element is not a string", rec.ID)
		}
		out = append(out, e.Str)
	}
	return out
}

// TestSettleFactsAreReserved (plan Task 6, Step 3): the three
// settlement schema refs join the reserved set before the fact
// class exists in the wild — AppendFact and AppendBatch carrying
// any of them are refused with ErrReservedSchemaRef and append
// nothing (the ADV-3 law, applied prospectively per Decision 4).
func TestSettleFactsAreReserved(t *testing.T) {
	reserved := []string{store.SettleSelectSchemaRef, store.SettleApplySchemaRef, store.SettleDiscardSchemaRef}
	for _, ref := range reserved {
		s := openTemp(t)
		d := draft(t, ref, `{"n":1}`)
		if _, err := s.AppendFact("runs/a", d); !errors.Is(err, store.ErrReservedSchemaRef) {
			t.Fatalf("AppendFact with %s: err = %v, want ErrReservedSchemaRef", ref, err)
		}
		if _, err := s.AppendBatch("runs/a", "intent-1", []store.RecordDraft{d}); !errors.Is(err, store.ErrReservedSchemaRef) {
			t.Fatalf("AppendBatch with %s: err = %v, want ErrReservedSchemaRef", ref, err)
		}
		recs, err := s.PathRecords("runs/a")
		if err != nil {
			t.Fatalf("PathRecords: %v", err)
		}
		if len(recs) != 0 {
			t.Fatalf("reserved appends left %d records on runs/a, want 0", len(recs))
		}
	}
}

// TestMintSettleCitesFrontier (plan Task 6, Step 3): a minted
// settlement fact cites the owner path's frontier at mint time —
// the root witness record on an empty path (Frontier's empty-path
// law), the path's tail after appends — and its body round-trips
// exactly the constructor's fields.
func TestMintSettleCitesFrontier(t *testing.T) {
	s := openTemp(t)
	const path = "runs/settle"

	r1, err := s.MintSettleSelect(path, "op-1", "parent", "run-a", "head-a", "head-b")
	if err != nil {
		t.Fatalf("MintSettleSelect: %v", err)
	}
	rec, err := s.GetRecord(r1.RecordID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if got := causedByOf(t, rec); !reflect.DeepEqual(got, []string{record.RootWitnessRecordID()}) {
		t.Fatalf("select fact caused_by = %v, want [root witness %s]", got, record.RootWitnessRecordID())
	}
	for key, want := range map[string]string{
		"op": "op-1", "parent_scope": "parent", "run_scope": "run-a",
		"parent_head": "head-a", "run_head": "head-b",
	} {
		if got := bodyField(t, rec, key); got != want {
			t.Fatalf("select fact body[%q] = %q, want %q", key, got, want)
		}
	}

	// A plain append moves the path's frontier; the next mint
	// cites it.
	plain, err := s.AppendFact(path, draft(t, "test.fact", `{"n":2}`))
	if err != nil {
		t.Fatalf("AppendFact: %v", err)
	}
	r2, err := s.MintSettleApply(path, "op-2", "parent", "run-a", "fork-c", "result-c", []string{"a.txt", "sub/b.txt"})
	if err != nil {
		t.Fatalf("MintSettleApply: %v", err)
	}
	rec, err = s.GetRecord(r2.RecordID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if got := causedByOf(t, rec); !reflect.DeepEqual(got, []string{plain.RecordID}) {
		t.Fatalf("apply fact caused_by = %v, want [frontier %s]", got, plain.RecordID)
	}
	for key, want := range map[string]string{
		"op": "op-2", "parent_scope": "parent", "run_scope": "run-a",
		"fork_commit": "fork-c", "result_commit": "result-c",
	} {
		if got := bodyField(t, rec, key); got != want {
			t.Fatalf("apply fact body[%q] = %q, want %q", key, got, want)
		}
	}
	body, _ := recordField(rec.Record, "body")
	paths, ok := recordField(body, "settled_paths")
	if !ok || paths.Kind != canonical.KindArray {
		t.Fatalf("apply fact body has no settled_paths array")
	}
	var gotPaths []string
	for _, e := range paths.Elems {
		gotPaths = append(gotPaths, e.Str)
	}
	if !reflect.DeepEqual(gotPaths, []string{"a.txt", "sub/b.txt"}) {
		t.Fatalf("apply fact settled_paths = %v, want [a.txt sub/b.txt]", gotPaths)
	}

	r3, err := s.MintSettleDiscard(path, "op-3", "run-a", r2.RecordID, "refs/vcscore/archive/run-a")
	if err != nil {
		t.Fatalf("MintSettleDiscard: %v", err)
	}
	rec, err = s.GetRecord(r3.RecordID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if got := causedByOf(t, rec); !reflect.DeepEqual(got, []string{r2.RecordID}) {
		t.Fatalf("discard fact caused_by = %v, want [frontier %s]", got, r2.RecordID)
	}
	for key, want := range map[string]string{
		"op": "op-3", "run_scope": "run-a", "head": r2.RecordID,
		"archive_ref": "refs/vcscore/archive/run-a",
	} {
		if got := bodyField(t, rec, key); got != want {
			t.Fatalf("discard fact body[%q] = %q, want %q", key, got, want)
		}
	}
}
