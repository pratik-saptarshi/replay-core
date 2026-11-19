package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
)

// appendCrashHook, when non-nil, is called at named stages of
// AppendBatch ("insert" after each record insert, "pre-commit"
// after the intent row is written). It exists for the
// fault-injection tests, which arm it in a child process to kill the
// process mid-batch and prove atomicity; it is nil in production.
var appendCrashHook func(stage string)

// AppendBatch appends drafts to ownerPath atomically under the
// caller's intent ID: either every draft lands, in order, with
// gap-free per-owner ordinals, or none does. Each draft's witness
// body is validated and digested, every CausedBy entry must name an
// existing record or an earlier draft in this batch, and the whole
// batch — records, witness bodies, edges, path entries, frontier,
// ordinal counter, and the intent row — commits in one BEGIN
// IMMEDIATE transaction. Any error rolls the whole batch back.
func (s *Store) AppendBatch(ownerPath, intentID string, drafts []RecordDraft) ([]Receipt, error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: acquire connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("store: begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var nextOrdinal int64
	err = conn.QueryRowContext(ctx,
		"SELECT next_ordinal FROM owner_ordinals WHERE owner_path = ?", ownerPath).Scan(&nextOrdinal)
	if errors.Is(err, sql.ErrNoRows) {
		nextOrdinal = 0
	} else if err != nil {
		return nil, fmt.Errorf("store: read ordinal counter for %q: %w", ownerPath, err)
	}

	receipts := make([]Receipt, 0, len(drafts))
	batchIDs := make([]string, 0, len(drafts))
	inBatch := make(map[string]bool, len(drafts))
	var tailID string
	for i := range drafts {
		d := drafts[i]
		if err := record.ValidateWitnessBody(d.WitnessBody); err != nil {
			return nil, fmt.Errorf("store: draft %d: %w", i, err)
		}
		witnessDigest, err := record.WitnessBodyDigest(d.WitnessSchemaRef, d.WitnessBody)
		if err != nil {
			return nil, fmt.Errorf("store: draft %d witness: %w", i, err)
		}
		for _, parent := range d.CausedBy {
			if inBatch[parent] {
				continue
			}
			var n int
			if err := conn.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM records WHERE record_id = ?", parent).Scan(&n); err != nil {
				return nil, fmt.Errorf("store: check citation %s: %w", parent, err)
			}
			if n == 0 {
				return nil, fmt.Errorf("store: draft %d cites unknown record %s", i, parent)
			}
		}
		recObj, err := record.RecordInput(d.SchemaRef, d.Mode, d.Body, d.CausedBy, witnessDigest)
		if err != nil {
			return nil, fmt.Errorf("store: draft %d: %w", i, err)
		}
		id, err := canonical.DigestKernel(recObj)
		if err != nil {
			return nil, fmt.Errorf("store: draft %d digest: %w", i, err)
		}
		raw, err := canonical.MarshalKernel(recObj)
		if err != nil {
			return nil, fmt.Errorf("store: draft %d marshal: %w", i, err)
		}
		witnessRaw, err := canonical.MarshalKernel(d.WitnessBody)
		if err != nil {
			return nil, fmt.Errorf("store: draft %d witness marshal: %w", i, err)
		}
		ordinal := nextOrdinal + int64(i)
		if _, err := conn.ExecContext(ctx,
			"INSERT OR IGNORE INTO witness_bodies (witness_digest, schema_ref, body_bytes) VALUES (?, ?, ?)",
			witnessDigest, d.WitnessSchemaRef, witnessRaw); err != nil {
			return nil, fmt.Errorf("store: insert witness body: %w", err)
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO records (record_id, digest, owner_path, owner_ordinal, schema_ref, mode, canonical_bytes, witness_digest) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			id, id, ownerPath, ordinal, d.SchemaRef, d.Mode, raw, witnessDigest); err != nil {
			return nil, fmt.Errorf("store: insert record %s: %w", id, err)
		}
		for _, parent := range d.CausedBy {
			if _, err := conn.ExecContext(ctx,
				"INSERT INTO record_edges (record_id, caused_by) VALUES (?, ?)",
				id, parent); err != nil {
				return nil, fmt.Errorf("store: insert edge %s -> %s: %w", id, parent, err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO path_entries (owner_path, owner_ordinal, record_id) VALUES (?, ?, ?)",
			ownerPath, ordinal, id); err != nil {
			return nil, fmt.Errorf("store: insert path entry: %w", err)
		}
		if appendCrashHook != nil {
			appendCrashHook("insert")
		}
		inBatch[id] = true
		batchIDs = append(batchIDs, id)
		receipts = append(receipts, Receipt{RecordID: id, OwnerOrdinal: ordinal})
		tailID = id
	}

	if len(drafts) > 0 {
		newNext := nextOrdinal + int64(len(drafts))
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO frontiers (owner_path, tail_record_id, tail_ordinal) VALUES (?, ?, ?) "+
				"ON CONFLICT(owner_path) DO UPDATE SET tail_record_id = excluded.tail_record_id, tail_ordinal = excluded.tail_ordinal",
			ownerPath, tailID, newNext-1); err != nil {
			return nil, fmt.Errorf("store: update frontier: %w", err)
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO owner_ordinals (owner_path, next_ordinal) VALUES (?, ?) "+
				"ON CONFLICT(owner_path) DO UPDATE SET next_ordinal = excluded.next_ordinal",
			ownerPath, newNext); err != nil {
			return nil, fmt.Errorf("store: update ordinal counter: %w", err)
		}
	}

	// Record the intent row: the batch digest is DigestKernel over
	// the array of the batch's record digests; receipts are stored
	// as canonical JSON. (Replay/conflict semantics for a repeated
	// intent are Task 2; here the insert is plain.)
	idValues := make([]canonical.Value, len(batchIDs))
	for i, id := range batchIDs {
		idValues[i] = canonical.StringOf(id)
	}
	batchDigest, err := canonical.DigestKernel(canonical.ArrayOf(idValues...))
	if err != nil {
		return nil, fmt.Errorf("store: batch digest: %w", err)
	}
	receiptValues := make([]canonical.Value, len(receipts))
	for i, r := range receipts {
		ord, err := canonical.IntOf(strconv.FormatInt(r.OwnerOrdinal, 10))
		if err != nil {
			return nil, fmt.Errorf("store: receipt ordinal: %w", err)
		}
		receiptValues[i] = canonical.ObjectOf(
			canonical.Pair{Key: "record_id", Value: canonical.StringOf(r.RecordID)},
			canonical.Pair{Key: "owner_ordinal", Value: ord},
		)
	}
	receiptsJSON, err := canonical.MarshalKernel(canonical.ArrayOf(receiptValues...))
	if err != nil {
		return nil, fmt.Errorf("store: marshal receipts: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		"INSERT INTO append_intents (intent_id, owner_path, batch_digest, receipts) VALUES (?, ?, ?, ?)",
		intentID, ownerPath, batchDigest, string(receiptsJSON)); err != nil {
		return nil, fmt.Errorf("store: insert intent %q: %w", intentID, err)
	}
	if appendCrashHook != nil {
		appendCrashHook("pre-commit")
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("store: commit: %w", err)
	}
	committed = true
	return receipts, nil
}

// AppendFact appends a single draft under a generated intent ID.
func (s *Store) AppendFact(ownerPath string, draft RecordDraft) (Receipt, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Receipt{}, fmt.Errorf("store: generate intent ID: %w", err)
	}
	receipts, err := s.AppendBatch(ownerPath, "fact-"+hex.EncodeToString(b[:]), []RecordDraft{draft})
	if err != nil {
		return Receipt{}, err
	}
	return receipts[0], nil
}
