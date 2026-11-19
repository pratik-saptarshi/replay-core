package store

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
)

// This file holds the constructors for the store-minted system
// records whose schema refs the public append boundary reserves
// (ErrReservedSchemaRef): cuts and abandons. (Milestones, the
// third reserved ref, are minted by AppendMilestone in
// milestone.go.) A mint method takes validated inputs — record
// IDs, paths — and builds the canonical body itself from the
// stored records, so the body's claims (a cut's
// through_owner_ordinal, an abandon's root) are always the
// store's own readings, never caller assertions.

// CutBody builds the canonical body of a cut record:
// {"owner_path": ..., "through_fact_id": ...,
// "through_owner_ordinal": ...}. MintCut derives every field from
// the through-fact's stored record; the shape is shared so the
// slice layer's ReadCut decodes exactly what was minted.
func CutBody(ownerPath, throughFactID string, throughOrdinal int64) (canonical.Value, error) {
	ordinal, err := canonical.IntOf(strconv.FormatInt(throughOrdinal, 10))
	if err != nil {
		return canonical.Value{}, fmt.Errorf("store: encode cut ordinal: %w", err)
	}
	return canonical.ObjectOf(
		canonical.Pair{Key: "owner_path", Value: canonical.StringOf(ownerPath)},
		canonical.Pair{Key: "through_fact_id", Value: canonical.StringOf(throughFactID)},
		canonical.Pair{Key: "through_owner_ordinal", Value: ordinal},
	), nil
}

// MintCut appends a cut record on ownerPath running through
// throughFactID and returns its receipt: a declaration-mode
// record under CutSchemaRef whose body names the path, the fact,
// and the fact's ordinal as read from the stored record, citing
// the through-fact, with the root witness body as witness (plan
// Decision 4(a)). The through-fact must live on ownerPath —
// ordinals are per-owner (AC-F2-4), so a through-fact from
// another path would transplant a foreign ordinal into this
// path's cut.
func (s *Store) MintCut(ownerPath, throughFactID string) (Receipt, error) {
	through, err := s.GetRecord(throughFactID)
	if err != nil {
		return Receipt{}, err
	}
	if through.OwnerPath != ownerPath {
		return Receipt{}, fmt.Errorf("store: mint cut on %q: through-fact %s is on owner path %q, not %q", ownerPath, throughFactID, through.OwnerPath, ownerPath)
	}
	fields, ok := rootFields()
	if !ok {
		return Receipt{}, errors.New("store: root witness body has no body field")
	}
	body, err := CutBody(ownerPath, throughFactID, through.OwnerOrdinal)
	if err != nil {
		return Receipt{}, err
	}
	return s.appendFact(ownerPath, RecordDraft{
		SchemaRef:        CutSchemaRef,
		Mode:             record.ModeDeclaration,
		Body:             body,
		CausedBy:         []string{throughFactID},
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      fields,
	})
}

// AbandonBody builds the canonical body of an abandon record:
// {"abandoned": abandonedRootID}.
func AbandonBody(abandonedRootID string) canonical.Value {
	return canonical.ObjectOf(
		canonical.Pair{Key: "abandoned", Value: canonical.StringOf(abandonedRootID)},
	)
}

// MintAbandon appends an abandon record on ownerPath naming
// abandonedRootID and returns its receipt: a declaration-mode
// record under AbandonSchemaRef citing causedBy, with the root
// witness body as witness (plan Decision 4(a)). Abandoning is a
// fact, not an edit: the append deletes and rewrites nothing, and
// the abandonment takes effect only as a projection in the slice
// layer.
//
// The target is validated the way MintCut validates its
// through-fact — fetched, and required to live on ownerPath —
// before anything is minted: an abandon naming a nonexistent or
// foreign-path target used to mint a success receipt for a
// projection that silently changed nothing, leaving the records
// the caller meant to abandon in the effective history (panel
// W3-4).
func (s *Store) MintAbandon(ownerPath, abandonedRootID string, causedBy []string) (Receipt, error) {
	target, err := s.GetRecord(abandonedRootID)
	if err != nil {
		return Receipt{}, err
	}
	if target.OwnerPath != ownerPath {
		return Receipt{}, fmt.Errorf("store: mint abandon on %q: target %s is on owner path %q, not %q", ownerPath, abandonedRootID, target.OwnerPath, ownerPath)
	}
	fields, ok := rootFields()
	if !ok {
		return Receipt{}, errors.New("store: root witness body has no body field")
	}
	return s.appendFact(ownerPath, RecordDraft{
		SchemaRef:        AbandonSchemaRef,
		Mode:             record.ModeDeclaration,
		Body:             AbandonBody(abandonedRootID),
		CausedBy:         causedBy,
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      fields,
	})
}
