package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// GarbageReport summarizes one CollectGarbage run: how many
// records were collected and how many were kept.
type GarbageReport struct {
	Collected, Kept int
}

// CollectGarbage removes every record that is unreachable from
// the retention roots (plan Decision 4(c)): every path's frontier
// tail, every record under CutSchemaRef, and every record under
// MilestoneSchemaRef. Reachability is defined per root class by
// what the root addresses: a frontier tail reaches its caused_by
// closure; a milestone reaches its caused_by closure; a
// published cut reaches every record on its owner path with
// ordinal at most the cut's through_owner_ordinal, plus that
// set's closure. A cut differs from the other classes because
// it is a published immutable address whose resolution is
// prefix-defined — ResolveCut returns the whole addressed set
// and publishes its digest, so the set must survive or the
// address mutates. A milestone publishes no such address:
// EpochSlice is a derived view over current state, so a
// milestone retains by closure alone and non-causal records
// inside an epoch window remain collectable. Which records
// belong to the cut and milestone classes is read from the
// records' canonical bytes, never the records.schema_ref
// column: the column is a denormalized copy, and a tampered
// cell must not change the outcome (issue #5). This is the
// store's single deletion mechanism
// and the one exception to the append-only rule; its guarantee
// is the contrapositive of the root definition — a record a
// frontier, a published cut, or a milestone can reach is never
// removed (AC-F2-7).
//
// The whole pass — loading the graph, computing the kept set, and
// deleting the complement — runs inside one BEGIN IMMEDIATE
// transaction on a dedicated connection, so the kept set is
// computed on the same snapshot the deletes apply to. An append
// either committed before the pass began (and is part of the
// graph it loads) or waits for the write lock and then validates
// its citations against the post-collection state; no append can
// commit in between and cite a record this pass deletes. (The
// pre-fix shape loaded the graph through the connection pool and
// took the write lock only for the deletes, which left exactly
// that window — Codex review of PR #2, 2026-11-19.)
//
// Collection removes the collected records' rows, their causal
// edges and path entries, witness bodies left unreferenced by any
// surviving record, and intent rows no receipted record of which
// survives. Surviving records are untouched: their ordinals
// never change (no renumbering), and frontiers — being roots —
// are never collected, so no frontier row moves.
func (s *Store) CollectGarbage() (GarbageReport, error) {
	ctx := context.Background()

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return GarbageReport{}, fmt.Errorf("store: gc acquire connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return GarbageReport{}, fmt.Errorf("store: gc begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	// Load the graph: every record ID, every causal edge, and the
	// retention roots — all on the GC connection, inside the
	// transaction, so the load is the state the deletes apply to.
	//
	// Root classification — which records are cuts, which are
	// milestones — is derived from each record's canonical
	// bytes, never from the records.schema_ref column (issue
	// #5): the column is a denormalized copy of the record's own
	// schema_ref field, and a tampered cell must not change what
	// GC retains. The bytes are the identity law's ground truth —
	// GetRecord re-digests them on every read — so they are what
	// GC classifies by. A record whose bytes do not decode, or
	// carry no schema_ref string, fails the pass: GC is the
	// store's one destructive operation and must not guess at
	// what a record is.
	all := make(map[string]bool)
	cutBodies := make(map[string][]byte)
	var milestoneIDs []string
	recRows, err := conn.QueryContext(ctx, "SELECT record_id, canonical_bytes FROM records")
	if err != nil {
		return GarbageReport{}, fmt.Errorf("store: gc list records: %w", err)
	}
	for recRows.Next() {
		var id string
		var raw []byte
		if err := recRows.Scan(&id, &raw); err != nil {
			recRows.Close()
			return GarbageReport{}, fmt.Errorf("store: gc scan record: %w", err)
		}
		recObj, err := canonical.DecodeKernel(raw)
		if err != nil {
			recRows.Close()
			return GarbageReport{}, fmt.Errorf("store: gc decode record %s: %w", id, err)
		}
		ref, ok := recordField(recObj, "schema_ref")
		if !ok || ref.Kind != canonical.KindString {
			recRows.Close()
			return GarbageReport{}, fmt.Errorf("store: gc record %s has no schema_ref string", id)
		}
		all[id] = true
		switch ref.Str {
		case CutSchemaRef:
			cutBodies[id] = raw
		case MilestoneSchemaRef:
			milestoneIDs = append(milestoneIDs, id)
		}
	}
	if err := recRows.Err(); err != nil {
		recRows.Close()
		return GarbageReport{}, fmt.Errorf("store: gc list records: %w", err)
	}
	recRows.Close()

	causes := make(map[string][]string)
	rows, err := conn.QueryContext(ctx, "SELECT record_id, caused_by FROM record_edges")
	if err != nil {
		return GarbageReport{}, fmt.Errorf("store: gc list edges: %w", err)
	}
	for rows.Next() {
		var id, cause string
		if err := rows.Scan(&id, &cause); err != nil {
			rows.Close()
			return GarbageReport{}, fmt.Errorf("store: gc scan edge: %w", err)
		}
		causes[id] = append(causes[id], cause)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return GarbageReport{}, fmt.Errorf("store: gc list edges: %w", err)
	}
	rows.Close()

	// Retention seeds, per root class (controller ruling on the
	// Codex P1, PR #2): retention covers what a root's own
	// resolution semantics return, where the root is a published
	// immutable address. A frontier tail addresses its causal
	// ancestry, and a milestone — which publishes no address —
	// likewise retains only its causal ancestry. A published
	// cut addresses an ordinal prefix of its path: ResolveCut
	// returns every record in the prefix, not just the
	// through-fact's ancestors, and publishes the set's digest,
	// so the cut's whole addressed set is seeded. The kept set
	// is the caused_by closure of all the seeds.
	seeds := make(map[string]bool)
	frontierIDs, err := gcScanIDs(ctx, conn, "frontiers", "SELECT tail_record_id FROM frontiers")
	if err != nil {
		return GarbageReport{}, err
	}
	for _, id := range frontierIDs {
		seeds[id] = true
	}

	// The cuts classified from canonical bytes during the load,
	// in ID order so the pass is deterministic: each cut seeds
	// itself and the whole prefix its body addresses.
	cutIDs := make([]string, 0, len(cutBodies))
	for id := range cutBodies {
		cutIDs = append(cutIDs, id)
	}
	sort.Strings(cutIDs)
	for _, id := range cutIDs {
		ownerPath, throughOrdinal, err := cutAddress(id, cutBodies[id])
		if err != nil {
			return GarbageReport{}, err
		}
		seeds[id] = true
		prefixIDs, err := gcPrefixIDs(ctx, conn, ownerPath, throughOrdinal)
		if err != nil {
			return GarbageReport{}, err
		}
		for _, pid := range prefixIDs {
			seeds[pid] = true
		}
	}

	for _, id := range milestoneIDs {
		seeds[id] = true
	}

	// Kept = the caused_by closure of the seeds, walked
	// iteratively. A kept record's causes are kept by construction,
	// so no surviving record can cite a collected one.
	kept := make(map[string]bool)
	stack := make([]string, 0, len(seeds))
	for id := range seeds {
		if all[id] && !kept[id] {
			kept[id] = true
			stack = append(stack, id)
		}
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, cause := range causes[id] {
			if all[cause] && !kept[cause] {
				kept[cause] = true
				stack = append(stack, cause)
			}
		}
	}
	var collected []string
	for id := range all {
		if !kept[id] {
			collected = append(collected, id)
		}
	}
	report := GarbageReport{Collected: len(collected), Kept: len(all) - len(collected)}
	if len(collected) == 0 {
		// Nothing to delete — and, no record having been deleted,
		// no witness body or intent row can have become
		// collectable either. Commit the read-only pass.
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return GarbageReport{}, fmt.Errorf("store: gc commit: %w", err)
		}
		committed = true
		return report, nil
	}
	// Intent rows with no surviving receipted record are
	// collected with them; an intent with any receipt in the
	// kept set stays, so its replay/conflict behavior is
	// unchanged. Deadness is judged against the kept set, not
	// against this pass's deletions: a batch whose records are
	// collected across several passes is dead once its last
	// receipt falls, even though no single pass collected them
	// all — judging by the pass's own collected set leaked
	// exactly those rows forever (Codex P2 on PR #2).
	var doomedIntents []string
	rows, err = conn.QueryContext(ctx, "SELECT intent_id, receipts FROM append_intents")
	if err != nil {
		return GarbageReport{}, fmt.Errorf("store: gc list intents: %w", err)
	}
	for rows.Next() {
		var intentID, receiptsJSON string
		if err := rows.Scan(&intentID, &receiptsJSON); err != nil {
			rows.Close()
			return GarbageReport{}, fmt.Errorf("store: gc scan intent: %w", err)
		}
		receipts, err := decodeReceipts(receiptsJSON)
		if err != nil {
			rows.Close()
			return GarbageReport{}, err
		}
		if len(receipts) == 0 {
			continue
		}
		survives := false
		for _, r := range receipts {
			if kept[r.RecordID] {
				survives = true
				break
			}
		}
		if !survives {
			doomedIntents = append(doomedIntents, intentID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return GarbageReport{}, fmt.Errorf("store: gc list intents: %w", err)
	}
	rows.Close()

	// Delete the complement, still inside the same transaction.
	for _, id := range collected {
		if _, err := conn.ExecContext(ctx, "DELETE FROM record_edges WHERE record_id = ? OR caused_by = ?", id, id); err != nil {
			return GarbageReport{}, fmt.Errorf("store: gc delete edges of %s: %w", id, err)
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM path_entries WHERE record_id = ?", id); err != nil {
			return GarbageReport{}, fmt.Errorf("store: gc delete path entries of %s: %w", id, err)
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM records WHERE record_id = ?", id); err != nil {
			return GarbageReport{}, fmt.Errorf("store: gc delete record %s: %w", id, err)
		}
	}
	// Witness bodies left unreferenced by any surviving record.
	// The root witness record stores an empty witness_digest and
	// has no witness_bodies row, so it needs no special case here.
	if _, err := conn.ExecContext(ctx,
		"DELETE FROM witness_bodies WHERE witness_digest NOT IN (SELECT witness_digest FROM records)"); err != nil {
		return GarbageReport{}, fmt.Errorf("store: gc delete unreferenced witness bodies: %w", err)
	}
	for _, intentID := range doomedIntents {
		if _, err := conn.ExecContext(ctx, "DELETE FROM append_intents WHERE intent_id = ?", intentID); err != nil {
			return GarbageReport{}, fmt.Errorf("store: gc delete intent %q: %w", intentID, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return GarbageReport{}, fmt.Errorf("store: gc commit: %w", err)
	}
	committed = true
	return report, nil
}

// gcPrefixIDs returns the IDs of every record on ownerPath with
// ordinal at most throughOrdinal — one root class's addressed
// prefix set. Membership is decided by the records table's
// placement columns, the same source ResolveCut resolves the
// cut's prefix from: seeding from the denormalized path_entries
// copy instead let a tampered row shrink the retained set below
// the set the published cut still resolves (panel W3-2).
func gcPrefixIDs(ctx context.Context, conn *sql.Conn, ownerPath string, throughOrdinal int64) ([]string, error) {
	return gcScanIDs(ctx, conn, "path prefix records",
		"SELECT record_id FROM records WHERE owner_path = ? AND owner_ordinal <= ?",
		ownerPath, throughOrdinal)
}

// cutAddress decodes a cut record's canonical bytes and returns
// the (owner_path, through_owner_ordinal) its body names — the
// prefix the cut addresses, extracted the way slice.ReadCut
// extracts it. The store is the lowest layer and cannot import
// slice, so the field walk is repeated here over the same body
// shape. A cut whose body does not parse fails the whole pass:
// GC must not guess at what a published cut retains.
func cutAddress(recordID string, raw []byte) (string, int64, error) {
	recObj, err := canonical.DecodeKernel(raw)
	if err != nil {
		return "", 0, fmt.Errorf("store: gc decode cut %s: %w", recordID, err)
	}
	body, ok := recordField(recObj, "body")
	if !ok || body.Kind != canonical.KindObject {
		return "", 0, fmt.Errorf("store: gc cut %s has no body object", recordID)
	}
	ownerPath, ok := recordField(body, "owner_path")
	if !ok || ownerPath.Kind != canonical.KindString {
		return "", 0, fmt.Errorf("store: gc cut %s body has no owner_path string", recordID)
	}
	ordinalValue, ok := recordField(body, "through_owner_ordinal")
	if !ok || ordinalValue.Kind != canonical.KindInt {
		return "", 0, fmt.Errorf("store: gc cut %s body has no through_owner_ordinal integer", recordID)
	}
	ordinal, err := strconv.ParseInt(ordinalValue.Int, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("store: gc cut %s through_owner_ordinal: %w", recordID, err)
	}
	return ownerPath.Str, ordinal, nil
}

// gcScanIDs runs a single-column query on the GC connection and
// returns the column's values. what names the query's subject
// for error messages.
func gcScanIDs(ctx context.Context, conn *sql.Conn, what, query string, args ...any) ([]string, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: gc list %s: %w", what, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: gc scan %s: %w", what, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: gc list %s: %w", what, err)
	}
	return ids, nil
}
