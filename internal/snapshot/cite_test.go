package snapshot_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// openTraceStore opens a trace store on a fresh file under
// t.TempDir and closes it at test cleanup.
func openTraceStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store Close: %v", err)
		}
	})
	return s
}

// traceRootFields extracts the witness fields object — the body
// field of record.RootWitnessBody() (plan Decision 4(a)).
func traceRootFields(t *testing.T) canonical.Value {
	t.Helper()
	for _, p := range record.RootWitnessBody().Pairs {
		if p.Key == "body" {
			return p.Value
		}
	}
	t.Fatal("root witness body has no body field")
	return canonical.Value{}
}

// appendPlainFact appends one capture-mode test.fact record to
// path and returns its record ID.
func appendPlainFact(t *testing.T, s *store.Store, path, bodyJSON string, causedBy []string) string {
	t.Helper()
	body, err := canonical.FromJSON([]byte(bodyJSON))
	if err != nil {
		t.Fatalf("FromJSON(%s): %v", bodyJSON, err)
	}
	r, err := s.AppendFact(path, store.RecordDraft{
		SchemaRef:        "test.fact",
		Mode:             record.ModeCapture,
		Body:             body,
		CausedBy:         causedBy,
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      traceRootFields(t),
	})
	if err != nil {
		t.Fatalf("AppendFact(%s, %s): %v", path, bodyJSON, err)
	}
	return r.RecordID
}

func TestSnapshotAtResolvesCitedCommit(t *testing.T) { // AC-F4-4
	// Workspace v1 -> snap1 = c1; modify workspace -> snap2 = c2.
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "hello.txt"), "v1\n")
	snaps, err := snapshot.Open(filepath.Join(t.TempDir(), "snap.git"))
	if err != nil {
		t.Fatalf("snapshot.Open: %v", err)
	}
	c1, err := snaps.Snapshot(ws, map[string]string{"run": "cite-test"})
	if err != nil {
		t.Fatalf("Snapshot v1: %v", err)
	}
	writeFile(t, filepath.Join(ws, "hello.txt"), "v2\n")
	c2, err := snaps.Snapshot(ws, map[string]string{"run": "cite-test"})
	if err != nil {
		t.Fatalf("Snapshot v2: %v", err)
	}
	if c1 == c2 {
		t.Fatalf("fixture broken: c1 == c2 == %s, want distinct commits", c1)
	}

	// The milestone cites c1; the cut terminates at the milestone.
	trace := openTraceStore(t)
	milestone, err := trace.AppendMilestone("runs/a", "task-1", c1, nil)
	if err != nil {
		t.Fatalf("AppendMilestone: %v", err)
	}
	cut, err := slice.PublishCut(trace, "runs/a", milestone.RecordID)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	got, err := snapshot.SnapshotAt(trace, cut)
	if err != nil {
		t.Fatalf("SnapshotAt: %v", err)
	}
	if got != c1 {
		t.Fatalf("SnapshotAt = %s, want the cited commit %s — not the newer %s", got, c1, c2)
	}
}

func TestSnapshotAtRejectsNonMilestoneCut(t *testing.T) { // AC-F4-4
	trace := openTraceStore(t)
	fact := appendPlainFact(t, trace, "runs/a", `{"n":"plain"}`, []string{record.RootWitnessRecordID()})
	cut, err := slice.PublishCut(trace, "runs/a", fact)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	_, err = snapshot.SnapshotAt(trace, cut)
	if err == nil {
		t.Fatal("SnapshotAt over a cut through a plain fact: got nil error, want a descriptive rejection")
	}
	if !strings.Contains(err.Error(), "milestone") {
		t.Fatalf("SnapshotAt error = %q, want it to name the missing milestone", err)
	}
}
