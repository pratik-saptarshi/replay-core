package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
)

// appendCrashHook, when non-nil, is called at named stages of
// AppendBatch ("insert" after each record insert, "pre-commit"
// after the intent row is written). It exists for the
// fault-injection tests, which arm it in a child process to kill the
// process mid-batch and prove atomicity; it is nil in production.
var appendCrashHook func(stage string)

// ErrEmptyBatch is returned by AppendBatch when the batch carries
// no drafts: an append must append something, or it would record an
// intent and mint receipts for nothing.
var ErrEmptyBatch = errors.New("store: append batch is empty")

// IntentConflictError reports an append whose intent ID was already
// recorded with a different batch digest: the intent names a
// different batch than the one being appended, so nothing is
// appended. The same intent ID with the same batch digest is a
// replay and returns the stored receipts instead of erroring.
type IntentConflictError struct {
	IntentID, ExistingDigest, IncomingDigest string
}

func (e *IntentConflictError) Error() string {
	return fmt.Sprintf("store: append intent %q already recorded with a different batch digest", e.IntentID)
}

// preparedDraft is one draft with everything the write transaction
// needs precomputed outside it: the record's ID, its canonical
// bytes, and its witness digest and bytes. Record digests are
// deterministic functions of the draft, so they are computed before
// the transaction opens — the batch digest derived from them is
// what intent replay and conflict detection compare against.
type preparedDraft struct {
	id            string
	recRaw        []byte
	witnessDigest string
	witnessRaw    []byte
}

// prepareDrafts validates each draft's witness body and computes
// its record digest and canonical bytes, in draft order.
func prepareDrafts(drafts []RecordDraft) ([]preparedDraft, error) {
	prepared := make([]preparedDraft, len(drafts))
	for i := range drafts {
		d := drafts[i]
		if err := record.ValidateWitnessBody(d.WitnessBody); err != nil {
			return nil, fmt.Errorf("store: draft %d: %w", i, err)
		}
		witnessDigest, err := record.WitnessBodyDigest(d.WitnessSchemaRef, d.WitnessBody)
		if err != nil {
			return nil, fmt.Errorf("store: draft %d witness: %w", i, err)
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
		prepared[i] = preparedDraft{id: id, recRaw: raw, witnessDigest: witnessDigest, witnessRaw: witnessRaw}
	}
	return prepared, nil
}

// batchDigestOf computes the batch digest: DigestKernel over the
// array of the batch's record digests, in draft order.
func batchDigestOf(ids []string) (string, error) {
	idValues := make([]canonical.Value, len(ids))
	for i, id := range ids {
		idValues[i] = canonical.StringOf(id)
	}
	return canonical.DigestKernel(canonical.ArrayOf(idValues...))
}

// decodeReceipts parses the receipts stored on an intent row —
// canonical bytes of an array of {"record_id", "owner_ordinal"}
// objects — back into Receipts.
func decodeReceipts(data string) ([]Receipt, error) {
	v, err := canonical.DecodeKernel([]byte(data))
	if err != nil {
		return nil, fmt.Errorf("store: decode stored receipts: %w", err)
	}
	if v.Kind != canonical.KindArray {
		return nil, errors.New("store: stored receipts are not an array")
	}
	receipts := make([]Receipt, 0, len(v.Elems))
	for _, elem := range v.Elems {
		var r Receipt
		for _, p := range elem.Pairs {
			switch p.Key {
			case "record_id":
				r.RecordID = p.Value.Str
			case "owner_ordinal":
				n, err := strconv.ParseInt(p.Value.Int, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("store: decode stored receipt ordinal: %w", err)
				}
				r.OwnerOrdinal = n
			}
		}
		receipts = append(receipts, r)
	}
	return receipts, nil
}

// rowQuerier is the QueryRowContext surface shared by *sql.DB and
// *sql.Conn, so the intent lookup runs identically before the write
// transaction (on the pool) and inside it (on the append's
// dedicated connection).
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// resolveIntent applies the intent-idempotency rule for one append
// whose batch digest is already computed: if the intent ID is not
// recorded, it reports handled = false and the append proceeds; if
// it is recorded with the same batch digest, it returns the stored
// receipts (a replay — nothing is appended); if it is recorded with
// a different batch digest, it returns *IntentConflictError.
func resolveIntent(ctx context.Context, q rowQuerier, intentID, batchDigest string) (receipts []Receipt, handled bool, err error) {
	var existingDigest, receiptsJSON string
	err = q.QueryRowContext(ctx,
		"SELECT batch_digest, receipts FROM append_intents WHERE intent_id = ?", intentID).
		Scan(&existingDigest, &receiptsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: look up intent %q: %w", intentID, err)
	}
	if existingDigest != batchDigest {
		return nil, false, &IntentConflictError{IntentID: intentID, ExistingDigest: existingDigest, IncomingDigest: batchDigest}
	}
	receipts, err = decodeReceipts(receiptsJSON)
	if err != nil {
		return nil, false, err
	}
	return receipts, true, nil
}

// AppendBatch appends drafts to ownerPath atomically under the
// caller's intent ID: either every draft lands, in order, with
// gap-free per-owner ordinals, or none does. Each draft's witness
// body is validated and digested, every CausedBy entry must name an
// existing record or an earlier draft in this batch, and the whole
// batch — records, witness bodies, edges, path entries, frontier,
// ordinal counter, and the intent row — commits in one BEGIN
// IMMEDIATE transaction. Any error rolls the whole batch back.
//
// Appends are intent-idempotent: repeating a call with the same
// intent ID and the same drafts returns the original receipts and
// appends nothing; the same intent ID with different drafts fails
// with *IntentConflictError and appends nothing. Degenerate inputs
// — an empty batch, an empty intent ID, an empty owner path, or a
// reserved replaycore. owner path — are rejected before any work.
func (s *Store) AppendBatch(ownerPath, intentID string, drafts []RecordDraft) ([]Receipt, error) {
	if len(drafts) == 0 {
		return nil, ErrEmptyBatch
	}
	if intentID == "" {
		return nil, errors.New("store: append intent ID is empty")
	}
	if ownerPath == "" {
		return nil, errors.New("store: owner path is empty")
	}
	if strings.HasPrefix(ownerPath, "replaycore.") {
		return nil, fmt.Errorf("store: owner path %q is in the reserved replaycore. namespace", ownerPath)
	}

	ctx := context.Background()

	// Record digests are deterministic: compute them, and the batch
	// digest over them, before opening the write transaction, so a
	// recorded intent can be resolved without taking the write lock.
	prepared, err := prepareDrafts(drafts)
	if err != nil {
		return nil, err
	}
	batchIDs := make([]string, len(prepared))
	for i, p := range prepared {
		batchIDs[i] = p.id
	}
	batchDigest, err := batchDigestOf(batchIDs)
	if err != nil {
		return nil, fmt.Errorf("store: batch digest: %w", err)
	}
	if receipts, handled, err := resolveIntent(ctx, s.db, intentID, batchDigest); err != nil {
		return nil, err
	} else if handled {
		return receipts, nil
	}

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

	// Re-check the intent row under the write lock: a racing append
	// with the same intent may have committed while this one waited
	// on BEGIN IMMEDIATE. The loser of that race resolves against
	// the winner's row here — replay returns the winner's receipts,
	// conflict errors — and never reaches the intent insert, so it
	// can never surface a primary-key error.
	if receipts, handled, err := resolveIntent(ctx, conn, intentID, batchDigest); err != nil {
		return nil, err
	} else if handled {
		return receipts, nil
	}

	var nextOrdinal int64
	err = conn.QueryRowContext(ctx,
		"SELECT next_ordinal FROM owner_ordinals WHERE owner_path = ?", ownerPath).Scan(&nextOrdinal)
	if errors.Is(err, sql.ErrNoRows) {
		nextOrdinal = 0
	} else if err != nil {
		return nil, fmt.Errorf("store: read ordinal counter for %q: %w", ownerPath, err)
	}

	receipts := make([]Receipt, 0, len(drafts))
	inBatch := make(map[string]bool, len(drafts))
	var tailID string
	for i := range drafts {
		d := drafts[i]
		p := prepared[i]
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
		ordinal := nextOrdinal + int64(i)
		if _, err := conn.ExecContext(ctx,
			"INSERT OR IGNORE INTO witness_bodies (witness_digest, schema_ref, body_bytes) VALUES (?, ?, ?)",
			p.witnessDigest, d.WitnessSchemaRef, p.witnessRaw); err != nil {
			return nil, fmt.Errorf("store: insert witness body: %w", err)
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO records (record_id, digest, owner_path, owner_ordinal, schema_ref, mode, canonical_bytes, witness_digest) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			p.id, p.id, ownerPath, ordinal, d.SchemaRef, d.Mode, p.recRaw, p.witnessDigest); err != nil {
			return nil, fmt.Errorf("store: insert record %s: %w", p.id, err)
		}
		for _, parent := range d.CausedBy {
			if _, err := conn.ExecContext(ctx,
				"INSERT INTO record_edges (record_id, caused_by) VALUES (?, ?)",
				p.id, parent); err != nil {
				return nil, fmt.Errorf("store: insert edge %s -> %s: %w", p.id, parent, err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO path_entries (owner_path, owner_ordinal, record_id) VALUES (?, ?, ?)",
			ownerPath, ordinal, p.id); err != nil {
			return nil, fmt.Errorf("store: insert path entry: %w", err)
		}
		if appendCrashHook != nil {
			appendCrashHook("insert")
		}
		inBatch[p.id] = true
		receipts = append(receipts, Receipt{RecordID: p.id, OwnerOrdinal: ordinal})
		tailID = p.id
	}

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

	// Record the intent row with the batch digest computed before
	// the transaction and the receipts as canonical JSON. The
	// in-transaction re-check above has already resolved any
	// recorded intent, so this insert names a fresh intent.
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
