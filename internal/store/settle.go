package store

import (
	"errors"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
)

// This file holds the constructors for the settlement records
// (Phase 2, plan Decision 4): the facts the settlement verbs
// append when a run scope is selected, applied, or discarded.
// Their schema refs are reserved (ErrReservedSchemaRef) exactly
// like the Phase 1 system refs in mint.go — consume-once
// enforcement and journal recovery read settlement facts as
// authority, so the class is never appendable through the
// unguarded path (the wave-2 ADV-3 lesson, applied
// prospectively). The body constructors below are the one
// construction site for the bodies the settlement layer asserts
// over; every mint cites the owner path's frontier at mint time
// as its sole cause, with the root witness fields as witness
// (Phase 1 plan Decision 4(a): the house convention for
// system-authored facts).

// SettleSelectBody builds the canonical body of a select fact:
// {"op": ..., "parent_scope": ..., "parent_head": ...,
// "run_head": ..., "run_scope": ...} — the caller-supplied
// operation ID, the two scopes, and the parent head before and
// the run head the parent scope advanced to.
func SettleSelectBody(op, parentScope, runScope, parentHead, runHead string) canonical.Value {
	return canonical.ObjectOf(
		canonical.Pair{Key: "op", Value: canonical.StringOf(op)},
		canonical.Pair{Key: "parent_scope", Value: canonical.StringOf(parentScope)},
		canonical.Pair{Key: "parent_head", Value: canonical.StringOf(parentHead)},
		canonical.Pair{Key: "run_head", Value: canonical.StringOf(runHead)},
		canonical.Pair{Key: "run_scope", Value: canonical.StringOf(runScope)},
	)
}

// SettleApplyBody builds the canonical body of an apply fact:
// {"fork_commit": ..., "op": ..., "parent_scope": ...,
// "result_commit": ..., "run_scope": ..., "settled_paths": [...]}
// — the fork point the apply was computed against, the commit it
// produced, and the run-side paths it settled.
func SettleApplyBody(op, parentScope, runScope, forkCommit, resultCommit string, settledPaths []string) canonical.Value {
	paths := make([]canonical.Value, 0, len(settledPaths))
	for _, p := range settledPaths {
		paths = append(paths, canonical.StringOf(p))
	}
	return canonical.ObjectOf(
		canonical.Pair{Key: "fork_commit", Value: canonical.StringOf(forkCommit)},
		canonical.Pair{Key: "op", Value: canonical.StringOf(op)},
		canonical.Pair{Key: "parent_scope", Value: canonical.StringOf(parentScope)},
		canonical.Pair{Key: "result_commit", Value: canonical.StringOf(resultCommit)},
		canonical.Pair{Key: "run_scope", Value: canonical.StringOf(runScope)},
		canonical.Pair{Key: "settled_paths", Value: canonical.ArrayOf(paths...)},
	)
}

// SettleDiscardBody builds the canonical body of a discard fact:
// {"archive_ref": ..., "head": ..., "op": ..., "run_scope": ...}
// — the run scope's head at discard time and the archive ref it
// was retired to.
func SettleDiscardBody(op, runScope, head, archiveRef string) canonical.Value {
	return canonical.ObjectOf(
		canonical.Pair{Key: "archive_ref", Value: canonical.StringOf(archiveRef)},
		canonical.Pair{Key: "head", Value: canonical.StringOf(head)},
		canonical.Pair{Key: "op", Value: canonical.StringOf(op)},
		canonical.Pair{Key: "run_scope", Value: canonical.StringOf(runScope)},
	)
}

// mintSettle appends one settlement fact on ownerPath under
// schemaRef with the given body: a declaration-mode record whose
// sole cause is the path's frontier at mint time (so the fact
// hangs off the scope's tail, AC-F6-7) and whose witness is the
// root witness body.
func (s *Store) mintSettle(ownerPath, schemaRef string, body canonical.Value) (Receipt, error) {
	fields, ok := rootFields()
	if !ok {
		return Receipt{}, errors.New("store: root witness body has no body field")
	}
	frontier, err := s.Frontier(ownerPath)
	if err != nil {
		return Receipt{}, err
	}
	return s.appendFact(ownerPath, RecordDraft{
		SchemaRef:        schemaRef,
		Mode:             record.ModeDeclaration,
		Body:             body,
		CausedBy:         []string{frontier},
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      fields,
	})
}

// MintSettleSelect appends a select fact on ownerPath (the run
// scope's trace path, plan Decision 3) and returns its receipt.
func (s *Store) MintSettleSelect(ownerPath, op, parentScope, runScope, parentHead, runHead string) (Receipt, error) {
	return s.mintSettle(ownerPath, SettleSelectSchemaRef,
		SettleSelectBody(op, parentScope, runScope, parentHead, runHead))
}

// MintSettleApply appends an apply fact on ownerPath (the run
// scope's trace path, plan Decision 3) and returns its receipt.
func (s *Store) MintSettleApply(ownerPath, op, parentScope, runScope, forkCommit, resultCommit string, settledPaths []string) (Receipt, error) {
	return s.mintSettle(ownerPath, SettleApplySchemaRef,
		SettleApplyBody(op, parentScope, runScope, forkCommit, resultCommit, settledPaths))
}

// MintSettleDiscard appends a discard fact on ownerPath (the run
// scope's trace path, plan Decision 3) and returns its receipt.
func (s *Store) MintSettleDiscard(ownerPath, op, runScope, head, archiveRef string) (Receipt, error) {
	return s.mintSettle(ownerPath, SettleDiscardSchemaRef,
		SettleDiscardBody(op, runScope, head, archiveRef))
}
