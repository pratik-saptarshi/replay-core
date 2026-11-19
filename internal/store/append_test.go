package store_test

import (
	"fmt"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// draft builds a RecordDraft from a JSON body whose witness material
// is the root witness fields under the root witness schema.
func draft(t *testing.T, schemaRef, bodyJSON string) store.RecordDraft {
	t.Helper()
	body, err := canonical.FromJSON([]byte(bodyJSON))
	if err != nil {
		t.Fatalf("FromJSON(%s): %v", bodyJSON, err)
	}
	return store.RecordDraft{
		SchemaRef:        schemaRef,
		Mode:             record.ModeCapture,
		Body:             body,
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      rootFields(t),
	}
}

func TestAppendBatchOrdinalsGapFreePerOwner(t *testing.T) { // AC-F2-4
	s := openTemp(t)
	// Interleave batches on two owner paths; each path's ordinals must be
	// exactly 0..n-1 across all of its batches. Draft bodies are distinct
	// per (path, iteration, slot): record identity is content-derived and
	// the owner path is not part of it, so reusing a body would collide on
	// records.record_id instead of exercising ordinal allocation.
	var a, b []store.Receipt
	for i := 0; i < 3; i++ {
		ra, err := s.AppendBatch("runs/a", fmt.Sprintf("a-%d", i), []store.RecordDraft{
			draft(t, "test.fact", fmt.Sprintf(`{"path":"a","batch":%d,"slot":0}`, i)),
			draft(t, "test.fact", fmt.Sprintf(`{"path":"a","batch":%d,"slot":1}`, i)),
		})
		if err != nil {
			t.Fatalf("runs/a batch %d: %v", i, err)
		}
		rb, err := s.AppendBatch("runs/b", fmt.Sprintf("b-%d", i), []store.RecordDraft{
			draft(t, "test.fact", fmt.Sprintf(`{"path":"b","batch":%d,"slot":0}`, i)),
		})
		if err != nil {
			t.Fatalf("runs/b batch %d: %v", i, err)
		}
		a = append(a, ra...)
		b = append(b, rb...)
	}
	if len(a) != 6 || len(b) != 3 {
		t.Fatalf("receipt counts: runs/a = %d (want 6), runs/b = %d (want 3)", len(a), len(b))
	}
	for i, r := range a {
		if r.OwnerOrdinal != int64(i) {
			t.Fatalf("runs/a ordinal[%d] = %d", i, r.OwnerOrdinal)
		}
	}
	for i, r := range b {
		if r.OwnerOrdinal != int64(i) {
			t.Fatalf("runs/b ordinal[%d] = %d", i, r.OwnerOrdinal)
		}
	}
}

func TestAppendBatchRejectsUnknownAndSelfCitations(t *testing.T) { // Review Focus 2
	s := openTemp(t)
	bad := draft(t, "test.fact", `{"n":1}`)
	bad.CausedBy = []string{"sha256:0000000000000000000000000000000000000000000000000000000000000000"}
	if _, err := s.AppendBatch("runs/a", "x-1", []store.RecordDraft{bad}); err == nil {
		t.Fatal("unknown citation accepted")
	}
	// a draft citing a later draft in the same batch (self/cycle class) is rejected the same way
	d0 := draft(t, "test.fact", `{"n":1}`)
	d1 := draft(t, "test.fact", `{"n":2}`)
	d0.CausedBy = nil
	id1, _ := record.RecordDigest(d1.SchemaRef, d1.Mode, d1.Body, nil, rootWitnessDigest(t))
	d0.CausedBy = []string{id1} // cites a record that does not exist yet at its position
	if _, err := s.AppendBatch("runs/a", "x-2", []store.RecordDraft{d0, d1}); err == nil {
		t.Fatal("forward citation accepted")
	}
	if recs, _ := s.PathRecords("runs/a"); len(recs) != 0 {
		t.Fatalf("rejected batches left %d records", len(recs))
	}
}
