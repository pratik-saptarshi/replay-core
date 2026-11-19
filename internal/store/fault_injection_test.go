package store

// Fault-injection tests for the atomic write core. Both tests use
// the child-process pattern: the test binary re-executes itself with
// an env flag set, the child performs the fault (hard exit mid-batch,
// or exit without Close), and the parent reopens the store file and
// asserts what survived.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
)

// childDraft builds a draft with the root witness fields as witness
// material, without a *testing.T (child processes exit directly on
// error).
func childDraft(n int) RecordDraft {
	body, err := canonical.FromJSON([]byte(fmt.Sprintf(`{"n":%d}`, n)))
	if err != nil {
		os.Exit(2)
	}
	var fields canonical.Value
	for _, p := range record.RootWitnessBody().Pairs {
		if p.Key == "body" {
			fields = p.Value
		}
	}
	return RecordDraft{
		SchemaRef:        "test.fact",
		Mode:             record.ModeCapture,
		Body:             body,
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      fields,
	}
}

// TestAppendBatchCrashMidBatchLeavesNothing (AC-F2-1): the child
// opens the store at REPLAYCORE_STORE_PATH, arms appendCrashHook to
// os.Exit(3) on the Nth "insert" stage, and appends a 6-record
// batch. The parent expects exit 3, reopens the store, and asserts
// the batch left nothing behind: no records on the path and the
// frontier still at the root witness record.
func TestAppendBatchCrashMidBatchLeavesNothing(t *testing.T) {
	if n := os.Getenv("REPLAYCORE_CRASH_AT"); n != "" {
		limit, err := strconv.Atoi(n)
		if err != nil {
			os.Exit(2)
		}
		s, err := Open(os.Getenv("REPLAYCORE_STORE_PATH"))
		if err != nil {
			os.Exit(2)
		}
		inserts := 0
		appendCrashHook = func(stage string) {
			if stage != "insert" {
				return
			}
			inserts++
			if inserts >= limit {
				os.Exit(3)
			}
		}
		drafts := make([]RecordDraft, 6)
		for i := range drafts {
			drafts[i] = childDraft(i + 1)
		}
		_, _ = s.AppendBatch("runs/crash", "crash-1", drafts)
		os.Exit(0) // hook never fired: the crash did not happen
	}

	path := filepath.Join(t.TempDir(), "trace.db")
	cmd := exec.Command(os.Args[0], "-test.run", "^TestAppendBatchCrashMidBatchLeavesNothing$")
	cmd.Env = append(os.Environ(),
		"REPLAYCORE_CRASH_AT=3",
		"REPLAYCORE_STORE_PATH="+path,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("child exit = %v, want exit code 3\n%s", err, out)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer s.Close()
	recs, err := s.PathRecords("runs/crash")
	if err != nil {
		t.Fatalf("PathRecords: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("crashed batch left %d records; want 0 (atomicity)", len(recs))
	}
	tail, err := s.Frontier("runs/crash")
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}
	if tail != record.RootWitnessRecordID() {
		t.Fatalf("frontier after crash = %s; want root ID %s", tail, record.RootWitnessRecordID())
	}
}

// TestStoreReopenAfterUncleanClose (Review Focus 1): the child
// appends a 3-record batch and os.Exit(0)s without Close, leaving
// the WAL behind. The parent reopens and asserts every record is
// readable, the frontier sits at the last receipt, and the next
// append continues ordinals without reuse.
func TestStoreReopenAfterUncleanClose(t *testing.T) {
	if os.Getenv("REPLAYCORE_UNCLEAN_CHILD") != "" {
		s, err := Open(os.Getenv("REPLAYCORE_STORE_PATH"))
		if err != nil {
			os.Exit(2)
		}
		drafts := []RecordDraft{childDraft(1), childDraft(2), childDraft(3)}
		if _, err := s.AppendBatch("runs/a", "u-1", drafts); err != nil {
			os.Exit(2)
		}
		os.Exit(0) // unclean shutdown: no Close
	}

	path := filepath.Join(t.TempDir(), "trace.db")
	cmd := exec.Command(os.Args[0], "-test.run", "^TestStoreReopenAfterUncleanClose$")
	cmd.Env = append(os.Environ(),
		"REPLAYCORE_UNCLEAN_CHILD=1",
		"REPLAYCORE_STORE_PATH="+path,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after unclean close: %v", err)
	}
	defer s.Close()
	recs, err := s.PathRecords("runs/a")
	if err != nil {
		t.Fatalf("PathRecords: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("PathRecords = %d records; want 3", len(recs))
	}
	for i, r := range recs {
		if r.OwnerOrdinal != int64(i) {
			t.Fatalf("ordinal[%d] = %d; want %d", i, r.OwnerOrdinal, i)
		}
		if _, err := s.GetRecord(r.ID); err != nil {
			t.Fatalf("GetRecord(%s) after reopen: %v", r.ID, err)
		}
	}
	tail, err := s.Frontier("runs/a")
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}
	if tail != recs[2].ID {
		t.Fatalf("frontier after reopen = %s; want last receipt %s", tail, recs[2].ID)
	}
	rc, err := s.AppendFact("runs/a", childDraft(4))
	if err != nil {
		t.Fatalf("AppendFact after reopen: %v", err)
	}
	if rc.OwnerOrdinal != 3 {
		t.Fatalf("ordinal after reopen = %d; want 3 (no reuse)", rc.OwnerOrdinal)
	}
}
