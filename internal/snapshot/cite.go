package snapshot

import (
	"fmt"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// fieldOf returns the value stored under key in a canonical object
// value, or false if v is not an object or the key is absent.
func fieldOf(v canonical.Value, key string) (canonical.Value, bool) {
	if v.Kind != canonical.KindObject {
		return canonical.Value{}, false
	}
	for _, p := range v.Pairs {
		if p.Key == key {
			return p.Value, true
		}
	}
	return canonical.Value{}, false
}

// SnapshotAt resolves the snapshot commit cited by the milestone
// the given cut terminates at: read the cut, take its through-fact,
// require that fact to be a milestone record
// (store.MilestoneSchemaRef), and return the milestone body's
// snapshot_ref. The citation, and only the citation, decides —
// never "the latest snapshot".
//
// The body is read against store.MilestoneBody, the single
// construction site for the citation body (plan Decision 4(d)):
// the stored body must carry exactly the constructor's fields, so
// the shape the writer writes and the shape this resolver reads
// cannot drift apart.
func SnapshotAt(st *store.Store, cutID string) (string, error) {
	cut, err := slice.ReadCut(st, cutID)
	if err != nil {
		return "", err
	}
	rec, err := st.GetRecord(cut.ThroughFactID)
	if err != nil {
		return "", fmt.Errorf("snapshot: resolve citation at cut %s: %w", cutID, err)
	}
	schemaRef, ok := fieldOf(rec.Record, "schema_ref")
	if !ok || schemaRef.Kind != canonical.KindString || schemaRef.Str != store.MilestoneSchemaRef {
		return "", fmt.Errorf("snapshot: cut %s does not terminate at a milestone: through-fact %s is not a %s record", cutID, cut.ThroughFactID, store.MilestoneSchemaRef)
	}
	body, ok := fieldOf(rec.Record, "body")
	if !ok || body.Kind != canonical.KindObject {
		return "", fmt.Errorf("snapshot: milestone %s has no body object", cut.ThroughFactID)
	}
	shape := store.MilestoneBody("", "")
	if len(body.Pairs) != len(shape.Pairs) {
		return "", fmt.Errorf("snapshot: milestone %s body does not match the MilestoneBody shape", cut.ThroughFactID)
	}
	for _, p := range shape.Pairs {
		if _, ok := fieldOf(body, p.Key); !ok {
			return "", fmt.Errorf("snapshot: milestone %s body does not match the MilestoneBody shape: missing field %q", cut.ThroughFactID, p.Key)
		}
	}
	ref, ok := fieldOf(body, "snapshot_ref")
	if !ok || ref.Kind != canonical.KindString {
		return "", fmt.Errorf("snapshot: milestone %s body has no snapshot_ref string", cut.ThroughFactID)
	}
	return ref.Str, nil
}
