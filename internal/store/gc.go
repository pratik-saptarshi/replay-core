package store

import (
	"context"
	"fmt"
)

// GarbageReport summarizes one CollectGarbage run: how many
// records were collected and how many were kept.
type GarbageReport struct {
	Collected, Kept int
}

// CollectGarbage removes every record that is unreachable — via
// caused_by closure — from the retention roots (plan Decision
// 4(c)): every path's frontier tail, every record under
// CutSchemaRef, and every record under MilestoneSchemaRef. This is
// the store's single deletion mechanism and the one exception to
// the append-only rule; its guarantee is the contrapositive of the
// root definition — a record a frontier, a published cut, or a
// milestone can reach is never removed (AC-F2-7).
//
// Collection removes, in one BEGIN IMMEDIATE transaction, the
// collected records' rows, their causal edges and path entries,
// witness bodies left unreferenced by any surviving record, and
// intent rows whose every receipted record was collected.
// Surviving records are untouched: their ordinals never change
// (no renumbering), and frontiers — being roots — are never
// collected, so no frontier row moves.
func (s *Store) CollectGarbage() (GarbageReport, error) {
	ctx := context.Background()

	// Load the graph: every record ID, every causal edge, and the
	// retention roots. The kept set is computed iteratively in Go
	// (explicit stack — the same iterative discipline as Task 7's
	// closure), then the complement is deleted by explicit ID, so
	// a record appended concurrently after this load is never in
	// the deletion set.
	all := make(map[string]bool)
	rows, err := s.db.QueryContext(ctx, "SELECT record_id FROM records")
	if err != nil {
		return GarbageReport{}, fmt.Errorf("store: gc list records: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return GarbageReport{}, fmt.Errorf("store: gc scan record: %w", err)
		}
		all[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return GarbageReport{}, fmt.Errorf("store: gc list records: %w", err)
	}
	rows.Close()

	causes := make(map[string][]string)
	rows, err = s.db.QueryContext(ctx, "SELECT record_id, caused_by FROM record_edges")
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

	roots := make(map[string]bool)
	rows, err = s.db.QueryContext(ctx, "SELECT tail_record_id FROM frontiers")
	if err != nil {
		return GarbageReport{}, fmt.Errorf("store: gc list frontiers: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return GarbageReport{}, fmt.Errorf("store: gc scan frontier: %w", err)
		}
		roots[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return GarbageReport{}, fmt.Errorf("store: gc list frontiers: %w", err)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx,
		"SELECT record_id FROM records WHERE schema_ref IN (?, ?)", CutSchemaRef, MilestoneSchemaRef)
	if err != nil {
		return GarbageReport{}, fmt.Errorf("store: gc list root-schema records: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return GarbageReport{}, fmt.Errorf("store: gc scan root-schema record: %w", err)
		}
		roots[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return GarbageReport{}, fmt.Errorf("store: gc list root-schema records: %w", err)
	}
	rows.Close()

	// Kept = the caused_by closure of the roots, walked
	// iteratively. A kept record's causes are kept by construction,
	// so no surviving record can cite a collected one.
	kept := make(map[string]bool)
	stack := make([]string, 0, len(roots))
	for id := range roots {
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
		return report, nil
	}
	collectedSet := make(map[string]bool, len(collected))
	for _, id := range collected {
		collectedSet[id] = true
	}

	// Intent rows whose every receipted record was collected are
	// collected with them; an intent with any surviving receipted
	// record stays, so its replay/conflict behavior is unchanged.
	var doomedIntents []string
	rows, err = s.db.QueryContext(ctx, "SELECT intent_id, receipts FROM append_intents")
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
		allCollected := true
		for _, r := range receipts {
			if !collectedSet[r.RecordID] {
				allCollected = false
				break
			}
		}
		if allCollected {
			doomedIntents = append(doomedIntents, intentID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return GarbageReport{}, fmt.Errorf("store: gc list intents: %w", err)
	}
	rows.Close()

	// Delete the complement in a single BEGIN IMMEDIATE
	// transaction, on a dedicated connection like AppendBatch.
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
