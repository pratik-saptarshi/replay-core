package slice_test

import (
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// prefixClosure resolves the prefix closure at a milestone by
// publishing a cut through it and resolving the cut — the Task 7
// machinery as the test oracle for the partition law.
func prefixClosure(t *testing.T, s *store.Store, path, throughID string) map[string]bool {
	t.Helper()
	cutID, err := slice.PublishCut(s, path, throughID)
	if err != nil {
		t.Fatalf("PublishCut(%s): %v", throughID, err)
	}
	sl, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut(%s): %v", cutID, err)
	}
	set := make(map[string]bool, len(sl.Records))
	for _, id := range sl.Order() {
		set[id] = true
	}
	return set
}

func TestEpochSliceBetweenMilestones(t *testing.T) { // AC-F3-5
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()
	// Fixture: r1, M1, r2, r3, M2, r4 on one path (M = milestones),
	// each record citing the previous; r1 cites the root.
	path := "runs/epoch"
	r1 := appendFact(t, s, path, `{"n":"r1"}`, []string{rootID})
	m1r, err := s.AppendMilestone(path, "task-1", "", []string{r1})
	if err != nil {
		t.Fatalf("AppendMilestone M1: %v", err)
	}
	m1 := m1r.RecordID
	r2 := appendFact(t, s, path, `{"n":"r2"}`, []string{m1})
	r3 := appendFact(t, s, path, `{"n":"r3"}`, []string{r2})
	m2r, err := s.AppendMilestone(path, "task-2", "", []string{r3})
	if err != nil {
		t.Fatalf("AppendMilestone M2: %v", err)
	}
	m2 := m2r.RecordID
	r4 := appendFact(t, s, path, `{"n":"r4"}`, []string{m2})

	epoch, err := slice.EpochSlice(s, path, m1, m2)
	if err != nil {
		t.Fatalf("EpochSlice(M1, M2): %v", err)
	}
	// epoch(M1, M2) == {r2, r3, M2} exactly (M1 and r1 excluded,
	// M2 included), in the prefix closure's order.
	want := []string{r2, r3, m2}
	got := epoch.Order()
	if len(got) != len(want) {
		t.Fatalf("epoch order = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("epoch order = %v, want exactly %v", got, want)
		}
	}
	// r4 sits past the to milestone: not in the epoch.
	for _, id := range got {
		if id == r4 {
			t.Errorf("epoch contains r4 (%s), appended after M2", r4)
		}
	}
	// Edges are restricted to the epoch's records: {r3 -> r2, M2 -> r3}.
	wantEdges := map[[2]string]bool{{r3, r2}: true, {m2, r3}: true}
	if len(epoch.Edges) != len(wantEdges) {
		t.Fatalf("epoch edges = %v, want exactly %v", epoch.Edges, wantEdges)
	}
	for _, e := range epoch.Edges {
		if !wantEdges[e] {
			t.Errorf("epoch edge %v is not one of the expected restricted edges", e)
		}
	}
	// An epoch is not a published cut: CutID is empty, and the
	// frontier is the to milestone.
	if epoch.CutID != "" {
		t.Errorf("CutID = %q, want %q for an epoch", epoch.CutID, "")
	}
	if epoch.Frontier != m2 {
		t.Errorf("Frontier = %q, want the to milestone %q", epoch.Frontier, m2)
	}

	// Partition law: epoch(root..M1] ∪ epoch(M1, M2] == prefix
	// closure at M2, disjoint — where the first segment is the
	// prefix closure at M1 itself.
	head := prefixClosure(t, s, path, m1)
	full := prefixClosure(t, s, path, m2)
	epochSet := make(map[string]bool, len(got))
	for _, id := range got {
		epochSet[id] = true
	}
	for id := range epochSet {
		if head[id] {
			t.Errorf("epoch and prefix closure at M1 both contain %s — partition must be disjoint", id)
		}
	}
	union := make(map[string]bool, len(head)+len(epochSet))
	for id := range head {
		union[id] = true
	}
	for id := range epochSet {
		union[id] = true
	}
	if len(union) != len(full) {
		t.Fatalf("partition union has %d records, prefix closure at M2 has %d", len(union), len(full))
	}
	for id := range full {
		if !union[id] {
			t.Errorf("prefix closure at M2 contains %s, missing from the partition union", id)
		}
	}
}

func TestEffectAtCursorContract(t *testing.T) { // AC-F3-6
	s := openTemp(t)
	// Fixture slice in order: D1 (declaration), C1 (capture),
	// D2 (declaration), C2 (capture) — a declaration precedes the
	// captures, so position-indexing is distinguishable from
	// capture-indexing (under capture-indexing, position 0 would
	// wrongly yield C1). The chain is citation-linked and D1 cites
	// nothing, so the resolved slice is exactly these four records.
	path := "runs/cursor"
	d1 := appendWithMode(t, s, path, record.ModeDeclaration, `{"n":"d1"}`, nil)
	c1 := appendWithMode(t, s, path, record.ModeCapture, `{"n":"c1"}`, []string{d1})
	d2 := appendWithMode(t, s, path, record.ModeDeclaration, `{"n":"d2"}`, []string{c1})
	c2 := appendWithMode(t, s, path, record.ModeCapture, `{"n":"c2"}`, []string{d2})

	cutID, err := slice.PublishCut(s, path, c2)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	sl, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut: %v", err)
	}
	wantOrder := []string{d1, c1, d2, c2}
	gotOrder := sl.Order()
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("slice order = %v, want exactly %v (fixture guard)", gotOrder, wantOrder)
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("slice order = %v, want exactly %v (fixture guard)", gotOrder, wantOrder)
		}
	}

	// The E3-A contract, per position: position i names the i-th
	// record of the slice; the lookup answers that record's digest
	// only when that record is capture-mode.
	if id, ok := sl.EffectAt(0); ok || id != "" {
		t.Errorf("EffectAt(0) = (%q, %v), want (\"\", false) — D1's position exposes no effect", id, ok)
	}
	if id, ok := sl.EffectAt(1); !ok || id != c1 {
		t.Errorf("EffectAt(1) = (%q, %v), want (%q, true)", id, ok, c1)
	}
	if id, ok := sl.EffectAt(2); ok || id != "" {
		t.Errorf("EffectAt(2) = (%q, %v), want (\"\", false) — D2's position exposes no effect", id, ok)
	}
	if id, ok := sl.EffectAt(3); !ok || id != c2 {
		t.Errorf("EffectAt(3) = (%q, %v), want (%q, true)", id, ok, c2)
	}
	if id, ok := sl.EffectAt(4); ok || id != "" {
		t.Errorf("EffectAt(4) = (%q, %v), want (\"\", false) — out of range", id, ok)
	}
	if id, ok := sl.EffectAt(-1); ok || id != "" {
		t.Errorf("EffectAt(-1) = (%q, %v), want (\"\", false) — out of range", id, ok)
	}
}

func TestEpochSliceRejectsBadMilestones(t *testing.T) {
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()
	// Path A: a1, M1, a2, M2 in order. Path B: b1, MB.
	pathA, pathB := "runs/epoch-a", "runs/epoch-b"
	a1 := appendFact(t, s, pathA, `{"n":"a1"}`, []string{rootID})
	m1r, err := s.AppendMilestone(pathA, "task-a1", "", []string{a1})
	if err != nil {
		t.Fatalf("AppendMilestone M1: %v", err)
	}
	m1 := m1r.RecordID
	a2 := appendFact(t, s, pathA, `{"n":"a2"}`, []string{m1})
	m2r, err := s.AppendMilestone(pathA, "task-a2", "", []string{a2})
	if err != nil {
		t.Fatalf("AppendMilestone M2: %v", err)
	}
	m2 := m2r.RecordID
	b1 := appendFact(t, s, pathB, `{"n":"b1"}`, []string{rootID})
	mbr, err := s.AppendMilestone(pathB, "task-b1", "", []string{b1})
	if err != nil {
		t.Fatalf("AppendMilestone MB: %v", err)
	}
	mb := mbr.RecordID

	cases := []struct {
		name           string
		path, from, to string
	}{
		{"from == to", pathA, m1, m1},
		{"reversed pair", pathA, m2, m1},
		{"cross-path to", pathA, m1, mb},
		{"cross-path from", pathA, mb, m2},
		{"from is not a milestone", pathA, a1, m2},
		{"to is not a milestone", pathA, m1, a2},
		{"unknown from", pathA, "sha256:0000000000000000000000000000000000000000000000000000000000000000", m2},
	}
	for _, tc := range cases {
		if _, err := slice.EpochSlice(s, tc.path, tc.from, tc.to); err == nil {
			t.Errorf("%s: EpochSlice(%q, %s, %s) succeeded, want error", tc.name, tc.path, tc.from, tc.to)
		}
	}
	// The well-formed pair still succeeds.
	if _, err := slice.EpochSlice(s, pathA, m1, m2); err != nil {
		t.Errorf("EpochSlice(M1, M2) on one path: %v, want success", err)
	}
}
