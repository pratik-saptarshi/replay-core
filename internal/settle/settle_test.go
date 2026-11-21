package settle_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/carrier"
	"github.com/pratik-saptarshi/replay-core/internal/settle"
	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The settle fixture (plan Task 6, Step 5): a real git working
// tree as parentDir; snapshot it; Fork a parent scope at the
// snapshot; settle.Fork a run scope; the run harness advances
// the run line by snapshotting the layer and AdvanceScope-ing
// the run scope (the caller's composition, per the plan's
// Interfaces). The settle Context.Root sits on a btrfs scratch
// dir under the working directory — never a tmpfs temp dir — so
// Fork's carrier selection on Root/layers lands on the copy
// carrier (AC-F5-2's recorded truth for btrfs) and every verb
// fixture in Tasks 6–8 exercises the same carrier
// deterministically; on tmpfs, selection would hand Fork a
// kernel-overlay layer and the carrier under test would vary
// with the host.
type fixture struct {
	ctx       *settle.Context
	trace     *store.Store
	snaps     *snapshot.Store
	root      string // the settle Context.Root
	parentDir string
	parent0   string // the parent scope's initial head
}

func writeTestFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func gitWorkTree(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=settle-test", "-c", "user.email=settle-test@localhost", "commit", "-q", "-m", "parent"},
	} {
		full := append([]string{"-C", dir}, args...)
		cmd := exec.Command("git", full...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	scratch, err := os.MkdirTemp(wd, ".settle-fixture-")
	if err != nil {
		t.Fatalf("create btrfs scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })

	parentDir := filepath.Join(scratch, "parent")
	writeTestFile(t, filepath.Join(parentDir, "keep.txt"), "keep\n", 0o644)
	writeTestFile(t, filepath.Join(parentDir, "mod.txt"), "original\n", 0o644)
	gitWorkTree(t, parentDir)

	traceStore, err := store.Open(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = traceStore.Close() })
	snaps, err := snapshot.Open(filepath.Join(t.TempDir(), "snaps.git"))
	if err != nil {
		t.Fatalf("snapshot.Open: %v", err)
	}
	parent0, err := snaps.Snapshot(parentDir, map[string]string{"run": "settle-fixture"})
	if err != nil {
		t.Fatalf("Snapshot parent: %v", err)
	}
	if err := snaps.Fork("parent", parent0); err != nil {
		t.Fatalf("Fork parent scope: %v", err)
	}
	return &fixture{
		ctx:       &settle.Context{Trace: traceStore, Snaps: snaps, Root: filepath.Join(scratch, "settle-root")},
		trace:     traceStore,
		snaps:     snaps,
		root:      filepath.Join(scratch, "settle-root"),
		parentDir: parentDir,
		parent0:   parent0,
	}
}

// advanceRun is the run harness: snapshot the layer's current
// state and advance the run scope to it (the caller's
// composition of Snapshot + AdvanceScope).
func (f *fixture) advanceRun(t *testing.T, layer *carrier.Layer, runScope, from string) string {
	t.Helper()
	commit, err := f.snaps.Snapshot(layer.Dir, map[string]string{"run": "settle-fixture"})
	if err != nil {
		t.Fatalf("Snapshot layer: %v", err)
	}
	if err := f.snaps.AdvanceScope(runScope, from, commit); err != nil {
		t.Fatalf("AdvanceScope run: %v", err)
	}
	return commit
}

// stringField extracts a string field from a canonical object.
func stringField(v canonical.Value, key string) (string, bool) {
	if v.Kind != canonical.KindObject {
		return "", false
	}
	for _, p := range v.Pairs {
		if p.Key == key {
			if p.Value.Kind != canonical.KindString {
				return "", false
			}
			return p.Value.Str, true
		}
	}
	return "", false
}

// settleFacts returns the run scope's settlement facts (records
// under any of the three settlement schema refs), in path order.
func (f *fixture) settleFacts(t *testing.T, runScope string) []store.StoredRecord {
	t.Helper()
	recs, err := f.trace.PathRecords(runScope)
	if err != nil {
		t.Fatalf("PathRecords(%q): %v", runScope, err)
	}
	var out []store.StoredRecord
	for _, rec := range recs {
		ref, _ := stringField(rec.Record, "schema_ref")
		switch ref {
		case store.SettleSelectSchemaRef, store.SettleApplySchemaRef, store.SettleDiscardSchemaRef:
			out = append(out, rec)
		}
	}
	return out
}

// factBody returns a settlement fact's body object.
func factBody(t *testing.T, rec store.StoredRecord) canonical.Value {
	t.Helper()
	for _, p := range rec.Record.Pairs {
		if p.Key == "body" {
			return p.Value
		}
	}
	t.Fatalf("record %s has no body", rec.ID)
	return canonical.Value{}
}

// factCausedBy returns a record's caused_by list.
func factCausedBy(t *testing.T, rec store.StoredRecord) []string {
	t.Helper()
	for _, p := range rec.Record.Pairs {
		if p.Key == "caused_by" {
			out := make([]string, 0, len(p.Value.Elems))
			for _, e := range p.Value.Elems {
				out = append(out, e.Str)
			}
			return out
		}
	}
	t.Fatalf("record %s has no caused_by", rec.ID)
	return nil
}

func readParentFile(t *testing.T, f *fixture, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.parentDir, rel))
	if err != nil {
		t.Fatalf("read parent %s: %v", rel, err)
	}
	return string(b)
}

// TestSelectFastForwardsUnadvancedParent (AC-F6-1): the run's
// work lands in the parent directory exactly — modes included —
// the parent scope fast-forwards to the run head, and exactly
// one select fact is minted, last, citing the pre-verb frontier.
func TestSelectFastForwardsUnadvancedParent(t *testing.T) {
	f := newFixture(t)
	layer, err := settle.Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	writeTestFile(t, filepath.Join(layer.Dir, "added.txt"), "added\n", 0o644)
	writeTestFile(t, filepath.Join(layer.Dir, "run.sh"), "#!/bin/sh\n", 0o755)
	writeTestFile(t, filepath.Join(layer.Dir, "mod.txt"), "modified\n", 0o644)
	runHead := f.advanceRun(t, layer, "run-a", f.parent0)

	preFrontier, err := f.trace.Frontier("run-a")
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}
	receipt, err := settle.Select(f.ctx, "parent", f.parentDir, "run-a", "op-1")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}

	if got := readParentFile(t, f, "added.txt"); got != "added\n" {
		t.Fatalf("parent added.txt = %q, want %q", got, "added\n")
	}
	if got := readParentFile(t, f, "mod.txt"); got != "modified\n" {
		t.Fatalf("parent mod.txt = %q, want %q", got, "modified\n")
	}
	if got := readParentFile(t, f, "keep.txt"); got != "keep\n" {
		t.Fatalf("parent keep.txt = %q, want %q", got, "keep\n")
	}
	info, err := os.Stat(filepath.Join(f.parentDir, "run.sh"))
	if err != nil {
		t.Fatalf("stat parent run.sh: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("parent run.sh mode = %o, want 755 (executable intact, not flattened)", info.Mode().Perm())
	}

	head, err := f.snaps.ScopeHead("parent")
	if err != nil {
		t.Fatalf("ScopeHead(parent): %v", err)
	}
	if head != runHead {
		t.Fatalf("ScopeHead(parent) = %s, want run head %s", head, runHead)
	}

	facts := f.settleFacts(t, "run-a")
	if len(facts) != 1 {
		t.Fatalf("settle facts on run-a = %d, want exactly 1", len(facts))
	}
	fact := facts[0]
	if fact.ID != receipt.RecordID {
		t.Fatalf("fact ID = %s, receipt = %s", fact.ID, receipt.RecordID)
	}
	if ref, _ := stringField(fact.Record, "schema_ref"); ref != store.SettleSelectSchemaRef {
		t.Fatalf("fact schema_ref = %q, want %q", ref, store.SettleSelectSchemaRef)
	}
	if got := factCausedBy(t, fact); !reflect.DeepEqual(got, []string{preFrontier}) {
		t.Fatalf("fact caused_by = %v, want [pre-verb frontier %s]", got, preFrontier)
	}
	body := factBody(t, fact)
	for key, want := range map[string]string{
		"op": "op-1", "parent_scope": "parent", "run_scope": "run-a",
		"parent_head": f.parent0, "run_head": runHead,
	} {
		if got, _ := stringField(body, key); got != want {
			t.Fatalf("fact body[%q] = %q, want %q", key, got, want)
		}
	}
}

// TestSelectRefusesAdvancedParent (AC-F6-1's failure witness):
// a parent that advanced past the fork point is refused
// outright — no ref move, no directory writes, no fact, no
// merge or rebase artifact. The second leg advances the parent
// by a meta-only snapshot (identical workspace tree, new
// commit): advancement is a property of the head, not the tree,
// so it is still refused.
func TestSelectRefusesAdvancedParent(t *testing.T) {
	f := newFixture(t)
	if _, err := settle.Fork(f.ctx, "parent", "run-a"); err != nil {
		t.Fatalf("Fork run-a: %v", err)
	}

	// The parent advances with a content change.
	writeTestFile(t, filepath.Join(f.parentDir, "sneak.txt"), "sneak\n", 0o644)
	c2, err := f.snaps.Snapshot(f.parentDir, map[string]string{"run": "settle-fixture"})
	if err != nil {
		t.Fatalf("Snapshot parent advance: %v", err)
	}
	if err := f.snaps.AdvanceScope("parent", f.parent0, c2); err != nil {
		t.Fatalf("AdvanceScope parent: %v", err)
	}

	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "run-a", "op-1"); !errors.Is(err, settle.ErrParentAdvanced) {
		t.Fatalf("Select onto advanced parent: err = %v, want ErrParentAdvanced", err)
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != c2 {
		t.Fatalf("parent head after refused Select = %s, %v; want %s unmoved", head, err, c2)
	}
	if got := readParentFile(t, f, "sneak.txt"); got != "sneak\n" {
		t.Fatalf("parent sneak.txt = %q after refused Select, want unchanged %q", got, "sneak\n")
	}
	if got := readParentFile(t, f, "mod.txt"); got != "original\n" {
		t.Fatalf("parent mod.txt = %q after refused Select, want unchanged %q", got, "original\n")
	}
	if facts := f.settleFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("refused Select minted %d facts, want 0", len(facts))
	}

	// Second leg: meta-only advancement — same workspace tree,
	// new commit — is still advancement.
	if _, err := settle.Fork(f.ctx, "parent", "run-b"); err != nil {
		t.Fatalf("Fork run-b: %v", err)
	}
	c3, err := f.snaps.Snapshot(f.parentDir, map[string]string{"run": "settle-fixture", "leg": "meta-only"})
	if err != nil {
		t.Fatalf("Snapshot meta-only advance: %v", err)
	}
	tree2, err := f.snaps.WorkspaceTreeID(c2)
	if err != nil {
		t.Fatalf("WorkspaceTreeID(c2): %v", err)
	}
	tree3, err := f.snaps.WorkspaceTreeID(c3)
	if err != nil {
		t.Fatalf("WorkspaceTreeID(c3): %v", err)
	}
	if tree2 != tree3 {
		t.Fatalf("meta-only leg is not meta-only: workspace trees differ (%s vs %s)", tree2, tree3)
	}
	if err := f.snaps.AdvanceScope("parent", c2, c3); err != nil {
		t.Fatalf("AdvanceScope parent meta-only: %v", err)
	}
	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "run-b", "op-2"); !errors.Is(err, settle.ErrParentAdvanced) {
		t.Fatalf("Select onto meta-only-advanced parent: err = %v, want ErrParentAdvanced", err)
	}
	if facts := f.settleFacts(t, "run-b"); len(facts) != 0 {
		t.Fatalf("refused meta-only Select minted %d facts, want 0", len(facts))
	}
}

// TestDiscardArchivesNeverDeletes (AC-F6-5): discard retires
// the run scope to the archive class at the fork commit — the
// history stays readable — and removes the layer and sidecar.
func TestDiscardArchivesNeverDeletes(t *testing.T) {
	f := newFixture(t)
	if _, err := settle.Fork(f.ctx, "parent", "run-a"); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	layerRoot := filepath.Join(f.root, "layers", "run-a")
	if _, err := os.Stat(layerRoot); err != nil {
		t.Fatalf("layer root after Fork: %v", err)
	}
	sidecar := filepath.Join(f.root, "forks", "run-a.json")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar after Fork: %v", err)
	}

	receipt, err := settle.Discard(f.ctx, "run-a", "op-1")
	if err != nil {
		t.Fatalf("Discard: %v", err)
	}

	refs, err := f.snaps.ListScopes()
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	foundArchive := false
	for _, r := range refs {
		if r.Kind == snapshot.RefScope && r.Name == "run-a" {
			t.Fatalf("scope run-a still present after Discard: %+v", r)
		}
		if r.Kind == snapshot.RefArchive && r.Name == "run-a" {
			if r.CommitID != f.parent0 {
				t.Fatalf("archive/run-a at %s, want fork commit %s", r.CommitID, f.parent0)
			}
			foundArchive = true
		}
	}
	if !foundArchive {
		t.Fatalf("ListScopes = %v, want archive/run-a at %s", refs, f.parent0)
	}
	if _, err := os.Stat(layerRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("layer root after Discard: err = %v, want gone", err)
	}
	if _, err := os.Stat(sidecar); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar after Discard: err = %v, want gone", err)
	}

	// The archived history still resolves.
	files, err := f.snaps.WorkspaceFiles(f.parent0)
	if err != nil {
		t.Fatalf("WorkspaceFiles(archived commit): %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("archived commit workspace = %v, want the parent's 2 files", files)
	}

	facts := f.settleFacts(t, "run-a")
	if len(facts) != 1 || facts[0].ID != receipt.RecordID {
		t.Fatalf("settle facts on run-a = %v, want exactly the discard receipt %s", facts, receipt.RecordID)
	}
	body := factBody(t, facts[0])
	for key, want := range map[string]string{
		"op": "op-1", "run_scope": "run-a", "head": f.parent0,
		"archive_ref": "refs/vcscore/archive/run-a",
	} {
		if got, _ := stringField(body, key); got != want {
			t.Fatalf("discard fact body[%q] = %q, want %q", key, got, want)
		}
	}
}

// TestConsumeOnceAcrossVerbs (AC-F6-6): a scope settled by any
// verb is consumed for every verb — the settlement fact on the
// run scope's path is the authority, whatever its op.
func TestConsumeOnceAcrossVerbs(t *testing.T) {
	f := newFixture(t)
	layer, err := settle.Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork run-a: %v", err)
	}
	writeTestFile(t, filepath.Join(layer.Dir, "added.txt"), "added\n", 0o644)
	f.advanceRun(t, layer, "run-a", f.parent0)
	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "run-a", "op-1"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "run-a", "op-2"); !errors.Is(err, settle.ErrScopeConsumed) {
		t.Fatalf("second Select: err = %v, want ErrScopeConsumed", err)
	}
	if _, err := settle.Discard(f.ctx, "run-a", "op-3"); !errors.Is(err, settle.ErrScopeConsumed) {
		t.Fatalf("Discard after Select: err = %v, want ErrScopeConsumed", err)
	}

	if _, err := settle.Fork(f.ctx, "parent", "run-b"); err != nil {
		t.Fatalf("Fork run-b: %v", err)
	}
	if _, err := settle.Discard(f.ctx, "run-b", "op-4"); err != nil {
		t.Fatalf("Discard run-b: %v", err)
	}
	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "run-b", "op-5"); !errors.Is(err, settle.ErrScopeConsumed) {
		t.Fatalf("Select after Discard: err = %v, want ErrScopeConsumed", err)
	}
}

// TestSelectConcurrentRace (plan Task 6, Step 7 — timing
// shape): two goroutines Select the same (parent, run) with
// different op IDs, released together. Exactly one returns
// success; the loser is refused (ErrScopeConsumed or
// ErrScopeHeadMoved); exactly one settle-select fact exists;
// the parent head is the run head. The commit-point order —
// compare-and-swap before the fact — is what makes the race
// converge. Run under -race in the package suite.
func TestSelectConcurrentRace(t *testing.T) {
	f := newFixture(t)
	layer, err := settle.Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	writeTestFile(t, filepath.Join(layer.Dir, "added.txt"), "added\n", 0o644)
	runHead := f.advanceRun(t, layer, "run-a", f.parent0)

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, op := range []string{"op-a", "op-b"} {
		wg.Add(1)
		go func(i int, op string) {
			defer wg.Done()
			<-start
			_, errs[i] = settle.Select(f.ctx, "parent", f.parentDir, "run-a", op)
		}(i, op)
	}
	close(start)
	wg.Wait()

	successes := 0
	for i, err := range errs {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, settle.ErrScopeConsumed) && !errors.Is(err, snapshot.ErrScopeHeadMoved) {
			t.Fatalf("loser %d err = %v, want ErrScopeConsumed or ErrScopeHeadMoved", i, err)
		}
	}
	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1 (errs = %v)", successes, errs)
	}
	facts := f.settleFacts(t, "run-a")
	if len(facts) != 1 {
		t.Fatalf("settle facts on run-a = %d, want exactly 1", len(facts))
	}
	if ref, _ := stringField(facts[0].Record, "schema_ref"); ref != store.SettleSelectSchemaRef {
		t.Fatalf("fact schema_ref = %q, want %q", ref, store.SettleSelectSchemaRef)
	}
	head, err := f.snaps.ScopeHead("parent")
	if err != nil {
		t.Fatalf("ScopeHead(parent): %v", err)
	}
	if head != runHead {
		t.Fatalf("ScopeHead(parent) = %s, want run head %s", head, runHead)
	}
}

// TestSettleVerbsRejectDegenerateInputs: empty op IDs, unknown
// scopes, and a scope ref with no fork provenance are each a
// descriptive refusal with nothing written anywhere.
func TestSettleVerbsRejectDegenerateInputs(t *testing.T) {
	f := newFixture(t)
	if _, err := settle.Fork(f.ctx, "parent", "run-a"); err != nil {
		t.Fatalf("Fork: %v", err)
	}

	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "run-a", ""); err == nil {
		t.Fatal("Select with empty op ID succeeded, want a refusal")
	}
	if _, err := settle.Discard(f.ctx, "run-a", ""); err == nil {
		t.Fatal("Discard with empty op ID succeeded, want a refusal")
	}
	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "nope", "op-1"); !errors.Is(err, settle.ErrNoForkProvenance) {
		t.Fatalf("Select unknown run scope: err = %v, want ErrNoForkProvenance", err)
	}
	if _, err := settle.Select(f.ctx, "no-parent", f.parentDir, "run-a", "op-1"); err == nil {
		t.Fatal("Select with unknown parent scope succeeded, want a refusal")
	}

	// A scope ref created outside settle.Fork has no sidecar:
	// the verbs refuse it as unprovenanced rather than adopting
	// a foreign path (Decision 3).
	if err := f.snaps.Fork("ghost", f.parent0); err != nil {
		t.Fatalf("Fork ghost scope: %v", err)
	}
	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "ghost", "op-1"); !errors.Is(err, settle.ErrNoForkProvenance) {
		t.Fatalf("Select ghost scope: err = %v, want ErrNoForkProvenance", err)
	}
	if _, err := settle.Discard(f.ctx, "ghost", "op-1"); !errors.Is(err, settle.ErrNoForkProvenance) {
		t.Fatalf("Discard ghost scope: err = %v, want ErrNoForkProvenance", err)
	}

	// Nothing was written anywhere by any refusal above.
	if facts := f.settleFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("degenerate calls minted %d facts on run-a, want 0", len(facts))
	}
	if facts := f.settleFacts(t, "ghost"); len(facts) != 0 {
		t.Fatalf("degenerate calls minted %d facts on ghost, want 0", len(facts))
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after degenerate calls; want %s unmoved", head, err, f.parent0)
	}
}

// TestDiscardCompletesAfterCrashPastArchive (PR #7 review,
// Codex P2): a Discard that dies after ArchiveScope lands but
// before the fact is minted leaves the active scope ref gone
// and no fact anywhere — and on the unfixed code a retry dies
// with it: ScopeHead refuses the missing ref (or, one step
// later, the sidecar is already gone), so the discard can
// never complete and consume-once has no authoritative fact to
// read. The archive ref holds the head the fact must cite, so
// a retry must complete the discard from the surviving state:
// finish the cleanup, mint the fact citing the archived head,
// and report the scope consumed thereafter.
func TestDiscardCompletesAfterCrashPastArchive(t *testing.T) {
	assertCompleted := func(t *testing.T, f *fixture, receipt store.Receipt) {
		t.Helper()
		refs, err := f.snaps.ListScopes()
		if err != nil {
			t.Fatalf("ListScopes: %v", err)
		}
		foundArchive := false
		for _, r := range refs {
			if r.Kind == snapshot.RefScope && r.Name == "run-a" {
				t.Fatalf("scope run-a still present after completed Discard: %+v", r)
			}
			if r.Kind == snapshot.RefArchive && r.Name == "run-a" {
				if r.CommitID != f.parent0 {
					t.Fatalf("archive/run-a at %s, want fork commit %s", r.CommitID, f.parent0)
				}
				foundArchive = true
			}
		}
		if !foundArchive {
			t.Fatalf("ListScopes = %v, want archive/run-a at %s", refs, f.parent0)
		}
		if _, err := os.Stat(filepath.Join(f.root, "layers", "run-a")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("layer root after completed Discard: err = %v, want gone", err)
		}
		if _, err := os.Stat(filepath.Join(f.root, "forks", "run-a.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("sidecar after completed Discard: err = %v, want gone", err)
		}
		facts := f.settleFacts(t, "run-a")
		if len(facts) != 1 || facts[0].ID != receipt.RecordID {
			t.Fatalf("settle facts on run-a = %v, want exactly the discard receipt %s", facts, receipt.RecordID)
		}
		body := factBody(t, facts[0])
		for key, want := range map[string]string{
			"op": "op-1", "run_scope": "run-a", "head": f.parent0,
			"archive_ref": "refs/vcscore/archive/run-a",
		} {
			if got, _ := stringField(body, key); got != want {
				t.Fatalf("discard fact body[%q] = %q, want %q (the archived head, cited as recorded)", key, got, want)
			}
		}
		// The completion is authoritative: a further retry
		// reads the fact and reports the scope consumed.
		if _, err := settle.Discard(f.ctx, "run-a", "op-2"); !errors.Is(err, settle.ErrScopeConsumed) {
			t.Fatalf("Discard after completed Discard: err = %v, want ErrScopeConsumed", err)
		}
	}

	t.Run("crash right after the archive", func(t *testing.T) {
		f := newFixture(t)
		if _, err := settle.Fork(f.ctx, "parent", "run-a"); err != nil {
			t.Fatalf("Fork: %v", err)
		}
		// The crashed Discard's surviving state: the archive
		// landed, the layer and sidecar remain, no fact.
		if err := f.snaps.ArchiveScope("run-a"); err != nil {
			t.Fatalf("ArchiveScope (the crashed verb's last landed step): %v", err)
		}
		receipt, err := settle.Discard(f.ctx, "run-a", "op-1")
		if err != nil {
			t.Fatalf("Discard completing a discard crashed past the archive: %v", err)
		}
		assertCompleted(t, f, receipt)
	})

	t.Run("crash after layer removal", func(t *testing.T) {
		f := newFixture(t)
		if _, err := settle.Fork(f.ctx, "parent", "run-a"); err != nil {
			t.Fatalf("Fork: %v", err)
		}
		if err := f.snaps.ArchiveScope("run-a"); err != nil {
			t.Fatalf("ArchiveScope (the crashed verb's last landed step): %v", err)
		}
		if err := os.RemoveAll(filepath.Join(f.root, "layers", "run-a")); err != nil {
			t.Fatalf("remove layer (the crashed verb's cleanup): %v", err)
		}
		receipt, err := settle.Discard(f.ctx, "run-a", "op-1")
		if err != nil {
			t.Fatalf("Discard completing a discard crashed after layer removal: %v", err)
		}
		assertCompleted(t, f, receipt)
	})
}
