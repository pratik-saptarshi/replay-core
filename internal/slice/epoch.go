package slice

import (
	"fmt"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// EpochSlice resolves the epoch between two milestones on one
// owner path (AC-F3-5): the causal closure of the path prefix up
// to and including the to milestone, minus the same closure up to
// and including the from milestone — from excluded, to included.
// The order is the prefix closure's order restricted to the
// difference, and the edges are restricted likewise.
//
// An epoch is not a published cut: the result's CutID is empty and
// its Frontier is the to milestone ID. Epochs are per owner path
// only (AC-F3-7): both milestones must sit on ownerPath.
//
// Errors: either ID is not a milestone record, the milestones sit
// on different owner paths (either one off ownerPath), from == to,
// or from comes after to on the path.
func EpochSlice(s *store.Store, ownerPath, fromMilestoneID, toMilestoneID string) (Slice, error) {
	if fromMilestoneID == toMilestoneID {
		return Slice{}, fmt.Errorf("slice: epoch on %q: from and to are the same milestone %s", ownerPath, fromMilestoneID)
	}
	from, err := milestoneRecord(s, fromMilestoneID)
	if err != nil {
		return Slice{}, fmt.Errorf("slice: epoch on %q: from milestone: %w", ownerPath, err)
	}
	to, err := milestoneRecord(s, toMilestoneID)
	if err != nil {
		return Slice{}, fmt.Errorf("slice: epoch on %q: to milestone: %w", ownerPath, err)
	}
	if from.OwnerPath != to.OwnerPath {
		return Slice{}, fmt.Errorf("slice: epoch on %q: milestones sit on different owner paths %q and %q", ownerPath, from.OwnerPath, to.OwnerPath)
	}
	if from.OwnerPath != ownerPath || to.OwnerPath != ownerPath {
		return Slice{}, fmt.Errorf("slice: epoch on %q: milestones sit on owner path %q, not %q", ownerPath, from.OwnerPath, ownerPath)
	}
	if from.OwnerOrdinal > to.OwnerOrdinal {
		return Slice{}, fmt.Errorf("slice: epoch on %q: from milestone %s (ordinal %d) comes after to milestone %s (ordinal %d)", ownerPath, fromMilestoneID, from.OwnerOrdinal, toMilestoneID, to.OwnerOrdinal)
	}

	// The to milestone's prefix closure, fully ordered; the epoch
	// is that order restricted to the set difference against the
	// from milestone's prefix closure (prefix closure by ordinal
	// bound, then set difference by record ID). The from closure
	// is a subset of the to closure — the prefix grows
	// monotonically with the ordinal bound on one path — so the
	// difference is exactly the (from, to] segment.
	toSelected, toCauses, err := closureWalk(s, ownerPath, to.OwnerOrdinal)
	if err != nil {
		return Slice{}, err
	}
	fromSelected, _, err := closureWalk(s, ownerPath, from.OwnerOrdinal)
	if err != nil {
		return Slice{}, err
	}
	ordered, edges, err := orderRecords(toSelected, toCauses)
	if err != nil {
		return Slice{}, err
	}
	kept := make([]store.StoredRecord, 0, len(ordered))
	retained := make(map[string]bool, len(ordered))
	for _, rec := range ordered {
		if _, excluded := fromSelected[rec.ID]; excluded {
			continue
		}
		retained[rec.ID] = true
		kept = append(kept, rec)
	}
	var keptEdges [][2]string
	for _, e := range edges {
		if retained[e[0]] && retained[e[1]] {
			keptEdges = append(keptEdges, e)
		}
	}
	return Slice{
		CutID:    "",
		Records:  kept,
		Edges:    keptEdges,
		Frontier: toMilestoneID,
	}, nil
}

// milestoneRecord reads the record with the given ID and requires
// it to be a milestone record (store.MilestoneSchemaRef); anything
// else is an error, never a silently misread boundary.
func milestoneRecord(s *store.Store, id string) (store.StoredRecord, error) {
	rec, err := s.GetRecord(id)
	if err != nil {
		return store.StoredRecord{}, err
	}
	schemaRef, ok := fieldOf(rec.Record, "schema_ref")
	if !ok || schemaRef.Kind != canonical.KindString || schemaRef.Str != store.MilestoneSchemaRef {
		return store.StoredRecord{}, fmt.Errorf("slice: record %s is not a milestone: schema_ref is not %s", id, store.MilestoneSchemaRef)
	}
	return rec, nil
}
