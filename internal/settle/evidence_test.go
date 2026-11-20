package settle_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/settle"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The graph-evidence test (plan Task 9, Step 1; AC-F6-7): the
// assembled proof that settlement facts are execution-graph
// structure. Two run scopes fork from one parent; a third,
// unrelated owner path carries its own traffic interleaved
// throughout, so at every verb the run scope's tail is NOT the
// store's global frontier. Select settles run-1, Discard
// settles run-2. For each: exactly one fact, on the run scope's
// own path, with the exact schema ref and body fields, and
// caused_by == [that path's frontier immediately before the
// verb] — the interleaved third-path traffic is cited by
// nothing and disturbs nothing. The store's causal queries
// (ProjectEffectiveHistory, EpochSlice) see the facts as graph
// nodes with that same single edge.

// rootWitnessFields extracts the witness fields object — the
// body field of record.RootWitnessBody() — for hand-built test
// drafts, following the slice tests' pattern.
func rootWitnessFields(t *testing.T) canonical.Value {
	t.Helper()
	for _, p := range record.RootWitnessBody().Pairs {
		if p.Key == "body" {
			return p.Value
		}
	}
	t.Fatal("root witness body has no body field")
	return canonical.Value{}
}

// appendTraceFact appends one capture-mode test.fact record to
// path and returns its record ID: a run's own execution traffic,
// or the unrelated third path's.
func appendTraceFact(t *testing.T, s *store.Store, path, bodyJSON string, causedBy []string) string {
	t.Helper()
	body, err := canonical.FromJSON([]byte(bodyJSON))
	if err != nil {
		t.Fatalf("FromJSON(%s): %v", bodyJSON, err)
	}
	r, err := s.AppendFact(path, store.RecordDraft{
		SchemaRef:        "test.fact",
		Mode:             record.ModeCapture,
		Body:             body,
		CausedBy:         causedBy,
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      rootWitnessFields(t),
	})
	if err != nil {
		t.Fatalf("AppendFact(%s, %s): %v", path, bodyJSON, err)
	}
	return r.RecordID
}

// sliceHas reports whether a resolved Slice contains recordID.
func sliceHas(sl slice.Slice, recordID string) bool {
	for _, rec := range sl.Records {
		if rec.ID == recordID {
			return true
		}
	}
	return false
}

// sliceHasEdge reports whether a resolved Slice carries the
// {record, cause} edge (factID, causeID).
func sliceHasEdge(sl slice.Slice, factID, causeID string) bool {
	for _, e := range sl.Edges {
		if e == [2]string{factID, causeID} {
			return true
		}
	}
	return false
}

func TestSettlementFactsFormTheGraphEvidence(t *testing.T) {
	f := newFixture(t)

	// Third-path traffic starts before any fork exists.
	t1 := appendTraceFact(t, f.trace, "other-path", `{"note":"other 1"}`, nil)

	layer1, err := settle.Fork(f.ctx, "parent", "run-1")
	if err != nil {
		t.Fatalf("Fork run-1: %v", err)
	}
	if _, err := settle.Fork(f.ctx, "parent", "run-2"); err != nil {
		t.Fatalf("Fork run-2: %v", err)
	}

	t2 := appendTraceFact(t, f.trace, "other-path", `{"note":"other 2"}`, []string{t1})

	// run-1 does its work: a layer edit, the snapshot advance,
	// and its own execution facts on its own path — an execution
	// fact, then a milestone citing it. The milestone is the
	// path's frontier when Select runs.
	writeTestFile(t, filepath.Join(layer1.Dir, "added.txt"), "added\n", 0o644)
	runHead1 := f.advanceRun(t, layer1, "run-1", f.parent0)
	e1 := appendTraceFact(t, f.trace, "run-1", `{"note":"run-1 executed"}`, nil)
	m1, err := f.trace.AppendMilestone("run-1", "run-1-task", runHead1, []string{e1})
	if err != nil {
		t.Fatalf("AppendMilestone m1: %v", err)
	}

	// More third-path traffic: the store's global tail is now a
	// third-path record, not run-1's frontier.
	t3 := appendTraceFact(t, f.trace, "other-path", `{"note":"other 3"}`, []string{t2})

	preFrontier1, err := f.trace.Frontier("run-1")
	if err != nil {
		t.Fatalf("Frontier(run-1): %v", err)
	}
	if preFrontier1 != m1.RecordID {
		t.Fatalf("pre-Select frontier of run-1 = %s, want its own tail %s (not the global tail %s)", preFrontier1, m1.RecordID, t3)
	}
	selReceipt, err := settle.Select(f.ctx, "parent", f.parentDir, "run-1", "op-select-1")
	if err != nil {
		t.Fatalf("Select run-1: %v", err)
	}
	m2, err := f.trace.AppendMilestone("run-1", "run-1-settled", runHead1, []string{selReceipt.RecordID})
	if err != nil {
		t.Fatalf("AppendMilestone m2: %v", err)
	}

	t4 := appendTraceFact(t, f.trace, "other-path", `{"note":"other 4"}`, []string{t3})

	// run-2 gets its own execution fact, more third-path
	// traffic lands, then Discard settles it.
	e2 := appendTraceFact(t, f.trace, "run-2", `{"note":"run-2 executed"}`, nil)
	t5 := appendTraceFact(t, f.trace, "other-path", `{"note":"other 5"}`, []string{t4})
	preFrontier2, err := f.trace.Frontier("run-2")
	if err != nil {
		t.Fatalf("Frontier(run-2): %v", err)
	}
	if preFrontier2 != e2 {
		t.Fatalf("pre-Discard frontier of run-2 = %s, want its own tail %s", preFrontier2, e2)
	}
	disReceipt, err := settle.Discard(f.ctx, "run-2", "op-discard-2")
	if err != nil {
		t.Fatalf("Discard run-2: %v", err)
	}

	// run-1: exactly one fact — the select fact, exact schema
	// ref and body, caused_by == [run-1's pre-verb frontier].
	facts1 := f.settleFacts(t, "run-1")
	if len(facts1) != 1 {
		t.Fatalf("settle facts on run-1 = %d, want exactly 1", len(facts1))
	}
	selFact := facts1[0]
	if selFact.ID != selReceipt.RecordID {
		t.Fatalf("run-1 fact ID = %s, receipt = %s", selFact.ID, selReceipt.RecordID)
	}
	if ref, _ := stringField(selFact.Record, "schema_ref"); ref != store.SettleSelectSchemaRef {
		t.Fatalf("run-1 fact schema_ref = %q, want %q", ref, store.SettleSelectSchemaRef)
	}
	if got := factCausedBy(t, selFact); !reflect.DeepEqual(got, []string{preFrontier1}) {
		t.Fatalf("select fact caused_by = %v, want [run-1 pre-verb frontier %s]", got, preFrontier1)
	}
	selBody := factBody(t, selFact)
	for key, want := range map[string]string{
		"op": "op-select-1", "parent_scope": "parent", "run_scope": "run-1",
		"parent_head": f.parent0, "run_head": runHead1,
	} {
		if got, _ := stringField(selBody, key); got != want {
			t.Fatalf("select fact body[%q] = %q, want %q", key, got, want)
		}
	}

	// run-2: exactly one fact — the discard fact, same law.
	facts2 := f.settleFacts(t, "run-2")
	if len(facts2) != 1 {
		t.Fatalf("settle facts on run-2 = %d, want exactly 1", len(facts2))
	}
	disFact := facts2[0]
	if disFact.ID != disReceipt.RecordID {
		t.Fatalf("run-2 fact ID = %s, receipt = %s", disFact.ID, disReceipt.RecordID)
	}
	if ref, _ := stringField(disFact.Record, "schema_ref"); ref != store.SettleDiscardSchemaRef {
		t.Fatalf("run-2 fact schema_ref = %q, want %q", ref, store.SettleDiscardSchemaRef)
	}
	if got := factCausedBy(t, disFact); !reflect.DeepEqual(got, []string{preFrontier2}) {
		t.Fatalf("discard fact caused_by = %v, want [run-2 pre-verb frontier %s]", got, preFrontier2)
	}
	disBody := factBody(t, disFact)
	for key, want := range map[string]string{
		"op": "op-discard-2", "run_scope": "run-2", "head": f.parent0,
		"archive_ref": "refs/vcscore/archive/run-2",
	} {
		if got, _ := stringField(disBody, key); got != want {
			t.Fatalf("discard fact body[%q] = %q, want %q", key, got, want)
		}
	}

	// The third path is undisturbed and uncited: its records
	// are exactly its own chain, none is a settlement fact, its
	// frontier is its own tail, and no record on either run
	// path cites any third-path record.
	otherRecs, err := f.trace.PathRecords("other-path")
	if err != nil {
		t.Fatalf("PathRecords(other-path): %v", err)
	}
	wantOther := []string{t1, t2, t3, t4, t5}
	if len(otherRecs) != len(wantOther) {
		t.Fatalf("other-path records = %d, want %d", len(otherRecs), len(wantOther))
	}
	for i, rec := range otherRecs {
		if rec.ID != wantOther[i] {
			t.Fatalf("other-path record %d = %s, want %s", i, rec.ID, wantOther[i])
		}
		if ref, _ := stringField(rec.Record, "schema_ref"); ref == store.SettleSelectSchemaRef ||
			ref == store.SettleApplySchemaRef || ref == store.SettleDiscardSchemaRef {
			t.Fatalf("other-path record %s carries settlement schema_ref %q", rec.ID, ref)
		}
	}
	if frontier, err := f.trace.Frontier("other-path"); err != nil || frontier != t5 {
		t.Fatalf("Frontier(other-path) = %s, %v; want its own tail %s", frontier, err, t5)
	}
	thirdIDs := map[string]bool{t1: true, t2: true, t3: true, t4: true, t5: true}
	for _, path := range []string{"run-1", "run-2"} {
		recs, err := f.trace.PathRecords(path)
		if err != nil {
			t.Fatalf("PathRecords(%s): %v", path, err)
		}
		for _, rec := range recs {
			for _, cause := range factCausedBy(t, rec) {
				if thirdIDs[cause] {
					t.Fatalf("%s record %s cites third-path record %s", path, rec.ID, cause)
				}
			}
		}
	}

	// The causal queries see the facts as graph structure:
	// each path's effective history contains its settlement
	// fact with the {fact, pre-verb frontier} edge, and the
	// third path's history contains neither fact.
	proj1, err := slice.ProjectEffectiveHistory(f.trace, "run-1")
	if err != nil {
		t.Fatalf("ProjectEffectiveHistory(run-1): %v", err)
	}
	if !sliceHas(proj1, selReceipt.RecordID) {
		t.Fatalf("run-1 effective history omits the select fact %s", selReceipt.RecordID)
	}
	if !sliceHasEdge(proj1, selReceipt.RecordID, preFrontier1) {
		t.Fatalf("run-1 effective history lacks edge {%s, %s}", selReceipt.RecordID, preFrontier1)
	}
	proj2, err := slice.ProjectEffectiveHistory(f.trace, "run-2")
	if err != nil {
		t.Fatalf("ProjectEffectiveHistory(run-2): %v", err)
	}
	if !sliceHas(proj2, disReceipt.RecordID) {
		t.Fatalf("run-2 effective history omits the discard fact %s", disReceipt.RecordID)
	}
	if !sliceHasEdge(proj2, disReceipt.RecordID, preFrontier2) {
		t.Fatalf("run-2 effective history lacks edge {%s, %s}", disReceipt.RecordID, preFrontier2)
	}
	projOther, err := slice.ProjectEffectiveHistory(f.trace, "other-path")
	if err != nil {
		t.Fatalf("ProjectEffectiveHistory(other-path): %v", err)
	}
	if sliceHas(projOther, selReceipt.RecordID) || sliceHas(projOther, disReceipt.RecordID) {
		t.Fatal("other-path effective history contains a settlement fact from a run path")
	}

	// The epoch between run-1's two milestones is exactly the
	// settled segment: the select fact and the closing
	// milestone, the opening milestone excluded.
	epoch, err := slice.EpochSlice(f.trace, "run-1", m1.RecordID, m2.RecordID)
	if err != nil {
		t.Fatalf("EpochSlice(run-1, m1, m2): %v", err)
	}
	if !sliceHas(epoch, selReceipt.RecordID) {
		t.Fatalf("epoch (m1, m2] omits the select fact %s", selReceipt.RecordID)
	}
	if !sliceHas(epoch, m2.RecordID) {
		t.Fatalf("epoch (m1, m2] omits its closing milestone %s", m2.RecordID)
	}
	if sliceHas(epoch, m1.RecordID) {
		t.Fatalf("epoch (m1, m2] contains its opening milestone %s, want it excluded", m1.RecordID)
	}
	if sliceHas(epoch, e1) {
		t.Fatalf("epoch (m1, m2] contains pre-milestone record %s, want it excluded", e1)
	}
}
