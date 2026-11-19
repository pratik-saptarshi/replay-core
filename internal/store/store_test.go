package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// openTemp opens a store on a fresh file under t.TempDir and closes
// it at test cleanup.
func openTemp(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// rootFields extracts the witness fields object — the body field of
// record.RootWitnessBody() — since Phase 0 exports the fields only in
// that wrapped form (plan Decision 4(a)).
func rootFields(t *testing.T) canonical.Value {
	t.Helper()
	for _, p := range record.RootWitnessBody().Pairs {
		if p.Key == "body" {
			return p.Value
		}
	}
	t.Fatal("root witness body has no body field")
	return canonical.Value{}
}

// rootWitnessDigest is the digest of the root witness fields under
// the root witness schema.
func rootWitnessDigest(t *testing.T) string {
	t.Helper()
	d, err := record.WitnessBodyDigest(record.RootWitnessSchemaRef, rootFields(t))
	if err != nil {
		t.Fatalf("WitnessBodyDigest: %v", err)
	}
	return d
}

// TestStoreOpensInWALMode is the regression pin for the defect
// Task 6's concurrent-append proof found: the store's DSN was
// written with _pragma= parameters, which mattn/go-sqlite3
// silently ignores, so the database actually ran in the default
// delete journal mode with the driver's built-in busy_timeout of
// 5000 — not WAL with the configured timeout. Journal mode
// persists in the database file, so a fresh raw connection can
// audit it after Close. (The timeout half of the fix is pinned
// behaviorally by TestConcurrentAppendsAcrossProcesses: at a
// 5000ms ceiling, 4-way contention surfaces caller-visible
// "database is locked" errors in this environment.)
func TestStoreOpensInWALMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer raw.Close()
	var jm string
	if err := raw.QueryRow("PRAGMA journal_mode").Scan(&jm); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if jm != "wal" {
		t.Fatalf("journal_mode = %s, want wal (the store DSN must use the driver's named parameters)", jm)
	}
}

func TestStoreBootstrapRootWitness(t *testing.T) {
	s := openTemp(t)
	rec, err := s.GetRecord(record.RootWitnessRecordID())
	if err != nil {
		t.Fatalf("root witness record missing after Open: %v", err)
	}
	if rec.ID != "sha256:0b67497cb4c0f2ed954b7988ecc321b481e7d173c4fc201adeb86591b2fd6310" {
		t.Fatalf("root ID = %s", rec.ID)
	}
	tail, err := s.Frontier("runs/a")
	if err != nil || tail != rec.ID {
		t.Fatalf("empty path frontier = %q, %v; want root ID", tail, err)
	}
}
