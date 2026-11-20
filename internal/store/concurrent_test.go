package store_test

// Concurrent-append proof (Task 6, T-F2-5). Two multi-process
// proofs, both using the child-process pattern of the
// fault-injection tests: the test binary re-executes itself with
// env flags set, the children append against one store file, and
// the parent audits the result.
//
//   - TestConcurrentAppendsAcrossProcesses discharges AC-F2-6:
//     4 processes x 25 batches x 4 records on one shared owner path
//     complete with no lost batches, ordinals exactly 0..399, every
//     record digest-valid on read, and every intent receipted.
//   - TestSameIntentRaceAcrossProcesses is the concurrent leg of
//     AC-F2-2 (assigned here by Task 2's report: the sequential
//     legs landed there, the race proof here): racing appends with
//     the same intent ID resolve through AppendBatch's
//     in-transaction intent re-check — identical batches replay to
//     the winner's receipts, different batches fail with
//     *IntentConflictError — never a primary-key error and never a
//     double append.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// appenderDraft builds the draft for (app, batch, slot) in the
// shared-append fixture. Bodies are distinct per (app, batch,
// slot): record identity is content-derived and the owner path is
// not part of it, so a reused body would collide on
// records.record_id instead of exercising append contention.
func appenderDraft(app, batch, slot int) (store.RecordDraft, error) {
	body, err := canonical.FromJSON([]byte(fmt.Sprintf(`{"app":%d,"batch":%d,"slot":%d}`, app, batch, slot)))
	if err != nil {
		return store.RecordDraft{}, err
	}
	var fields canonical.Value
	for _, p := range record.RootWitnessBody().Pairs {
		if p.Key == "body" {
			fields = p.Value
		}
	}
	return store.RecordDraft{
		SchemaRef:        "test.fact",
		Mode:             record.ModeCapture,
		Body:             body,
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      fields,
	}, nil
}

// appenderBatch builds the 4 drafts of one appender batch.
func appenderBatch(app, batch int) ([]store.RecordDraft, error) {
	drafts := make([]store.RecordDraft, 4)
	for slot := range drafts {
		d, err := appenderDraft(app, batch, slot)
		if err != nil {
			return nil, err
		}
		drafts[slot] = d
	}
	return drafts, nil
}

// raceBatch builds the 4 drafts of one same-intent race batch.
// Variant selects the batch content: racers in the identical-batch
// leg all use variant 0; racers in the conflict leg use their own
// variant, so the two batches digest differently under one intent.
// The leg is part of every body: record identity is content-derived
// and global, so batches from different legs must not share content
// or the later leg's inserts would collide with the earlier leg's
// records on records.record_id.
func raceBatch(leg string, variant int) ([]store.RecordDraft, error) {
	drafts := make([]store.RecordDraft, 4)
	for slot := range drafts {
		body, err := canonical.FromJSON([]byte(fmt.Sprintf(`{"leg":%q,"variant":%d,"slot":%d}`, leg, variant, slot)))
		if err != nil {
			return nil, err
		}
		var fields canonical.Value
		for _, p := range record.RootWitnessBody().Pairs {
			if p.Key == "body" {
				fields = p.Value
			}
		}
		drafts[slot] = store.RecordDraft{
			SchemaRef:        "test.fact",
			Mode:             record.ModeCapture,
			Body:             body,
			WitnessSchemaRef: record.RootWitnessSchemaRef,
			WitnessBody:      fields,
		}
	}
	return drafts, nil
}

// waitForGoFile blocks a child process until the parent creates the
// go file, so racing children release their first appends together
// instead of whenever process startup happens to land. A child
// whose parent never releases it exits 2 rather than hanging.
func waitForGoFile(path string) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	os.Exit(2)
}

// TestAppenderProcess is the re-exec helper for
// TestConcurrentAppendsAcrossProcesses: it skips unless
// REPLAYCORE_APPENDER_ID is set. As a child it opens the store at
// REPLAYCORE_STORE_PATH and appends 25 batches x 4 records to the
// shared path runs/shared with intent IDs app<id>-<i>, exiting
// non-zero on any error — a caller-visible locking error included:
// per AC-F2-6's terms that is a failure of the append protocol,
// not an acceptable outcome.
func TestAppenderProcess(t *testing.T) {
	idStr := os.Getenv("REPLAYCORE_APPENDER_ID")
	if idStr == "" {
		t.Skip("child-process helper: runs only under TestConcurrentAppendsAcrossProcesses")
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
		fmt.Fprintf(os.Stderr, "appender %d: open: %v\n", id, err)
		os.Exit(2)
	}
	defer s.Close()
	for i := 0; i < 25; i++ {
		drafts, err := appenderBatch(id, i)
		if err != nil {
			os.Exit(2)
		}
		if _, err := s.AppendBatch("runs/shared", fmt.Sprintf("app%d-%d", id, i), drafts); err != nil {
			fmt.Fprintf(os.Stderr, "appender %d batch %d: %v\n", id, i, err)
			os.Exit(3)
		}
	}
}

// TestRaceProcess is the re-exec helper for
// TestSameIntentRaceAcrossProcesses: it skips unless
// REPLAYCORE_RACE_MODE is set. As a child it appends one batch
// under the shared intent REPLAYCORE_RACE_INTENT to
// REPLAYCORE_RACE_PATH, with content REPLAYCORE_RACE_VARIANT.
// In "same" mode every racer appends the identical batch and must
// succeed (append or replay). In "conflict" mode each racer
// appends its own variant: the winner succeeds and the loser must
// fail with *store.IntentConflictError — any other error, above
// all a primary-key/constraint error, exits non-zero.
func TestRaceProcess(t *testing.T) {
	mode := os.Getenv("REPLAYCORE_RACE_MODE")
	if mode == "" {
		t.Skip("child-process helper: runs only under TestSameIntentRaceAcrossProcesses")
	}
	variant, err := strconv.Atoi(os.Getenv("REPLAYCORE_RACE_VARIANT"))
	if err != nil {
		os.Exit(2)
	}
	if goFile := os.Getenv("REPLAYCORE_GO_FILE"); goFile != "" {
		waitForGoFile(goFile)
	}
	s, err := store.Open(os.Getenv("REPLAYCORE_STORE_PATH"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "racer %s/%d: open: %v\n", mode, variant, err)
		os.Exit(2)
	}
	defer s.Close()
	drafts, err := raceBatch(os.Getenv("REPLAYCORE_RACE_LEG"), variant)
	if err != nil {
		os.Exit(2)
	}
	_, err = s.AppendBatch(os.Getenv("REPLAYCORE_RACE_PATH"), os.Getenv("REPLAYCORE_RACE_INTENT"), drafts)
	switch {
	case err == nil:
		// Winner, or (in "same" mode) a replay of the winner's batch.
	case mode == "conflict" && errors.As(err, new(*store.IntentConflictError)):
		// Loser of the conflict leg: the clean conflict outcome.
	case err != nil && strings.Contains(err.Error(), "constraint"):
		fmt.Fprintf(os.Stderr, "racer %s/%d: constraint error surfaced to caller: %v\n", mode, variant, err)
		os.Exit(4)
	default:
		fmt.Fprintf(os.Stderr, "racer %s/%d: append: %v\n", mode, variant, err)
		os.Exit(3)
	}
}

// spawnChildren starts n children running the named helper test
// with the given extra env, then creates the go file to release
// them together and waits for all of them, failing the test with
// the child's output if any exits non-zero.
func spawnChildren(t *testing.T, helper string, n int, goFile string, envFor func(i int) []string) {
	t.Helper()
	cmds := make([]*exec.Cmd, n)
	outs := make([]*bytes.Buffer, n)
	for i := 0; i < n; i++ {
		cmd := exec.Command(os.Args[0], "-test.run", "^"+helper+"$")
		buf := &bytes.Buffer{}
		cmd.Stdout = buf
		cmd.Stderr = buf
		cmd.Env = append(os.Environ(), envFor(i)...)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child %d (%s): %v", i, helper, err)
		}
		cmds[i] = cmd
		outs[i] = buf
	}
	if err := os.WriteFile(goFile, []byte("go"), 0o644); err != nil {
		t.Fatalf("release children: %v", err)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %d (%s) failed: %v\n%s", i, helper, err, outs[i])
		}
	}
}

// createStoreFile creates the store file (schema and root bootstrap)
// before any race starts: the proofs are about appends to one
// store, and processes racing first-open would contend on the
// bootstrap, not on appends.
func createStoreFile(t *testing.T, path string) {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}
}

// draftIDs computes the record IDs a batch's drafts will land
// under, in draft order.
func draftIDs(t *testing.T, drafts []store.RecordDraft) []string {
	t.Helper()
	ids := make([]string, len(drafts))
	for i, d := range drafts {
		id, err := record.RecordDigest(d.SchemaRef, d.Mode, d.Body, d.CausedBy, rootWitnessDigest(t))
		if err != nil {
			t.Fatalf("RecordDigest draft %d: %v", i, err)
		}
		ids[i] = id
	}
	return ids
}

func TestConcurrentAppendsAcrossProcesses(t *testing.T) { // AC-F2-6
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.db")
	goFile := filepath.Join(dir, "go")
	createStoreFile(t, path)

	// spawn 4 x TestAppenderProcess (25 batches x 4 records each, one shared owner path)
	const appenders = 4
	spawnChildren(t, "TestAppenderProcess", appenders, goFile, func(i int) []string {
		return []string{
			"REPLAYCORE_APPENDER_ID=" + strconv.Itoa(i),
			"REPLAYCORE_STORE_PATH=" + path,
			"REPLAYCORE_GO_FILE=" + goFile,
		}
	})

	// audit after all children exit 0:
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open for audit: %v", err)
	}
	defer s.Close()
	recs, err := s.PathRecords("runs/shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 400 {
		t.Fatalf("records = %d, want 400 (lost batches)", len(recs))
	}
	byID := make(map[string]int64, len(recs))
	for i, r := range recs { // ordinals exactly 0..399, no collisions, no gaps
		if r.OwnerOrdinal != int64(i) {
			t.Fatalf("ordinal[%d] = %d", i, r.OwnerOrdinal)
		}
		if _, err := s.GetRecord(r.ID); err != nil {
			t.Fatalf("record %s fails validation: %v", r.ID, err)
		}
		byID[r.ID] = r.OwnerOrdinal
	}
	// every intent receipted: replaying one intent per appender returns its original receipts
	for a := 0; a < appenders; a++ {
		const batch = 12
		drafts, err := appenderBatch(a, batch)
		if err != nil {
			t.Fatal(err)
		}
		receipts, err := s.AppendBatch("runs/shared", fmt.Sprintf("app%d-%d", a, batch), drafts)
		if err != nil {
			t.Fatalf("replay app%d-%d: %v", a, batch, err)
		}
		want := draftIDs(t, drafts)
		if len(receipts) != len(want) {
			t.Fatalf("replay app%d-%d returned %d receipts, want %d", a, batch, len(receipts), len(want))
		}
		for j, rc := range receipts {
			if rc.RecordID != want[j] {
				t.Fatalf("replay app%d-%d receipt %d = %s, want original %s", a, batch, j, rc.RecordID, want[j])
			}
			if byID[rc.RecordID] != rc.OwnerOrdinal {
				t.Fatalf("replay app%d-%d receipt %d ordinal = %d, stored ordinal %d", a, batch, j, rc.OwnerOrdinal, byID[rc.RecordID])
			}
		}
	}
	if recs, err := s.PathRecords("runs/shared"); err != nil || len(recs) != 400 {
		t.Fatalf("after replays: %d records, err %v; want 400, nil (replays append nothing)", len(recs), err)
	}
}

func TestSameIntentRaceAcrossProcesses(t *testing.T) { // AC-F2-2, concurrent leg
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.db")
	createStoreFile(t, path)

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open for audit: %v", err)
	}
	defer s.Close()

	// Leg 1 — identical batch, one intent, 4 racers: exactly one
	// append lands; every racer succeeds, the losers by replaying
	// the winner's receipts through the in-transaction re-check.
	goSame := filepath.Join(dir, "go-same")
	spawnChildren(t, "TestRaceProcess", 4, goSame, func(i int) []string {
		return []string{
			"REPLAYCORE_RACE_MODE=same",
			"REPLAYCORE_RACE_LEG=same",
			"REPLAYCORE_RACE_VARIANT=0",
			"REPLAYCORE_RACE_PATH=runs/race-same",
			"REPLAYCORE_RACE_INTENT=race-same-1",
			"REPLAYCORE_STORE_PATH=" + path,
			"REPLAYCORE_GO_FILE=" + goSame,
		}
	})
	sameDrafts, err := raceBatch("same", 0)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := s.PathRecords("runs/race-same")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 {
		t.Fatalf("same-intent race landed %d records, want exactly 4 (one batch, no double append)", len(recs))
	}
	wantSame := draftIDs(t, sameDrafts)
	for i, r := range recs {
		if r.ID != wantSame[i] || r.OwnerOrdinal != int64(i) {
			t.Fatalf("race-same record %d = (%s, ordinal %d), want (%s, %d)", i, r.ID, r.OwnerOrdinal, wantSame[i], i)
		}
	}
	receipts, err := s.AppendBatch("runs/race-same", "race-same-1", sameDrafts)
	if err != nil {
		t.Fatalf("post-race replay: %v", err)
	}
	for j, rc := range receipts {
		if rc.RecordID != wantSame[j] || rc.OwnerOrdinal != int64(j) {
			t.Fatalf("post-race replay receipt %d = (%s, %d), want (%s, %d)", j, rc.RecordID, rc.OwnerOrdinal, wantSame[j], j)
		}
	}

	// Leg 2 — different batches, one intent, 2 racers: exactly one
	// batch lands; the loser resolves to *IntentConflictError
	// (asserted in the child — a constraint error exits it
	// non-zero). The parent then proves which variant won: it
	// replays cleanly, the other conflicts, and the landed records
	// are the winner's.
	goConflict := filepath.Join(dir, "go-conflict")
	spawnChildren(t, "TestRaceProcess", 2, goConflict, func(i int) []string {
		return []string{
			"REPLAYCORE_RACE_MODE=conflict",
			"REPLAYCORE_RACE_LEG=conflict",
			"REPLAYCORE_RACE_VARIANT=" + strconv.Itoa(i),
			"REPLAYCORE_RACE_PATH=runs/race-conflict",
			"REPLAYCORE_RACE_INTENT=race-conflict-1",
			"REPLAYCORE_STORE_PATH=" + path,
			"REPLAYCORE_GO_FILE=" + goConflict,
		}
	})
	recs, err = s.PathRecords("runs/race-conflict")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 {
		t.Fatalf("conflict race landed %d records, want exactly 4 (one batch only)", len(recs))
	}
	variants := make([][]store.RecordDraft, 2)
	for v := range variants {
		variants[v], err = raceBatch("conflict", v)
		if err != nil {
			t.Fatal(err)
		}
	}
	winner := -1
	for v := range variants {
		_, err := s.AppendBatch("runs/race-conflict", "race-conflict-1", variants[v])
		if err == nil {
			if winner >= 0 {
				t.Fatalf("both variants replay: intents %d and %d both recorded", winner, v)
			}
			winner = v
			continue
		}
		var conflict *store.IntentConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("variant %d post-race append err = %v, want replay or IntentConflictError", v, err)
		}
	}
	if winner < 0 {
		t.Fatal("neither variant replays: the winning batch is not recorded under the raced intent")
	}
	wantWinner := draftIDs(t, variants[winner])
	for i, r := range recs {
		if r.ID != wantWinner[i] {
			t.Fatalf("race-conflict record %d = %s, want winner variant %d's %s", i, r.ID, winner, wantWinner[i])
		}
	}
	if recs, err := s.PathRecords("runs/race-conflict"); err != nil || len(recs) != 4 {
		t.Fatalf("after winner determination: %d records, err %v; want 4, nil", len(recs), err)
	}
}
