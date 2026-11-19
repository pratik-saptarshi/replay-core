package conformance_test

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The Phase 1 gate (plan Task 17, T-GATE-P1): one synthetic run
// round-trips through all three Phase 1 features in a temp dir —
// trace store (F2), cuts/slices/projections/epochs (F3), snapshot
// store (F4) — discharging AC-GATE-1, and re-discharging AC-F2-6
// (concurrent appends) and AC-F2-7 (GC survival) in assembled form:
// the concurrent appenders contend on the gate run's own store
// file, and garbage collection runs against the gate run's own
// published cut, whose slice must survive it byte-for-byte.
//
// The step-7 appenders are child processes re-executing this test
// binary. Unlike the store package's helpers, the child entry
// point is TestPhase1Gate itself, gated on an environment
// variable: a separate helper test would report SKIP in ordinary
// runs (the store helpers do, by design), and both gate scripts
// scan this package for zero skips — Phase 0's gate included. In
// child mode the function appends and returns; any failure exits
// the process non-zero, which the parent reports as a gate
// failure.

// gateConcurrentPath is the second owner path the step-7 appender
// processes contend on.
const gateConcurrentPath = "runs/concurrent"

const (
	gateAppenderProcesses = 2
	gateAppenderBatches   = 10
	gateAppenderBatchSize = 2
)

// gateRootFields extracts the witness fields object — the body
// field of record.RootWitnessBody() (plan Decision 4(a)).
func gateRootFields() (canonical.Value, bool) {
	for _, p := range record.RootWitnessBody().Pairs {
		if p.Key == "body" {
			return p.Value, true
		}
	}
	return canonical.Value{}, false
}

// gateDraft builds a capture-mode test.fact RecordDraft from a
// JSON body citing causedBy, witnessed by the root witness fields.
func gateDraft(t *testing.T, bodyJSON string, causedBy []string) store.RecordDraft {
	t.Helper()
	body, err := canonical.FromJSON([]byte(bodyJSON))
	if err != nil {
		t.Fatalf("FromJSON(%s): %v", bodyJSON, err)
	}
	fields, ok := gateRootFields()
	if !ok {
		t.Fatal("root witness body has no body field")
	}
	return store.RecordDraft{
		SchemaRef:        "test.fact",
		Mode:             record.ModeCapture,
		Body:             body,
		CausedBy:         causedBy,
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      fields,
	}
}

// gateDraftID computes the record ID a draft will land under, so a
// batch can cite its own earlier records.
func gateDraftID(t *testing.T, d store.RecordDraft) string {
	t.Helper()
	fields, ok := gateRootFields()
	if !ok {
		t.Fatal("root witness body has no body field")
	}
	wd, err := record.WitnessBodyDigest(d.WitnessSchemaRef, fields)
	if err != nil {
		t.Fatalf("WitnessBodyDigest: %v", err)
	}
	id, err := record.RecordDigest(d.SchemaRef, d.Mode, d.Body, d.CausedBy, wd)
	if err != nil {
		t.Fatalf("RecordDigest: %v", err)
	}
	return id
}

// gateAppendFact appends one test.fact record to path and returns
// its record ID.
func gateAppendFact(t *testing.T, s *store.Store, path, bodyJSON string, causedBy []string) string {
	t.Helper()
	r, err := s.AppendFact(path, gateDraft(t, bodyJSON, causedBy))
	if err != nil {
		t.Fatalf("AppendFact(%s, %s): %v", path, bodyJSON, err)
	}
	return r.RecordID
}

// gateModeOf returns a stored record's mode field.
func gateModeOf(rec store.StoredRecord) string {
	for _, p := range rec.Record.Pairs {
		if p.Key == "mode" {
			return p.Value.Str
		}
	}
	return ""
}

// gateIDSet builds a membership set from record IDs.
func gateIDSet(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// gateAppenderMain is the child-process body for step 7: it opens
// the gate run's store at REPLAYCORE_GATE_STORE_PATH and appends
// gateAppenderBatches batches of gateAppenderBatchSize records to
// gateConcurrentPath, with intent IDs gate-app<id>-<i>. Any error
// — a caller-visible locking error included, per AC-F2-6's terms —
// exits the process non-zero.
func gateAppenderMain(idStr string) {
	fail := func(code int, format string, args ...any) {
		fmt.Fprintf(os.Stderr, "gate appender %s: %s\n", idStr, fmt.Sprintf(format, args...))
		os.Exit(code)
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		fail(2, "bad appender id: %v", err)
	}
	// Release barrier: wait for the parent's go file so both
	// children release their first appends together instead of
	// whenever process startup happens to land.
	if goFile := os.Getenv("REPLAYCORE_GATE_GO_FILE"); goFile != "" {
		deadline := time.Now().Add(60 * time.Second)
		for {
			if _, err := os.Stat(goFile); err == nil {
				break
			}
			if time.Now().After(deadline) {
				fail(2, "parent never released the appenders")
			}
			time.Sleep(time.Millisecond)
		}
	}
	fields, ok := gateRootFields()
	if !ok {
		fail(2, "root witness body has no body field")
	}
	witnessDigest, err := record.WitnessBodyDigest(record.RootWitnessSchemaRef, fields)
	if err != nil {
		fail(2, "witness digest: %v", err)
	}
	s, err := store.Open(os.Getenv("REPLAYCORE_GATE_STORE_PATH"))
	if err != nil {
		fail(2, "open: %v", err)
	}
	defer s.Close()
	rootID := record.RootWitnessRecordID()
	mkDraft := func(bodyJSON string, causedBy []string) store.RecordDraft {
		body, err := canonical.FromJSON([]byte(bodyJSON))
		if err != nil {
			fail(2, "body %s: %v", bodyJSON, err)
		}
		return store.RecordDraft{
			SchemaRef:        "test.fact",
			Mode:             record.ModeCapture,
			Body:             body,
			CausedBy:         causedBy,
			WitnessSchemaRef: record.RootWitnessSchemaRef,
			WitnessBody:      fields,
		}
	}
	for i := 0; i < gateAppenderBatches; i++ {
		first := mkDraft(fmt.Sprintf(`{"appender":%d,"batch":%d,"n":0}`, id, i), []string{rootID})
		firstID, err := record.RecordDigest(first.SchemaRef, first.Mode, first.Body, first.CausedBy, witnessDigest)
		if err != nil {
			fail(2, "digest: %v", err)
		}
		second := mkDraft(fmt.Sprintf(`{"appender":%d,"batch":%d,"n":1}`, id, i), []string{firstID})
		if _, err := s.AppendBatch(gateConcurrentPath, fmt.Sprintf("gate-app%d-%d", id, i),
			[]store.RecordDraft{first, second}); err != nil {
			fail(3, "batch %d: %v", i, err)
		}
	}
}

func TestPhase1Gate(t *testing.T) { // AC-GATE-1; AC-F2-6, AC-F2-7 assembled
	// Child-process mode for step 7 (see the file comment): no
	// skip, no separate helper test.
	if idStr := os.Getenv("REPLAYCORE_GATE_APPENDER_ID"); idStr != "" {
		gateAppenderMain(idStr)
		return
	}

	dir := t.TempDir()
	const path = "runs/gate"
	rootID := record.RootWitnessRecordID()

	// Step 1 — workspace: 3 files + a subdirectory; snapshot
	// store + trace store opened.
	ws := filepath.Join(dir, "workspace")
	for name, contents := range map[string]string{
		"alpha.txt":     "alpha\n",
		"beta.txt":      "beta\n",
		"gamma.txt":     "gamma\n",
		"sub/delta.txt": "delta\n",
	} {
		full := filepath.Join(ws, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	snapStore, err := snapshot.Open(filepath.Join(dir, "snapshots.git"))
	if err != nil {
		t.Fatalf("snapshot.Open: %v", err)
	}
	tracePath := filepath.Join(dir, "trace.db")
	ts, err := store.Open(tracePath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	closed := false
	closeTS := func() {
		if !closed {
			if err := ts.Close(); err != nil {
				t.Errorf("trace store Close: %v", err)
			}
			closed = true
		}
	}
	t.Cleanup(closeTS)

	// Step 2 — run records appended: start batch (2 records),
	// step batch (3 records).
	start1 := gateDraft(t, `{"n":"start-1"}`, []string{rootID})
	start2 := gateDraft(t, `{"n":"start-2"}`, []string{gateDraftID(t, start1)})
	if _, err := ts.AppendBatch(path, "gate-start", []store.RecordDraft{start1, start2}); err != nil {
		t.Fatalf("AppendBatch start: %v", err)
	}
	s1, s2 := gateDraftID(t, start1), gateDraftID(t, start2)
	step1 := gateDraft(t, `{"n":"step-1"}`, []string{s2})
	step2 := gateDraft(t, `{"n":"step-2"}`, []string{gateDraftID(t, step1)})
	step3 := gateDraft(t, `{"n":"step-3"}`, []string{gateDraftID(t, step2)})
	if _, err := ts.AppendBatch(path, "gate-steps", []store.RecordDraft{step1, step2, step3}); err != nil {
		t.Fatalf("AppendBatch steps: %v", err)
	}
	st1, st2, st3 := gateDraftID(t, step1), gateDraftID(t, step2), gateDraftID(t, step3)

	// Step 3 — snap1 := Snapshot(ws, {"run": "gate"}); milestone
	// M1 citing snap1 (task "gate-run").
	snap1, err := snapStore.Snapshot(ws, map[string]string{"run": "gate"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	m1, err := ts.AppendMilestone(path, "gate-run", snap1, []string{st3})
	if err != nil {
		t.Fatalf("AppendMilestone M1: %v", err)
	}

	// Step 4 — more step records; an experiment side-branch (2
	// records citing a step record, cited by nothing).
	c1 := gateAppendFact(t, ts, path, `{"n":"step-4"}`, []string{m1.RecordID})
	c2 := gateAppendFact(t, ts, path, `{"n":"step-5"}`, []string{c1})
	x1 := gateAppendFact(t, ts, path, `{"n":"experiment-1"}`, []string{st2})
	x2 := gateAppendFact(t, ts, path, `{"n":"experiment-2"}`, []string{x1})

	// Step 5 — an abandoned draft record + Abandon fact over it.
	// The run then continues: its final step record supersedes
	// the draft, citing both the abandon fact and the draft, so
	// the closing milestone in step 6 causally follows both —
	// and step 10's projection leg can exercise the abandon law
	// against records that step 8's collection retained.
	d1 := gateAppendFact(t, ts, path, `{"n":"draft"}`, []string{c2})
	abandon, err := slice.Abandon(ts, path, d1, []string{c2})
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	f1 := gateAppendFact(t, ts, path, `{"n":"step-final"}`, []string{abandon.RecordID, d1})

	// Step 6 — cut := PublishCut(path, M1); slice1 :=
	// ResolveCut(cut, nil); digest1 := slice1.Digest().
	cutID, err := slice.PublishCut(ts, path, m1.RecordID)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	// A second milestone, M2, closes the run, citing its final
	// step record. Step 10's epoch is defined over two
	// milestones, and M2 must be appended before step 8's
	// collection: as a milestone it is a retention root, and its
	// ancestry is what keeps the run's post-M1 records (the
	// step-4 chain, the draft, the abandon fact, the final step)
	// reachable through GC — AC-F2-7's retention law, exercised
	// in assembled form. (The cut's own ancestry reaches only
	// M1; without M2 those records would be unreachable and
	// collected, exactly as the law prescribes.)
	m2, err := ts.AppendMilestone(path, "gate-run", snap1, []string{f1})
	if err != nil {
		t.Fatalf("AppendMilestone M2: %v", err)
	}
	slice1, err := slice.ResolveCut(ts, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut (pre-GC): %v", err)
	}
	digest1, err := slice1.Digest()
	if err != nil {
		t.Fatalf("slice1.Digest: %v", err)
	}
	order1 := slice1.Order()
	want1 := gateIDSet([]string{rootID, s1, s2, st1, st2, st3, m1.RecordID})
	if len(order1) != len(want1) {
		t.Fatalf("slice1 holds %d records, want %d (root, start batch, step batch, M1)", len(order1), len(want1))
	}
	for _, id := range order1 {
		if !want1[id] {
			t.Errorf("slice1 contains unexpected record %s", id)
		}
	}

	// Step 7 — two appender processes x 10 batches onto a second
	// path (the AC-F2-6 shape, assembled). The parent's handle is
	// closed first — the Task 6 shape: children contend on the
	// file itself, and the parent reopens to audit the result.
	closeTS()
	goFile := filepath.Join(dir, "go")
	cmds := make([]*exec.Cmd, gateAppenderProcesses)
	outs := make([]*bytes.Buffer, gateAppenderProcesses)
	for i := 0; i < gateAppenderProcesses; i++ {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestPhase1Gate$")
		buf := &bytes.Buffer{}
		cmd.Stdout = buf
		cmd.Stderr = buf
		cmd.Env = append(os.Environ(),
			"REPLAYCORE_GATE_APPENDER_ID="+strconv.Itoa(i),
			"REPLAYCORE_GATE_STORE_PATH="+tracePath,
			"REPLAYCORE_GATE_GO_FILE="+goFile,
		)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start appender %d: %v", i, err)
		}
		cmds[i] = cmd
		outs[i] = buf
	}
	if err := os.WriteFile(goFile, []byte("go"), 0o644); err != nil {
		t.Fatalf("release appenders: %v", err)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("appender %d failed: %v\n%s", i, err, outs[i])
		}
	}
	ts, err = store.Open(tracePath)
	if err != nil {
		t.Fatalf("store.Open (reopen after appenders): %v", err)
	}
	closed = false
	conc, err := ts.PathRecords(gateConcurrentPath)
	if err != nil {
		t.Fatalf("PathRecords(%q): %v", gateConcurrentPath, err)
	}
	wantConc := gateAppenderProcesses * gateAppenderBatches * gateAppenderBatchSize
	if len(conc) != wantConc {
		t.Fatalf("concurrent path holds %d records, want %d (no lost batches)", len(conc), wantConc)
	}
	for i, rec := range conc {
		if rec.OwnerOrdinal != int64(i) {
			t.Fatalf("concurrent path ordinal at position %d = %d, want gap-free %d", i, rec.OwnerOrdinal, i)
		}
		if _, err := ts.GetRecord(rec.ID); err != nil {
			t.Fatalf("GetRecord(concurrent %s): %v", rec.ID, err)
		}
	}

	// Step 8 — CollectGarbage(); slice2 := ResolveCut(cut, nil);
	// the slice survives GC unchanged and every sliced record
	// still reads (AC-F2-7, assembled). The unreachable
	// experiment branch is collected — the assembled proof that
	// GC actually ran.
	report, err := ts.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if report.Collected == 0 {
		t.Errorf("CollectGarbage collected 0 records; the unreachable side branch must be collected")
	}
	slice2, err := slice.ResolveCut(ts, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut (post-GC): %v", err)
	}
	digest2, err := slice2.Digest()
	if err != nil {
		t.Fatalf("slice2.Digest: %v", err)
	}
	if digest2 != digest1 {
		t.Errorf("slice digest changed across GC: pre %s, post %s", digest1, digest2)
	}
	order2 := slice2.Order()
	if len(order2) != len(order1) {
		t.Fatalf("slice2 holds %d records, want %d (slice1's count)", len(order2), len(order1))
	}
	for i := range order1 {
		if order2[i] != order1[i] {
			t.Errorf("slice order changed across GC at position %d: %s -> %s", i, order1[i], order2[i])
		}
	}
	for _, id := range order1 {
		if _, err := ts.GetRecord(id); err != nil {
			t.Errorf("GetRecord(%s) after GC: %v — every sliced record must still read", id, err)
		}
	}
	for _, id := range []string{x1, x2} {
		if _, err := ts.GetRecord(id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetRecord(side-branch %s) after GC: err = %v, want ErrNotFound (collected)", id, err)
		}
	}

	// Step 9 — SnapshotAt(traceStore, cut) == snap1: the cut
	// terminates at M1, whose citation names snap1.
	at, err := snapshot.SnapshotAt(ts, cutID)
	if err != nil {
		t.Fatalf("SnapshotAt: %v", err)
	}
	if at != snap1 {
		t.Errorf("SnapshotAt(cut) = %s, want snap1 %s", at, snap1)
	}

	// Step 10 — ProjectEffectiveHistory excludes the abandoned
	// record; EpochSlice + EffectAt walk the M1 epoch, (M1, M2]
	// over the milestone appended in step 6.
	epoch, err := slice.EpochSlice(ts, path, m1.RecordID, m2.RecordID)
	if err != nil {
		t.Fatalf("EpochSlice(M1, M2): %v", err)
	}
	epochSet := gateIDSet(epoch.Order())
	for _, id := range []string{c1, c2, f1, m2.RecordID} {
		if !epochSet[id] {
			t.Errorf("epoch (M1, M2] is missing record %s", id)
		}
	}
	for _, id := range []string{m1.RecordID, st3} {
		if epochSet[id] {
			t.Errorf("epoch (M1, M2] contains %s, which belongs to M1's own prefix", id)
		}
	}
	// The EffectAt walk: position by position, a capture record
	// answers its own digest and a declaration answers
	// ("", false) — the positional cursor contract (AC-F3-6),
	// exercised over the epoch. The walk must not be vacuous:
	// the epoch holds both modes.
	captures, declarations := 0, 0
	for i, rec := range epoch.Records {
		id, ok := epoch.EffectAt(i)
		switch gateModeOf(rec) {
		case record.ModeCapture:
			if !ok || id != rec.ID {
				t.Errorf("EffectAt(%d) = (%q, %v), want (%q, true) for the capture record at that position", i, id, ok, rec.ID)
			}
			captures++
		default:
			if ok || id != "" {
				t.Errorf("EffectAt(%d) = (%q, %v), want (\"\", false) for the declaration record at that position", i, id, ok)
			}
			declarations++
		}
	}
	if captures == 0 || declarations == 0 {
		t.Errorf("epoch EffectAt walk is vacuous: %d capture positions, %d declaration positions", captures, declarations)
	}
	if id, ok := epoch.EffectAt(-1); ok || id != "" {
		t.Errorf("EffectAt(-1) = (%q, %v), want (\"\", false)", id, ok)
	}
	if id, ok := epoch.EffectAt(len(epoch.Records)); ok || id != "" {
		t.Errorf("EffectAt(len) = (%q, %v), want (\"\", false)", id, ok)
	}

	proj, err := slice.ProjectEffectiveHistory(ts, path)
	if err != nil {
		t.Fatalf("ProjectEffectiveHistory: %v", err)
	}
	projSet := gateIDSet(proj.Order())
	if projSet[d1] {
		t.Errorf("projection contains the abandoned draft %s", d1)
	}
	if projSet[f1] {
		t.Errorf("projection contains %s, a descendant of the abandoned draft", f1)
	}
	for _, id := range []string{m1.RecordID, c1, c2, abandon.RecordID} {
		if !projSet[id] {
			t.Errorf("projection is missing %s, which no abandon touches", id)
		}
	}
	if proj.CutID != "" {
		t.Errorf("projection CutID = %q, want %q", proj.CutID, "")
	}
	if proj.Frontier != m2.RecordID {
		t.Errorf("projection Frontier = %q, want the path tail %q", proj.Frontier, m2.RecordID)
	}
	// Projection, never deletion (spec §5): the abandoned draft's
	// bytes remain in the store, readable by ID.
	if _, err := ts.GetRecord(d1); err != nil {
		t.Errorf("GetRecord(abandoned draft) after projection: %v — abandoned bytes must remain readable", err)
	}

	// Step 11 — byte-fidelity close: re-encode every record in
	// slice2 via canonical.MarshalKernel and assert the bytes
	// equal the store's stored canonical bytes for that record.
	raw, err := sql.Open("sqlite3", "file:"+tracePath)
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close raw handle: %v", err)
		}
	}()
	for _, rec := range slice2.Records {
		var stored []byte
		if err := raw.QueryRow(
			"SELECT canonical_bytes FROM records WHERE record_id = ?", rec.ID,
		).Scan(&stored); err != nil {
			t.Fatalf("read stored bytes for %s: %v", rec.ID, err)
		}
		reencoded, err := canonical.MarshalKernel(rec.Record)
		if err != nil {
			t.Fatalf("MarshalKernel(%s): %v", rec.ID, err)
		}
		if !bytes.Equal(reencoded, stored) {
			t.Errorf("record %s: re-encoded bytes differ from the store's stored canonical bytes", rec.ID)
		}
	}
}
