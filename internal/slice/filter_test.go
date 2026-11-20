package slice_test

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// draftWithMode builds a test.fact RecordDraft in the given mode
// from a JSON body citing causedBy, witnessed by the root witness
// fields. (cut_test.go's draft is capture-only.)
func draftWithMode(t *testing.T, mode, bodyJSON string, causedBy []string) store.RecordDraft {
	t.Helper()
	body, err := canonical.FromJSON([]byte(bodyJSON))
	if err != nil {
		t.Fatalf("FromJSON(%s): %v", bodyJSON, err)
	}
	return store.RecordDraft{
		SchemaRef:        "test.fact",
		Mode:             mode,
		Body:             body,
		CausedBy:         causedBy,
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      rootFields(t),
	}
}

// appendWithMode appends one test.fact record in the given mode to
// path and returns its record ID.
func appendWithMode(t *testing.T, s *store.Store, path, mode, bodyJSON string, causedBy []string) string {
	t.Helper()
	r, err := s.AppendFact(path, draftWithMode(t, mode, bodyJSON, causedBy))
	if err != nil {
		t.Fatalf("AppendFact(%s, %s): %v", path, bodyJSON, err)
	}
	return r.RecordID
}

// modeAndSchema extracts the mode and schema_ref fields from a
// stored record object.
func modeAndSchema(rec store.StoredRecord) (mode, schemaRef string) {
	for _, p := range rec.Record.Pairs {
		switch p.Key {
		case "mode":
			mode = p.Value.Str
		case "schema_ref":
			schemaRef = p.Value.Str
		}
	}
	return mode, schemaRef
}

// filterFixture is a deterministic trace (fixed-seed generation)
// for the mode-filter purity law: interleaved capture/declaration
// records with cross-citations on runs/filter, a side path
// runs/other feeding cross-path ancestors in, a milestone and an
// inner cut published inside the range, and a final cut through
// the tail.
type filterFixture struct {
	cutID       string
	milestone   string
	innerCut    string
	declaration string // an ordinary declaration-mode record in range
}

// buildFilterFixture appends the fixture trace to s and returns
// its landmarks.
func buildFilterFixture(t *testing.T, s *store.Store) filterFixture {
	t.Helper()
	rng := rand.New(rand.NewSource(20261118))
	rootID := record.RootWitnessRecordID()
	mode := func() string {
		if rng.Intn(2) == 0 {
			return record.ModeCapture
		}
		return record.ModeDeclaration
	}

	// Side path: a short chain the main path cross-cites, so the
	// closure pulls cross-path ancestors of both modes in.
	var other []string
	prev := rootID
	for i := 0; i < 4; i++ {
		id := appendWithMode(t, s, "runs/other", mode(), fmt.Sprintf(`{"side":%d}`, i), []string{prev})
		other = append(other, id)
		prev = id
	}

	// Main path: 24 generated records, each citing the running
	// tail, with seeded extra citations to earlier main-path
	// records and to the side path. A milestone is appended after
	// the 8th record and an inner cut published after the 16th;
	// both land on the path inside the final cut's range and join
	// the chain as the running tail.
	var fx filterFixture
	var main []string
	tail := rootID
	for i := 0; i < 24; i++ {
		candidates := []string{tail}
		if len(main) > 0 && rng.Intn(3) == 0 {
			candidates = append(candidates, main[rng.Intn(len(main))])
		}
		if rng.Intn(4) == 0 {
			candidates = append(candidates, other[rng.Intn(len(other))])
		}
		// The store rejects a draft that cites the same record
		// twice (duplicate causal edge), so dedupe — the seeded
		// extra citation can land on the tail itself.
		causedBy := candidates[:1]
		for _, c := range candidates[1:] {
			dupe := false
			for _, kept := range causedBy {
				if c == kept {
					dupe = true
					break
				}
			}
			if !dupe {
				causedBy = append(causedBy, c)
			}
		}
		m := mode()
		id := appendWithMode(t, s, "runs/filter", m, fmt.Sprintf(`{"i":%d}`, i), causedBy)
		main = append(main, id)
		tail = id
		if m == record.ModeDeclaration && fx.declaration == "" {
			fx.declaration = id
		}
		if i == 7 {
			r, err := s.AppendMilestone("runs/filter", "task-alpha", "", []string{tail})
			if err != nil {
				t.Fatalf("AppendMilestone: %v", err)
			}
			fx.milestone = r.RecordID
			tail = fx.milestone
		}
		if i == 15 {
			inner, err := slice.PublishCut(s, "runs/filter", tail)
			if err != nil {
				t.Fatalf("PublishCut (inner): %v", err)
			}
			fx.innerCut = inner
			tail = inner
		}
	}
	cutID, err := slice.PublishCut(s, "runs/filter", tail)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	fx.cutID = cutID
	return fx
}

// expectedFiltered applies the filter law's own drop rule to the
// unfiltered resolution: drop records whose mode is not in the
// filter, except milestone and cut records, which are always
// retained (Decision 4(b)); restrict edges to the retained
// records. An empty filter admits all.
func expectedFiltered(unfiltered slice.Slice, filter []string) slice.Slice {
	admit := make(map[string]bool, len(filter))
	for _, m := range filter {
		admit[m] = true
	}
	kept := make(map[string]bool, len(unfiltered.Records))
	out := slice.Slice{CutID: unfiltered.CutID, Frontier: unfiltered.Frontier}
	for _, rec := range unfiltered.Records {
		mode, schemaRef := modeAndSchema(rec)
		if len(filter) == 0 || admit[mode] || schemaRef == store.MilestoneSchemaRef || schemaRef == store.CutSchemaRef {
			kept[rec.ID] = true
			out.Records = append(out.Records, rec)
		}
	}
	for _, e := range unfiltered.Edges {
		if kept[e[0]] && kept[e[1]] {
			out.Edges = append(out.Edges, e)
		}
	}
	return out
}

func TestModeFilterPurityLaw(t *testing.T) { // AC-F3-3
	s := openTemp(t)
	fx := buildFilterFixture(t, s)

	unfiltered, err := slice.ResolveCut(s, fx.cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut (unfiltered): %v", err)
	}
	// Fixture sanity: the range really contains the landmarks the
	// law treats specially, plus ordinary records of both modes —
	// otherwise the purity assertions below would be vacuous.
	present := make(map[string]bool, len(unfiltered.Records))
	var captures, declarations int
	for _, rec := range unfiltered.Records {
		present[rec.ID] = true
		mode, schemaRef := modeAndSchema(rec)
		if schemaRef == store.MilestoneSchemaRef || schemaRef == store.CutSchemaRef {
			continue
		}
		switch mode {
		case record.ModeCapture:
			captures++
		case record.ModeDeclaration:
			declarations++
		}
	}
	if !present[fx.milestone] {
		t.Fatalf("unfiltered slice does not contain the milestone %s", fx.milestone)
	}
	if !present[fx.innerCut] {
		t.Fatalf("unfiltered slice does not contain the inner cut %s", fx.innerCut)
	}
	if present[fx.cutID] {
		t.Fatalf("unfiltered slice contains the cut record itself %s", fx.cutID)
	}
	if captures == 0 || declarations == 0 {
		t.Fatalf("fixture is degenerate: %d ordinary captures, %d ordinary declarations in range", captures, declarations)
	}

	filters := [][]string{
		{record.ModeCapture},
		{record.ModeDeclaration},
		{record.ModeCapture, record.ModeDeclaration},
		{},
	}
	for _, filter := range filters {
		want := expectedFiltered(unfiltered, filter)
		got, err := slice.ResolveCut(s, fx.cutID, filter)
		if err != nil {
			t.Fatalf("ResolveCut(filter=%v): %v", filter, err)
		}
		if !reflect.DeepEqual(got.Order(), want.Order()) {
			t.Errorf("filter=%v: Order() = %v, want exactly the filtered unfiltered order %v", filter, got.Order(), want.Order())
		}
		if !reflect.DeepEqual(got.Edges, want.Edges) {
			t.Errorf("filter=%v: Edges = %v, want the unfiltered edges restricted to retained records %v", filter, got.Edges, want.Edges)
		}
		wantDigest, err := want.Digest()
		if err != nil {
			t.Fatalf("filter=%v: expected Digest: %v", filter, err)
		}
		gotDigest, err := got.Digest()
		if err != nil {
			t.Fatalf("filter=%v: Digest: %v", filter, err)
		}
		if gotDigest != wantDigest {
			t.Errorf("filter=%v: Digest = %s, want %s", filter, gotDigest, wantDigest)
		}
		if got.CutID != fx.cutID || got.Frontier != unfiltered.Frontier {
			t.Errorf("filter=%v: CutID/Frontier = %q/%q, want %q/%q — filtering must not move the cut", filter, got.CutID, got.Frontier, fx.cutID, unfiltered.Frontier)
		}
	}
}

func TestModeFilterExemptsMilestonesAndCuts(t *testing.T) { // Decision 4(b)
	s := openTemp(t)
	fx := buildFilterFixture(t, s)

	// Milestones and cuts are declaration-mode records; under a
	// capture-only filter the exemption is the only thing keeping
	// them, and epoch boundaries with them.
	got, err := slice.ResolveCut(s, fx.cutID, []string{record.ModeCapture})
	if err != nil {
		t.Fatalf("ResolveCut (capture only): %v", err)
	}
	present := make(map[string]bool, len(got.Records))
	for _, rec := range got.Records {
		present[rec.ID] = true
	}
	if !present[fx.milestone] {
		t.Errorf("capture-only slice dropped the milestone %s — Decision 4(b) exemption violated", fx.milestone)
	}
	if !present[fx.innerCut] {
		t.Errorf("capture-only slice dropped the inner cut %s — Decision 4(b) exemption violated", fx.innerCut)
	}
	if fx.declaration != "" && present[fx.declaration] {
		t.Errorf("capture-only slice kept ordinary declaration record %s — the filter is not filtering", fx.declaration)
	}
}

func TestModeFilterRejectsUnknownMode(t *testing.T) {
	s := openTemp(t)
	r1 := appendFact(t, s, "runs/a", `{"n":"r1"}`, []string{record.RootWitnessRecordID()})
	cutID, err := slice.PublishCut(s, "runs/a", r1)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	if _, err := slice.ResolveCut(s, cutID, []string{"bogus"}); !errors.Is(err, slice.ErrUnknownMode) {
		t.Fatalf("err = %v, want ErrUnknownMode", err)
	}
	// An unknown mode is rejected even alongside a known one.
	if _, err := slice.ResolveCut(s, cutID, []string{record.ModeCapture, "bogus"}); !errors.Is(err, slice.ErrUnknownMode) {
		t.Fatalf("mixed filter: err = %v, want ErrUnknownMode", err)
	}
}
