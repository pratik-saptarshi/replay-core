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
//
// Settlement is single-caller (spec §9): for a given Root and
// parent scope, at most one caller may run a verb or Recover
// at a time — across processes, not only goroutines. Entry
// recovery cannot distinguish a live operation from a crashed
// one, so a second caller's recovery can roll back or
// overwrite an in-flight operation's journal and staging. The
// verbs neither detect nor prevent concurrent use; keeping to
// one caller per Root and parent scope is the caller's
// contract.
package settle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
// sidecars, base extractions, layers, staging, fork staging,
// and journals live under Root; nothing in F5/F6 writes
// anywhere else except the parent directory at a verb's
// commit point and the two stores through their own APIs.
type Context struct {
	Trace *store.Store
	Snaps *snapshot.Store
	Root  string // forks/, bases/, layers/, staging/, fork-staging/, journals/
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

// resolveParentDir returns the absolute, symlink-free form of
// a settlement parent directory, or a descriptive error naming
// the path as given when it cannot be resolved or does not
// name a directory. It is the settle half of the root law the
// snapshot store states for its workspace root
// (resolveWorkspaceRoot) and the carrier for its base root: a
// directory handed over as a tree root is resolved once,
// before it is inventoried, captured, or recorded, so every
// consumer of the directory agrees on the one spelling — the
// real tree's path. filepath.WalkDir does not follow a
// symlinked root: settle's inventory walk visited the link
// itself as the parent's only entry, so a settlement spelled
// through a symlink planned every wanted file as a creation
// and the root link as a deletion, and preflight failed
// capturing the link's preimage over the file preimages —
// while the same verb's probe snapshot, resolving its root,
// succeeded on the very same spelling. The verbs resolve at
// their entries, and the materializer resolves again on the
// one path both verbs record the journal through, so the
// journal's parent_dir — and with it the anchor's vouch and
// Recover's rollback target — names the real tree whatever
// spelling the caller used: relative (the wave-2 arc) or
// symlinked (the wave-6 arc).
func resolveParentDir(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("settle: parent directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("settle: resolve parent directory %s: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("settle: resolve parent directory %s: %w", dir, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("settle: stat parent directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("settle: parent directory %s is not a directory", dir)
	}
	return resolved, nil
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
// Root/fork-staging — and only then creates the scope ref, writes the
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
	forkStagingDir := filepath.Join(ctx.Root, "fork-staging")
	for _, dir := range []string{basesDir, layersDir, forkStagingDir} {
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
	// The temporary layer root lives in Fork's own namespace
	// under the Root — never handed out — so a remnant of a
	// crashed Fork is removed outright rather than adopted.
	// The namespace is structural, not a naming convention:
	// settlement staging is keyed by caller-supplied operation
	// ID under Root/staging, and no operation ID can name a
	// path outside that directory, so no settlement's staging
	// can ever stand where this removal lands (and this
	// removal can never land on a settlement's staging).
	tmpRoot := filepath.Join(forkStagingDir, runScope)
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
	// The parent root is resolved once, here (see
	// resolveParentDir): the probe snapshot below resolves it
	// internally and the materializer records the resolved
	// path in the journal, so resolving at the entry makes
	// every consumer of parentDir in this verb — inventory,
	// preimage capture, journal — agree on the real tree's
	// path, whatever spelling the caller used.
	resolvedDir, err := resolveParentDir(parentDir)
	if err != nil {
		return store.Receipt{}, err
	}
	parentDir = resolvedDir
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
// retry reports the scope consumed. A crash inside
// ArchiveScope itself, between its create and its delete,
// leaves both refs naming the same head — and ArchiveScope
// resumes exactly that state (the standing archive is its own
// half-done work, completed at the delete; an archive naming
// a different head is a genuine conflict it refuses), so a
// retried Discard that still finds the active scope completes
// the archival on its way through, with no special routing.
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

// inventoryExcludedName reports whether an entry name is
// excluded from the workspace inventory: an entry with this
// name is never workspace content, whatever its filesystem
// type — a directory in an ordinary checkout, a regular file
// in a linked worktree or submodule checkout (where it holds
// a "gitdir:" pointer), a symlink in stranger layouts. The
// law is one law stated in three places: the snapshot walk
// (internal/snapshot, which spells the same test literally),
// walkDirFiles below, and the journal's unit-path validation
// (journal.go). This predicate is the settle package's single
// spelling of it — the inventory skips what it names, and
// materialize's replacement refusal
// (refuseUncapturedReplacements, journal.go) keys off it —
// so the set of content a wholesale directory removal must
// not destroy uncaptured is by construction the set the
// inventory excludes, and the two can never diverge.
func inventoryExcludedName(name string) bool {
	return name == ".git"
}

// errExcludedEntryFound stops excludedEntryBeneath's walk
// early: the first excluded entry is the whole answer.
var errExcludedEntryFound = errors.New("settle: excluded entry found")

// excludedEntryBeneath returns the slash-separated path,
// relative to dir, of the first inventory-excluded entry
// beneath dir, or "" if there is none. The walk does not
// follow symlinks, mirroring the inventory walks: content
// beneath a symlinked entry is neither inventoried nor
// destroyed by a wholesale removal of dir (RemoveAll unlinks
// the symlink itself, never its target's tree), so it is not
// this scan's concern. dir itself is exempt, as in the
// walks: it is the tree being examined, not an entry in it.
func excludedEntryBeneath(dir string) (string, error) {
	found := ""
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != dir && inventoryExcludedName(d.Name()) {
			rel, rerr := filepath.Rel(dir, path)
			if rerr != nil {
				return rerr
			}
			found = filepath.ToSlash(rel)
			return errExcludedEntryFound
		}
		return nil
	})
	if err != nil && !errors.Is(err, errExcludedEntryFound) {
		return "", fmt.Errorf("settle: scan %s for excluded content: %w", dir, err)
	}
	return found, nil
}

// walkDirFiles inventories dir's non-directory entries by
// slash-separated relative path, plus every directory below dir
// (for empty-dir pruning). Entries the inventory excludes
// (see inventoryExcludedName) are skipped whatever their
// type, as in the snapshot walk.
func walkDirFiles(dir string) (map[string]dirFile, []string, error) {
	files := map[string]dirFile{}
	var dirs []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("settle: walk %s: %w", path, err)
		}
		if path != dir && inventoryExcludedName(d.Name()) {
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
// bytes. The comparison is incremental — a size fast-path,
// then bounded chunks — because it sits on the recovery path
// (restorePreimage's already-restored check) and in the
// planner, where either file can be as large as the trees
// being settled: neither file is ever loaded whole.
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
	fa, err := os.Open(a)
	if err != nil {
		return false, fmt.Errorf("settle: read %s: %w", a, err)
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, fmt.Errorf("settle: read %s: %w", b, err)
	}
	defer fb.Close()
	bufA := make([]byte, 64<<10)
	bufB := make([]byte, 64<<10)
	for {
		na, ea := io.ReadFull(fa, bufA)
		if ea != nil && ea != io.EOF && ea != io.ErrUnexpectedEOF {
			return false, fmt.Errorf("settle: read %s: %w", a, ea)
		}
		nb, eb := io.ReadFull(fb, bufB)
		if eb != nil && eb != io.EOF && eb != io.ErrUnexpectedEOF {
			return false, fmt.Errorf("settle: read %s: %w", b, eb)
		}
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		if ea != nil || eb != nil {
			// Both streams ended on this chunk — the
			// sizes were equal, so they end together —
			// and every chunk matched.
			return true, nil
		}
	}
}

// permForMode maps a git tree mode for a regular file to the
// permission bits that mode denotes.
func permForMode(mode string) os.FileMode {
	if mode == "100755" {
		return 0o755
	}
	return 0o644
}

// syncTempName returns the temp name a write to dst lands
// through, in dst's directory: ".synctree-" (a regular file)
// or ".synctree-link-" (a symlink) plus the first 32 hex
// chars of the SHA-256 of the destination path and the
// operation ID, NUL-separated. The name is a pure function
// of state the settlement journal already records — every
// write unit's destination, and the journal's op — so it is
// knowable before any temp exists, and rollback can compute
// a killed write's temp name from the journal alone (PR #7
// wave 9). The op ID is in the derivation so two operations
// settling the same destination — the concurrent-Select
// shape — write through different temp names and never
// refuse each other at exclusive creation; one operation's
// creation and its cleanup derivations agree exactly,
// because both read the op from the same journal.
// Computing the name is not removing it: rollback removes a
// derived-name temp only for a unit whose journaled
// temp-created record proves this settlement created it
// (see removeUnitTemps). The hash keeps the name from
// colliding with a sibling unit's temp or with any name a
// tree plausibly carries; creation is exclusive (O_EXCL /
// symlink's own exclusivity), so a file already standing at
// the derived name is refused descriptively, never
// overwritten — it is not a temp this writer made.
func syncTempName(dst, op string, link bool) string {
	sum := sha256.Sum256([]byte(dst + "\x00" + op))
	prefix := ".synctree-"
	if link {
		prefix = ".synctree-link-"
	}
	return prefix + hex.EncodeToString(sum[:])[:32]
}

// copyFileAtomic writes src's bytes to dst with perm,
// atomically: a temp file in dst's directory, renamed over dst.
// A directory standing at dst is removed first — the target
// tree's shape wins. The temp takes its derived name
// (syncTempName, from dst and the governing operation's ID)
// and is created exclusively: a kill between the creation
// and the rename leaves a temp rollback can name, and a
// file already at that name is a refusal, not an overwrite.
// onTempCreated, when non-nil, is invoked after the temp's
// exclusive creation succeeds and before its bytes are
// written, with the kind created (false: this writer's file
// kind) and the temp's identity — for a file temp, the
// digest and size of the content it was created to carry
// (the bytes below, already read from src, so the identity
// costs no second read) — the journaled materializer's hook
// for persisting the write unit's per-kind temp-ownership
// record, identity bound into the same journal write, at
// the one moment the proof exists, so rollback may later
// remove that kind's temp as the settlement's own once it
// verifies the occupant against the identity. Rollback's
// restores pass their own hook, recording the restore temp
// on the unit being restored; callers whose temps need no
// ownership record (preimage capture, the staging builders)
// pass nil.
func copyFileAtomic(src, dst string, perm os.FileMode, op string, onTempCreated func(link bool, identity string) error) error {
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
	tmpName := filepath.Join(parent, syncTempName(dst, op, false))
	tmp, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("settle: create temp %s: %w", tmpName, err)
	}
	defer func() { _ = os.Remove(tmpName) }()
	if onTempCreated != nil {
		if err := onTempCreated(false, tempFileIdentity(data)); err != nil {
			_ = tmp.Close()
			return err
		}
	}
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
// renamed over dst. The temp name is the derived one
// (syncTempName): a symlink creation is itself exclusive, so
// a name already taken is a descriptive refusal and nothing
// is removed that this writer did not create — and a kill
// between the creation and the rename leaves a temp link
// rollback can name. onTempCreated, when non-nil, is
// invoked after the link's creation succeeds and before the
// rename, with the kind created (true: this writer's link
// kind) and the temp's identity — for a link temp, the
// link's target, the whole of what the temp is — the
// materializer's hook for persisting the write unit's
// per-kind temp-ownership record, as in copyFileAtomic;
// callers whose temp links need no record pass nil.
func replaceWithSymlink(dst, target, op string, onTempCreated func(link bool, identity string) error) error {
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("settle: create %s: %w", parent, err)
	}
	if info, err := os.Lstat(dst); err == nil && info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("settle: remove directory at %s: %w", dst, err)
		}
	}
	tmpName := filepath.Join(parent, syncTempName(dst, op, true))
	if err := os.Symlink(target, tmpName); err != nil {
		return fmt.Errorf("settle: create temp symlink %s: %w", tmpName, err)
	}
	// The removal is deferred only once the link exists and
	// is this writer's: a creation that failed because the
	// name was already taken must leave that file untouched.
	defer func() { _ = os.Remove(tmpName) }()
	if onTempCreated != nil {
		if err := onTempCreated(true, target); err != nil {
			return err
		}
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
