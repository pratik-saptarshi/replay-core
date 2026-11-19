package slice_test

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// openTemp opens a store on a fresh file under t.TempDir and closes
// it at test cleanup.
func openTemp(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// rootFields extracts the witness fields object — the body field of
// record.RootWitnessBody() — since Phase 0 exports the fields only
// in that wrapped form (plan Decision 4(a)).
func rootFields(t *testing.T) canonical.Value {
	t.Helper()
	for _, p := range record.RootWitnessBody().Pairs {
		if p.Key == "body" {
			return p.Value
		}
	}
	t.Fatal("root witness body has no body field")
	return canonical.Value{}
}

// rootWitnessDigest is the digest of the root witness fields under
// the root witness schema.
func rootWitnessDigest(t *testing.T) string {
	t.Helper()
	d, err := record.WitnessBodyDigest(record.RootWitnessSchemaRef, rootFields(t))
	if err != nil {
		t.Fatalf("WitnessBodyDigest: %v", err)
	}
	return d
}

// draft builds a capture-mode test.fact RecordDraft from a JSON body
// citing causedBy, witnessed by the root witness fields.
func draft(t *testing.T, bodyJSON string, causedBy []string) store.RecordDraft {
	t.Helper()
	body, err := canonical.FromJSON([]byte(bodyJSON))
	if err != nil {
		t.Fatalf("FromJSON(%s): %v", bodyJSON, err)
	}
	return store.RecordDraft{
		SchemaRef:        "test.fact",
		Mode:             record.ModeCapture,
		Body:             body,
		CausedBy:         causedBy,
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      rootFields(t),
	}
}

// appendFact appends one test.fact record to path and returns its
// record ID.
func appendFact(t *testing.T, s *store.Store, path, bodyJSON string, causedBy []string) string {
	t.Helper()
	r, err := s.AppendFact(path, draft(t, bodyJSON, causedBy))
	if err != nil {
		t.Fatalf("AppendFact(%s, %s): %v", path, bodyJSON, err)
	}
	return r.RecordID
}

func TestPublishCutResolvesCausalClosure(t *testing.T) { // AC-F3-1
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()
	// Fixture: path runs/a: r1 -> r2 -> r3 -> r4 (each cites the
	// previous; r1 cites the root). Path runs/b: x1 citing r1 — a
	// cross-path descendant of r1, not an ancestor of anything on
	// runs/a, so it must stay out of the slice; the root itself is
	// the cross-path ancestor the closure pulls in via GetRecord.
	r1 := appendFact(t, s, "runs/a", `{"n":"r1"}`, []string{rootID})
	r2 := appendFact(t, s, "runs/a", `{"n":"r2"}`, []string{r1})
	r3 := appendFact(t, s, "runs/a", `{"n":"r3"}`, []string{r2})
	r4 := appendFact(t, s, "runs/a", `{"n":"r4"}`, []string{r3})
	x1 := appendFact(t, s, "runs/b", `{"n":"x1"}`, []string{r1})

	cutID, err := slice.PublishCut(s, "runs/a", r3)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	cut, err := slice.ReadCut(s, cutID)
	if err != nil {
		t.Fatalf("ReadCut: %v", err)
	}
	if cut.OwnerPath != "runs/a" || cut.ThroughFactID != r3 || cut.ThroughOrdinal != 2 {
		t.Errorf("ReadCut = %+v, want {OwnerPath:runs/a ThroughFactID:%s ThroughOrdinal:2}", cut, r3)
	}

	// Independently computed closure of r3: a plain caused_by walk
	// from the through-fact via GetRecord, sharing no machinery
	// with the implementation's path-prefix seed.
	want := map[string]bool{}
	stack := []string{r3}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if want[id] {
			continue
		}
		want[id] = true
		rec, err := s.GetRecord(id)
		if err != nil {
			t.Fatalf("GetRecord(%s): %v", id, err)
		}
		for _, p := range rec.Record.Pairs {
			if p.Key == "caused_by" {
				for _, e := range p.Value.Elems {
					stack = append(stack, e.Str)
				}
			}
		}
	}
	if len(want) != 4 {
		t.Fatalf("independent closure of r3 = %v, want exactly {root, r1, r2, r3}", want)
	}

	sl, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut: %v", err)
	}
	got := map[string]bool{}
	for _, id := range sl.Order() {
		got[id] = true
	}
	for id := range want {
		if !got[id] {
			t.Errorf("slice is missing ancestor %s", id)
		}
	}
	if len(got) != len(want) {
		t.Errorf("slice has %d records, want %d: %v", len(got), len(want), sl.Order())
	}
	if got[r4] {
		t.Errorf("slice contains r4 (%s), which is past the cut", r4)
	}
	if got[x1] {
		t.Errorf("slice contains x1 (%s), a cross-path descendant, not an ancestor", x1)
	}
	if got[cutID] {
		t.Errorf("slice contains the cut record itself (%s)", cutID)
	}
	// The chain forces exactly one causal order.
	wantOrder := []string{rootID, r1, r2, r3}
	if !reflect.DeepEqual(sl.Order(), wantOrder) {
		t.Errorf("Order() = %v, want %v", sl.Order(), wantOrder)
	}
	// Edges match the caused_by pairs among the selected records.
	wantEdges := map[[2]string]bool{{r1, rootID}: true, {r2, r1}: true, {r3, r2}: true}
	if len(sl.Edges) != len(wantEdges) {
		t.Errorf("Edges = %v, want the %d caused_by pairs among the selected records", sl.Edges, len(wantEdges))
	}
	for _, e := range sl.Edges {
		if !wantEdges[e] {
			t.Errorf("unexpected edge %v", e)
		}
	}
	if sl.CutID != cutID {
		t.Errorf("CutID = %q, want %q", sl.CutID, cutID)
	}
	if sl.Frontier != r3 {
		t.Errorf("Frontier = %q, want the cut's through-fact %q", sl.Frontier, r3)
	}
}

func TestResolvedSliceImmutable(t *testing.T) { // AC-F3-2
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()
	r1 := appendFact(t, s, "runs/a", `{"n":"r1"}`, []string{rootID})
	r2 := appendFact(t, s, "runs/a", `{"n":"r2"}`, []string{r1})
	r3 := appendFact(t, s, "runs/a", `{"n":"r3"}`, []string{r2})

	cutID, err := slice.PublishCut(s, "runs/a", r3)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	before, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut (before growth): %v", err)
	}
	digestBefore, err := before.Digest()
	if err != nil {
		t.Fatalf("Digest (before growth): %v", err)
	}

	// Grow the same path past the cut; the resolved slice must not
	// move.
	r4 := appendFact(t, s, "runs/a", `{"n":"r4"}`, []string{r3})
	appendFact(t, s, "runs/a", `{"n":"r5"}`, []string{r4})

	after, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut (after growth): %v", err)
	}
	digestAfter, err := after.Digest()
	if err != nil {
		t.Fatalf("Digest (after growth): %v", err)
	}
	if !reflect.DeepEqual(before.Order(), after.Order()) {
		t.Errorf("record ID sets changed across appends: before %v, after %v", before.Order(), after.Order())
	}
	if digestBefore != digestAfter {
		t.Errorf("Slice.Digest changed across appends: before %s, after %s", digestBefore, digestAfter)
	}
}

func TestResolveCutDeepChain(t *testing.T) { // Review Focus 5
	s := openTemp(t)
	const n = 10000
	const batchSize = 500
	fields := rootFields(t)
	witnessDigest := rootWitnessDigest(t)

	// A 10,000-record linear chain on one path, appended in batches
	// of 500: each record cites the previous one, the first cites
	// the root. Record IDs are content-derived, so each draft's ID
	// is computed up front to name it as the next draft's parent.
	prev := record.RootWitnessRecordID()
	var tail string
	for start := 0; start < n; start += batchSize {
		drafts := make([]store.RecordDraft, 0, batchSize)
		for i := start; i < start+batchSize && i < n; i++ {
			body, err := canonical.FromJSON([]byte(fmt.Sprintf(`{"n":%d}`, i)))
			if err != nil {
				t.Fatalf("FromJSON: %v", err)
			}
			d := store.RecordDraft{
				SchemaRef:        "test.fact",
				Mode:             record.ModeCapture,
				Body:             body,
				CausedBy:         []string{prev},
				WitnessSchemaRef: record.RootWitnessSchemaRef,
				WitnessBody:      fields,
			}
			id, err := record.RecordDigest(d.SchemaRef, d.Mode, d.Body, d.CausedBy, witnessDigest)
			if err != nil {
				t.Fatalf("RecordDigest(%d): %v", i, err)
			}
			drafts = append(drafts, d)
			prev, tail = id, id
		}
		if _, err := s.AppendBatch("runs/deep", fmt.Sprintf("chain-%d", start/batchSize), drafts); err != nil {
			t.Fatalf("AppendBatch at %d: %v", start, err)
		}
	}

	cutID, err := slice.PublishCut(s, "runs/deep", tail)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	sl, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut: %v", err)
	}
	if len(sl.Records) != n+1 {
		t.Fatalf("resolved %d records, want %d (chain + root)", len(sl.Records), n+1)
	}
	order := sl.Order()
	if order[0] != record.RootWitnessRecordID() {
		t.Errorf("Order()[0] = %s, want the root witness record", order[0])
	}
	if order[n] != tail {
		t.Errorf("Order()[%d] = %s, want the chain tail %s", n, order[n], tail)
	}
	if _, err := sl.Digest(); err != nil {
		t.Errorf("Digest: %v", err)
	}
}

func TestResolveCutRejectsNonEmptyFilter(t *testing.T) {
	// Task 7 honesty rule: until Task 8 implements mode filters, a
	// non-empty filter is rejected outright — silently treating it
	// as nil would be a false claim. Task 8 removes this test when
	// it removes the rejection.
	s := openTemp(t)
	r1 := appendFact(t, s, "runs/a", `{"n":"r1"}`, []string{record.RootWitnessRecordID()})
	cutID, err := slice.PublishCut(s, "runs/a", r1)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	if _, err := slice.ResolveCut(s, cutID, []string{record.ModeCapture}); err == nil {
		t.Fatal("ResolveCut with a non-empty mode filter: got nil error, want rejection until Task 8")
	}
}
