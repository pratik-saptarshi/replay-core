package slice_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The ReadCut cross-check suite (issue #6): a cut body carries
// placement claims — owner_path and through_owner_ordinal — that
// MintCut copied from the through record's stored placement at
// publish time. ReadCut used to return those claims unchecked,
// so a cut whose claims disagree with the through record's
// stored placement (owner path/ordinal live in bookkeeping
// columns outside the record digest) resolved a slice over the
// wrong prefix. ReadCut now fetches the through record and
// requires both claims to match its stored placement.
//
// The tamper below moves the through record's stored placement
// with direct SQL after the store is closed — placement is
// bookkeeping, so the cut record itself stays byte-identical
// and digest-valid; only the cross-check can catch it.

// seedCutFixture writes two facts on runs/x, publishes a cut
// through the second (stored ordinal 1), closes the store, and
// returns the store path, the cut ID, and the through-fact ID.
func seedCutFixture(t *testing.T) (string, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rootID := record.RootWitnessRecordID()
	appendFact(t, s, "runs/x", `{"n":"f1"}`, []string{rootID})
	through := appendFact(t, s, "runs/x", `{"n":"f2"}`, []string{rootID})
	cutID, err := slice.PublishCut(s, "runs/x", through)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, cutID, through
}

// rawStoreExec runs one statement against the store file through
// a raw handle, bypassing the store package entirely.
func rawStoreExec(t *testing.T, path, query string, args ...any) {
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

func TestReadCutCrossChecksThroughPlacement(t *testing.T) {
	t.Run("untampered cut reads fine", func(t *testing.T) {
		path, cutID, through := seedCutFixture(t)
		s, err := store.Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s.Close()
		cut, err := slice.ReadCut(s, cutID)
		if err != nil {
			t.Fatalf("ReadCut: %v", err)
		}
		if cut.OwnerPath != "runs/x" || cut.ThroughFactID != through || cut.ThroughOrdinal != 1 {
			t.Fatalf("ReadCut = %+v, want {OwnerPath:runs/x ThroughFactID:%s ThroughOrdinal:1}", cut, through)
		}
	})

	t.Run("stored ordinal moved", func(t *testing.T) {
		path, cutID, through := seedCutFixture(t)
		rawStoreExec(t, path, "UPDATE records SET owner_ordinal = 99 WHERE record_id = ?", through)
		s, err := store.Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s.Close()
		_, err = slice.ReadCut(s, cutID)
		if err == nil {
			t.Fatal("ReadCut accepted a cut whose body claims ordinal 1 for a through-fact stored at ordinal 99")
		}
		if !strings.Contains(err.Error(), "ordinal") {
			t.Fatalf("ReadCut error = %q, want it to name the ordinal mismatch", err)
		}
	})

	t.Run("stored owner path moved", func(t *testing.T) {
		path, cutID, through := seedCutFixture(t)
		rawStoreExec(t, path, "UPDATE records SET owner_path = 'runs/other' WHERE record_id = ?", through)
		s, err := store.Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s.Close()
		_, err = slice.ReadCut(s, cutID)
		if err == nil {
			t.Fatal("ReadCut accepted a cut whose body claims runs/x for a through-fact stored on runs/other")
		}
		if !strings.Contains(err.Error(), "owner path") {
			t.Fatalf("ReadCut error = %q, want it to name the owner-path mismatch", err)
		}
	})
}
