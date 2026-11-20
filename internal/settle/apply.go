package settle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// ErrOverlap is returned by Apply when the run's delta and the
// parent's post-fork delta touch the same file — equal path,
// prefix path, or alias (AC-F6-3). The refusal is fail-closed:
// it is returned before anything is written but probe snapshots,
// so the parent ref and tree are left exactly as they were, and
// no content merge is ever synthesized in place of it.
var ErrOverlap = errors.New("settle: run and parent changes overlap")

// treeDelta is one side's change set against the fork tree:
// added and changed paths carry the target tree's entries
// (changed means blob-changed or mode-changed — a mode-only
// change, the same blob under a new mode, is a delta change,
// amendment F2), deleted paths carry none.
type treeDelta struct {
	set     map[string]snapshot.WorkspaceEntry // added + changed
	deleted map[string]bool
	paths   []string // every delta path, sorted
}

// deltaAgainst computes target's change set against base.
func deltaAgainst(base, target map[string]snapshot.WorkspaceEntry) treeDelta {
	d := treeDelta{
		set:     map[string]snapshot.WorkspaceEntry{},
		deleted: map[string]bool{},
	}
	for rel, e := range target {
		if old, ok := base[rel]; !ok || old != e {
			d.set[rel] = e
		}
	}
	for rel := range base {
		if _, ok := target[rel]; !ok {
			d.deleted[rel] = true
		}
	}
	d.paths = make([]string, 0, len(d.set)+len(d.deleted))
	for rel := range d.set {
		d.paths = append(d.paths, rel)
	}
	for rel := range d.deleted {
		d.paths = append(d.paths, rel)
	}
	sort.Strings(d.paths)
	return d
}

// Apply settles a run scope into its parent path-disjointly
// (AC-F6-2/AC-F6-3): where Select demands an unadvanced parent,
// Apply composes the two sides' disjoint change sets since the
// fork. The shape is artifacts before side effects:
//
//  1. degenerate and consume-once checks, sidecar load;
//  2. probe-snapshot the parent (Decision 8(c)) and compute the
//     three file maps — fork tree F, run head R, parent current
//     P — and the two deltas against F;
//  3. the disjointness census over run-delta × parent-delta —
//     any overlap refuses with ErrOverlap naming the offending
//     pair, and nothing has been written but probe snapshots;
//  4. build the expected union map U, then build the union tree
//     in a staging directory and snapshot it — the result
//     commit exists as a durable artifact before the parent
//     directory is touched, and its file map must equal U
//     exactly or the verb fails as an internal error with the
//     parent untouched;
//  5. materialize the parent directory from the union tree
//     under the journal (Task 8) and verify by re-snapshotting
//     inside the materializer (file map == U);
//  6. AdvanceScope(parent, headAtStart, resultCommit) — the
//     compare-and-swap is the commit point;
//  7. fact last: MintSettleApply, with settled_paths = the run
//     delta's paths, sorted.
//
// An empty run delta is a descriptive refusal: there is nothing
// to settle (Select is the verb for the no-parent-change case).
// Like Select, Apply commits through the journaled
// materializer: the journal records the complete apply fact
// body at planned time, and Recover at the next verb's entry
// completes or rolls back a crashed apply.
func Apply(ctx *Context, parentScope, parentDir, runScope, opID string) (store.Receipt, error) {
	if opID == "" {
		return store.Receipt{}, errors.New("settle: apply: op ID is empty")
	}
	// Journal recovery at verb entry (Task 8): a crashed
	// predecessor on this parent scope is completed or rolled
	// back before this verb is judged.
	if err := Recover(ctx, parentScope); err != nil {
		return store.Receipt{}, err
	}
	if err := ensureNotConsumed(ctx, runScope); err != nil {
		return store.Receipt{}, err
	}
	sc, err := readSidecar(ctx.Root, runScope)
	if err != nil {
		return store.Receipt{}, err
	}
	if sc.ParentScope != parentScope {
		return store.Receipt{}, fmt.Errorf("settle: apply: scope %q was forked from %q, not %q", runScope, sc.ParentScope, parentScope)
	}
	parentHeadStart, err := ctx.Snaps.ScopeHead(parentScope)
	if err != nil {
		return store.Receipt{}, fmt.Errorf("settle: apply %q: %w", runScope, err)
	}
	runHead, err := ctx.Snaps.ScopeHead(runScope)
	if err != nil {
		return store.Receipt{}, fmt.Errorf("settle: apply %q: %w", runScope, err)
	}
	// Probe snapshot (plan Decision 8(c)): the parent's current
	// tree must be recordable by the snapshot store before
	// anything is written. The probe commit moves no ref; its
	// file map is the parent side of the census.
	probeCommit, err := ctx.Snaps.Snapshot(parentDir, map[string]string{"settle": "apply-probe"})
	if err != nil {
		return store.Receipt{}, fmt.Errorf("settle: apply %q: probe snapshot of parent directory: %w", runScope, err)
	}
	forkMap, err := ctx.Snaps.WorkspaceFiles(sc.ForkCommit)
	if err != nil {
		return store.Receipt{}, fmt.Errorf("settle: apply %q: %w", runScope, err)
	}
	runMap, err := ctx.Snaps.WorkspaceFiles(runHead)
	if err != nil {
		return store.Receipt{}, fmt.Errorf("settle: apply %q: %w", runScope, err)
	}
	parentMap, err := ctx.Snaps.WorkspaceFiles(probeCommit)
	if err != nil {
		return store.Receipt{}, fmt.Errorf("settle: apply %q: %w", runScope, err)
	}

	runDelta := deltaAgainst(forkMap, runMap)
	if len(runDelta.paths) == 0 {
		return store.Receipt{}, fmt.Errorf("settle: apply %q: run delta against the fork point is empty — nothing to settle (select is the verb for the no-parent-change case)", runScope)
	}
	parentDelta := deltaAgainst(forkMap, parentMap)

	// The disjointness census: any overlap refuses here, before
	// anything but probe snapshots has been written.
	for _, rp := range runDelta.paths {
		for _, pp := range parentDelta.paths {
			if pathsOverlapIn(parentDir, rp, pp) {
				return store.Receipt{}, fmt.Errorf("%w: run path %q and parent path %q", ErrOverlap, rp, pp)
			}
		}
	}

	// The expected union: the fork map with both deltas applied
	// (the census proved them disjoint, so order cannot matter).
	union := make(map[string]snapshot.WorkspaceEntry, len(forkMap))
	for rel, e := range forkMap {
		union[rel] = e
	}
	for _, d := range []treeDelta{parentDelta, runDelta} {
		for rel := range d.deleted {
			delete(union, rel)
		}
		for rel, e := range d.set {
			union[rel] = e
		}
	}

	// Build the union tree in staging and snapshot it: the
	// result commit is a durable artifact before the parent
	// directory is touched.
	resultCommit, err := buildUnionTree(ctx, sc.ForkCommit, probeCommit, runHead, parentDelta, runDelta, union)
	if err != nil {
		return store.Receipt{}, fmt.Errorf("settle: apply %q: %w", runScope, err)
	}

	// Materialize the parent directory from the union tree
	// under the journal (Task 8), verified by re-snapshotting
	// inside the materializer, with the complete apply fact
	// body recorded at planned time, before the first write.
	fact := store.SettleApplyBody(opID, parentScope, runScope, sc.ForkCommit, resultCommit, runDelta.paths)
	if err := materialize(ctx, materializeRequest{
		verb:         verbApply,
		opID:         opID,
		parentScope:  parentScope,
		runScope:     runScope,
		parentDir:    parentDir,
		oldHead:      parentHeadStart,
		resultCommit: resultCommit,
		want:         union,
		fact:         fact,
	}); err != nil {
		return store.Receipt{}, fmt.Errorf("settle: apply %q: %w", runScope, err)
	}

	if err := ctx.Snaps.AdvanceScope(parentScope, parentHeadStart, resultCommit); err != nil {
		return store.Receipt{}, fmt.Errorf("settle: apply %q: %w", runScope, err)
	}
	// Fact last (Review Focus): the ref has moved and the tree
	// is verified; only now is the act recorded. If the CAS or
	// the mint fails, the verified journal stays in place and
	// Recover completes the commit point at the next verb's
	// entry — the post-CAS/pre-fact window is closed.
	receipt, err := ctx.Trace.MintSettleApply(runScope, opID, parentScope, runScope, sc.ForkCommit, resultCommit, runDelta.paths)
	if err != nil {
		return store.Receipt{}, err
	}
	if err := clearJournalState(ctx.Root, parentScope, opID); err != nil {
		return store.Receipt{}, fmt.Errorf("settle: apply %q: %w", runScope, err)
	}
	return receipt, nil
}

// buildUnionTree assembles the union tree in a staging
// directory — extract the fork tree, overlay the parent delta
// from an extraction of the parent's current tree, overlay the
// run delta from an extraction of the run head — and snapshots
// it. The snapshot's file map must equal union exactly; a
// mismatch is an internal error, returned with the parent
// directory untouched. The staging directory is removed on
// every path: the result commit is the durable artifact, not
// the directory it was built in.
func buildUnionTree(ctx *Context, forkCommit, parentCommit, runCommit string, parentDelta, runDelta treeDelta, union map[string]snapshot.WorkspaceEntry) (string, error) {
	stagingDir := filepath.Join(ctx.Root, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return "", fmt.Errorf("settle: create staging dir: %w", err)
	}
	stage, err := os.MkdirTemp(stagingDir, "apply-")
	if err != nil {
		return "", fmt.Errorf("settle: create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := ctx.Snaps.ExtractWorkspace(forkCommit, stage); err != nil {
		return "", err
	}
	if err := overlayDelta(ctx, stage, parentCommit, parentDelta); err != nil {
		return "", err
	}
	if err := overlayDelta(ctx, stage, runCommit, runDelta); err != nil {
		return "", err
	}
	resultCommit, err := ctx.Snaps.Snapshot(stage, map[string]string{"settle": "apply-union"})
	if err != nil {
		return "", fmt.Errorf("settle: snapshot union tree: %w", err)
	}
	got, err := ctx.Snaps.WorkspaceFiles(resultCommit)
	if err != nil {
		return "", err
	}
	if !reflect.DeepEqual(got, union) {
		return "", fmt.Errorf("settle: union tree's file map does not match the expected union (internal error)")
	}
	return resultCommit, nil
}

// overlayDelta applies one side's delta to the staging tree in
// a single pass over the delta's paths in sorted order: each
// path is either deleted or copied whole from an extraction of
// the side's tree — blobs come from that tree and nothing else;
// no content is merged. Sorted order places a prefix path
// before its descendants, so a deletion that changes a path's
// shape lands before the entries beneath it are written, and a
// deletion whose path is already gone is not an error.
func overlayDelta(ctx *Context, stage, commit string, d treeDelta) error {
	if len(d.paths) == 0 {
		return nil
	}
	extractDir := filepath.Join(ctx.Root, "staging")
	src, err := os.MkdirTemp(extractDir, "apply-src-")
	if err != nil {
		return fmt.Errorf("settle: create overlay extraction dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(src) }()
	if err := ctx.Snaps.ExtractWorkspace(commit, src); err != nil {
		return err
	}
	for _, rel := range d.paths {
		dst := filepath.Join(stage, filepath.FromSlash(rel))
		if d.deleted[rel] {
			if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("settle: apply deletion of %s in union staging: %w", rel, err)
			}
			continue
		}
		entry := d.set[rel]
		srcPath := filepath.Join(src, filepath.FromSlash(rel))
		if entry.Symlink {
			target, err := os.Readlink(srcPath)
			if err != nil {
				return fmt.Errorf("settle: read overlay symlink %s: %w", srcPath, err)
			}
			if err := replaceWithSymlink(dst, target); err != nil {
				return err
			}
			continue
		}
		if err := copyFileAtomic(srcPath, dst, permForMode(entry.Mode)); err != nil {
			return err
		}
	}
	return nil
}
