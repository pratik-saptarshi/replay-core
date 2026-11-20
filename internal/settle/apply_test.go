package settle_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/settle"
	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The Apply tests (plan Task 7) build on Task 6's fixture: the
// parent tree is extended before the fork by editing parentDir,
// snapshotting, and advancing the parent scope — the fork point
// is then the extended head. The settle Context.Root stays on
// the fixture's btrfs scratch dir (amendment F1), so the carrier
// under test is the copy carrier throughout.

// advanceParent snapshots parentDir as it currently stands and
// advances the parent scope to the new commit.
func advanceParent(t *testing.T, f *fixture) string {
	t.Helper()
	head, err := f.snaps.ScopeHead("parent")
	if err != nil {
		t.Fatalf("ScopeHead(parent): %v", err)
	}
	c, err := f.snaps.Snapshot(f.parentDir, map[string]string{"run": "settle-fixture"})
	if err != nil {
		t.Fatalf("Snapshot parent: %v", err)
	}
	if err := f.snaps.AdvanceScope("parent", head, c); err != nil {
		t.Fatalf("AdvanceScope parent: %v", err)
	}
	return c
}

// extendParentTree mutates the parent directory and advances
// the parent scope over the mutation, returning the new head —
// the fork point for a subsequent settle.Fork.
func extendParentTree(t *testing.T, f *fixture, build func(dir string)) string {
	t.Helper()
	build(f.parentDir)
	return advanceParent(t, f)
}

// walkHash is a deterministic hash of a directory tree: every
// entry's relative path, kind, mode, and content (file bytes or
// symlink target), in walk order. Entries named .git are
// skipped, mirroring the snapshot walk's law.
func walkHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
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
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			fmt.Fprintf(h, "dir %s\n", rel)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "symlink %s -> %s\n", rel, target)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "file %s mode %o size %d\n", rel, info.Mode().Perm(), len(b))
			h.Write(b)
			h.Write([]byte("\n"))
		default:
			return fmt.Errorf("walkHash: unsupported entry %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walkHash(%s): %v", dir, err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// factStringList extracts a string-array field from a fact body.
func factStringList(t *testing.T, body canonical.Value, key string) []string {
	t.Helper()
	for _, p := range body.Pairs {
		if p.Key == key {
			out := make([]string, 0, len(p.Value.Elems))
			for _, e := range p.Value.Elems {
				out = append(out, e.Str)
			}
			return out
		}
	}
	t.Fatalf("fact body has no %q field", key)
	return nil
}

// applyFacts returns the run scope's apply facts.
func applyFacts(t *testing.T, f *fixture, runScope string) []store.StoredRecord {
	t.Helper()
	var out []store.StoredRecord
	for _, rec := range f.settleFacts(t, runScope) {
		if ref, _ := stringField(rec.Record, "schema_ref"); ref == store.SettleApplySchemaRef {
			out = append(out, rec)
		}
	}
	return out
}

// unionOf recomputes the expected union map from the three
// workspace maps the way the plan's step (4) prescribes: the
// fork map with each side's delta against the fork map applied
// in turn — a side contributes only the paths it added, changed
// (blob or mode), or deleted relative to the fork, never its
// unchanged entries (the deltas are disjoint, so the order
// cannot matter).
func unionOf(base, parent, run map[string]snapshot.WorkspaceEntry) map[string]snapshot.WorkspaceEntry {
	out := map[string]snapshot.WorkspaceEntry{}
	for rel, e := range base {
		out[rel] = e
	}
	for _, target := range []map[string]snapshot.WorkspaceEntry{parent, run} {
		for rel, e := range target {
			if old, ok := base[rel]; !ok || old != e {
				out[rel] = e
			}
		}
		for rel := range base {
			if _, ok := target[rel]; !ok {
				delete(out, rel)
			}
		}
	}
	return out
}

// TestApplySettlesDisjointUnionByteExact (plan Task 7, Step 3;
// AC-F6-2, the gate shape in miniature): disjoint change sets
// on both sides — including the prefix near-miss a/b.txt vs
// a/b-c.txt — settle to exactly the expected union: the parent
// directory byte-for-byte, the result commit's file map equal
// to the union map, and the apply fact's settled_paths equal to
// the run delta, sorted.
func TestApplySettlesDisjointUnionByteExact(t *testing.T) {
	f := newFixture(t)
	forkHead := extendParentTree(t, f, func(dir string) {
		writeTestFile(t, filepath.Join(dir, "a", "b.txt"), "b original\n", 0o644)
		writeTestFile(t, filepath.Join(dir, "a", "b-c.txt"), "b-c original\n", 0o644)
		writeTestFile(t, filepath.Join(dir, "old.txt"), "old\n", 0o644)
		writeTestFile(t, filepath.Join(dir, "p.txt"), "p original\n", 0o644)
	})
	layer, err := settle.Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	// The run changes a/b.txt, adds x/y.txt, deletes old.txt.
	writeTestFile(t, filepath.Join(layer.Dir, "a", "b.txt"), "b run\n", 0o644)
	writeTestFile(t, filepath.Join(layer.Dir, "x", "y.txt"), "y run\n", 0o644)
	if err := os.Remove(filepath.Join(layer.Dir, "old.txt")); err != nil {
		t.Fatalf("run deletes old.txt: %v", err)
	}
	runHead := f.advanceRun(t, layer, "run-a", forkHead)

	// The parent changes a/b-c.txt (the near-miss) and p.txt.
	writeTestFile(t, filepath.Join(f.parentDir, "a", "b-c.txt"), "b-c parent\n", 0o644)
	writeTestFile(t, filepath.Join(f.parentDir, "p.txt"), "p parent\n", 0o644)
	parentHead := advanceParent(t, f)

	receipt, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-a", "op-1")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The parent directory serves exactly the expected union,
	// byte-for-byte: walk-hash against a hand-built tree.
	expectedDir := filepath.Join(filepath.Dir(f.root), "expected")
	writeTestFile(t, filepath.Join(expectedDir, "keep.txt"), "keep\n", 0o644)
	writeTestFile(t, filepath.Join(expectedDir, "mod.txt"), "original\n", 0o644)
	writeTestFile(t, filepath.Join(expectedDir, "a", "b.txt"), "b run\n", 0o644)
	writeTestFile(t, filepath.Join(expectedDir, "a", "b-c.txt"), "b-c parent\n", 0o644)
	writeTestFile(t, filepath.Join(expectedDir, "p.txt"), "p parent\n", 0o644)
	writeTestFile(t, filepath.Join(expectedDir, "x", "y.txt"), "y run\n", 0o644)
	if got, want := walkHash(t, f.parentDir), walkHash(t, expectedDir); got != want {
		t.Fatalf("parentDir walk hash = %s, want expected union %s", got, want)
	}

	// The parent scope head is the result commit, and its file
	// map is the union of the three maps.
	result, err := f.snaps.ScopeHead("parent")
	if err != nil {
		t.Fatalf("ScopeHead(parent): %v", err)
	}
	forkMap, err := f.snaps.WorkspaceFiles(forkHead)
	if err != nil {
		t.Fatalf("WorkspaceFiles(fork): %v", err)
	}
	runMap, err := f.snaps.WorkspaceFiles(runHead)
	if err != nil {
		t.Fatalf("WorkspaceFiles(run): %v", err)
	}
	parentMap, err := f.snaps.WorkspaceFiles(parentHead)
	if err != nil {
		t.Fatalf("WorkspaceFiles(parent): %v", err)
	}
	resultMap, err := f.snaps.WorkspaceFiles(result)
	if err != nil {
		t.Fatalf("WorkspaceFiles(result): %v", err)
	}
	if want := unionOf(forkMap, parentMap, runMap); !reflect.DeepEqual(resultMap, want) {
		t.Fatalf("WorkspaceFiles(result) = %v, want union %v", resultMap, want)
	}

	// Exactly one apply fact, minted last; its settled_paths
	// are the run delta, sorted.
	facts := applyFacts(t, f, "run-a")
	if len(facts) != 1 || facts[0].ID != receipt.RecordID {
		t.Fatalf("apply facts on run-a = %v, want exactly the apply receipt %s", facts, receipt.RecordID)
	}
	body := factBody(t, facts[0])
	for key, want := range map[string]string{
		"op": "op-1", "parent_scope": "parent", "run_scope": "run-a",
		"fork_commit": forkHead, "result_commit": result,
	} {
		if got, _ := stringField(body, key); got != want {
			t.Fatalf("apply fact body[%q] = %q, want %q", key, got, want)
		}
	}
	wantPaths := []string{"a/b.txt", "old.txt", "x/y.txt"}
	if got := factStringList(t, body, "settled_paths"); !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("settled_paths = %v, want run delta sorted %v", got, wantPaths)
	}
}

// TestApplySettlesModeOnlyChange (plan Task 7, Step 3;
// amendment F2): a mode-only change — the same blob under a new
// mode — is a delta change: it lands in the union with its new
// mode and is named in settled_paths.
func TestApplySettlesModeOnlyChange(t *testing.T) {
	f := newFixture(t)
	forkHead := extendParentTree(t, f, func(dir string) {
		writeTestFile(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\n", 0o644)
	})
	layer, err := settle.Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	// The run chmods run.sh: same blob, 100644 → 100755.
	if err := os.Chmod(filepath.Join(layer.Dir, "run.sh"), 0o755); err != nil {
		t.Fatalf("run chmods run.sh: %v", err)
	}
	f.advanceRun(t, layer, "run-a", forkHead)

	// The parent edits a disjoint file.
	writeTestFile(t, filepath.Join(f.parentDir, "mod.txt"), "parent edit\n", 0o644)
	advanceParent(t, f)

	if _, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-a", "op-1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	result, err := f.snaps.ScopeHead("parent")
	if err != nil {
		t.Fatalf("ScopeHead(parent): %v", err)
	}
	files, err := f.snaps.WorkspaceFiles(result)
	if err != nil {
		t.Fatalf("WorkspaceFiles(result): %v", err)
	}
	if got := files["run.sh"].Mode; got != "100755" {
		t.Fatalf("settled run.sh mode = %q, want %q (mode-only change is a change)", got, "100755")
	}
	info, err := os.Stat(filepath.Join(f.parentDir, "run.sh"))
	if err != nil {
		t.Fatalf("stat parent run.sh: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("parent run.sh mode = %o, want 755", info.Mode().Perm())
	}

	facts := applyFacts(t, f, "run-a")
	if len(facts) != 1 {
		t.Fatalf("apply facts on run-a = %d, want 1", len(facts))
	}
	got := factStringList(t, factBody(t, facts[0]), "settled_paths")
	if !reflect.DeepEqual(got, []string{"run.sh"}) {
		t.Fatalf("settled_paths = %v, want [run.sh]", got)
	}
	if !sort.IsSorted(sort.StringSlice(got)) {
		t.Fatalf("settled_paths = %v, want sorted", got)
	}
}

// TestApplySettlesAliasShapedDisjoint (plan Task 7, Step 3;
// amendment F5): an alias-shaped near-miss that is genuinely
// disjoint must settle — refusal is keyed to the delta census,
// never to the layer's own path spelling. The fork tree
// contains link → sub (a symlink); the run modifies the file
// through the alias (link/f.txt, canonical: sub/f.txt) and the
// parent modifies sub/g.txt (canonical: sub/g.txt). Apply
// succeeds: the parent tree serves sub/f.txt with the run's
// bytes and sub/g.txt with the parent's, byte-exact.
func TestApplySettlesAliasShapedDisjoint(t *testing.T) {
	f := newFixture(t)
	forkHead := extendParentTree(t, f, func(dir string) {
		writeTestFile(t, filepath.Join(dir, "sub", "f.txt"), "f original\n", 0o644)
		writeTestFile(t, filepath.Join(dir, "sub", "g.txt"), "g original\n", 0o644)
		if err := os.Symlink("sub", filepath.Join(dir, "link")); err != nil {
			t.Fatalf("create link -> sub: %v", err)
		}
	})
	layer, err := settle.Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	// The run modifies link/f.txt — through the alias, so the
	// bytes land on sub/f.txt in its tree.
	writeTestFile(t, filepath.Join(layer.Dir, "link", "f.txt"), "f run\n", 0o644)
	f.advanceRun(t, layer, "run-a", forkHead)

	// The parent modifies sub/g.txt — genuinely disjoint.
	writeTestFile(t, filepath.Join(f.parentDir, "sub", "g.txt"), "g parent\n", 0o644)
	advanceParent(t, f)

	if _, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-a", "op-1"); err != nil {
		t.Fatalf("Apply of genuinely disjoint alias-shaped changes: %v (no false refusal)", err)
	}
	if got := readParentFile(t, f, "sub/f.txt"); got != "f run\n" {
		t.Fatalf("parent sub/f.txt = %q, want run's bytes %q", got, "f run\n")
	}
	if got := readParentFile(t, f, "sub/g.txt"); got != "g parent\n" {
		t.Fatalf("parent sub/g.txt = %q, want parent's bytes %q", got, "g parent\n")
	}
	// The alias itself survives settlement untouched.
	target, err := os.Readlink(filepath.Join(f.parentDir, "link"))
	if err != nil || target != "sub" {
		t.Fatalf("parent link = %q, %v; want symlink to sub intact", target, err)
	}
}

// TestApplySettlesReplacedAliasDisjoint (plan Task 7,
// amendment F5; pins review finding F-1): the run REPLACES the
// alias with a real directory — its delta is {link deleted,
// link/f.txt added} — while the parent modifies sub/g.txt.
// Canonical form resolves a path's ancestors only, never the
// path itself, so the deleted symlink's canonical form is link,
// not sub: it does not drag everything under its target into
// the run's delta. The changes are genuinely disjoint and
// settle: the parent tree serves link/f.txt with the run's
// bytes (under a real directory), sub/g.txt with the parent's,
// and sub/f.txt with the fork's.
func TestApplySettlesReplacedAliasDisjoint(t *testing.T) {
	f := newFixture(t)
	forkHead := extendParentTree(t, f, func(dir string) {
		writeTestFile(t, filepath.Join(dir, "sub", "f.txt"), "f original\n", 0o644)
		writeTestFile(t, filepath.Join(dir, "sub", "g.txt"), "g original\n", 0o644)
		if err := os.Symlink("sub", filepath.Join(dir, "link")); err != nil {
			t.Fatalf("create link -> sub: %v", err)
		}
	})
	layer, err := settle.Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	// The run replaces the alias with a real directory and
	// writes link/f.txt inside it.
	if err := os.Remove(filepath.Join(layer.Dir, "link")); err != nil {
		t.Fatalf("run removes the link alias: %v", err)
	}
	writeTestFile(t, filepath.Join(layer.Dir, "link", "f.txt"), "f run\n", 0o644)
	f.advanceRun(t, layer, "run-a", forkHead)

	// The parent modifies sub/g.txt — genuinely disjoint from
	// the run's {link, link/f.txt}.
	writeTestFile(t, filepath.Join(f.parentDir, "sub", "g.txt"), "g parent\n", 0o644)
	advanceParent(t, f)

	if _, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-a", "op-1"); err != nil {
		t.Fatalf("Apply of genuinely disjoint replaced-alias changes: %v (no false refusal)", err)
	}
	if got := readParentFile(t, f, "link/f.txt"); got != "f run\n" {
		t.Fatalf("parent link/f.txt = %q, want run's bytes %q", got, "f run\n")
	}
	if got := readParentFile(t, f, "sub/g.txt"); got != "g parent\n" {
		t.Fatalf("parent sub/g.txt = %q, want parent's bytes %q", got, "g parent\n")
	}
	if got := readParentFile(t, f, "sub/f.txt"); got != "f original\n" {
		t.Fatalf("parent sub/f.txt = %q, want fork's bytes %q", got, "f original\n")
	}
	// The alias is gone from the settled tree: link is now the
	// run's real directory.
	info, err := os.Lstat(filepath.Join(f.parentDir, "link"))
	if err != nil {
		t.Fatalf("lstat parent link: %v", err)
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		t.Fatalf("parent link mode = %v, want a real directory (the alias replaced)", info.Mode())
	}
	// Exactly one apply fact; its settled_paths are the run
	// delta — the deleted alias and the added file — sorted.
	facts := applyFacts(t, f, "run-a")
	if len(facts) != 1 {
		t.Fatalf("apply facts on run-a = %d, want 1", len(facts))
	}
	got := factStringList(t, factBody(t, facts[0]), "settled_paths")
	if want := []string{"link", "link/f.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("settled_paths = %v, want run delta sorted %v", got, want)
	}
}

// TestConsumeOnceAcrossVerbsWithApply (plan Task 7, Step 7;
// AC-F6-6, amendment F13): the consume-once scan is shared by
// every verb, so a run settled by any one of them is consumed
// for all — these cases pin the scan for Apply, the verb Task 6
// could only name in a test comment.
func TestConsumeOnceAcrossVerbsWithApply(t *testing.T) {
	f := newFixture(t)

	// After a successful Select: Apply on the same run scope is
	// refused as consumed.
	layer, err := settle.Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork run-a: %v", err)
	}
	writeTestFile(t, filepath.Join(layer.Dir, "added.txt"), "added\n", 0o644)
	f.advanceRun(t, layer, "run-a", f.parent0)
	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "run-a", "op-1"); err != nil {
		t.Fatalf("Select run-a: %v", err)
	}
	if _, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-a", "op-2"); !errors.Is(err, settle.ErrScopeConsumed) {
		t.Fatalf("Apply after Select: err = %v, want ErrScopeConsumed", err)
	}

	// After a successful Apply: Select, a second Apply, and
	// Discard on the same run scope are all refused as consumed.
	layer, err = settle.Fork(f.ctx, "parent", "run-b")
	if err != nil {
		t.Fatalf("Fork run-b: %v", err)
	}
	writeTestFile(t, filepath.Join(layer.Dir, "b-added.txt"), "b added\n", 0o644)
	forkHead, err := f.snaps.ScopeHead("parent")
	if err != nil {
		t.Fatalf("ScopeHead(parent): %v", err)
	}
	f.advanceRun(t, layer, "run-b", forkHead)
	if _, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-b", "op-3"); err != nil {
		t.Fatalf("Apply run-b: %v", err)
	}
	if _, err := settle.Select(f.ctx, "parent", f.parentDir, "run-b", "op-4"); !errors.Is(err, settle.ErrScopeConsumed) {
		t.Fatalf("Select after Apply: err = %v, want ErrScopeConsumed", err)
	}
	if _, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-b", "op-5"); !errors.Is(err, settle.ErrScopeConsumed) {
		t.Fatalf("second Apply: err = %v, want ErrScopeConsumed", err)
	}
	if _, err := settle.Discard(f.ctx, "run-b", "op-6"); !errors.Is(err, settle.ErrScopeConsumed) {
		t.Fatalf("Discard after Apply: err = %v, want ErrScopeConsumed", err)
	}
}

// assertNoStagingLeft checks the refusal (or settlement) left
// no staging behind under the settle root: staging is transient
// state, removed on every path.
func assertNoStagingLeft(t *testing.T, f *fixture) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.root, "staging"))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("staging left behind: %v", names)
	}
}

// TestApplyRefusesOverlapShapes (plan Task 7, Step 5; AC-F6-3
// — one fixture per overlap shape): any overlap makes Apply
// refuse with ErrOverlap naming the offending pair, and the
// parent scope head and the parentDir walk hash are exactly as
// before; no fact is minted and no staging is left behind. The
// alias leg is the Review Focus leg: the parent tree contains
// link → sub (a symlink), the run's delta carries link/f.txt
// (its layer replaced the alias with a real directory), and the
// parent's delta carries sub/f.txt — different strings, the
// same file, because a run path is judged against the parent's
// tree, where link still denotes sub.
func TestApplyRefusesOverlapShapes(t *testing.T) {
	shapes := []struct {
		name        string
		build       func(dir string)
		runEdits    func(t *testing.T, layerDir string)
		parentEdits func(t *testing.T, dir string)
		pairNames   []string
	}{
		{
			name: "equal",
			runEdits: func(t *testing.T, layerDir string) {
				writeTestFile(t, filepath.Join(layerDir, "mod.txt"), "run edit\n", 0o644)
			},
			parentEdits: func(t *testing.T, dir string) {
				writeTestFile(t, filepath.Join(dir, "mod.txt"), "parent edit\n", 0o644)
			},
			pairNames: []string{"mod.txt"},
		},
		{
			name: "prefix",
			build: func(dir string) {
				writeTestFile(t, filepath.Join(dir, "a", "b"), "ab file\n", 0o644)
			},
			runEdits: func(t *testing.T, layerDir string) {
				writeTestFile(t, filepath.Join(layerDir, "a", "b"), "ab run\n", 0o644)
			},
			parentEdits: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "a", "b")); err != nil {
					t.Fatalf("parent removes a/b file: %v", err)
				}
				writeTestFile(t, filepath.Join(dir, "a", "b", "c"), "c parent\n", 0o644)
			},
			pairNames: []string{"a/b"},
		},
		{
			name: "reverse prefix",
			build: func(dir string) {
				writeTestFile(t, filepath.Join(dir, "a", "b", "c"), "c original\n", 0o644)
			},
			runEdits: func(t *testing.T, layerDir string) {
				writeTestFile(t, filepath.Join(layerDir, "a", "b", "c"), "c run\n", 0o644)
			},
			parentEdits: func(t *testing.T, dir string) {
				if err := os.RemoveAll(filepath.Join(dir, "a", "b")); err != nil {
					t.Fatalf("parent removes a/b dir: %v", err)
				}
				writeTestFile(t, filepath.Join(dir, "a", "b"), "ab parent\n", 0o644)
			},
			pairNames: []string{"a/b"},
		},
		{
			name: "alias",
			build: func(dir string) {
				writeTestFile(t, filepath.Join(dir, "sub", "f.txt"), "f original\n", 0o644)
				writeTestFile(t, filepath.Join(dir, "sub", "g.txt"), "g original\n", 0o644)
				if err := os.Symlink("sub", filepath.Join(dir, "link")); err != nil {
					t.Fatalf("create link -> sub: %v", err)
				}
			},
			runEdits: func(t *testing.T, layerDir string) {
				if err := os.Remove(filepath.Join(layerDir, "link")); err != nil {
					t.Fatalf("run removes the link alias: %v", err)
				}
				writeTestFile(t, filepath.Join(layerDir, "link", "f.txt"), "f run\n", 0o644)
			},
			parentEdits: func(t *testing.T, dir string) {
				writeTestFile(t, filepath.Join(dir, "sub", "f.txt"), "f parent\n", 0o644)
			},
			pairNames: []string{"link", "sub/f.txt"},
		},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			f := newFixture(t)
			forkHead := f.parent0
			if sh.build != nil {
				forkHead = extendParentTree(t, f, sh.build)
			}
			layer, err := settle.Fork(f.ctx, "parent", "run-a")
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			sh.runEdits(t, layer.Dir)
			f.advanceRun(t, layer, "run-a", forkHead)
			sh.parentEdits(t, f.parentDir)
			advanceParent(t, f)

			headBefore, err := f.snaps.ScopeHead("parent")
			if err != nil {
				t.Fatalf("ScopeHead(parent): %v", err)
			}
			hashBefore := walkHash(t, f.parentDir)

			_, err = settle.Apply(f.ctx, "parent", f.parentDir, "run-a", "op-1")
			if !errors.Is(err, settle.ErrOverlap) {
				t.Fatalf("Apply err = %v, want ErrOverlap", err)
			}
			for _, name := range sh.pairNames {
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("ErrOverlap %q does not name %q (the offending pair)", err, name)
				}
			}
			if head, err := f.snaps.ScopeHead("parent"); err != nil || head != headBefore {
				t.Fatalf("parent head after refused Apply = %s, %v; want %s unmoved", head, err, headBefore)
			}
			if got := walkHash(t, f.parentDir); got != hashBefore {
				t.Fatalf("parentDir walk hash after refused Apply = %s, want %s (tree exactly as before)", got, hashBefore)
			}
			if facts := f.settleFacts(t, "run-a"); len(facts) != 0 {
				t.Fatalf("refused Apply minted %d facts, want 0", len(facts))
			}
			assertNoStagingLeft(t, f)
		})
	}
}

// assertSettledUnion pins a successful Apply's end state: the
// parent directory serves exactly the hand-built expected tree
// byte-for-byte (walk hash), the parent scope head's file map
// equals the union of the three maps, and exactly one apply
// fact — the returned receipt — names wantPaths as its
// settled_paths.
func assertSettledUnion(t *testing.T, f *fixture, receipt store.Receipt, forkHead, parentHead, runHead, expectedDir string, wantPaths []string) {
	t.Helper()
	if got, want := walkHash(t, f.parentDir), walkHash(t, expectedDir); got != want {
		t.Fatalf("parentDir walk hash = %s, want expected union %s", got, want)
	}
	result, err := f.snaps.ScopeHead("parent")
	if err != nil {
		t.Fatalf("ScopeHead(parent): %v", err)
	}
	forkMap, err := f.snaps.WorkspaceFiles(forkHead)
	if err != nil {
		t.Fatalf("WorkspaceFiles(fork): %v", err)
	}
	runMap, err := f.snaps.WorkspaceFiles(runHead)
	if err != nil {
		t.Fatalf("WorkspaceFiles(run): %v", err)
	}
	parentMap, err := f.snaps.WorkspaceFiles(parentHead)
	if err != nil {
		t.Fatalf("WorkspaceFiles(parent): %v", err)
	}
	resultMap, err := f.snaps.WorkspaceFiles(result)
	if err != nil {
		t.Fatalf("WorkspaceFiles(result): %v", err)
	}
	if want := unionOf(forkMap, parentMap, runMap); !reflect.DeepEqual(resultMap, want) {
		t.Fatalf("WorkspaceFiles(result) = %v, want union %v", resultMap, want)
	}
	facts := applyFacts(t, f, "run-a")
	if len(facts) != 1 || facts[0].ID != receipt.RecordID {
		t.Fatalf("apply facts on run-a = %v, want exactly the apply receipt %s", facts, receipt.RecordID)
	}
	if got := factStringList(t, factBody(t, facts[0]), "settled_paths"); !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("settled_paths = %v, want run delta sorted %v", got, wantPaths)
	}
}

// TestApplySettlesDirToFileSwap (whole-branch final review,
// blocking item (b); AC-F6-2): a directory→file shape swap —
// a directory in the fork tree replaced by a file — on either
// side is a genuinely disjoint delta when the other side never
// touched that subtree, and it must settle to the byte-exact
// union. Inside the union builder, the sorted overlay pass
// lands the file at old.txt first, clearing the directory; the
// delete of the orphaned descendant old.txt/inner.txt then
// names a path under a file. That target is already gone —
// the journal's targetGone tolerance (Task 8) mirrored in the
// overlay — not an abort.
func TestApplySettlesDirToFileSwap(t *testing.T) {
	build := func(dir string) {
		writeTestFile(t, filepath.Join(dir, "old.txt", "inner.txt"), "inner original\n", 0o644)
		writeTestFile(t, filepath.Join(dir, "p.txt"), "p original\n", 0o644)
		writeTestFile(t, filepath.Join(dir, "r.txt"), "r original\n", 0o644)
	}
	swap := func(t *testing.T, dir, content string) {
		t.Helper()
		if err := os.RemoveAll(filepath.Join(dir, "old.txt")); err != nil {
			t.Fatalf("remove old.txt directory: %v", err)
		}
		writeTestFile(t, filepath.Join(dir, "old.txt"), content, 0o644)
	}

	t.Run("run side", func(t *testing.T) {
		f := newFixture(t)
		forkHead := extendParentTree(t, f, build)
		layer, err := settle.Fork(f.ctx, "parent", "run-a")
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		swap(t, layer.Dir, "swapped by run\n")
		runHead := f.advanceRun(t, layer, "run-a", forkHead)

		// The parent changes a disjoint path.
		writeTestFile(t, filepath.Join(f.parentDir, "p.txt"), "p parent\n", 0o644)
		parentHead := advanceParent(t, f)

		receipt, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-a", "op-1")
		if err != nil {
			t.Fatalf("Apply of disjoint run-side dir-to-file swap: %v", err)
		}
		expectedDir := filepath.Join(filepath.Dir(f.root), "expected-run-swap")
		writeTestFile(t, filepath.Join(expectedDir, "keep.txt"), "keep\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "mod.txt"), "original\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "old.txt"), "swapped by run\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "p.txt"), "p parent\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "r.txt"), "r original\n", 0o644)
		assertSettledUnion(t, f, receipt, forkHead, parentHead, runHead, expectedDir,
			[]string{"old.txt", "old.txt/inner.txt"})
	})

	t.Run("parent side", func(t *testing.T) {
		f := newFixture(t)
		forkHead := extendParentTree(t, f, build)
		layer, err := settle.Fork(f.ctx, "parent", "run-a")
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		writeTestFile(t, filepath.Join(layer.Dir, "r.txt"), "r run\n", 0o644)
		runHead := f.advanceRun(t, layer, "run-a", forkHead)

		// The parent performs the swap on its side.
		swap(t, f.parentDir, "swapped by parent\n")
		parentHead := advanceParent(t, f)

		receipt, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-a", "op-1")
		if err != nil {
			t.Fatalf("Apply of disjoint parent-side dir-to-file swap: %v", err)
		}
		expectedDir := filepath.Join(filepath.Dir(f.root), "expected-parent-swap")
		writeTestFile(t, filepath.Join(expectedDir, "keep.txt"), "keep\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "mod.txt"), "original\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "old.txt"), "swapped by parent\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "p.txt"), "p original\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "r.txt"), "r run\n", 0o644)
		assertSettledUnion(t, f, receipt, forkHead, parentHead, runHead, expectedDir,
			[]string{"r.txt"})
	})
}

// TestApplySettlesFileToDirSwap (whole-branch final review,
// blocking item (b); AC-F6-2): the symmetric shape swap — a
// file in the fork tree replaced by a directory with contents
// — on either side. The sorted overlay pass deletes the file
// at old.txt before writing the entries beneath it, so this
// direction settles through the same pass; these fixtures pin
// that it does, to the byte-exact union.
func TestApplySettlesFileToDirSwap(t *testing.T) {
	build := func(dir string) {
		writeTestFile(t, filepath.Join(dir, "old.txt"), "file original\n", 0o644)
		writeTestFile(t, filepath.Join(dir, "p.txt"), "p original\n", 0o644)
		writeTestFile(t, filepath.Join(dir, "r.txt"), "r original\n", 0o644)
	}
	swap := func(t *testing.T, dir, content string) {
		t.Helper()
		if err := os.Remove(filepath.Join(dir, "old.txt")); err != nil {
			t.Fatalf("remove old.txt file: %v", err)
		}
		writeTestFile(t, filepath.Join(dir, "old.txt", "inner.txt"), content, 0o644)
	}

	t.Run("run side", func(t *testing.T) {
		f := newFixture(t)
		forkHead := extendParentTree(t, f, build)
		layer, err := settle.Fork(f.ctx, "parent", "run-a")
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		swap(t, layer.Dir, "inner by run\n")
		runHead := f.advanceRun(t, layer, "run-a", forkHead)

		// The parent changes a disjoint path.
		writeTestFile(t, filepath.Join(f.parentDir, "p.txt"), "p parent\n", 0o644)
		parentHead := advanceParent(t, f)

		receipt, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-a", "op-1")
		if err != nil {
			t.Fatalf("Apply of disjoint run-side file-to-dir swap: %v", err)
		}
		expectedDir := filepath.Join(filepath.Dir(f.root), "expected-run-swap")
		writeTestFile(t, filepath.Join(expectedDir, "keep.txt"), "keep\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "mod.txt"), "original\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "old.txt", "inner.txt"), "inner by run\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "p.txt"), "p parent\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "r.txt"), "r original\n", 0o644)
		assertSettledUnion(t, f, receipt, forkHead, parentHead, runHead, expectedDir,
			[]string{"old.txt", "old.txt/inner.txt"})
	})

	t.Run("parent side", func(t *testing.T) {
		f := newFixture(t)
		forkHead := extendParentTree(t, f, build)
		layer, err := settle.Fork(f.ctx, "parent", "run-a")
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		writeTestFile(t, filepath.Join(layer.Dir, "r.txt"), "r run\n", 0o644)
		runHead := f.advanceRun(t, layer, "run-a", forkHead)

		// The parent performs the swap on its side.
		swap(t, f.parentDir, "inner by parent\n")
		parentHead := advanceParent(t, f)

		receipt, err := settle.Apply(f.ctx, "parent", f.parentDir, "run-a", "op-1")
		if err != nil {
			t.Fatalf("Apply of disjoint parent-side file-to-dir swap: %v", err)
		}
		expectedDir := filepath.Join(filepath.Dir(f.root), "expected-parent-swap")
		writeTestFile(t, filepath.Join(expectedDir, "keep.txt"), "keep\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "mod.txt"), "original\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "old.txt", "inner.txt"), "inner by parent\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "p.txt"), "p original\n", 0o644)
		writeTestFile(t, filepath.Join(expectedDir, "r.txt"), "r run\n", 0o644)
		assertSettledUnion(t, f, receipt, forkHead, parentHead, runHead, expectedDir,
			[]string{"r.txt"})
	})
}
