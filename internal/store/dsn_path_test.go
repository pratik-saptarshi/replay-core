package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The DSN tests: Open builds the driver's file: URI from the
// caller's path, and on Unix '#', '?' and '%' are valid filename
// characters but URI syntax — '#' opens a fragment, '?' opens the
// query string, and a valid %XX sequence is decoded by the URI
// parser. Concatenating the path unescaped therefore opens a
// DIFFERENT file than the caller named: trace#one.db and
// trace#two.db both land on the file "trace", silently mixing two
// stores the caller meant to keep separate. Open must escape the
// path so it is treated as a literal filesystem path.

// openAtPath opens a store at an exact path and closes it at test
// cleanup.
func openAtPath(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close(%q): %v", path, err)
		}
	})
	return s
}

// appendFactID appends one test.fact record to ownerPath and
// returns its record ID.
func appendFactID(t *testing.T, s *store.Store, ownerPath, bodyJSON string) string {
	t.Helper()
	r, err := s.AppendFact(ownerPath, draft(t, "test.fact", bodyJSON))
	if err != nil {
		t.Fatalf("AppendFact(%q, %s): %v", ownerPath, bodyJSON, err)
	}
	return r.RecordID
}

func TestOpenDistinguishesFragmentPaths(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "trace#one.db")
	two := filepath.Join(dir, "trace#two.db")

	s1 := openAtPath(t, one)
	factID := appendFactID(t, s1, "runs/a", `{"n":"one"}`)
	s2 := openAtPath(t, two)

	// Both stores exist on disk under their exact names, and no
	// truncated "trace" file was created in their place.
	for _, p := range []string{one, two} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("store file %q: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "trace")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a truncated store file %q exists (stat err = %v): the '#' fragment swallowed the rest of both names", filepath.Join(dir, "trace"), err)
	}

	// The second store does not see the first store's fact: they
	// are separate stores, not two handles on one file.
	if _, err := s2.GetRecord(factID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second store GetRecord(%s) err = %v, want ErrNotFound — the two stores share one file", factID, err)
	}
	recs2, err := s2.PathRecords("runs/a")
	if err != nil {
		t.Fatalf("second store PathRecords: %v", err)
	}
	if len(recs2) != 0 {
		t.Fatalf("second store sees %d records on runs/a, want 0", len(recs2))
	}

	// The first store still sees exactly its own fact.
	recs1, err := s1.PathRecords("runs/a")
	if err != nil {
		t.Fatalf("first store PathRecords: %v", err)
	}
	if len(recs1) != 1 || recs1[0].ID != factID {
		t.Fatalf("first store PathRecords = %v, want exactly [%s]", recs1, factID)
	}
}

func TestOpenRoundTripsURISignificantNames(t *testing.T) {
	// "trace?q.db": '?' opens the URI query string, so the
	// unescaped path opens the file "trace".
	// "trace%23.db": %23 is a valid escape, so the URI parser
	// decodes the name to "trace#.db" — a different file. (A '%'
	// followed by non-hex characters is left alone by the
	// decoder, so only a valid escape sequence discriminates.)
	for _, name := range []string{"trace?q.db", "trace%23.db"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)

			s, err := store.Open(path)
			if err != nil {
				t.Fatalf("Open(%q): %v", path, err)
			}
			factID := appendFactID(t, s, "runs/a", `{"n":"x"}`)
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// The store file exists under the exact name given.
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("store file %q: %v", path, err)
			}

			// Reopening the same path sees the appended fact:
			// the name round-trips to the same file.
			s2, err := store.Open(path)
			if err != nil {
				t.Fatalf("reopen %q: %v", path, err)
			}
			defer s2.Close()
			if _, err := s2.GetRecord(factID); err != nil {
				t.Fatalf("reopened store GetRecord(%s): %v", factID, err)
			}
		})
	}
}
