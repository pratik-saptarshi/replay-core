package store_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// seedStore opens a store on a fresh file, appends one valid record
// through the store's own API, closes the store, and returns the
// file path.
func seedStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.AppendBatch("runs/a", "seed-1", []store.RecordDraft{
		draft(t, "test.fact", `{"seed":1}`),
	}); err != nil {
		t.Fatalf("seed AppendBatch: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// openRaw opens a second, raw database/sql handle on the store
// file. The sweep's bypass attempts are real SQL against the file,
// not calls into the store's own API.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close raw handle: %v", err)
		}
	})
	return db
}

func TestIdentityCheckRejectsMismatchedInsert(t *testing.T) { // AC-F2-3, as written
	path := seedStore(t) // a store with one valid record, closed
	raw := openRaw(t, path)
	_, err := raw.Exec(`INSERT INTO records (record_id, digest, owner_path, owner_ordinal, schema_ref, mode, canonical_bytes, witness_digest)
		VALUES ('sha256:1111111111111111111111111111111111111111111111111111111111111111',
			'sha256:2222222222222222222222222222222222222222222222222222222222222222',
			'runs/a', 99, 'test.fact', 'capture', x'00', 'sha256:22e3bf15914b5007dea08a1079d1542470a369bcbed067687f345e7e85721130')`)
	if err == nil || !strings.Contains(err.Error(), "CHECK constraint failed: record_id = digest") {
		t.Fatalf("mismatched INSERT err = %v, want the identity CHECK itself to reject", err)
	}
}

func TestIdentityMutationSweep(t *testing.T) {
	// every insert path that could create a row with record_id != digest must fail:
	cases := []string{
		`UPDATE records SET record_id = 'sha256:3333333333333333333333333333333333333333333333333333333333333333' WHERE record_id IN (SELECT record_id FROM records LIMIT 1)`,
		`UPDATE records SET digest = 'sha256:4444444444444444444444444444444444444444444444444444444444444444'`,
		`INSERT INTO records SELECT 'sha256:5555555555555555555555555555555555555555555555555555555555555555', digest, owner_path, 100, schema_ref, mode, canonical_bytes, witness_digest FROM records LIMIT 1`,
	}
	// each executed against a freshly seeded store; each must error with a constraint violation
	for i, stmt := range cases {
		path := seedStore(t)
		raw := openRaw(t, path)
		if _, err := raw.Exec(stmt); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed: record_id = digest") {
			t.Errorf("case %d: err = %v, want the identity CHECK itself to reject\nstatement: %s", i, err, stmt)
			continue
		}
		// the rejected statement must leave no row behind with record_id != digest
		var bad int
		if err := raw.QueryRow(`SELECT COUNT(*) FROM records WHERE record_id <> digest`).Scan(&bad); err != nil {
			t.Fatalf("case %d: count rows with record_id != digest: %v", i, err)
		}
		if bad != 0 {
			t.Errorf("case %d: %d rows with record_id != digest after rejected mutation", i, bad)
		}
	}
	// by-construction leg: every row AppendBatch writes satisfies record_id == digest AND sha256-of-canonical-bytes == record_id
	path := seedStore(t)
	raw := openRaw(t, path)
	rows, err := raw.Query(`SELECT record_id, digest, canonical_bytes FROM records`)
	if err != nil {
		t.Fatalf("read back records: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id, digest string
		var canonicalBytes []byte
		if err := rows.Scan(&id, &digest, &canonicalBytes); err != nil {
			t.Fatalf("scan record row: %v", err)
		}
		if id != digest {
			t.Errorf("row %d: record_id %s != digest %s", n, id, digest)
		}
		sum := sha256.Sum256(canonicalBytes)
		if want := "sha256:" + hex.EncodeToString(sum[:]); id != want {
			t.Errorf("row %d: record_id %s != sha256-of-canonical-bytes %s", n, id, want)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate records: %v", err)
	}
	if n < 2 {
		t.Fatalf("seeded store holds %d records, want the root witness record plus the appended record", n)
	}
}
