package conformance_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/carrier"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/settle"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The Phase 2 gate (plan Task 10, T-GATE-P2): one settlement
// story assembled end-to-end over the completed Phase 2 tasks —
// carriers (F5) and journaled settlement (F6) — discharging
// AC-F5-6 outright (fork a real repo workspace, via the copy
// carrier) and re-discharging AC-F6-1, AC-F6-2, AC-F6-3 and
// AC-F6-4 in assembled form. The gate claims exactly what the
// host-deferred register (plan Decision 7) allows: it does not
// discharge, and must never be read as discharging, AC-F5-8,
// AC-F5-9, or AC-F5-10's extent-sharing clause — those drivers'
// real-workspace evidence belongs to a host that can exercise
// them.
//
// Leg 8's crashed apply runs in a child process re-executing
// this test binary. Like the Phase 1 gate, the child entry
// point is TestPhase2Gate itself, gated on an environment
// variable: a separate helper test would report SKIP in
// ordinary runs, and both gate scripts scan this package for
// zero skips. The child cannot arm the settle package's
// unexported crash hook, so it kills itself by watching the
// settlement journal on disk — the journal is the
// materialization's own durable progress record: when the
// recorded done-marks reach the kill count (or the journal
// reaches the verified state, the commit-point boundary), the
// child exits 3, mid-Apply, exactly where Task 8's hook fires.
// The parent then recovers from a different process, as a real
// post-crash caller would.

const (
	p2ChildEnv       = "REPLAYCORE_GATE2_CHILD"
	p2ChildTrace     = "REPLAYCORE_GATE2_TRACE"
	p2ChildSnaps     = "REPLAYCORE_GATE2_SNAPS"
	p2ChildRoot      = "REPLAYCORE_GATE2_ROOT"
	p2ChildParentDir = "REPLAYCORE_GATE2_PARENT_DIR"
	p2ChildRunScope  = "REPLAYCORE_GATE2_RUN_SCOPE"
	p2ChildOp        = "REPLAYCORE_GATE2_OP"
	p2ChildKillAt    = "REPLAYCORE_GATE2_KILL_AT"
)

// p2Write writes one fixture file with an exact mode.
func p2Write(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if err := os.Chmod(full, 0o644); err != nil {
		t.Fatalf("chmod %s: %v", rel, err)
	}
}

// p2GitCommit makes dir a git working tree with one commit, as
// the settle fixtures do for their parent workspaces.
func p2GitCommit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=phase2-gate", "-c", "user.email=phase2-gate@localhost", "commit", "-q", "-m", "parent"},
	} {
		full := append([]string{"-C", dir}, args...)
		cmd := exec.Command("git", full...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// p2WalkHash hashes a directory's non-directory entries —
// relative path, kind, mode, and bytes (or symlink target) —
// into one digest: the walk hash the story compares
// byte-for-byte, with the settle recovery tests' semantics.
// Entries named .git are skipped, mirroring the snapshot walk.
func p2WalkHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	var rels []string
	entries := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != dir && d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entries[rel] = "symlink:" + target
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entries[rel] = fmt.Sprintf("file:%o:%x", info.Mode().Perm(), sha256.Sum256(b))
		default:
			entries[rel] = "other"
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("p2WalkHash(%s): %v", dir, err)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		fmt.Fprintf(h, "%s %s\n", rel, entries[rel])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// p2SnapshotAdvance snapshots dir and advances scope from
// oldCommit to the new snapshot commit (the caller's
// composition the verbs are built over).
func p2SnapshotAdvance(t *testing.T, snaps *snapshot.Store, dir, scope, oldCommit string) string {
	t.Helper()
	commit, err := snaps.Snapshot(dir, map[string]string{"run": "phase2-gate"})
	if err != nil {
		t.Fatalf("Snapshot(%s): %v", dir, err)
	}
	if err := snaps.AdvanceScope(scope, oldCommit, commit); err != nil {
		t.Fatalf("AdvanceScope(%s): %v", scope, err)
	}
	return commit
}

// p2StringField extracts a string field from a canonical object.
func p2StringField(v canonical.Value, key string) (string, bool) {
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

// p2FactBody returns a stored record's body object.
func p2FactBody(t *testing.T, rec store.StoredRecord) canonical.Value {
	t.Helper()
	for _, p := range rec.Record.Pairs {
		if p.Key == "body" {
			return p.Value
		}
	}
	t.Fatalf("record %s has no body", rec.ID)
	return canonical.Value{}
}

// p2CausedBy returns a record's caused_by list.
func p2CausedBy(t *testing.T, rec store.StoredRecord) []string {
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

// p2SettleFacts returns runScope's settlement facts (records
// under any of the three settlement schema refs), in path order.
func p2SettleFacts(t *testing.T, ts *store.Store, runScope string) []store.StoredRecord {
	t.Helper()
	recs, err := ts.PathRecords(runScope)
	if err != nil {
		t.Fatalf("PathRecords(%q): %v", runScope, err)
	}
	var out []store.StoredRecord
	for _, rec := range recs {
		ref, _ := p2StringField(rec.Record, "schema_ref")
		switch ref {
		case store.SettleSelectSchemaRef, store.SettleApplySchemaRef, store.SettleDiscardSchemaRef:
			out = append(out, rec)
		}
	}
	return out
}

// p2IsFixtureScratch reports whether a slash-relative path is
// another package's transient in-tree fixture directory
// (internal/<pkg>/.<name>): test scratch that lives inside the
// checkout only while its own package's tests run. Such
// directories are not repository content, and under a parallel
// `go test ./...` run they appear and vanish independently of
// anything the gate does.
func p2IsFixtureScratch(rel string) bool {
	parts := strings.Split(rel, "/")
	return len(parts) == 3 && parts[0] == "internal" && strings.HasPrefix(parts[2], ".")
}

// p2CopyRepoTree copies the checkout's working tree to dst,
// byte-for-byte with modes and symlink targets, excluding
// exactly two classes: .git (the snapshot walk's own exclusion
// — the snapshot store records a workspace, not a repository's
// internals) and other packages' transient fixture scratch
// (p2IsFixtureScratch), whose mid-walk vanishing would
// otherwise fail a walk over the live tree under a parallel
// full-suite run. It returns the number of files copied.
func p2CopyRepoTree(t *testing.T, src, dst string) int {
	t.Helper()
	files := 0
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(dst, info.Mode().Perm())
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		switch {
		case d.IsDir():
			if d.Name() == ".git" || p2IsFixtureScratch(rel) {
				return filepath.SkipDir
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, b, info.Mode().Perm()); err != nil {
				return err
			}
			if err := os.Chmod(target, info.Mode().Perm()); err != nil {
				return err
			}
			files++
			return nil
		default:
			return fmt.Errorf("repo tree holds a special file at %s", rel)
		}
	})
	if err != nil {
		t.Fatalf("copy this repository's tree: %v", err)
	}
	return files
}

// p2Git runs git in dir and returns its stdout.
func p2Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// p2IsScratchLine reports whether a porcelain line is an
// untracked test-fixture scratch directory directly under a
// package dir (internal/<pkg>/.<name>/). Such directories are
// other packages' transient fixtures: under a parallel
// `go test ./...` run they appear and vanish independently of
// anything the fork does, so the leg-2 comparison filters
// exactly those lines — and nothing else. Tracked content,
// ordinary untracked files, and every other line must match
// byte-for-byte; the gate script's own sequential per-package
// runs have no foreign fixtures to filter.
func p2IsScratchLine(line string) bool {
	if !strings.HasPrefix(line, "?? internal/") || !strings.HasSuffix(line, "/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(line, "?? internal/"), "/")
	return len(parts) == 3 && strings.HasPrefix(parts[1], ".")
}

// p2GitStatus returns the base repo's filtered porcelain
// status (see p2IsScratchLine).
func p2GitStatus(t *testing.T, repoRoot string) string {
	t.Helper()
	raw := p2Git(t, repoRoot, "status", "--porcelain")
	var kept []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" || p2IsScratchLine(line) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// p2RefDigest hashes the base repo's for-each-ref listing:
// the refs-untouched evidence of leg 2 (and the assembled form
// of the plan's determinism discipline — state equality is
// proven on refs and records, never on wall-clock commit IDs).
func p2RefDigest(t *testing.T, repoRoot string) string {
	t.Helper()
	out := p2Git(t, repoRoot, "for-each-ref")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(out)))
}

// p2CrashChildMain is the child-process body for leg 8: it
// opens the gate run's stores by path, starts a watcher on the
// settlement journal, and runs run-5's Apply. When the
// journal's recorded done-marks reach the kill count — or the
// journal reaches the verified state, the commit-point
// boundary — the watcher exits the process with code 3,
// mid-Apply, at exactly the boundaries Task 8's crash hook
// enumerates. Exit 0 means Apply completed (the kill missed);
// exit 2 means the child failed before or inside Apply.
func p2CrashChildMain() {
	fail := func(code int, format string, args ...any) {
		fmt.Fprintf(os.Stderr, "phase2 gate crash child: %s\n", fmt.Sprintf(format, args...))
		os.Exit(code)
	}
	killAt, err := strconv.Atoi(os.Getenv(p2ChildKillAt))
	if err != nil {
		fail(2, "bad kill count: %v", err)
	}
	trace, err := store.Open(os.Getenv(p2ChildTrace))
	if err != nil {
		fail(2, "open trace: %v", err)
	}
	snaps, err := snapshot.Open(os.Getenv(p2ChildSnaps))
	if err != nil {
		fail(2, "open snaps: %v", err)
	}
	root := os.Getenv(p2ChildRoot)
	journalFile := filepath.Join(root, "journals", "parent.json")
	go func() {
		for {
			if b, err := os.ReadFile(journalFile); err == nil {
				if bytes.Count(b, []byte(`"done":true`)) >= killAt ||
					bytes.Contains(b, []byte(`"state":"verified"`)) {
					os.Exit(3)
				}
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()
	ctx := &settle.Context{Trace: trace, Snaps: snaps, Root: root}
	if _, err := settle.Apply(ctx, "parent", os.Getenv(p2ChildParentDir),
		os.Getenv(p2ChildRunScope), os.Getenv(p2ChildOp)); err != nil {
		fail(2, "Apply: %v", err)
	}
	os.Exit(0)
}

// p2SpawnCrashChild runs TestPhase2Gate in a child process in
// crash mode against the gate run's roots and returns its exit
// code and combined output.
func p2SpawnCrashChild(t *testing.T, tracePath, snapsPath, root, parentDir, runScope, op string, killAt int) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestPhase2Gate$")
	cmd.Env = append(os.Environ(),
		p2ChildEnv+"=1",
		p2ChildTrace+"="+tracePath,
		p2ChildSnaps+"="+snapsPath,
		p2ChildRoot+"="+root,
		p2ChildParentDir+"="+parentDir,
		p2ChildRunScope+"="+runScope,
		p2ChildOp+"="+op,
		p2ChildKillAt+"="+strconv.Itoa(killAt),
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

func TestPhase2Gate(t *testing.T) { // AC-F5-6; AC-F6-1, AC-F6-2, AC-F6-3, AC-F6-4 assembled
	// Child-process mode for leg 8 (see the file comment): no
	// skip, no separate helper test.
	if os.Getenv(p2ChildEnv) != "" {
		p2CrashChildMain()
		return
	}

	rootID := record.RootWitnessRecordID()

	// Leg 1 — trace/snapshot stores opened in a temp dir; the
	// settle Context.Root on a btrfs scratch dir under the
	// working directory — never the temp dir: Fork selects its
	// carrier on Root/layers, and on a tmpfs root selection
	// returns kernel-overlay, silently discharging leg 2
	// through the wrong carrier (AC-F5-6 names the copy
	// carrier).
	tmp := t.TempDir()
	tracePath := filepath.Join(tmp, "trace.db")
	ts, err := store.Open(tracePath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = ts.Close() })
	snapsPath := filepath.Join(tmp, "snaps.git")
	snaps, err := snapshot.Open(snapsPath)
	if err != nil {
		t.Fatalf("snapshot.Open: %v", err)
	}

	// This repository — the checkout the test runs in, located
	// from this file's path — is prepared for leg 2: the
	// working tree is copied (p2CopyRepoTree: excluding .git
	// and other packages' transient fixture scratch, whose
	// mid-walk vanishing under a parallel full-suite run would
	// otherwise fail a snapshot walk over the live tree) and
	// the snapshot is taken over the copy. Leg 2's git-status,
	// ref-digest, and base-tree assertions still run against
	// the live checkout itself.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: no file information")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if st, err := os.Stat(filepath.Join(repoRoot, ".git")); err != nil || !st.IsDir() {
		t.Fatalf("repo root %s: no .git directory (stat: %v)", repoRoot, err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("repo root %s: no go.mod: %v", repoRoot, err)
	}
	repoCopy := filepath.Join(tmp, "repo-copy")
	if n := p2CopyRepoTree(t, repoRoot, repoCopy); n < 150 {
		t.Fatalf("repo copy holds %d files, want the repository's working tree (150+)", n)
	}
	repoCommit, err := snaps.Snapshot(repoCopy, map[string]string{"run": "phase2-gate-repo"})
	if err != nil {
		t.Fatalf("Snapshot(this repository): %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	scratch, err := os.MkdirTemp(wd, ".phase2-gate-")
	if err != nil {
		t.Fatalf("create btrfs scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	root := filepath.Join(scratch, "settle-root")
	ctx := &settle.Context{Trace: ts, Snaps: snaps, Root: root}

	// A fixture git repo as the parent workspace (files in a/,
	// x/, old.txt); snapshot it; parent scope adopted at that
	// commit.
	parentDir := filepath.Join(scratch, "parent")
	p2Write(t, parentDir, "a/b.txt", "b v1\n")
	p2Write(t, parentDir, "a/b-c.txt", "b-c v1\n")
	p2Write(t, parentDir, "x/x.txt", "x v1\n")
	p2Write(t, parentDir, "old.txt", "old\n")
	p2Write(t, parentDir, "p.txt", "p v1\n")
	p2GitCommit(t, parentDir)
	parent0, err := snaps.Snapshot(parentDir, map[string]string{"run": "phase2-gate"})
	if err != nil {
		t.Fatalf("Snapshot(parent): %v", err)
	}
	if err := snaps.Fork("parent", parent0); err != nil {
		t.Fatalf("Fork parent scope: %v", err)
	}

	// Leg 2 — AC-F5-6: fork THIS repository via the copy
	// carrier. The forked layer must report the copy driver —
	// the selection record is the evidence the AC ran as
	// specified — and the base repo must be exactly as it was:
	// status unchanged, refs untouched, and a probe file
	// written in the layer absent from the base working tree.
	if err := snaps.Fork("repo-parent", repoCommit); err != nil {
		t.Fatalf("Fork repo-parent scope: %v", err)
	}
	statusBefore := p2GitStatus(t, repoRoot)
	refsBefore := p2RefDigest(t, repoRoot)
	repoLayer, err := settle.Fork(ctx, "repo-parent", "repo-run")
	if err != nil {
		t.Fatalf("settle.Fork(this repository): %v", err)
	}
	if repoLayer.Driver != carrier.DriverCopy {
		t.Fatalf("repo fork layer driver = %q, want %q (the selection record is the AC-F5-6 evidence)", repoLayer.Driver, carrier.DriverCopy)
	}
	sel, err := carrier.Select(filepath.Join(root, "layers"))
	if err != nil {
		t.Fatalf("carrier.Select(layers): %v", err)
	}
	if sel.Driver != carrier.DriverCopy {
		t.Fatalf("selection on the settle layers root = %q, want %q (probes: %v)", sel.Driver, carrier.DriverCopy, sel.Probes)
	}
	// The layer is the checkout's own working tree: hallmark
	// files are present, including this gate's own source —
	// uncommitted at the gate's first runs, so its presence
	// proves the fork took the live tree, not a committed
	// archive.
	for _, hallmark := range []string{
		"go.mod",
		"docs/plans/2026-11-19-replay-core-phase-2-plan.html",
		"internal/conformance/phase2_gate_test.go",
	} {
		if _, err := os.Stat(filepath.Join(repoLayer.Dir, filepath.FromSlash(hallmark))); err != nil {
			t.Fatalf("repo layer lacks %s: %v (the fork must carry this repository's working tree)", hallmark, err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoLayer.Dir, "phase2-gate-probe.txt"), []byte("probe\n"), 0o644); err != nil {
		t.Fatalf("write probe file in repo layer: %v", err)
	}
	if got := p2GitStatus(t, repoRoot); got != statusBefore {
		t.Fatalf("base repo status changed across the fork:\nbefore:\n%s\nafter:\n%s", statusBefore, got)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "phase2-gate-probe.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("probe file in the base working tree: stat err = %v, want not-exist (the layer's writes must not leak)", err)
	}
	if got := p2RefDigest(t, repoRoot); got != refsBefore {
		t.Fatalf("base repo refs changed across the fork: before %s, after %s", refsBefore, got)
	}

	// Leg 3 — run-1 forked from the fixture parent; the run
	// adds x/y.txt, modifies a/b.txt, deletes old.txt
	// (snapshot + advance). Two trace records on run-1's path
	// give the coming apply fact a real tail to cite.
	run1, err := settle.Fork(ctx, "parent", "run-1")
	if err != nil {
		t.Fatalf("Fork run-1: %v", err)
	}
	p2Write(t, run1.Dir, "x/y.txt", "y v1\n")
	p2Write(t, run1.Dir, "a/b.txt", "b v2 run-1\n")
	if err := os.Remove(filepath.Join(run1.Dir, "old.txt")); err != nil {
		t.Fatalf("run-1 deletes old.txt: %v", err)
	}
	p2SnapshotAdvance(t, snaps, run1.Dir, "run-1", parent0)
	r1a := gateAppendFact(t, ts, "run-1", `{"n":"run-1-start"}`, []string{rootID})
	r1b := gateAppendFact(t, ts, "run-1", `{"n":"run-1-step"}`, []string{r1a})

	// Leg 4 — the parent advances disjointly: modifies
	// a/b-c.txt and p.txt (snapshot + advance).
	p2Write(t, parentDir, "a/b-c.txt", "b-c v2 parent\n")
	p2Write(t, parentDir, "p.txt", "p v2 parent\n")
	parent1 := p2SnapshotAdvance(t, snaps, parentDir, "parent", parent0)

	// Leg 5 — Apply(run-1): the parent tree equals the
	// hand-computed union byte-for-byte, and the apply fact
	// cites run-1's tail (AC-F6-2 + AC-F6-7, assembled).
	expected1 := filepath.Join(scratch, "expected-apply")
	p2Write(t, expected1, "a/b.txt", "b v2 run-1\n")
	p2Write(t, expected1, "a/b-c.txt", "b-c v2 parent\n")
	p2Write(t, expected1, "x/x.txt", "x v1\n")
	p2Write(t, expected1, "x/y.txt", "y v1\n")
	p2Write(t, expected1, "p.txt", "p v2 parent\n")
	frontier1, err := ts.Frontier("run-1")
	if err != nil {
		t.Fatalf("Frontier(run-1): %v", err)
	}
	if frontier1 != r1b {
		t.Fatalf("Frontier(run-1) = %s, want the run's tail %s", frontier1, r1b)
	}
	applyReceipt, err := settle.Apply(ctx, "parent", parentDir, "run-1", "op-gate-apply-1")
	if err != nil {
		t.Fatalf("Apply(run-1): %v", err)
	}
	if got, want := p2WalkHash(t, parentDir), p2WalkHash(t, expected1); got != want {
		t.Fatalf("parent tree after Apply(run-1): walk hash %s, want the hand-computed union %s", got, want)
	}
	parentHead1, err := snaps.ScopeHead("parent")
	if err != nil {
		t.Fatalf("ScopeHead(parent) after apply: %v", err)
	}
	if parentHead1 == parent1 {
		t.Fatalf("parent head did not move across Apply(run-1): still %s", parent1)
	}
	facts1 := p2SettleFacts(t, ts, "run-1")
	if len(facts1) != 1 {
		t.Fatalf("settle facts on run-1 = %d, want exactly 1", len(facts1))
	}
	applyFact := facts1[0]
	if applyFact.ID != applyReceipt.RecordID {
		t.Fatalf("apply fact ID = %s, receipt = %s", applyFact.ID, applyReceipt.RecordID)
	}
	if ref, _ := p2StringField(applyFact.Record, "schema_ref"); ref != store.SettleApplySchemaRef {
		t.Fatalf("apply fact schema_ref = %q, want %q", ref, store.SettleApplySchemaRef)
	}
	if got := p2CausedBy(t, applyFact); !reflect.DeepEqual(got, []string{r1b}) {
		t.Fatalf("apply fact caused_by = %v, want [run-1's tail %s]", got, r1b)
	}
	applyBody := p2FactBody(t, applyFact)
	for key, want := range map[string]string{
		"op": "op-gate-apply-1", "parent_scope": "parent", "run_scope": "run-1",
		"fork_commit": parent0, "result_commit": parentHead1,
	} {
		if got, _ := p2StringField(applyBody, key); got != want {
			t.Fatalf("apply fact body[%q] = %q, want %q", key, got, want)
		}
	}
	var settledPaths []string
	for _, p := range applyBody.Pairs {
		if p.Key == "settled_paths" {
			for _, e := range p.Value.Elems {
				settledPaths = append(settledPaths, e.Str)
			}
		}
	}
	if want := []string{"a/b.txt", "old.txt", "x/y.txt"}; !reflect.DeepEqual(settledPaths, want) {
		t.Fatalf("apply fact settled_paths = %v, want %v", settledPaths, want)
	}

	// Leg 6 — run-2 forked; both run-2 and the parent modify
	// the same path; Apply(run-2) refuses with ErrOverlap and
	// the parent head and tree hash are exactly as before
	// (AC-F6-3, assembled).
	run2, err := settle.Fork(ctx, "parent", "run-2")
	if err != nil {
		t.Fatalf("Fork run-2: %v", err)
	}
	p2Write(t, run2.Dir, "a/b.txt", "b v3 run-2\n")
	p2SnapshotAdvance(t, snaps, run2.Dir, "run-2", parentHead1)
	p2Write(t, parentDir, "a/b.txt", "b v3 parent\n")
	parentHead2 := p2SnapshotAdvance(t, snaps, parentDir, "parent", parentHead1)
	hashBeforeOverlap := p2WalkHash(t, parentDir)
	if _, err := settle.Apply(ctx, "parent", parentDir, "run-2", "op-gate-apply-2"); !errors.Is(err, settle.ErrOverlap) {
		t.Fatalf("Apply(run-2) over a shared path: err = %v, want ErrOverlap", err)
	}
	if head, err := snaps.ScopeHead("parent"); err != nil || head != parentHead2 {
		t.Fatalf("parent head after refused Apply = %s, %v; want %s unmoved", head, err, parentHead2)
	}
	if got := p2WalkHash(t, parentDir); got != hashBeforeOverlap {
		t.Fatalf("parent tree changed across refused Apply: walk hash %s, want %s", got, hashBeforeOverlap)
	}
	if facts := p2SettleFacts(t, ts, "run-2"); len(facts) != 0 {
		t.Fatalf("refused Apply minted %d facts on run-2, want 0", len(facts))
	}

	// Leg 7 — run-3 forked; parent idle; Select(run-3)
	// succeeds. The parent then advances past a fresh fork:
	// run-4 is forked, the parent advances, and Select(run-4)
	// is refused with ErrParentAdvanced (AC-F6-1, assembled).
	run3, err := settle.Fork(ctx, "parent", "run-3")
	if err != nil {
		t.Fatalf("Fork run-3: %v", err)
	}
	p2Write(t, run3.Dir, "z.txt", "z v1\n")
	p2Write(t, run3.Dir, "x/x.txt", "x v2 run-3\n")
	run3Head := p2SnapshotAdvance(t, snaps, run3.Dir, "run-3", parentHead2)
	r3a := gateAppendFact(t, ts, "run-3", `{"n":"run-3-step"}`, []string{rootID})
	selectReceipt, err := settle.Select(ctx, "parent", parentDir, "run-3", "op-gate-select-3")
	if err != nil {
		t.Fatalf("Select(run-3): %v", err)
	}
	if got, want := p2WalkHash(t, parentDir), p2WalkHash(t, run3.Dir); got != want {
		t.Fatalf("parent tree after Select(run-3): walk hash %s, want the run head's tree %s", got, want)
	}
	if head, err := snaps.ScopeHead("parent"); err != nil || head != run3Head {
		t.Fatalf("parent head after Select = %s, %v; want run-3 head %s", head, err, run3Head)
	}
	facts3 := p2SettleFacts(t, ts, "run-3")
	if len(facts3) != 1 || facts3[0].ID != selectReceipt.RecordID {
		t.Fatalf("settle facts on run-3 = %v, want exactly the select receipt %s", facts3, selectReceipt.RecordID)
	}
	if ref, _ := p2StringField(facts3[0].Record, "schema_ref"); ref != store.SettleSelectSchemaRef {
		t.Fatalf("select fact schema_ref = %q, want %q", ref, store.SettleSelectSchemaRef)
	}
	if got := p2CausedBy(t, facts3[0]); !reflect.DeepEqual(got, []string{r3a}) {
		t.Fatalf("select fact caused_by = %v, want [run-3's tail %s]", got, r3a)
	}
	selectBody := p2FactBody(t, facts3[0])
	for key, want := range map[string]string{
		"op": "op-gate-select-3", "parent_scope": "parent", "run_scope": "run-3",
		"parent_head": parentHead2, "run_head": run3Head,
	} {
		if got, _ := p2StringField(selectBody, key); got != want {
			t.Fatalf("select fact body[%q] = %q, want %q", key, got, want)
		}
	}

	if _, err := settle.Fork(ctx, "parent", "run-4"); err != nil {
		t.Fatalf("Fork run-4: %v", err)
	}
	p2Write(t, parentDir, "p.txt", "p v3 parent\n")
	parentHead4 := p2SnapshotAdvance(t, snaps, parentDir, "parent", run3Head)
	if _, err := settle.Select(ctx, "parent", parentDir, "run-4", "op-gate-select-4"); !errors.Is(err, settle.ErrParentAdvanced) {
		t.Fatalf("Select(run-4) onto an advanced parent: err = %v, want ErrParentAdvanced", err)
	}
	if head, err := snaps.ScopeHead("parent"); err != nil || head != parentHead4 {
		t.Fatalf("parent head after refused Select = %s, %v; want %s unmoved", head, err, parentHead4)
	}
	if facts := p2SettleFacts(t, ts, "run-4"); len(facts) != 0 {
		t.Fatalf("refused Select minted %d facts on run-4, want 0", len(facts))
	}

	// Leg 8 — journal leg: run-5's apply is killed
	// mid-materialization by a child process (see the file
	// comment); Recover from this process leaves the parent
	// exactly pre- or post-apply (AC-F6-4, assembled). The
	// run's delta is eleven units — ten writes, one delete —
	// and the child dies at the third recorded done-mark.
	run5, err := settle.Fork(ctx, "parent", "run-5")
	if err != nil {
		t.Fatalf("Fork run-5: %v", err)
	}
	p2Write(t, run5.Dir, "a/b.txt", "b v4 run-5\n")
	p2Write(t, run5.Dir, "z.txt", "z v2 run-5\n")
	for i := 1; i <= 8; i++ {
		p2Write(t, run5.Dir, fmt.Sprintf("n/%02d.txt", i), fmt.Sprintf("n%d\n", i))
	}
	if err := os.Remove(filepath.Join(run5.Dir, "x/y.txt")); err != nil {
		t.Fatalf("run-5 deletes x/y.txt: %v", err)
	}
	p2SnapshotAdvance(t, snaps, run5.Dir, "run-5", parentHead4)
	preHash := p2WalkHash(t, parentDir)
	expected5 := filepath.Join(scratch, "expected-crash")
	p2Write(t, expected5, "a/b.txt", "b v4 run-5\n")
	p2Write(t, expected5, "a/b-c.txt", "b-c v2 parent\n")
	p2Write(t, expected5, "x/x.txt", "x v2 run-3\n")
	p2Write(t, expected5, "z.txt", "z v2 run-5\n")
	p2Write(t, expected5, "p.txt", "p v3 parent\n")
	for i := 1; i <= 8; i++ {
		p2Write(t, expected5, fmt.Sprintf("n/%02d.txt", i), fmt.Sprintf("n%d\n", i))
	}
	postHash := p2WalkHash(t, expected5)

	code, out := p2SpawnCrashChild(t, tracePath, snapsPath, root, parentDir, "run-5", "op-gate-apply-5", 3)
	if code != 3 {
		t.Fatalf("crash child exit = %d, want 3 (killed mid-materialization); child output:\n%s", code, out)
	}
	if err := settle.Recover(ctx, "parent"); err != nil {
		t.Fatalf("Recover after the crash: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "journals", "parent.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("journal after Recover: stat err = %v, want gone", err)
	}
	run5FactID := ""
	switch got := p2WalkHash(t, parentDir); got {
	case preHash:
		if facts := p2SettleFacts(t, ts, "run-5"); len(facts) != 0 {
			t.Fatalf("rolled-back crash left %d facts on run-5, want 0", len(facts))
		}
		if head, err := snaps.ScopeHead("parent"); err != nil || head != parentHead4 {
			t.Fatalf("parent head after rollback = %s, %v; want %s unmoved", head, err, parentHead4)
		}
	case postHash:
		facts := p2SettleFacts(t, ts, "run-5")
		if len(facts) != 1 {
			t.Fatalf("completed crash recovery left %d facts on run-5, want exactly 1", len(facts))
		}
		if ref, _ := p2StringField(facts[0].Record, "schema_ref"); ref != store.SettleApplySchemaRef {
			t.Fatalf("recovered fact schema_ref = %q, want %q", ref, store.SettleApplySchemaRef)
		}
		run5FactID = facts[0].ID
		if head, err := snaps.ScopeHead("parent"); err != nil || head == parentHead4 {
			t.Fatalf("parent head after completion = %s, %v; want advanced past %s", head, err, parentHead4)
		}
	default:
		t.Fatalf("parent walk hash after Recover = %s, neither pre-apply %s nor post-apply %s — recovery did not converge", got, preHash, postHash)
	}

	// Leg 9 — close: CollectGarbage on the trace store —
	// every settlement fact survives (each is its path's
	// frontier or an ancestor of it); a cut published over
	// the settled history still resolves with an identical
	// digest. Two orphan records branching off run-1's chain,
	// cited by nothing and ancestral to no root, prove the
	// collection actually ran.
	settledFactIDs := []string{applyReceipt.RecordID, selectReceipt.RecordID}
	if run5FactID != "" {
		settledFactIDs = append(settledFactIDs, run5FactID)
	}
	// The orphans come first, the closing milestone second:
	// GC's roots are each path's tail, the milestones, and the
	// published cuts (a cut addresses an ordinal prefix of its
	// path), so a record is collectable exactly when no root's
	// closure or addressed prefix reaches it. The milestone —
	// appended after the orphans and citing the apply fact —
	// becomes run-1's tail, and neither its closure nor the
	// cut's prefix (through the apply fact) reaches the orphan
	// branch.
	orphan1 := gateAppendFact(t, ts, "run-1", `{"n":"gate-orphan-1"}`, []string{r1a})
	orphan2 := gateAppendFact(t, ts, "run-1", `{"n":"gate-orphan-2"}`, []string{orphan1})
	parentHeadNow, err := snaps.ScopeHead("parent")
	if err != nil {
		t.Fatalf("ScopeHead(parent) at close: %v", err)
	}
	milestone, err := ts.AppendMilestone("run-1", "phase2-gate-close", parentHeadNow, []string{applyReceipt.RecordID})
	if err != nil {
		t.Fatalf("AppendMilestone over the settled history: %v", err)
	}
	if frontier, err := ts.Frontier("run-1"); err != nil || frontier != milestone.RecordID {
		t.Fatalf("Frontier(run-1) at close = %s, %v; want the closing milestone %s (the tail whose closure excludes the orphans)", frontier, err, milestone.RecordID)
	}
	cutID, err := slice.PublishCut(ts, "run-1", applyReceipt.RecordID)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	cutPre, err := slice.ResolveCut(ts, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut (pre-GC): %v", err)
	}
	digestPre, err := cutPre.Digest()
	if err != nil {
		t.Fatalf("cut Digest (pre-GC): %v", err)
	}
	cutCoversFact := false
	for _, id := range cutPre.Order() {
		if id == applyReceipt.RecordID {
			cutCoversFact = true
		}
	}
	if !cutCoversFact {
		t.Fatalf("published cut does not cover the settled history: apply fact %s absent from its slice", applyReceipt.RecordID)
	}
	report, err := ts.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if report.Collected < 2 {
		t.Fatalf("CollectGarbage collected %d records, want at least the 2 planted orphans", report.Collected)
	}
	for _, id := range settledFactIDs {
		if _, err := ts.GetRecord(id); err != nil {
			t.Errorf("settlement fact %s did not survive GC: %v", id, err)
		}
	}
	for _, id := range []string{orphan1, orphan2} {
		if _, err := ts.GetRecord(id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetRecord(orphan %s) after GC: err = %v, want ErrNotFound (collected)", id, err)
		}
	}
	cutPost, err := slice.ResolveCut(ts, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut (post-GC): %v", err)
	}
	digestPost, err := cutPost.Digest()
	if err != nil {
		t.Fatalf("cut Digest (post-GC): %v", err)
	}
	if digestPost != digestPre {
		t.Fatalf("cut digest changed across GC: pre %s, post %s", digestPre, digestPost)
	}
}
