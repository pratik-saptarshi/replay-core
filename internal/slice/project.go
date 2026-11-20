package slice

import (
	"fmt"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// Abandon appends an abandon fact to ownerPath and returns its
// receipt: a declaration-mode record under store.AbandonSchemaRef
// with body {"abandoned": abandonedRootID}, citing causedBy, with
// the root witness body as witness (plan Decision 4(a)). The
// record is minted by store.MintAbandon, the constructor the
// reserved AbandonSchemaRef belongs to.
//
// Abandoning is a fact, not an edit (spec §5, §7): the append
// deletes and rewrites nothing, and the abandoned records stay
// readable via GetRecord. The abandonment takes effect only as a
// projection — ProjectEffectiveHistory omits the abandoned root
// and its descendants from the path's effective history.
func Abandon(s *store.Store, ownerPath, abandonedRootID string, causedBy []string) (store.Receipt, error) {
	receipt, err := s.MintAbandon(ownerPath, abandonedRootID, causedBy)
	if err != nil {
		return store.Receipt{}, fmt.Errorf("slice: abandon %s on %q: %w", abandonedRootID, ownerPath, err)
	}
	return receipt, nil
}

// ProjectEffectiveHistory resolves ownerPath's effective history:
// the path's full causal closure to its frontier, minus every
// abandoned root named by an abandon fact in the closure and all
// of each root's descendants within the closure (descendants
// computed over the closure's reversed edges). The result is a
// projection Slice — CutID "" and Frontier the path tail.
//
// The projection is a pure read (spec §5): bytes are never
// deleted, so abandoned records remain readable via GetRecord and
// on the path's raw record list; only this view omits them.
func ProjectEffectiveHistory(s *store.Store, ownerPath string) (Slice, error) {
	frontier, err := s.Frontier(ownerPath)
	if err != nil {
		return Slice{}, err
	}
	pathRecs, err := s.PathRecords(ownerPath)
	if err != nil {
		return Slice{}, err
	}
	// The full closure to the frontier is the closure walk through
	// the tail's ordinal; a path with no records of its own admits
	// nothing (throughOrdinal -1).
	throughOrdinal := int64(-1)
	if len(pathRecs) > 0 {
		throughOrdinal = pathRecs[len(pathRecs)-1].OwnerOrdinal
	}
	selected, causes, err := closureWalk(s, ownerPath, throughOrdinal)
	if err != nil {
		return Slice{}, err
	}

	// Every abandon fact in the closure names one abandoned root.
	var roots []string
	for _, rec := range selected {
		schemaRef, ok := fieldOf(rec.Record, "schema_ref")
		if !ok || schemaRef.Kind != canonical.KindString || schemaRef.Str != store.AbandonSchemaRef {
			continue
		}
		body, ok := fieldOf(rec.Record, "body")
		if !ok || body.Kind != canonical.KindObject {
			return Slice{}, fmt.Errorf("slice: abandon record %s has no body object", rec.ID)
		}
		abandoned, ok := fieldOf(body, "abandoned")
		if !ok || abandoned.Kind != canonical.KindString {
			return Slice{}, fmt.Errorf("slice: abandon record %s body has no abandoned string", rec.ID)
		}
		roots = append(roots, abandoned.Str)
	}

	if len(roots) > 0 {
		// Reversed edges over the closure: each cause mapped to
		// the selected records that cite it.
		children := make(map[string][]string)
		for id, cs := range causes {
			for _, cause := range cs {
				if _, ok := selected[cause]; ok {
					children[cause] = append(children[cause], id)
				}
			}
		}
		removed := make(map[string]bool)
		stack := roots
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if removed[id] {
				continue
			}
			if _, ok := selected[id]; !ok {
				continue // a root outside this closure removes nothing
			}
			removed[id] = true
			stack = append(stack, children[id]...)
		}
		for id := range removed {
			delete(selected, id)
			delete(causes, id)
		}
	}

	ordered, edges, err := orderRecords(selected, causes)
	if err != nil {
		return Slice{}, err
	}
	return Slice{
		CutID:    "",
		Records:  ordered,
		Edges:    edges,
		Frontier: frontier,
	}, nil
}
