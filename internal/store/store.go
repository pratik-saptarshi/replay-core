// Package store is replay-core's append-only, content-addressed
// trace store (F2): one SQLite file holding records under the
// Phase 0 identity law. Identity is enforced in the database itself
// by a CHECK (record_id = digest) constraint and re-validated on
// read. The store adds no fields to the record itself; it indexes
// records by owner path with per-owner ordinals, tracks causal
// edges, append intents, and per-path frontiers.
package store

import (
	"database/sql"
	"errors"
	"fmt"

	_ "github.com/mattn/go-sqlite3"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
)

// Reserved schema_refs minted by the store itself, and the reserved
// owner path the root witness record lives on. The schema constants
// live here — the lowest layer — so garbage collection can recognize
// cut and milestone records as retention roots without an import
// cycle (plan Decision 3).
const (
	MilestoneSchemaRef = "replaycore.milestone.v1"
	CutSchemaRef       = "replaycore.cut.v1"
	AbandonSchemaRef   = "replaycore.abandon.v1"
	SystemPath         = "replaycore.system"
)

// ErrNotFound is returned by GetRecord when no record carries the
// requested ID.
var ErrNotFound = errors.New("store: record not found")

// RecordDraft is one record to append. Witness material is explicit
// (plan Decision 4(a)): WitnessBody is the witness fields object —
// the value record.ValidateWitnessBody and record.WitnessBodyDigest
// consume — and the store computes the witness digest from it.
type RecordDraft struct {
	SchemaRef, Mode  string
	Body             canonical.Value
	CausedBy         []string
	WitnessSchemaRef string
	WitnessBody      canonical.Value
}

// Receipt reports where one appended record landed.
type Receipt struct {
	RecordID     string
	OwnerOrdinal int64
}

// StoredRecord is a record read back from the store: its identity,
// placement, the full record object, and its witness fields object.
// The root witness record carries the empty witness sentinel and
// has no witness body; its WitnessBody is its own body (the root
// witness fields).
type StoredRecord struct {
	ID           string
	OwnerPath    string
	OwnerOrdinal int64
	Record       canonical.Value // the full record object
	WitnessBody  canonical.Value
}

// Store is a handle on one trace store file.
type Store struct {
	db *sql.DB
}

// schemaDDL is the store schema, executed idempotently at Open.
// Table names follow the design evidence; identity is enforced by
// CHECK (record_id = digest) in the records table itself.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS records (
  record_id TEXT PRIMARY KEY, digest TEXT NOT NULL,
  owner_path TEXT NOT NULL, owner_ordinal INTEGER NOT NULL,
  schema_ref TEXT NOT NULL, mode TEXT NOT NULL,
  canonical_bytes BLOB NOT NULL, witness_digest TEXT NOT NULL,
  UNIQUE(owner_path, owner_ordinal), CHECK (record_id = digest));
CREATE TABLE IF NOT EXISTS witness_bodies (
  witness_digest TEXT PRIMARY KEY, schema_ref TEXT NOT NULL, body_bytes BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS record_edges (
  record_id TEXT NOT NULL, caused_by TEXT NOT NULL, PRIMARY KEY (record_id, caused_by));
CREATE TABLE IF NOT EXISTS path_entries (
  owner_path TEXT NOT NULL, owner_ordinal INTEGER NOT NULL, record_id TEXT NOT NULL,
  PRIMARY KEY (owner_path, owner_ordinal));
CREATE TABLE IF NOT EXISTS append_intents (
  intent_id TEXT PRIMARY KEY, owner_path TEXT NOT NULL,
  batch_digest TEXT NOT NULL, receipts TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS frontiers (
  owner_path TEXT PRIMARY KEY, tail_record_id TEXT NOT NULL, tail_ordinal INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS owner_ordinals (
  owner_path TEXT PRIMARY KEY, next_ordinal INTEGER NOT NULL);
`

// Open opens (creating if needed) the store at path, applies the
// schema, and bootstraps the root witness record if absent.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &Store{db: db}
	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	if err := s.bootstrapRoot(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the store's database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// rootFields returns the root witness fields object: the body field
// of record.RootWitnessBody(). Phase 0 exports the fields only in
// that wrapped form (plan Decision 4(a)).
func rootFields() (canonical.Value, bool) {
	for _, p := range record.RootWitnessBody().Pairs {
		if p.Key == "body" {
			return p.Value, true
		}
	}
	return canonical.Value{}, false
}

// bootstrapRoot inserts the root witness record on SystemPath at
// ordinal 0 if absent. The root record's body is the witness fields
// object itself, because Phase 0's root-record validation requires
// exactly that body, mode capture, no causal parents, and the empty
// witness sentinel; its computed ID must equal
// record.RootWitnessRecordID() — Open fails otherwise. Its records
// row stores an empty witness_digest and it has no witness_bodies
// row.
func (s *Store) bootstrapRoot() error {
	rootID := record.RootWitnessRecordID()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM records WHERE record_id = ?", rootID).Scan(&n); err != nil {
		return fmt.Errorf("store: check root witness record: %w", err)
	}
	if n > 0 {
		return nil
	}
	fields, ok := rootFields()
	if !ok {
		return errors.New("store: root witness body has no body field")
	}
	recObj, err := record.RecordInput(record.RootWitnessSchemaRef, record.ModeCapture, fields, nil, "")
	if err != nil {
		return fmt.Errorf("store: build root witness record: %w", err)
	}
	id, err := canonical.DigestKernel(recObj)
	if err != nil {
		return fmt.Errorf("store: digest root witness record: %w", err)
	}
	if id != rootID {
		return fmt.Errorf("store: root witness record ID = %s, want %s", id, rootID)
	}
	raw, err := canonical.MarshalKernel(recObj)
	if err != nil {
		return fmt.Errorf("store: marshal root witness record: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: bootstrap begin: %w", err)
	}
	defer tx.Rollback()
	inserts := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO records (record_id, digest, owner_path, owner_ordinal, schema_ref, mode, canonical_bytes, witness_digest) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			[]any{id, id, SystemPath, 0, record.RootWitnessSchemaRef, record.ModeCapture, raw, ""}},
		{"INSERT INTO path_entries (owner_path, owner_ordinal, record_id) VALUES (?, ?, ?)",
			[]any{SystemPath, 0, id}},
		{"INSERT INTO frontiers (owner_path, tail_record_id, tail_ordinal) VALUES (?, ?, ?)",
			[]any{SystemPath, id, 0}},
		{"INSERT INTO owner_ordinals (owner_path, next_ordinal) VALUES (?, ?)",
			[]any{SystemPath, 1}},
	}
	for _, in := range inserts {
		if _, err := tx.Exec(in.query, in.args...); err != nil {
			return fmt.Errorf("store: bootstrap root witness record: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: bootstrap commit: %w", err)
	}
	return nil
}
