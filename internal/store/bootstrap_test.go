package store_test

// Concurrent first-open proof (Codex P2 on PR #2). Open used to
// apply the schema through the connection pool and then run
// bootstrapRoot's existence check outside any transaction: two
// processes opening the same fresh store path raced — one could
// fail applying the schema ("database is locked") while the
// other held the file mid-initialization, and two openers that
// both observed "no root yet" collided on the root record's
// uniqueness constraint. The pre-existing concurrency proofs
// never exercised this: createStoreFile initializes the store
// before any race starts, precisely to keep bootstrap out of
// the append races. Here the race IS the bootstrap: the store
// file does not exist until the children create it.
//
// The child helper returns silently when its env flag is unset
// instead of calling t.Skip: scripts/check-phase1.sh tolerates
// exactly two named SKIP lines in this package (Task 6's
// helpers), so a new skipping helper would fail the gate. This
// follows commit 1's gc_contention_test.go pattern.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// TestBootstrapOpenerProcess is the re-exec helper for
// TestConcurrentFirstOpen: it acts only when
// REPLAYCORE_BOOTSTRAP_OPENER_ID is set (see the file header
// for why it does not t.Skip otherwise). As a child it waits
// for the shared start barrier, then opens the store at
// REPLAYCORE_STORE_PATH — a path no process has initialized —
// and asserts the root witness record is already readable: an
// opener that proceeds at all must observe a fully initialized
// store. Any Open error exits non-zero, whichever step of
// first-open produced it.
func TestBootstrapOpenerProcess(t *testing.T) {
	idStr := os.Getenv("REPLAYCORE_BOOTSTRAP_OPENER_ID")
	if idStr == "" {
		return
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		os.Exit(2)
	}
	if goFile := os.Getenv("REPLAYCORE_GO_FILE"); goFile != "" {
		waitForGoFile(goFile)
	}
	s, err := store.Open(os.Getenv("REPLAYCORE_STORE_PATH"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap opener %d: open: %v\n", id, err)
		os.Exit(2)
	}
	defer s.Close()
	if _, err := s.GetRecord(record.RootWitnessRecordID()); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap opener %d: root not readable after open: %v\n", id, err)
		os.Exit(3)
	}
}

// TestConcurrentFirstOpen races several processes opening the
// same fresh store path behind a start barrier. Every opener's
// Open must succeed; afterward the store holds exactly one
// root witness record and accepts appends.
func TestConcurrentFirstOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.db")
	goFile := filepath.Join(dir, "go")

	const children = 6
	spawnChildren(t, "TestBootstrapOpenerProcess", children, goFile, func(i int) []string {
		return []string{
			"REPLAYCORE_BOOTSTRAP_OPENER_ID=" + strconv.Itoa(i),
			"REPLAYCORE_STORE_PATH=" + path,
			"REPLAYCORE_GO_FILE=" + goFile,
		}
	})

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open after race: %v", err)
	}
	defer s.Close()
	sysRecs, err := s.PathRecords(store.SystemPath)
	if err != nil {
		t.Fatalf("PathRecords(system): %v", err)
	}
	if len(sysRecs) != 1 || sysRecs[0].ID != record.RootWitnessRecordID() {
		t.Fatalf("system path after race = %v records, want exactly the root witness record", len(sysRecs))
	}
	if _, err := s.AppendFact("runs/bootstrap", draftCiting(t, "test.fact", `{"n":"post-race"}`, []string{record.RootWitnessRecordID()})); err != nil {
		t.Fatalf("append after race: %v", err)
	}
}
