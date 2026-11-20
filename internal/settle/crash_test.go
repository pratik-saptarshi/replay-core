package settle

// Crash-injection tests (plan Task 8, Steps 4–5): the child
// process runs a real Apply against the fixture's roots and is
// killed by unitCrashHook at an enumerated unit boundary; the
// parent process — a different handle on the same stores — then
// runs Recover and asserts the settlement converged to exactly
// one of its two legal end states. The kill points are
// enumerated deterministically — {0, 1, 2, all} applied units
// spans every unit boundary of the fixture — which is strictly
// stronger than AC-F6-4's "randomized points" wording: every
// boundary a random draw could land on is exercised on every
// run (amendment F11).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// TestSettleCrashChild is the re-exec child entry for the crash
// tests: it reports SKIP in the parent process (the settle
// package's sole skip exception in check-phase2.sh) and, as a
// child, opens the fixture stores by path, arms unitCrashHook
// from REPLAYCORE_SETTLE_CRASH_KILL, and runs Apply. Kill values
// are unit counts (exit 3 when that many units are marked done)
// or "verified" (exit 3 when the journal on disk reaches the
// verified state — the commit-point hook). Exit 0 means Apply
// completed without the hook firing; exit 2 means the child
// failed before or inside Apply.
func TestSettleCrashChild(t *testing.T) {
	if os.Getenv("REPLAYCORE_SETTLE_CRASH") == "" {
		t.Skip("child-process helper: runs only under TestApplyCrashRecovery and TestRecoverCompletesVerifiedJournal")
	}
	trace, err := store.Open(os.Getenv("REPLAYCORE_SETTLE_TRACE"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "crash child: open trace: %v\n", err)
		os.Exit(2)
	}
	snaps, err := snapshot.Open(os.Getenv("REPLAYCORE_SETTLE_SNAPS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "crash child: open snaps: %v\n", err)
		os.Exit(2)
	}
	root := os.Getenv("REPLAYCORE_SETTLE_ROOT")
	parentScope := os.Getenv("REPLAYCORE_SETTLE_PARENT_SCOPE")
	switch kill := os.Getenv("REPLAYCORE_SETTLE_CRASH_KILL"); kill {
	case "verified":
		unitCrashHook = func(done int) {
			j, err := readJournal(root, parentScope)
			if err == nil && j != nil && j.State == journalStateVerified {
				os.Exit(3)
			}
		}
	default:
		limit, err := strconv.Atoi(kill)
		if err != nil {
			fmt.Fprintf(os.Stderr, "crash child: bad kill value %q\n", kill)
			os.Exit(2)
		}
		unitCrashHook = func(done int) {
			if done >= limit {
				os.Exit(3)
			}
		}
	}
	ctx := &Context{Trace: trace, Snaps: snaps, Root: root}
	_, err = Apply(ctx,
		parentScope,
		os.Getenv("REPLAYCORE_SETTLE_PARENT_DIR"),
		os.Getenv("REPLAYCORE_SETTLE_RUN_SCOPE"),
		os.Getenv("REPLAYCORE_SETTLE_OP"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "crash child: Apply: %v\n", err)
		os.Exit(2)
	}
	os.Exit(0)
}

// spawnCrashChild runs TestSettleCrashChild in a child process
// against f's roots and returns its exit code and output.
func spawnCrashChild(t *testing.T, f *journalFixture, runScope, op, kill string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestSettleCrashChild$")
	cmd.Env = append(os.Environ(),
		"REPLAYCORE_SETTLE_CRASH=1",
		"REPLAYCORE_SETTLE_CRASH_KILL="+kill,
		"REPLAYCORE_SETTLE_TRACE="+f.tracePath,
		"REPLAYCORE_SETTLE_SNAPS="+f.snapsPath,
		"REPLAYCORE_SETTLE_ROOT="+f.root,
		"REPLAYCORE_SETTLE_PARENT_DIR="+f.parentDir,
		"REPLAYCORE_SETTLE_PARENT_SCOPE=parent",
		"REPLAYCORE_SETTLE_RUN_SCOPE="+runScope,
		"REPLAYCORE_SETTLE_OP="+op,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("spawn crash child: %v: %s", err, out)
	return -1, string(out)
}

// setupCrashApply forks runScope from the fixture parent and
// advances the run so its apply has exactly four units against
// the idle parent: write mod.txt, write new1.txt, write
// sub/new2.txt, delete old.txt.
func setupCrashApply(t *testing.T, f *journalFixture, runScope string) {
	t.Helper()
	layer, err := Fork(f.ctx, "parent", runScope)
	if err != nil {
		t.Fatalf("Fork %q: %v", runScope, err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "run mod\n", 0o644)
	journalWriteFile(t, filepath.Join(layer.Dir, "new1.txt"), "new one\n", 0o644)
	journalWriteFile(t, filepath.Join(layer.Dir, "sub", "new2.txt"), "new two\n", 0o644)
	if err := os.Remove(filepath.Join(layer.Dir, "old.txt")); err != nil {
		t.Fatalf("remove layer old.txt: %v", err)
	}
	commit, err := f.snaps.Snapshot(layer.Dir, map[string]string{"run": "settle-crash-fixture"})
	if err != nil {
		t.Fatalf("Snapshot layer: %v", err)
	}
	if err := f.snaps.AdvanceScope(runScope, f.parent0, commit); err != nil {
		t.Fatalf("AdvanceScope run: %v", err)
	}
}

// expectedApplyDir builds the tree a completed crash-fixture
// apply leaves in the parent directory.
func expectedApplyDir(t *testing.T, f *journalFixture) string {
	t.Helper()
	dir := filepath.Join(f.scratch, "expected")
	journalWriteFile(t, filepath.Join(dir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(dir, "mod.txt"), "run mod\n", 0o644)
	journalWriteFile(t, filepath.Join(dir, "new1.txt"), "new one\n", 0o644)
	journalWriteFile(t, filepath.Join(dir, "sub", "new2.txt"), "new two\n", 0o644)
	return dir
}

// TestApplyCrashRecovery (plan Task 8, Step 4; AC-F6-4): for
// each enumerated kill point, the child dies mid-apply at that
// unit boundary; Recover from this process converges the
// settlement to exactly the pre-apply state (rolled back: no
// fact, scope unmoved, the same work applies cleanly after) or
// exactly the post-apply state (completed: fact minted, scope
// advanced) — never anything between.
func TestApplyCrashRecovery(t *testing.T) {
	// "all" is the fixture's full unit count (setupCrashApply).
	for _, kill := range []string{"0", "1", "2", "4"} {
		t.Run("killAfter="+kill, func(t *testing.T) {
			f := newJournalFixture(t)
			setupCrashApply(t, f, "run-a")
			preHash := hashTree(t, f.parentDir)
			postHash := hashTree(t, expectedApplyDir(t, f))

			code, out := spawnCrashChild(t, f, "run-a", "op-crash", kill)
			if code != 3 {
				t.Fatalf("child exit = %d, want 3 (killed at unit boundary %s); child output:\n%s", code, kill, out)
			}

			if err := Recover(f.ctx, "parent"); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
				t.Fatalf("journal after Recover: err = %v, want gone", err)
			}

			switch got := hashTree(t, f.parentDir); got {
			case preHash:
				if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
					t.Fatalf("rolled-back apply left %d facts, want 0", len(facts))
				}
				if head, err := f.snaps.ScopeHead("parent"); err != nil || head != f.parent0 {
					t.Fatalf("parent head = %s, %v after rollback; want %s unmoved", head, err, f.parent0)
				}
				// The same work, retried whole, succeeds.
				if _, err := Apply(f.ctx, "parent", f.parentDir, "run-a", "op-crash"); err != nil {
					t.Fatalf("fresh Apply after rollback: %v", err)
				}
				if got := hashTree(t, f.parentDir); got != postHash {
					t.Fatalf("parent walk hash after fresh Apply = %s, want post-apply %s", got, postHash)
				}
				if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
					t.Fatalf("settle facts after fresh Apply = %d, want exactly 1", len(facts))
				}
			case postHash:
				if facts := f.journalFacts(t, "run-a"); len(facts) != 1 {
					t.Fatalf("completed apply left %d facts, want exactly 1", len(facts))
				}
				if head, err := f.snaps.ScopeHead("parent"); err != nil || head == f.parent0 {
					t.Fatalf("parent head = %s, %v after completion; want advanced past %s", head, err, f.parent0)
				}
			default:
				t.Fatalf("parent walk hash = %s, neither pre-apply %s nor post-apply %s — recovery did not converge", got, preHash, postHash)
			}
		})
	}
}

// TestRecoverCompletesVerifiedJournal (plan Task 8, Step 5):
// the child dies after the journal reaches verified but before
// AdvanceScope — the post-CAS/pre-fact window Task 6's interim
// note recorded, one step earlier. Recover completes the commit
// point: the scope advances to the journal's result commit,
// exactly one fact carries the op (minted from the recorded
// body), and the tree stays post-apply.
func TestRecoverCompletesVerifiedJournal(t *testing.T) {
	f := newJournalFixture(t)
	setupCrashApply(t, f, "run-a")
	postHash := hashTree(t, expectedApplyDir(t, f))

	code, out := spawnCrashChild(t, f, "run-a", "op-verified", "verified")
	if code != 3 {
		t.Fatalf("child exit = %d, want 3 (killed at the commit point); child output:\n%s", code, out)
	}

	// The crash landed exactly in the window: tree materialized
	// and verified, ref unmoved, no fact.
	if got := hashTree(t, f.parentDir); got != postHash {
		t.Fatalf("parent walk hash after crash = %s, want post-apply %s (materialization had completed)", got, postHash)
	}
	if head, err := f.snaps.ScopeHead("parent"); err != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after crash; want %s unmoved (died before the CAS)", head, err, f.parent0)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("facts after crash = %d, want 0 (died before the mint)", len(facts))
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	head, err := f.snaps.ScopeHead("parent")
	if err != nil {
		t.Fatalf("ScopeHead(parent): %v", err)
	}
	if head == f.parent0 {
		t.Fatalf("parent head still at %s after Recover; want the journal's result commit", f.parent0)
	}
	headTree, err := f.snaps.WorkspaceTreeID(head)
	if err != nil {
		t.Fatalf("WorkspaceTreeID(head): %v", err)
	}
	wantTree, err := f.snaps.WorkspaceTreeID(mustSnapshot(t, f, expectedApplyDir(t, f)))
	if err != nil {
		t.Fatalf("WorkspaceTreeID(expected): %v", err)
	}
	if headTree != wantTree {
		t.Fatalf("advanced head's workspace tree = %s, want the settled tree %s", headTree, wantTree)
	}
	facts := f.journalFacts(t, "run-a")
	if len(facts) != 1 {
		t.Fatalf("settle facts after Recover = %d, want exactly 1", len(facts))
	}
	if ref, _ := stringField(facts[0].Record, "schema_ref"); ref != store.SettleApplySchemaRef {
		t.Fatalf("fact schema_ref = %q, want %q", ref, store.SettleApplySchemaRef)
	}
	for _, p := range facts[0].Record.Pairs {
		if p.Key == "body" {
			if op, _ := stringField(p.Value, "op"); op != "op-verified" {
				t.Fatalf("fact body op = %q, want %q", op, "op-verified")
			}
			if rc, _ := stringField(p.Value, "result_commit"); rc != head {
				t.Fatalf("fact body result_commit = %q, want the advanced head %q", rc, head)
			}
		}
	}
	if got := hashTree(t, f.parentDir); got != postHash {
		t.Fatalf("parent walk hash after Recover = %s, want post-apply %s (the tree stays settled)", got, postHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after Recover: err = %v, want gone", err)
	}
}

func mustSnapshot(t *testing.T, f *journalFixture, dir string) string {
	t.Helper()
	commit, err := f.snaps.Snapshot(dir, map[string]string{"run": "settle-crash-fixture"})
	if err != nil {
		t.Fatalf("Snapshot %s: %v", dir, err)
	}
	return commit
}
