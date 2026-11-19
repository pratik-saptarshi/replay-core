package store_test

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The corruption suite (AC-F2-5): a fixture trace written through
// the public API is tampered with through a raw database handle,
// and reads through the reopened store must refuse the tampered
// record with *store.CorruptionError — never return it.

// seedCorruptionFixture writes a 5-record fixture trace through the
// public API, closes the store, and returns the store file's path
// and the records' IDs in append order.
func seedCorruptionFixture(t *testing.T) (string, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	drafts := make([]store.RecordDraft, 5)
	for i := range drafts {
		drafts[i] = draft(t, "test.fact", fmt.Sprintf(`{"n":%d}`, i+1))
	}
	receipts, err := s.AppendBatch("runs/corrupt", "corruption-fixture", drafts)
	if err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ids := make([]string, len(receipts))
	for i, r := range receipts {
		ids[i] = r.RecordID
	}
	return path, ids
}

// reopenStore reopens the store at path through the public API.
func reopenStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// rawBlob reads one BLOB cell through a raw handle on the store
// file, bypassing the store package entirely.
func rawBlob(t *testing.T, path, query string, args ...any) []byte {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	var blob []byte
	if err := db.QueryRow(query, args...).Scan(&blob); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	return blob
}

// rawExec runs one statement through a raw handle on the store
// file, bypassing the store package entirely.
func rawExec(t *testing.T, path, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("raw exec: %v", err)
	}
}

// flipByte returns a copy of b with one byte flipped.
func flipByte(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	out[len(out)/2] ^= 0x01
	return out
}

// assertCorruption fails unless err is a *store.CorruptionError
// naming recordID.
func assertCorruption(t *testing.T, err error, recordID string) {
	t.Helper()
	var cerr *store.CorruptionError
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %v, want *store.CorruptionError", err)
	}
	if cerr.RecordID != recordID {
		t.Fatalf("CorruptionError.RecordID = %s, want %s", cerr.RecordID, recordID)
	}
}

func TestGetRecordRejectsTamperedBody(t *testing.T) { // (a) flip one byte in records.canonical_bytes
	path, ids := seedCorruptionFixture(t)
	target := ids[2]
	raw := rawBlob(t, path, "SELECT canonical_bytes FROM records WHERE record_id = ?", target)
	rawExec(t, path, "UPDATE records SET canonical_bytes = ? WHERE record_id = ?", flipByte(raw), target)

	s := reopenStore(t, path)
	_, err := s.GetRecord(target)
	assertCorruption(t, err, target)
}

func TestGetRecordRejectsTamperedWitnessBody(t *testing.T) { // (b) flip one byte in witness_bodies.body_bytes
	path, ids := seedCorruptionFixture(t)
	digest := rootWitnessDigest(t)
	raw := rawBlob(t, path, "SELECT body_bytes FROM witness_bodies WHERE witness_digest = ?", digest)
	rawExec(t, path, "UPDATE witness_bodies SET body_bytes = ? WHERE witness_digest = ?", flipByte(raw), digest)

	s := reopenStore(t, path)
	_, err := s.GetRecord(ids[0])
	assertCorruption(t, err, ids[0])
}

func TestGetRecordRejectsTamperedWitnessCitation(t *testing.T) { // (c) UPDATE records SET witness_digest = 'sha256:9999...'
	path, ids := seedCorruptionFixture(t)
	target := ids[1]
	rawExec(t, path, "UPDATE records SET witness_digest = ? WHERE record_id = ?",
		"sha256:9999999999999999999999999999999999999999999999999999999999999999", target)

	s := reopenStore(t, path)
	_, err := s.GetRecord(target)
	assertCorruption(t, err, target)
}

func TestGetRecordAcceptsUntamperedFixture(t *testing.T) { // control: all 5 records read back, digests intact
	path, ids := seedCorruptionFixture(t)
	s := reopenStore(t, path)
	for _, id := range ids {
		rec, err := s.GetRecord(id)
		if err != nil {
			t.Fatalf("GetRecord(%s): %v", id, err)
		}
		if rec.ID != id {
			t.Fatalf("ID = %s, want %s", rec.ID, id)
		}
		digest, err := canonical.DigestKernel(rec.Record)
		if err != nil {
			t.Fatalf("DigestKernel(%s): %v", id, err)
		}
		if digest != id {
			t.Fatalf("record %s re-digests to %s", id, digest)
		}
		witnessDigest, err := record.WitnessBodyDigest(record.RootWitnessSchemaRef, rec.WitnessBody)
		if err != nil {
			t.Fatalf("WitnessBodyDigest(%s): %v", id, err)
		}
		if witnessDigest != rootWitnessDigest(t) {
			t.Fatalf("record %s witness digest = %s, want %s", id, witnessDigest, rootWitnessDigest(t))
		}
	}
	// The root witness record — the sole exception to the witness
	// checks — must also read back cleanly.
	root, err := s.GetRecord(record.RootWitnessRecordID())
	if err != nil {
		t.Fatalf("GetRecord(root): %v", err)
	}
	if root.ID != record.RootWitnessRecordID() {
		t.Fatalf("root ID = %s", root.ID)
	}
}
