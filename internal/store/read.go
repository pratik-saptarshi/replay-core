package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
)

// GetRecord returns the stored record with the given ID: existence
// and parse only — witness-chain validation hardens this read path
// in Task 4. A missing ID returns ErrNotFound.
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
	rec, err := canonical.DecodeKernel(raw)
	if err != nil {
		return StoredRecord{}, fmt.Errorf("store: decode record %s: %w", recordID, err)
	}
	out := StoredRecord{ID: recordID, OwnerPath: ownerPath, OwnerOrdinal: ordinal, Record: rec}
	if witnessDigest != "" {
		var witnessRaw []byte
		if err := s.db.QueryRow(
			"SELECT body_bytes FROM witness_bodies WHERE witness_digest = ?",
			witnessDigest).Scan(&witnessRaw); err != nil {
			return StoredRecord{}, fmt.Errorf("store: read witness body %s: %w", witnessDigest, err)
		}
		witnessBody, err := canonical.DecodeKernel(witnessRaw)
		if err != nil {
			return StoredRecord{}, fmt.Errorf("store: decode witness body %s: %w", witnessDigest, err)
		}
		out.WitnessBody = witnessBody
	}
	return out, nil
}

// PathRecords returns every record on ownerPath in ordinal order.
func (s *Store) PathRecords(ownerPath string) ([]StoredRecord, error) {
	rows, err := s.db.Query(
		"SELECT record_id FROM path_entries WHERE owner_path = ? ORDER BY owner_ordinal ASC",
		ownerPath)
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
