package store

import (
	"errors"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
)

// MilestoneBody builds the canonical body of a milestone record:
// {"snapshot_ref": <snapshotRef>, "task_id": <taskID>}. The citation
// lives in the body because an append-only store cannot edit a
// record after the fact (plan Decision 4(d)); the constructor lives
// here, in the lowest layer, because the store must never import the
// snapshot package — consumers in that package build their citation
// through this function and AppendMilestone.
func MilestoneBody(taskID, snapshotRef string) canonical.Value {
	return canonical.ObjectOf(
		canonical.Pair{Key: "snapshot_ref", Value: canonical.StringOf(snapshotRef)},
		canonical.Pair{Key: "task_id", Value: canonical.StringOf(taskID)},
	)
}

// AppendMilestone appends a milestone record to ownerPath: a
// declaration-mode record under MilestoneSchemaRef whose body names
// a consumer-declared task boundary and, optionally, a snapshot the
// boundary cites. The witness is the root witness body (plan
// Decision 4(a)), like every store-minted record.
//
// Task boundaries are claimed, not enforced (AC-F2-9): taskID is
// recorded verbatim and is never validated against any registry —
// the store has no notion of which tasks exist, and that absence is
// the contract.
func (s *Store) AppendMilestone(ownerPath, taskID, snapshotRef string, causedBy []string) (Receipt, error) {
	fields, ok := rootFields()
	if !ok {
		return Receipt{}, errors.New("store: root witness body has no body field")
	}
	return s.appendFact(ownerPath, RecordDraft{
		SchemaRef:        MilestoneSchemaRef,
		Mode:             record.ModeDeclaration,
		Body:             MilestoneBody(taskID, snapshotRef),
		CausedBy:         causedBy,
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      fields,
	})
}
