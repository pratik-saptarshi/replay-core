// Package settle is replay-core's settlement layer (F6): the
// verbs that land a replay run's outcome back onto a real tree —
// fast-forward-only select, path-disjoint apply (Task 7),
// archive-on-discard — over carrier layers (F5), the snapshot
// store (F4), and the trace store (F2). Settlement is fail-closed
// (spec §8): the gates apply before anything touches a real tree,
// a verb's commit point is ordered materialize → verify → move
// the ref → mint the fact last, and a fact is minted only for an
// act that completed (Review Focus). A settlement scope is named
// once (plan Decision 3): the scope name is both the snapshot
// scope ref name and the trace owner path its settlement facts
// are appended to, so a run's trace path must be the scope name
// for the facts' evidence to mean what it says — the verbs
// neither discover nor adopt foreign paths.
package settle

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/carrier"
	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// Context bundles the two stores and the one root all settle
// operational state lives under (plan Decision 8(e)): fork
// sidecars, base extractions, layers, staging, and journals
// live under Root; nothing in F5/F6 writes anywhere else
// except the parent directory at a verb's commit point and
// the two stores through their own APIs.
type Context struct {
	Trace *store.Store
	Snaps *snapshot.Store
	Root  string // forks/, bases/, layers/, staging/, journals/
}

// ErrParentAdvanced is returned by Select when the parent scope's
// head no longer equals the fork point: the parent advanced past
// the fork, and select is fast-forward-only — no rebase or merge
// is attempted (AC-F6-1).
var ErrParentAdvanced = errors.New("settle: parent advanced past the fork point")

// ErrScopeConsumed is returned by every settlement verb when the
// run scope already carries a settlement fact: a scope settled
// by any verb is consumed for every verb (AC-F6-6).
var ErrScopeConsumed = errors.New("settle: scope already settled")

// ErrNoForkProvenance is returned when a run scope has no fork
// sidecar: the scope was not created by Fork (or its provenance
// was lost), and the verbs refuse to adopt a foreign path.
var ErrNoForkProvenance = errors.New("settle: no fork provenance for scope")

// archiveRefPrefix is the snapshot layout's archive class: a
// discarded scope's ref moves to refs/vcscore/archive/<scope>.
const archiveRefPrefix = "refs/vcscore/archive/"

// scopeNamePattern is the scope-name law, mirroring the
// snapshot store's: lowercase alphanumerics and hyphens,
// starting with an alphanumeric — a single ref path
// component, so a scope name can never carry a traversal
// component into a filesystem path built from it.
var scopeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validScopeName reports whether scope obeys the scope-name
// law.
func validScopeName(scope string) bool {
	return scopeNamePattern.MatchString(scope)
}

// checkScopeName refuses a scope name the law rejects. Every
// verb calls it for each scope name it is handed BEFORE
// constructing any filesystem path from the name — the base
// extraction directory and the temporary layer root Fork
// unconditionally removes, the sidecar path Select, Apply and
// Discard read, the journal path Recover reads — because a
// traversal name otherwise steers those paths outside the
// settle Root and reaches their extractions and removals
// before the snapshot store's own validation of the name
// (inside Snaps.Fork and Snaps.ScopeHead) ever runs.
func checkScopeName(kind, scope string) error {
	if !validScopeName(scope) {
		return fmt.Errorf("settle: invalid %s scope name %q (must match %s)", kind, scope, scopeNamePattern)
	}
	return nil
}

// forkSidecar is a fork's provenance (plan Decision 3): the
// parent scope, the fork commit (the parent head at fork time),
// and the base (workspace) tree the layer was materialized from.
// It is operational state recorded by Fork in
// Root/forks/<runScope>.json (canonical JSON, fsynced before the
// layer is handed out) — not a trace fact: AC-F6-7 enumerates
// the settlement facts as the three verbs' records, and Fork
// adds no fourth fact class.
type forkSidecar struct {
	ParentScope string
	RunScope    string
	ForkCommit  string
	BaseTree    string
}

func sidecarPath(root, runScope string) string {
	return filepath.Join(root, "forks", runScope+".json")
}

// writeSidecar records sc as runScope's fork provenance,
// fsynced before it is relied on.
func writeSidecar(root string, sc forkSidecar) error {
	v := canonical.ObjectOf(
		canonical.Pair{Key: "base_tree", Value: canonical.StringOf(sc.BaseTree)},
		canonical.Pair{Key: "fork_commit", Value: canonical.StringOf(sc.ForkCommit)},
		canonical.Pair{Key: "parent_scope", Value: canonical.StringOf(sc.ParentScope)},
		canonical.Pair{Key: "run_scope", Value: canonical.StringOf(sc.RunScope)},
	)
	b, err := canonical.MarshalKernel(v)
	if err != nil {
		return fmt.Errorf("settle: marshal fork sidecar for %q: %w", sc.RunScope, err)
	}
	dir := filepath.Join(root, "forks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("settle: create forks dir: %w", err)
	}
	path := sidecarPath(root, sc.RunScope)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("settle: write fork sidecar for %q: %w", sc.RunScope, err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("settle: write fork sidecar for %q: %w", sc.RunScope, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("settle: fsync fork sidecar for %q: %w", sc.RunScope, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("settle: close fork sidecar for %q: %w", sc.RunScope, err)
	}
	return syncDir(dir)
}

// readSidecar loads runScope's fork provenance. A missing
// sidecar is ErrNoForkProvenance; a sidecar that does not decode
// to the recorded shape is a descriptive error — provenance is
// never guessed.
func readSidecar(root, runScope string) (forkSidecar, error) {
	b, err := os.ReadFile(sidecarPath(root, runScope))
	if errors.Is(err, fs.ErrNotExist) {
		return forkSidecar{}, fmt.Errorf("%w: %q", ErrNoForkProvenance, runScope)
	}
	if err != nil {
		return forkSidecar{}, fmt.Errorf("settle: read fork sidecar for %q: %w", runScope, err)
	}
	v, err := canonical.DecodeKernel(b)
	if err != nil {
		return forkSidecar{}, fmt.Errorf("settle: fork sidecar for %q does not decode: %w", runScope, err)
	}
	var sc forkSidecar
	for key, dst := range map[string]*string{
		"parent_scope": &sc.ParentScope,
		"run_scope":    &sc.RunScope,
		"fork_commit":  &sc.ForkCommit,
		"base_tree":    &sc.BaseTree,
	} {
		s, ok := stringField(v, key)
		if !ok {
			return forkSidecar{}, fmt.Errorf("settle: fork sidecar for %q has no %q string", runScope, key)
		}
		*dst = s
	}
	if sc.RunScope != runScope {
		return forkSidecar{}, fmt.Errorf("settle: fork sidecar at %q names run scope %q", sidecarPath(root, runScope), sc.RunScope)
	}
	return sc, nil
}

// stringField returns the string stored under key in a canonical
// object value.
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

// syncDir fsyncs a directory so a rename or creation inside it
// is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("settle: open dir %s for fsync: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("settle: fsync dir %s: %w", dir, err)
	}
	return nil
}

// ensureNotConsumed is the consume-once scan (AC-F6-6): any
// settlement fact on the run scope's trace path — select, apply,
// or discard, whatever its op — consumes the scope for every
// verb. The scan runs before provenance is consulted, so a
// settled-then-discarded scope (whose sidecar is gone) still
// reports as consumed rather than unprovenanced.
func ensureNotConsumed(ctx *Context, runScope string) error {
	recs, err := ctx.Trace.PathRecords(runScope)
	if err != nil {
		return fmt.Errorf("settle: consume-once scan of %q: %w", runScope, err)
	}
	for _, rec := range recs {
		ref, _ := stringField(rec.Record, "schema_ref")
		switch ref {
		case store.SettleSelectSchemaRef, store.SettleApplySchemaRef, store.SettleDiscardSchemaRef:
			return fmt.Errorf("%w: %q (settled by %s)", ErrScopeConsumed, runScope, ref)
		}
	}
	return nil
}

// Fork creates a run scope forked from parentScope: it reads the
// parent head, extracts that commit's workspace tree to
// Root/bases/<runScope>, selects a carrier on Root/layers and
// materializes a layer into a temporary root under
// Root/staging — and only then creates the scope ref, writes the
// sidecar, and renames the layer root into place. A layer is
// never handed out without provenance; a crash between the ref
// and the sidecar leaves an inert ref the verbs refuse with
// ErrNoForkProvenance rather than a usable fork. Advancing the
// run line afterwards (snapshotting the layer, moving the run
// scope's head) is the caller's composition of snapshot.Snapshot
// + snapshot.AdvanceScope; Fork owns only the fork.
func Fork(ctx *Context, parentScope, runScope string) (*carrier.Layer, error) {
	// The scope-name law is enforced before any filesystem
	// path is constructed from either name (see checkScopeName):
	// the extraction and the unconditional removal below are
	// exactly the effects a traversal run scope must never
	// reach.
	if err := checkScopeName("parent", parentScope); err != nil {
		return nil, err
	}
	if err := checkScopeName("run", runScope); err != nil {
		return nil, err
	}
	parentHead, err := ctx.Snaps.ScopeHead(parentScope)
	if err != nil {
		return nil, fmt.Errorf("settle: fork %q from %q: %w", runScope, parentScope, err)
	}
	baseTree, err := ctx.Snaps.WorkspaceTreeID(parentHead)
	if err != nil {
		return nil, fmt.Errorf("settle: fork %q from %q: %w", runScope, parentScope, err)
	}
	basesDir := filepath.Join(ctx.Root, "bases")
	layersDir := filepath.Join(ctx.Root, "layers")
	stagingDir := filepath.Join(ctx.Root, "staging")
	for _, dir := range []string{basesDir, layersDir, stagingDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("settle: fork %q: create %s: %w", runScope, dir, err)
		}
	}
	baseDir := filepath.Join(basesDir, runScope)
	if err := ctx.Snaps.ExtractWorkspace(parentHead, baseDir); err != nil {
		return nil, fmt.Errorf("settle: fork %q: %w", runScope, err)
	}
	sel, err := carrier.Select(layersDir)
	if err != nil {
		_ = os.RemoveAll(baseDir)
		return nil, fmt.Errorf("settle: fork %q: %w", runScope, err)
	}
	// The temporary layer root is staging state — never handed
	// out — so a remnant of a crashed Fork is removed outright
	// rather than adopted.
	tmpRoot := filepath.Join(stagingDir, "fork-"+runScope)
	_ = os.RemoveAll(tmpRoot)
	layer, err := carrier.Materialize(sel, baseDir, baseTree, tmpRoot)
	if err != nil {
		_ = os.RemoveAll(baseDir)
		return nil, fmt.Errorf("settle: fork %q: %w", runScope, err)
	}
	fail := func(err error) (*carrier.Layer, error) {
		_ = layer.Remove()
		_ = os.RemoveAll(baseDir)
		return nil, err
	}
	if err := ctx.Snaps.Fork(runScope, parentHead); err != nil {
		return fail(fmt.Errorf("settle: fork %q: %w", runScope, err))
	}
	sc := forkSidecar{ParentScope: parentScope, RunScope: runScope, ForkCommit: parentHead, BaseTree: baseTree}
	if err := writeSidecar(ctx.Root, sc); err != nil {
		return fail(err)
	}
	finalRoot := filepath.Join(layersDir, runScope)
	if err := os.Rename(tmpRoot, finalRoot); err != nil {
		return fail(fmt.Errorf("settle: fork %q: move layer into place: %w", runScope, err))
	}
	// Re-acquire the handle at its final root: Materialize is
	// idempotent for the recorded (base, driver) and preserves
	// the layer's contents, and the renamed handle's root would
	// otherwise name a path that no longer exists.
	layer, err = carrier.Materialize(sel, baseDir, baseTree, finalRoot)
	if err != nil {
		return nil, fmt.Errorf("settle: fork %q: reopen layer at final root: %w", runScope, err)
	}
	return layer, nil
}

// Select settles a run scope into its parent wholesale
// (AC-F6-1): fast-forward-only. The parent directory is made to
// match the run head's tree exactly, the parent scope advances
// from the fork commit to the run head, and the select fact is
// minted last, on the run scope's trace path. The fast-forward
// check is a head comparison — ScopeHead(parentScope) must equal
// the sidecar's fork commit; advancement is a property of the
// parent head, not of its tree, so a parent that advanced by a
// meta-only commit (identical workspace tree, new commit) is
// still refused, and no rebase or merge is ever attempted.
func Select(ctx *Context, parentScope, parentDir, runScope, opID string) (store.Receipt, error) {
	if opID == "" {
		return store.Receipt{}, errors.New("settle: select: op ID is empty")
	}
	if err := checkScopeName("parent", parentScope); err != nil {
		return store.Receipt{}, err
	}
	if err := checkScopeName("run", runScope); err != nil {
		return store.Receipt{}, err
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
		return store.Receipt{}, fmt.Errorf("settle: select: scope %q was forked from %q, not %q", runScope, sc.ParentScope, parentScope)
	}
	parentHead, err := ctx.Snaps.ScopeHead(parentScope)
	if err != nil {
		return store.Receipt{}, fmt.Errorf("settle: select %q: %w", runScope, err)
	}
	if parentHead != sc.ForkCommit {
		return store.Receipt{}, fmt.Errorf("%w: parent scope %q is at %s, fork point is %s", ErrParentAdvanced, parentScope, parentHead, sc.ForkCommit)
	}
	runHead, err := ctx.Snaps.ScopeHead(runScope)
	if err != nil {
		return store.Receipt{}, fmt.Errorf("settle: select %q: %w", runScope, err)
	}
	// Probe snapshot (plan Decision 8(c)): the parent's current
	// tree must be recordable by the snapshot store before
	// anything is written — a tree it cannot record fails the
	// verb here, not mid-materialization. The probe is not the
	// advancement test (that is the head comparison above).
	if _, err := ctx.Snaps.Snapshot(parentDir, map[string]string{"settle": "select-probe"}); err != nil {
		return store.Receipt{}, fmt.Errorf("settle: select %q: probe snapshot of parent directory: %w", runScope, err)
	}
	want, err := ctx.Snaps.WorkspaceFiles(runHead)
	if err != nil {
		return store.Receipt{}, fmt.Errorf("settle: select %q: %w", runScope, err)
	}
	// Materialize under the journal (Task 8): the parent
	// directory is made to match the run head's tree exactly and
	// verified by re-snapshot inside the materializer, with the
	// complete select fact body recorded at planned time,
	// before the first write.
	fact := store.SettleSelectBody(opID, parentScope, runScope, sc.ForkCommit, runHead)
	if err := materialize(ctx, materializeRequest{
		verb:         verbSelect,
		opID:         opID,
		parentScope:  parentScope,
		runScope:     runScope,
		parentDir:    parentDir,
		oldHead:      sc.ForkCommit,
		resultCommit: runHead,
		want:         want,
		fact:         fact,
	}); err != nil {
		return store.Receipt{}, fmt.Errorf("settle: select %q: %w", runScope, err)
	}
	if err := ctx.Snaps.AdvanceScope(parentScope, sc.ForkCommit, runHead); err != nil {
		return store.Receipt{}, fmt.Errorf("settle: select %q: %w", runScope, err)
	}
	// Fact last (Review Focus): the ref has moved and the tree
	// is verified; only now is the act recorded. If the CAS or
	// the mint fails, the verified journal stays in place and
	// Recover completes the commit point at the next verb's
	// entry — the post-CAS/pre-fact window is closed.
	receipt, err := ctx.Trace.MintSettleSelect(runScope, opID, parentScope, runScope, sc.ForkCommit, runHead)
	if err != nil {
		return store.Receipt{}, err
	}
	if err := clearJournalState(ctx.Root, parentScope, opID); err != nil {
		return store.Receipt{}, fmt.Errorf("settle: select %q: %w", runScope, err)
	}
	return receipt, nil
}

// Discard retires a run scope without deleting its history
// (AC-F6-5): the scope ref is archived at its head, the layer
// is removed, the discard fact is minted last among the
// settlement effects (Decision 4), and the fork sidecar — the
// verb's own operational state — is removed after the mint,
// the same shape as Select and Apply clearing their journal
// state after theirs. An already-missing layer is not an
// error — the archive is the settlement, the layer is
// operational state.
//
// The order makes every prefix of the verb completable on
// retry, with no discard journal: a Discard that dies after
// the archive lands leaves the sidecar in place and the
// archive ref holding the head the fact must cite, so a
// retried Discard resolves its head from the archive, finishes
// the cleanup, and mints the fact; a Discard that dies after
// the mint leaves a fact the consume-once scan reads, and the
// retry reports the scope consumed. The one window this does
// not close is a crash inside ArchiveScope itself, between
// its create and its delete: both refs then name the same
// head, nothing is lost, and the retry fails with the
// archive's own already-exists refusal, descriptively.
//
// Before any of that, a pending settlement on the run's
// parent is resolved: a Select or Apply that crashed with a
// verified journal but before minting its fact left no fact
// for the consume-once scan to see, so without resolution
// Discard would archive the run and mint its own fact while
// the journal survived — and a later Recover would still
// complete the earlier settlement and mint its fact on top
// of the discard, even advancing the parent (in the pre-CAS
// window) to output Discard had reported discarded; a crashed
// applying journal would likewise leave the parent diverged
// from its scope head with no fact recording it. Discard
// therefore runs Recover for the parent scope the sidecar
// names — completing a verified settlement or rolling back
// an applying one — and rechecks consumption before any
// discard effect: a settlement Recover completed has consumed
// the scope, and Discard refuses as consumed.
func Discard(ctx *Context, runScope, opID string) (store.Receipt, error) {
	if opID == "" {
		return store.Receipt{}, errors.New("settle: discard: op ID is empty")
	}
	if err := checkScopeName("run", runScope); err != nil {
		return store.Receipt{}, err
	}
	if err := ensureNotConsumed(ctx, runScope); err != nil {
		return store.Receipt{}, err
	}
	sc, err := readSidecar(ctx.Root, runScope)
	if err != nil {
		return store.Receipt{}, err
	}
	// Journal recovery for the parent scope (see the doc
	// comment): the sidecar is the only record that names the
	// parent whose journal may be pending, so resolution runs
	// here — after provenance is known, before any discard
	// effect — and consumption is rechecked against whatever
	// Recover minted.
	if err := Recover(ctx, sc.ParentScope); err != nil {
		return store.Receipt{}, err
	}
	if err := ensureNotConsumed(ctx, runScope); err != nil {
		return store.Receipt{}, err
	}
	head, err := ctx.Snaps.ScopeHead(runScope)
	switch {
	case err == nil:
		if err := ctx.Snaps.ArchiveScope(runScope); err != nil {
			return store.Receipt{}, fmt.Errorf("settle: discard %q: %w", runScope, err)
		}
	case errors.Is(err, snapshot.ErrScopeNotFound):
		// The scope ref is gone. If the archive ref exists, a
		// previous Discard died after archiving but before the
		// mint: complete it, citing the archived head. If no
		// archive exists either, the scope is simply absent
		// and the refusal below is the one Discard has always
		// given for it.
		archived, found, aerr := archivedHead(ctx, runScope)
		if aerr != nil {
			return store.Receipt{}, fmt.Errorf("settle: discard %q: %w", runScope, aerr)
		}
		if !found {
			return store.Receipt{}, fmt.Errorf("settle: discard %q: %w", runScope, err)
		}
		head = archived
	default:
		return store.Receipt{}, fmt.Errorf("settle: discard %q: %w", runScope, err)
	}
	if err := removeLayer(ctx, sc); err != nil {
		return store.Receipt{}, fmt.Errorf("settle: discard %q: %w", runScope, err)
	}
	// Fact last (Review Focus): the scope is archived and the
	// layer is gone; only now is the act recorded.
	receipt, err := ctx.Trace.MintSettleDiscard(runScope, opID, runScope, head, archiveRefPrefix+runScope)
	if err != nil {
		return store.Receipt{}, err
	}
	if err := os.Remove(sidecarPath(ctx.Root, runScope)); err != nil {
		return store.Receipt{}, fmt.Errorf("settle: discard %q: remove sidecar: %w", runScope, err)
	}
	return receipt, nil
}

// archivedHead returns the commit runScope's archive ref
// names — the head a crashed Discard archived at — or found
// false when the scope was never archived.
func archivedHead(ctx *Context, runScope string) (head string, found bool, err error) {
	refs, err := ctx.Snaps.ListScopes()
	if err != nil {
		return "", false, err
	}
	for _, r := range refs {
		if r.Kind == snapshot.RefArchive && r.Name == runScope {
			return r.CommitID, true, nil
		}
	}
	return "", false, nil
}

// removeLayer tears down a run scope's carrier layer through
// the carrier (so driver state, e.g. a mount, is released by the
// driver that created it): the handle is re-acquired by an
// idempotent Materialize against the recorded base, then
// removed. A layer root that is already gone is not an error.
func removeLayer(ctx *Context, sc forkSidecar) error {
	layersDir := filepath.Join(ctx.Root, "layers")
	layerRoot := filepath.Join(layersDir, sc.RunScope)
	if _, err := os.Stat(layerRoot); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("settle: stat layer root %s: %w", layerRoot, err)
	}
	sel, err := carrier.Select(layersDir)
	if err != nil {
		return err
	}
	layer, err := carrier.Materialize(sel, filepath.Join(ctx.Root, "bases", sc.RunScope), sc.BaseTree, layerRoot)
	if err != nil {
		return err
	}
	return layer.Remove()
}

// pathParent returns rel's parent path in slash form, or "" for
// a top-level path.
func pathParent(rel string) string {
	i := strings.LastIndex(rel, "/")
	if i < 0 {
		return ""
	}
	return rel[:i]
}

// dirFile is one non-directory entry found by walkDirFiles.
type dirFile struct {
	symlink bool
	other   bool // neither regular file nor symlink (FIFO, socket, device)
	mode    string
}

// walkDirFiles inventories dir's non-directory entries by
// slash-separated relative path, plus every directory below dir
// (for empty-dir pruning). Entries named .git are skipped
// whatever their type, as in the snapshot walk.
func walkDirFiles(dir string) (map[string]dirFile, []string, error) {
	files := map[string]dirFile{}
	var dirs []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("settle: walk %s: %w", path, err)
		}
		if path != dir && d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("settle: relativize %s: %w", path, err)
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." {
				dirs = append(dirs, rel)
			}
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			files[rel] = dirFile{symlink: true, mode: "120000"}
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("settle: stat %s: %w", path, err)
			}
			mode := "100644"
			if info.Mode().Perm()&0o111 != 0 {
				mode = "100755"
			}
			files[rel] = dirFile{mode: mode}
		default:
			files[rel] = dirFile{other: true}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return files, dirs, nil
}

// filesEqual reports whether two regular files hold identical
// bytes.
func filesEqual(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, fmt.Errorf("settle: stat %s: %w", a, err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, fmt.Errorf("settle: stat %s: %w", b, err)
	}
	if ai.Size() != bi.Size() {
		return false, nil
	}
	ab, err := os.ReadFile(a)
	if err != nil {
		return false, fmt.Errorf("settle: read %s: %w", a, err)
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		return false, fmt.Errorf("settle: read %s: %w", b, err)
	}
	return bytes.Equal(ab, bb), nil
}

// permForMode maps a git tree mode for a regular file to the
// permission bits that mode denotes.
func permForMode(mode string) os.FileMode {
	if mode == "100755" {
		return 0o755
	}
	return 0o644
}

// copyFileAtomic writes src's bytes to dst with perm,
// atomically: a temp file in dst's directory, renamed over dst.
// A directory standing at dst is removed first — the target
// tree's shape wins.
func copyFileAtomic(src, dst string, perm os.FileMode) error {
	if err := clearBlockingDirs(dst); err != nil {
		return err
	}
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("settle: create %s: %w", parent, err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("settle: read staged %s: %w", src, err)
	}
	tmp, err := os.CreateTemp(parent, ".synctree-*")
	if err != nil {
		return fmt.Errorf("settle: create temp in %s: %w", parent, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("settle: write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("settle: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("settle: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("settle: rename %s to %s: %w", tmpName, dst, err)
	}
	return nil
}

// replaceWithSymlink makes dst a symlink to target, atomically:
// the link is created at a temp name in dst's directory and
// renamed over dst.
func replaceWithSymlink(dst, target string) error {
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("settle: create %s: %w", parent, err)
	}
	if info, err := os.Lstat(dst); err == nil && info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("settle: remove directory at %s: %w", dst, err)
		}
	}
	tmp, err := os.CreateTemp(parent, ".synctree-link-*")
	if err != nil {
		return fmt.Errorf("settle: create temp in %s: %w", parent, err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpName)
	defer func() { _ = os.Remove(tmpName) }()
	if err := os.Symlink(target, tmpName); err != nil {
		return fmt.Errorf("settle: symlink %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("settle: rename %s to %s: %w", tmpName, dst, err)
	}
	return nil
}

// clearBlockingDirs removes a directory standing exactly at dst
// so a file can take its place.
func clearBlockingDirs(dst string) error {
	if info, err := os.Lstat(dst); err == nil && info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("settle: remove directory at %s: %w", dst, err)
		}
	}
	return nil
}
