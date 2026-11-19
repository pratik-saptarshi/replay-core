package slice

import (
	"container/heap"
	"errors"
	"fmt"
	"strconv"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// Cut is the decoded body of a published cut record: the owner path
// the cut addresses, the fact it runs through, and that fact's
// ordinal on the path.
type Cut struct {
	OwnerPath, ThroughFactID string
	ThroughOrdinal           int64
}

// rootFields returns the root witness fields object: the body field
// of record.RootWitnessBody(). Phase 0 exports the fields only in
// that wrapped form (plan Decision 4(a)).
func rootFields() (canonical.Value, bool) {
	for _, p := range record.RootWitnessBody().Pairs {
		if p.Key == "body" {
			return p.Value, true
		}
	}
	return canonical.Value{}, false
}

// fieldOf returns the value of key in an object value.
func fieldOf(obj canonical.Value, key string) (canonical.Value, bool) {
	for _, p := range obj.Pairs {
		if p.Key == key {
			return p.Value, true
		}
	}
	return canonical.Value{}, false
}

// causedByOf extracts the caused_by ID list from a record object.
func causedByOf(rec canonical.Value) ([]string, error) {
	v, ok := fieldOf(rec, "caused_by")
	if !ok || v.Kind != canonical.KindArray {
		return nil, errors.New("record object has no caused_by array")
	}
	out := make([]string, 0, len(v.Elems))
	for _, e := range v.Elems {
		if e.Kind != canonical.KindString {
			return nil, errors.New("caused_by entry is not a string")
		}
		out = append(out, e.Str)
	}
	return out, nil
}

// PublishCut publishes a cut on ownerPath running through
// throughFactID and returns the cut record's ID. The cut record is
// appended on the target owner path itself — declaration mode under
// store.CutSchemaRef, body {owner_path, through_fact_id,
// through_owner_ordinal} with the ordinal read from the
// through-fact's stored record, caused_by = [throughFactID], root
// witness body as witness (plan Decision 4(a)). A published cut is
// a retention root (plan Decision 4(c)).
//
// The through-fact must live on ownerPath: the body's
// through_owner_ordinal is that fact's ordinal, and ordinals are
// per-owner (AC-F2-4), so a through-fact from another path would
// transplant a foreign ordinal into this path's cut.
func PublishCut(s *store.Store, ownerPath, throughFactID string) (string, error) {
	through, err := s.GetRecord(throughFactID)
	if err != nil {
		return "", fmt.Errorf("slice: publish cut on %q: %w", ownerPath, err)
	}
	if through.OwnerPath != ownerPath {
		return "", fmt.Errorf("slice: publish cut on %q: through-fact %s is on owner path %q, not %q", ownerPath, throughFactID, through.OwnerPath, ownerPath)
	}
	fields, ok := rootFields()
	if !ok {
		return "", errors.New("slice: root witness body has no body field")
	}
	ordinal, err := canonical.IntOf(strconv.FormatInt(through.OwnerOrdinal, 10))
	if err != nil {
		return "", fmt.Errorf("slice: publish cut on %q: encode ordinal: %w", ownerPath, err)
	}
	receipt, err := s.AppendFact(ownerPath, store.RecordDraft{
		SchemaRef: store.CutSchemaRef,
		Mode:      record.ModeDeclaration,
		Body: canonical.ObjectOf(
			canonical.Pair{Key: "owner_path", Value: canonical.StringOf(ownerPath)},
			canonical.Pair{Key: "through_fact_id", Value: canonical.StringOf(throughFactID)},
			canonical.Pair{Key: "through_owner_ordinal", Value: ordinal},
		),
		CausedBy:         []string{throughFactID},
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      fields,
	})
	if err != nil {
		return "", fmt.Errorf("slice: publish cut on %q: %w", ownerPath, err)
	}
	return receipt.RecordID, nil
}

// ReadCut reads and decodes the cut record with the given ID. The
// record must carry store.CutSchemaRef; anything else is an error,
// never a silently misread cut.
func ReadCut(s *store.Store, cutID string) (Cut, error) {
	rec, err := s.GetRecord(cutID)
	if err != nil {
		return Cut{}, fmt.Errorf("slice: read cut %s: %w", cutID, err)
	}
	schemaRef, ok := fieldOf(rec.Record, "schema_ref")
	if !ok || schemaRef.Kind != canonical.KindString || schemaRef.Str != store.CutSchemaRef {
		return Cut{}, fmt.Errorf("slice: record %s is not a cut: schema_ref is not %s", cutID, store.CutSchemaRef)
	}
	body, ok := fieldOf(rec.Record, "body")
	if !ok || body.Kind != canonical.KindObject {
		return Cut{}, fmt.Errorf("slice: cut %s has no body object", cutID)
	}
	ownerPath, ok := fieldOf(body, "owner_path")
	if !ok || ownerPath.Kind != canonical.KindString {
		return Cut{}, fmt.Errorf("slice: cut %s body has no owner_path string", cutID)
	}
	throughFactID, ok := fieldOf(body, "through_fact_id")
	if !ok || throughFactID.Kind != canonical.KindString {
		return Cut{}, fmt.Errorf("slice: cut %s body has no through_fact_id string", cutID)
	}
	ordinalValue, ok := fieldOf(body, "through_owner_ordinal")
	if !ok || ordinalValue.Kind != canonical.KindInt {
		return Cut{}, fmt.Errorf("slice: cut %s body has no through_owner_ordinal integer", cutID)
	}
	ordinal, err := strconv.ParseInt(ordinalValue.Int, 10, 64)
	if err != nil {
		return Cut{}, fmt.Errorf("slice: cut %s through_owner_ordinal: %w", cutID, err)
	}
	return Cut{OwnerPath: ownerPath.Str, ThroughFactID: throughFactID.Str, ThroughOrdinal: ordinal}, nil
}

// ErrUnknownMode is returned by ResolveCut when a mode filter
// names a mode other than capture or declaration.
var ErrUnknownMode = errors.New("slice: unknown mode in filter")

// ResolveCut resolves the cut with the given ID to its Slice: the
// target path's records with ordinal ≤ the cut's through-ordinal,
// closed transitively over caused_by — cross-path ancestors are
// pulled in via GetRecord — in causal order with ties broken by
// (OwnerPath, OwnerOrdinal, ID), so the Slice and its Digest are
// stable across resolutions.
//
// A mode filter is applied to the closed Slice, never to the
// closure input (the mode-filter law): the result is the full
// causally closed Slice minus the records whose mode is not in the
// filter, except milestone and cut records, which are exempt from
// mode filtering (Decision 4(b)) and always retained; edges are
// restricted to the retained records. An empty filter admits all.
// A filter naming a mode other than capture or declaration fails
// with ErrUnknownMode.
func ResolveCut(s *store.Store, cutID string, modeFilter []string) (Slice, error) {
	admit, err := modeSet(modeFilter)
	if err != nil {
		return Slice{}, err
	}
	cut, err := ReadCut(s, cutID)
	if err != nil {
		return Slice{}, err
	}
	selected, causes, err := closure(s, cut)
	if err != nil {
		return Slice{}, err
	}
	ordered, edges, err := orderRecords(selected, causes)
	if err != nil {
		return Slice{}, err
	}
	if admit != nil {
		ordered, edges, err = applyModeFilter(ordered, edges, admit)
		if err != nil {
			return Slice{}, err
		}
	}
	return Slice{
		CutID:    cutID,
		Records:  ordered,
		Edges:    edges,
		Frontier: cut.ThroughFactID,
	}, nil
}

// modeSet validates a ResolveCut mode filter and returns the set
// of admitted modes. A nil result means the filter is empty and
// admits all; any entry that is not a Phase 0 mode fails with
// ErrUnknownMode.
func modeSet(modeFilter []string) (map[string]bool, error) {
	if len(modeFilter) == 0 {
		return nil, nil
	}
	admit := make(map[string]bool, len(modeFilter))
	for _, mode := range modeFilter {
		if mode != record.ModeCapture && mode != record.ModeDeclaration {
			return nil, fmt.Errorf("%w: %q", ErrUnknownMode, mode)
		}
		admit[mode] = true
	}
	return admit, nil
}

// applyModeFilter drops the records whose mode is not in admit —
// except records under store.MilestoneSchemaRef or
// store.CutSchemaRef, which are exempt from mode filtering
// (Decision 4(b)): a filter that could drop a boundary marker
// would silently move epoch boundaries. Edges are restricted to
// the retained records. Filtering the ordered closure preserves
// the relative causal order of what remains, so the filtered
// Slice is exactly the filter applied to the unfiltered Slice.
func applyModeFilter(ordered []store.StoredRecord, edges [][2]string, admit map[string]bool) ([]store.StoredRecord, [][2]string, error) {
	retained := make(map[string]bool, len(ordered))
	kept := make([]store.StoredRecord, 0, len(ordered))
	for _, rec := range ordered {
		mode, ok := fieldOf(rec.Record, "mode")
		if !ok || mode.Kind != canonical.KindString {
			return nil, nil, fmt.Errorf("slice: record %s has no mode string", rec.ID)
		}
		schemaRef, ok := fieldOf(rec.Record, "schema_ref")
		if !ok || schemaRef.Kind != canonical.KindString {
			return nil, nil, fmt.Errorf("slice: record %s has no schema_ref string", rec.ID)
		}
		if admit[mode.Str] || schemaRef.Str == store.MilestoneSchemaRef || schemaRef.Str == store.CutSchemaRef {
			retained[rec.ID] = true
			kept = append(kept, rec)
		}
	}
	var keptEdges [][2]string
	for _, e := range edges {
		if retained[e[0]] && retained[e[1]] {
			keptEdges = append(keptEdges, e)
		}
	}
	return kept, keptEdges, nil
}

// closure computes a cut's record set iteratively (explicit stack —
// Review Focus 5): seed with the target path's records at ordinal ≤
// the through-ordinal, then walk caused_by transitively, fetching
// cross-path ancestors via GetRecord. It returns the selected
// records and each selected record's caused_by list; in a closed
// set every cause is itself selected.
func closure(s *store.Store, cut Cut) (map[string]store.StoredRecord, map[string][]string, error) {
	pathRecs, err := s.PathRecords(cut.OwnerPath)
	if err != nil {
		return nil, nil, err
	}
	selected := make(map[string]store.StoredRecord)
	causes := make(map[string][]string)
	var stack []string
	for _, rec := range pathRecs {
		if rec.OwnerOrdinal <= cut.ThroughOrdinal {
			if _, ok := selected[rec.ID]; !ok {
				selected[rec.ID] = rec
				stack = append(stack, rec.ID)
			}
		}
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		cs, err := causedByOf(selected[id].Record)
		if err != nil {
			return nil, nil, fmt.Errorf("slice: record %s: %w", id, err)
		}
		causes[id] = cs
		for _, cause := range cs {
			if _, ok := selected[cause]; ok {
				continue
			}
			rec, err := s.GetRecord(cause)
			if err != nil {
				return nil, nil, err
			}
			selected[cause] = rec
			stack = append(stack, cause)
		}
	}
	return selected, causes, nil
}

// recordHeap is a min-heap of records under the slice tie-break
// (OwnerPath, OwnerOrdinal, ID). The key is total: (OwnerPath,
// OwnerOrdinal) is unique per record, so pops are deterministic
// regardless of push order.
type recordHeap []store.StoredRecord

func (h recordHeap) Len() int { return len(h) }

func (h recordHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if a.OwnerPath != b.OwnerPath {
		return a.OwnerPath < b.OwnerPath
	}
	if a.OwnerOrdinal != b.OwnerOrdinal {
		return a.OwnerOrdinal < b.OwnerOrdinal
	}
	return a.ID < b.ID
}

func (h recordHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *recordHeap) Push(x any) { *h = append(*h, x.(store.StoredRecord)) }

func (h *recordHeap) Pop() any {
	old := *h
	n := len(old)
	v := old[n-1]
	*h = old[:n-1]
	return v
}

// orderRecords topologically sorts the selected records — every
// record after the records it cites — breaking ties by (OwnerPath,
// OwnerOrdinal, ID) via Kahn's algorithm over a heap. It also
// returns the {record, cause} edges among the selected records, in
// record order, causes in each record's caused_by order.
func orderRecords(selected map[string]store.StoredRecord, causes map[string][]string) ([]store.StoredRecord, [][2]string, error) {
	indegree := make(map[string]int, len(selected))
	children := make(map[string][]string)
	for id := range selected {
		indegree[id] = 0
	}
	for id, cs := range causes {
		seen := make(map[string]bool, len(cs))
		for _, cause := range cs {
			if _, ok := selected[cause]; !ok || seen[cause] {
				continue
			}
			seen[cause] = true
			indegree[id]++
			children[cause] = append(children[cause], id)
		}
	}
	ready := &recordHeap{}
	heap.Init(ready)
	for id, deg := range indegree {
		if deg == 0 {
			heap.Push(ready, selected[id])
		}
	}
	ordered := make([]store.StoredRecord, 0, len(selected))
	for ready.Len() > 0 {
		rec := heap.Pop(ready).(store.StoredRecord)
		ordered = append(ordered, rec)
		for _, child := range children[rec.ID] {
			indegree[child]--
			if indegree[child] == 0 {
				heap.Push(ready, selected[child])
			}
		}
	}
	if len(ordered) != len(selected) {
		return nil, nil, fmt.Errorf("slice: causal cycle among %d selected records", len(selected)-len(ordered))
	}
	var edges [][2]string
	for _, rec := range ordered {
		seen := make(map[string]bool, len(causes[rec.ID]))
		for _, cause := range causes[rec.ID] {
			if _, ok := selected[cause]; !ok || seen[cause] {
				continue
			}
			seen[cause] = true
			edges = append(edges, [2]string{rec.ID, cause})
		}
	}
	return ordered, edges, nil
}
