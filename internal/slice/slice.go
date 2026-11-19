// Package slice is replay-core's immutable read addressing over
// the trace store (F3): a published Cut names a point on an owner
// path, and resolving it yields the causally closed, deterministically
// ordered Slice of records the cut covers. Resolution is a pure
// read — it never mutates the store, and a cut's Slice cannot
// change when the store grows past the cut (AC-F3-2).
package slice

import (
	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// Slice is one resolved read over the trace store: the records a
// cut (or, from Task 9, a projection) selects, in causal order,
// with the causal edges among them.
type Slice struct {
	CutID    string               // "" for projections (Task 9) and epochs (Task 11)
	Records  []store.StoredRecord // causal (topological) order, deterministic tie-break
	Edges    [][2]string          // {record, cause} pairs among the selected records
	Frontier string               // the cut's through-fact, a projection's path tail, or an epoch's to milestone
}

// Order returns the slice's record IDs in their causal order.
func (sl Slice) Order() []string {
	ids := make([]string, len(sl.Records))
	for i, rec := range sl.Records {
		ids[i] = rec.ID
	}
	return ids
}

// Digest returns the slice's content digest: DigestKernel over
// {"cut": CutID, "kind": "slice", "records": [IDs in Order()]}.
// Because the record order is deterministic, the digest is stable
// across resolutions of the same cut.
func (sl Slice) Digest() (string, error) {
	ids := sl.Order()
	elems := make([]canonical.Value, len(ids))
	for i, id := range ids {
		elems[i] = canonical.StringOf(id)
	}
	return canonical.DigestKernel(canonical.ObjectOf(
		canonical.Pair{Key: "cut", Value: canonical.StringOf(sl.CutID)},
		canonical.Pair{Key: "kind", Value: canonical.StringOf("slice")},
		canonical.Pair{Key: "records", Value: canonical.ArrayOf(elems...)},
	))
}

// EffectAt returns the digest of the record at slice position i
// when that record is capture-mode, and ("", false) when position
// i holds a non-capture record or is out of range. A record's ID
// is its digest (the identity law: record_id == digest).
//
// EffectAt is the whole E3-A cursor contract (ruling, DECIDED):
// positional access over the slice — no cache, no store, no new
// type. It is position-indexed over slice order (AC-F3-6): position
// i names the i-th record of the slice, and the lookup answers
// that record's digest only when that record is capture-mode. It
// does not enumerate or skip to the i-th capture, so a declaration
// at position i answers ("", false) and shifts nothing.
func (sl Slice) EffectAt(i int) (string, bool) {
	if i < 0 || i >= len(sl.Records) {
		return "", false
	}
	rec := sl.Records[i]
	mode, ok := fieldOf(rec.Record, "mode")
	if !ok || mode.Kind != canonical.KindString || mode.Str != record.ModeCapture {
		return "", false
	}
	return rec.ID, true
}
