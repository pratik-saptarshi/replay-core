// Package store is replay-core's append-only, content-addressed
// trace store (F2): one SQLite file holding records under the
// Phase 0 identity law. Identity is enforced in the database itself
// by a CHECK (record_id = digest) constraint and re-validated on
// read. The store adds no fields to the record itself; it indexes
// records by owner path with per-owner ordinals, tracks causal
// edges, append intents, and per-path frontiers.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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

// dsnPathEscaper percent-encodes the three characters that are
// URI-significant inside a file: URI's path component — '%'
// (the escape introducer itself, encoded first so a literal
// percent survives the parser's decoding), '#' (fragment) and
// '?' (query). Every other byte of the caller's path, path
// separators included, passes into the DSN unchanged. The
// replacer scans its input once and never re-scans its own
// output, so the '%' of an escape it just emitted is not
// encoded again.
var dsnPathEscaper = strings.NewReplacer(
	"%", "%25",
	"#", "%23",
	"?", "%3F",
)

// Open opens (creating if needed) the store at path, applies the
// schema, and bootstraps the root witness record if absent.
func Open(path string) (*Store, error) {
	// The DSN uses the driver's named parameters, not _pragma=:
	// mattn/go-sqlite3 silently ignores _pragma, so the original
	// "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" never
	// took effect — connections actually ran with journal_mode=
	// delete and the driver's built-in busy_timeout of 5000. The
	// Task 6 multi-process proof exposed this: a caller's BEGIN
	// IMMEDIATE failed "database is locked" after exactly 5000ms
	// under 4-way contention. Named parameters verified by pragma
	// probe: journal_mode=wal, busy_timeout=30000. The timeout is
	// generous because concurrent writers serialize on BEGIN
	// IMMEDIATE and a caller-visible locking error is a defect
	// (AC-F2-6): a waiter must outlast a lock holder descheduled
	// under load.
	//
	// The path is percent-escaped before it enters the URI. On
	// Unix, '#', '?' and '%' are valid filename characters but
	// URI syntax: '#' opens a fragment, '?' opens the query
	// string, and a valid %XX sequence is decoded by the URI
	// parser — so the unescaped concatenation opened a different
	// file than the caller named (trace#one.db and trace#two.db
	// both landed on the file "trace", silently mixing two
	// stores; Codex P2 on PR #2, 2026-11-19). Escaping exactly
	// those three characters — separators and relative paths
	// pass through untouched — makes the URI name the literal
	// path: the parser decodes the escapes back to the caller's
	// bytes. (net/url's URL{Path: ...} was considered and
	// rejected on a probe: it renders a relative path as the
	// URI authority — file://trace.db — not as a path.) The
	// driver parameters are appended after the escaped path, so
	// the first literal '?' in the DSN is the intended query
	// separator.
	dsn := "file:" + dsnPathEscaper.Replace(path) + "?_busy_timeout=30000&_journal_mode=WAL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// init applies the schema and bootstraps the root witness
// record, serialized under the write lock: both steps run
// inside one BEGIN IMMEDIATE transaction on a dedicated
// connection. Concurrent first-open of a fresh store is a
// supported shape (the store's contract is multi-process), and
// the pre-fix shape raced it twice over — the schema went
// through the connection pool with no lock, so a second opener
// could fail "apply schema: database is locked" while the
// first held the file mid-initialization, and bootstrapRoot's
// existence check ran outside its insertion transaction, so
// two openers that both observed "no root yet" collided on the
// root record's uniqueness constraint (Codex P2 on PR #2,
// 2026-11-19). Under BEGIN IMMEDIATE a second opener waits for
// the first opener's commit, then observes the applied schema
// and the committed root, and proceeds.
//
// One contention point sits before the store's own lock
// discipline can apply: the driver runs the DSN pragmas —
// journal_mode=WAL above all — when it establishes each
// physical connection, and the WAL transition on a fresh file
// can answer "database is locked" in a way busy_timeout does
// not cover (observed: racing openers failing at connection
// acquisition while another opener was mid-init). init
// therefore retries the whole attempt on lock errors, with a
// bounded backoff: initOnce is transactional and idempotent
// (schema is IF NOT EXISTS; the bootstrap check runs inside
// the transaction), so a retry observes either the pre-init
// state or a peer's committed init, never a partial one. The
// bound is generous against an init that takes milliseconds;
// exhausting it surfaces the lock error rather than hiding it.
func (s *Store) init() error {
	var err error
	for attempt := 0; ; attempt++ {
		if err = s.initOnce(); err == nil || !isLockError(err) || attempt >= 100 {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// isLockError reports whether err is a SQLite locking failure
// (SQLITE_BUSY / SQLITE_LOCKED, as the driver surfaces them).
func isLockError(err error) bool {
	return strings.Contains(err.Error(), "database is locked") ||
		strings.Contains(err.Error(), "database table is locked")
}

func (s *Store) initOnce() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: init acquire connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: init begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	if _, err := conn.ExecContext(ctx, schemaDDL); err != nil {
		return fmt.Errorf("store: apply schema: %w", err)
	}
	if err := bootstrapRoot(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("store: init commit: %w", err)
	}
	committed = true
	return nil
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
// ordinal 0 if absent. It runs on Open's init connection inside
// the caller's BEGIN IMMEDIATE transaction, so the existence
// check and the insertion are atomic against concurrent
// openers: a second opener's check runs only after the first
// opener commits, and observes the root already present. The
// root record's body is the witness fields object itself,
// because Phase 0's root-record validation requires exactly
// that body, mode capture, no causal parents, and the empty
// witness sentinel; its computed ID must equal
// record.RootWitnessRecordID() — Open fails otherwise. Its records
// row stores an empty witness_digest and it has no witness_bodies
// row.
func bootstrapRoot(ctx context.Context, conn *sql.Conn) error {
	rootID := record.RootWitnessRecordID()
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM records WHERE record_id = ?", rootID).Scan(&n); err != nil {
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
		if _, err := conn.ExecContext(ctx, in.query, in.args...); err != nil {
			return fmt.Errorf("store: bootstrap root witness record: %w", err)
		}
	}
	return nil
}
