package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
)

// CorruptionError reports a stored record that failed read-time
// validation: the bytes or witness material in the database do not
// satisfy the identity law the record's ID claims. A read that
// fails validation returns this error and never the record.
type CorruptionError struct {
	RecordID, Reason string
}

func (e *CorruptionError) Error() string {
	return fmt.Sprintf("store: record %s failed validation: %s", e.RecordID, e.Reason)
}

// recordField returns the value of key in a decoded record object.
func recordField(rec canonical.Value, key string) (canonical.Value, bool) {
	for _, p := range rec.Pairs {
		if p.Key == key {
			return p.Value, true
		}
	}
	return canonical.Value{}, false
}

// GetRecord returns the stored record with the given ID, validated
// against the identity law before it is returned: (1) the stored
// canonical bytes must decode and re-digest to the record's ID;
// (2) the row's witness_digest column must equal the record
// object's witness field; (3) the stored witness body must
// re-digest via record.WitnessBodyDigest to that same digest and
// pass record.ValidateWitnessBody. The root witness record is the
// sole exception to (2)–(3): its witness is the empty sentinel, it
// has no witness_bodies row, and its WitnessBody is its own body
// (the root witness fields). Any validation failure returns
// *CorruptionError, never the record. A missing ID returns
// ErrNotFound.
func (s *Store) GetRecord(recordID string) (StoredRecord, error) {
	var ownerPath, witnessDigest string
	var ordinal int64
	var raw []byte
	err := s.db.QueryRow(
		"SELECT owner_path, owner_ordinal, canonical_bytes, witness_digest FROM records WHERE record_id = ?",
		recordID).Scan(&ownerPath, &ordinal, &raw, &witnessDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredRecord{}, ErrNotFound
	}
	if err != nil {
		return StoredRecord{}, fmt.Errorf("store: read record %s: %w", recordID, err)
	}

	// (1) The stored canonical bytes decode and re-digest to the
	// row's record_id. A byte flip that still parses is
	// indistinguishable from a different valid record except by
	// digest — that is the point of this check.
	rec, err := canonical.DecodeKernel(raw)
	if err != nil {
		return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: fmt.Sprintf("stored bytes do not decode: %v", err)}
	}
	digest, err := canonical.DigestKernel(rec)
	if err != nil {
		return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: fmt.Sprintf("stored bytes do not re-digest: %v", err)}
	}
	if digest != recordID {
		return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: fmt.Sprintf("stored bytes re-digest to %s", digest)}
	}
	out := StoredRecord{ID: recordID, OwnerPath: ownerPath, OwnerOrdinal: ordinal, Record: rec}

	// The root witness record is the sole exception to (2)–(3):
	// step (1) has already pinned its bytes to the root record ID,
	// its witness is the empty sentinel, it has no witness_bodies
	// row, and its WitnessBody is its own body (the root witness
	// fields).
	if recordID == record.RootWitnessRecordID() {
		body, ok := recordField(rec, "body")
		if !ok {
			return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: "root witness record has no body"}
		}
		out.WitnessBody = body
		return out, nil
	}

	// (2) The row's witness_digest column equals the record
	// object's witness field.
	witnessField, ok := recordField(rec, "witness")
	if !ok || witnessField.Kind != canonical.KindString {
		return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: "record object has no witness field"}
	}
	if witnessDigest == "" {
		return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: "row stores an empty witness digest for a non-root record"}
	}
	if witnessDigest != witnessField.Str {
		return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: fmt.Sprintf("row witness digest %s does not match the record's witness field %s", witnessDigest, witnessField.Str)}
	}

	// (3) The stored witness body re-digests to that same digest
	// and passes witness-body validation.
	var witnessSchemaRef string
	var witnessRaw []byte
	err = s.db.QueryRow(
		"SELECT schema_ref, body_bytes FROM witness_bodies WHERE witness_digest = ?",
		witnessDigest).Scan(&witnessSchemaRef, &witnessRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: fmt.Sprintf("witness body %s is not stored", witnessDigest)}
	}
	if err != nil {
		return StoredRecord{}, fmt.Errorf("store: read witness body %s: %w", witnessDigest, err)
	}
	witnessBody, err := canonical.DecodeKernel(witnessRaw)
	if err != nil {
		return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: fmt.Sprintf("stored witness body %s does not decode: %v", witnessDigest, err)}
	}
	if err := record.ValidateWitnessBody(witnessBody); err != nil {
		return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: fmt.Sprintf("stored witness body %s is invalid: %v", witnessDigest, err)}
	}
	recomputed, err := record.WitnessBodyDigest(witnessSchemaRef, witnessBody)
	if err != nil {
		return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: fmt.Sprintf("stored witness body %s does not re-digest: %v", witnessDigest, err)}
	}
	if recomputed != witnessDigest {
		return StoredRecord{}, &CorruptionError{RecordID: recordID, Reason: fmt.Sprintf("stored witness body re-digests to %s, not %s", recomputed, witnessDigest)}
	}
	out.WitnessBody = witnessBody
	return out, nil
}

// PathRecords returns every record on ownerPath in ordinal order.
// Each record goes through GetRecord, so reads are validated the
// same way.
//
// Membership and order are read from the records table's
// placement columns — the same source GetRecord reports as a
// record's placement, and the source the slice layer's cut
// cross-check validates against. path_entries is a denormalized
// copy of that placement, still written by append and bootstrap
// but consulted by no read path: when the two disagreed (a
// tampered path_entries row), resolution silently followed the
// copy while validation followed the records table, and a cut
// could resolve without its own through record (panel W3-3).
func (s *Store) PathRecords(ownerPath string) ([]StoredRecord, error) {
	ids, err := s.enumeratePathIDs(ownerPath,
		"SELECT record_id FROM records WHERE owner_path = ? ORDER BY owner_ordinal ASC",
		ownerPath)
	if err != nil {
		return nil, err
	}
	return s.fetchRecords(ids)
}

// PathRecordsThrough returns ownerPath's records with ordinal at
// most throughOrdinal, in ordinal order — the bounded prefix read
// behind cut resolution (slice.ResolveCut, epochs). The bound
// lives in the query, so out-of-prefix records are never
// enumerated or fetched.
//
// The bound is a correctness property, not an optimization: the
// whole-path read enumerates every ID on the path and then
// fetches each through GetRecord, so a garbage collection that
// collects an out-of-prefix record between the enumeration and
// its fetch fails the fetch — and with it the resolution of a
// fully retained published cut, whose addressed set the
// collection provably did not touch (Codex P2 on PR #2,
// 2026-11-19). Bounding the enumeration to the addressed prefix
// removes out-of-prefix records from the read entirely, so
// their collection cannot affect it. Each record goes through
// GetRecord, so reads are validated the same way.
func (s *Store) PathRecordsThrough(ownerPath string, throughOrdinal int64) ([]StoredRecord, error) {
	ids, err := s.enumeratePathIDs(ownerPath,
		"SELECT record_id FROM records WHERE owner_path = ? AND owner_ordinal <= ? ORDER BY owner_ordinal ASC",
		ownerPath, throughOrdinal)
	if err != nil {
		return nil, err
	}
	return s.fetchRecords(ids)
}

// enumeratePathIDs runs a record_id-selecting query over
// ownerPath's records and returns the IDs in row order. The
// result set is fully consumed and closed before returning:
// callers fetch the records through GetRecord afterwards, and
// no half-open enumeration may sit across those reads.
func (s *Store) enumeratePathIDs(ownerPath, query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list path %q: %w", ownerPath, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan path %q: %w", ownerPath, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: list path %q: %w", ownerPath, err)
	}
	rows.Close()
	return ids, nil
}

// fetchRecords loads each ID through GetRecord, in order.
func (s *Store) fetchRecords(ids []string) ([]StoredRecord, error) {
	out := make([]StoredRecord, 0, len(ids))
	for _, id := range ids {
		rec, err := s.GetRecord(id)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// Frontier returns the tail record ID of ownerPath — the root
// witness record ID for a path with no records of its own.
func (s *Store) Frontier(ownerPath string) (string, error) {
	var tail string
	err := s.db.QueryRow(
		"SELECT tail_record_id FROM frontiers WHERE owner_path = ?", ownerPath).Scan(&tail)
	if errors.Is(err, sql.ErrNoRows) {
		return record.RootWitnessRecordID(), nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read frontier for %q: %w", ownerPath, err)
	}
	return tail, nil
}
