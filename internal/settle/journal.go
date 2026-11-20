package settle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// This file is the settlement journal and the journaled
// materializer (plan Task 8, AC-F6-4): the mechanism that closes
// the two crash windows Task 6's interim note recorded — a crash
// mid-materialization leaving the parent directory between
// trees, and a crash after the ref moves but before the fact is
// minted. The verbs' contracts are unchanged; what changed is
// that every write to a real tree is preceded by a durable,
// fsynced record of what is about to happen, including the
// complete settlement fact body, so whichever handle or process
// comes next can finish the act or undo it exactly.

// Journal states. A journal is written in state planned before
// the first side effect, moves to applying as its units land,
// and reaches verified when the parent directory provably
// matches the result artifact; the verb then completes the
// commit point (CAS + fact) and clears the journal.
const (
	journalStatePlanned  = "planned"
	journalStateApplying = "applying"
	journalStateVerified = "verified"
)

// Journal verbs — the materializing settlement verbs. Discard
// moves no tree and keeps no journal.
const (
	verbSelect = "select"
	verbApply  = "apply"
)

// Journal unit kinds.
const (
	unitWrite  = "write"
	unitDelete = "delete"
)

// unitCrashHook is the crash-injection hook for tests (the
// Phase 1 store fault-injection pattern): when non-nil it is
// invoked after every journal write that records progress —
// after the planned write (done == 0), after each unit's journal
// mark (done == units completed), and once at the commit point,
// after the verified write (done == total units). Production
// code never sets it; the crash test's child process sets it
// from an environment variable and calls os.Exit.
var unitCrashHook func(done int)

func callCrashHook(done int) {
	if unitCrashHook != nil {
		unitCrashHook(done)
	}
}

// journalUnit is one materialization unit: a single path the
// materializer writes (from the result artifact) or deletes
// (an extra), with its completion mark.
type journalUnit struct {
	Path string
	Kind string // unitWrite | unitDelete
	Done bool
}

// journal is one parent scope's in-flight settlement, recorded
// at Root/journals/<parentScope>.json (canonical JSON, fsynced
// at every state change). Fact is the verb's complete settlement
// fact body (Decision 4's shapes), computed by the verb and
// recorded at planned time, before the first side effect, so
// Recover mints the fact from the journal alone and never
// reconstructs it from live state.
//
// One field goes beyond the plan's contents list, by necessity:
// ParentDir. The plan fixes Recover's signature as
// Recover(ctx, parentScope) — no directory — and rollback must
// name the tree it restores; the journal is the only record a
// recovering process can reach, so the materializer records the
// target directory at planned time alongside the rest. (Flagged
// in the Task 8 report.)
type journal struct {
	Op           string
	Verb         string // verbSelect | verbApply
	ParentScope  string
	RunScope     string
	State        string // journalState*
	OldHead      string // the parent scope's head the verb CASes from
	ResultCommit string // the commit the verb CASes the parent scope to
	ParentDir    string // the real tree the units apply to
	Fact         canonical.Value
	Units        []journalUnit
}

func journalPath(root, parentScope string) string {
	return filepath.Join(root, "journals", parentScope+".json")
}

func stagingDirFor(root, op string) string {
	return filepath.Join(root, "staging", op)
}

// validOpID reports whether op is safe to name a staging
// directory: a single path component, never a traversal.
func validOpID(op string) bool {
	return op != "" && op != "." && op != ".." && !strings.ContainsAny(op, `/\`)
}

// validUnitPath reports whether p is a safe journal unit path:
// a clean slash-relative path with no traversal and no .git
// component (mirroring the snapshot walk's law). A journal is
// read back after a crash — possibly a hostile one — and its
// paths steer rollback writes, so they are validated as hostile
// input, never trusted (Review Focus).
func validUnitPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, elem := range strings.Split(p, "/") {
		if elem == ".." || elem == ".git" {
			return false
		}
	}
	return true
}

// writeJournal records j as parentScope's journal: canonical
// JSON to a temp file in the journals directory, fsynced, then
// renamed into place and the directory fsynced. A state change
// is therefore atomic and durable — a crash mid-write leaves
// the previous complete journal, never a torn one.
func writeJournal(root string, j *journal) error {
	units := make([]canonical.Value, 0, len(j.Units))
	for _, u := range j.Units {
		units = append(units, canonical.ObjectOf(
			canonical.Pair{Key: "done", Value: canonical.BoolOf(u.Done)},
			canonical.Pair{Key: "kind", Value: canonical.StringOf(u.Kind)},
			canonical.Pair{Key: "path", Value: canonical.StringOf(u.Path)},
		))
	}
	v := canonical.ObjectOf(
		canonical.Pair{Key: "fact", Value: j.Fact},
		canonical.Pair{Key: "old_head", Value: canonical.StringOf(j.OldHead)},
		canonical.Pair{Key: "op", Value: canonical.StringOf(j.Op)},
		canonical.Pair{Key: "parent_dir", Value: canonical.StringOf(j.ParentDir)},
		canonical.Pair{Key: "parent_scope", Value: canonical.StringOf(j.ParentScope)},
		canonical.Pair{Key: "result_commit", Value: canonical.StringOf(j.ResultCommit)},
		canonical.Pair{Key: "run_scope", Value: canonical.StringOf(j.RunScope)},
		canonical.Pair{Key: "state", Value: canonical.StringOf(j.State)},
		canonical.Pair{Key: "units", Value: canonical.ArrayOf(units...)},
		canonical.Pair{Key: "verb", Value: canonical.StringOf(j.Verb)},
	)
	b, err := canonical.MarshalKernel(v)
	if err != nil {
		return fmt.Errorf("settle: marshal journal for %q: %w", j.ParentScope, err)
	}
	dir := filepath.Join(root, "journals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("settle: create journals dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".journal-*")
	if err != nil {
		return fmt.Errorf("settle: create journal temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("settle: write journal temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("settle: fsync journal temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("settle: close journal temp: %w", err)
	}
	if err := os.Rename(tmpName, journalPath(root, j.ParentScope)); err != nil {
		return fmt.Errorf("settle: rename journal into place: %w", err)
	}
	return syncDir(dir)
}

// readJournal loads and validates parentScope's journal. A
// missing journal is (nil, nil) — nothing to recover. A journal
// that fails to decode or to validate is a descriptive error:
// recovery never guesses from torn or hostile bytes (Review
// Focus), and the caller leaves the file in place as evidence.
func readJournal(root, parentScope string) (*journal, error) {
	b, err := os.ReadFile(journalPath(root, parentScope))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("settle: read journal for %q: %w", parentScope, err)
	}
	v, err := canonical.DecodeKernel(b)
	if err != nil {
		return nil, fmt.Errorf("settle: journal for %q does not decode: %w", parentScope, err)
	}
	j, err := decodeJournal(v)
	if err != nil {
		return nil, fmt.Errorf("settle: journal for %q is invalid: %w", parentScope, err)
	}
	if j.ParentScope != parentScope {
		return nil, fmt.Errorf("settle: journal at %q names parent scope %q", journalPath(root, parentScope), j.ParentScope)
	}
	return j, nil
}

func boolField(v canonical.Value, key string) (bool, bool) {
	if v.Kind != canonical.KindObject {
		return false, false
	}
	for _, p := range v.Pairs {
		if p.Key == key {
			if p.Value.Kind != canonical.KindBool {
				return false, false
			}
			return p.Value.Bool, true
		}
	}
	return false, false
}

func objectField(v canonical.Value, key string) (canonical.Value, bool) {
	if v.Kind != canonical.KindObject {
		return canonical.Value{}, false
	}
	for _, p := range v.Pairs {
		if p.Key == key {
			if p.Value.Kind != canonical.KindObject {
				return canonical.Value{}, false
			}
			return p.Value, true
		}
	}
	return canonical.Value{}, false
}

func arrayField(v canonical.Value, key string) ([]canonical.Value, bool) {
	if v.Kind != canonical.KindObject {
		return nil, false
	}
	for _, p := range v.Pairs {
		if p.Key == key {
			if p.Value.Kind != canonical.KindArray {
				return nil, false
			}
			return p.Value.Elems, true
		}
	}
	return nil, false
}

// stringArrayField returns the string elements stored under key
// in a canonical object value, or nil if the field is absent or
// not an array of strings.
func stringArrayField(v canonical.Value, key string) []string {
	elems, ok := arrayField(v, key)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		if e.Kind != canonical.KindString {
			return nil
		}
		out = append(out, e.Str)
	}
	return out
}

// decodeJournal validates a decoded journal value's shape and
// cross-field consistency: the recorded fact body must agree
// with the journal's own fields, so a journal whose parts
// contradict each other is refused rather than acted on.
func decodeJournal(v canonical.Value) (*journal, error) {
	if v.Kind != canonical.KindObject {
		return nil, errors.New("top level is not an object")
	}
	var j journal
	for key, dst := range map[string]*string{
		"op":            &j.Op,
		"verb":          &j.Verb,
		"parent_scope":  &j.ParentScope,
		"run_scope":     &j.RunScope,
		"state":         &j.State,
		"old_head":      &j.OldHead,
		"result_commit": &j.ResultCommit,
		"parent_dir":    &j.ParentDir,
	} {
		s, ok := stringField(v, key)
		if !ok || s == "" {
			return nil, fmt.Errorf("field %q is missing or not a non-empty string", key)
		}
		*dst = s
	}
	if j.Verb != verbSelect && j.Verb != verbApply {
		return nil, fmt.Errorf("unknown verb %q", j.Verb)
	}
	switch j.State {
	case journalStatePlanned, journalStateApplying, journalStateVerified:
	default:
		return nil, fmt.Errorf("unknown state %q", j.State)
	}
	if !validOpID(j.Op) {
		return nil, fmt.Errorf("op %q is not a single path component", j.Op)
	}
	if !filepath.IsAbs(j.ParentDir) {
		return nil, fmt.Errorf("parent_dir %q is not an absolute path", j.ParentDir)
	}
	fact, ok := objectField(v, "fact")
	if !ok {
		return nil, errors.New("field \"fact\" is missing or not an object")
	}
	j.Fact = fact
	unitVals, ok := arrayField(v, "units")
	if !ok {
		return nil, errors.New("field \"units\" is missing or not an array")
	}
	for i, uv := range unitVals {
		p, ok := stringField(uv, "path")
		if !ok || !validUnitPath(p) {
			return nil, fmt.Errorf("unit %d has an invalid path", i)
		}
		kind, ok := stringField(uv, "kind")
		if !ok || (kind != unitWrite && kind != unitDelete) {
			return nil, fmt.Errorf("unit %d has an invalid kind", i)
		}
		done, ok := boolField(uv, "done")
		if !ok {
			return nil, fmt.Errorf("unit %d has no done flag", i)
		}
		j.Units = append(j.Units, journalUnit{Path: p, Kind: kind, Done: done})
	}
	// The recorded fact body must agree with the journal: it is
	// the authority Recover mints from, so a disagreement means
	// the journal's parts were not written by one verb run.
	for key, want := range map[string]string{
		"op":           j.Op,
		"parent_scope": j.ParentScope,
		"run_scope":    j.RunScope,
	} {
		if got, ok := stringField(fact, key); !ok || got != want {
			return nil, fmt.Errorf("fact body field %q does not match the journal", key)
		}
	}
	switch j.Verb {
	case verbSelect:
		if got, ok := stringField(fact, "parent_head"); !ok || got != j.OldHead {
			return nil, errors.New("select fact's parent_head does not match old_head")
		}
		if got, ok := stringField(fact, "run_head"); !ok || got != j.ResultCommit {
			return nil, errors.New("select fact's run_head does not match result_commit")
		}
	case verbApply:
		if _, ok := stringField(fact, "fork_commit"); !ok {
			return nil, errors.New("apply fact has no fork_commit")
		}
		if got, ok := stringField(fact, "result_commit"); !ok || got != j.ResultCommit {
			return nil, errors.New("apply fact's result_commit does not match result_commit")
		}
		if _, ok := arrayField(fact, "settled_paths"); !ok {
			return nil, errors.New("apply fact has no settled_paths array")
		}
	}
	return &j, nil
}

// clearJournalState removes parentScope's journal and the op's
// staging directory (preimages and the result-artifact tree):
// the settlement is complete or undone, and its operational
// state goes with it.
func clearJournalState(root, parentScope, op string) error {
	if err := os.Remove(journalPath(root, parentScope)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("settle: remove journal for %q: %w", parentScope, err)
	}
	if err := os.RemoveAll(stagingDirFor(root, op)); err != nil {
		return fmt.Errorf("settle: remove staging for op %q: %w", op, err)
	}
	return nil
}

// materializeRequest is one verb's commit-point materialization:
// make parentDir match want (the workspace file map of
// resultCommit) under a journal that records the act — the
// verb's identity, its commit-point endpoints, and its complete
// settlement fact body — before the first write.
type materializeRequest struct {
	verb         string // verbSelect | verbApply
	opID         string
	parentScope  string
	runScope     string
	parentDir    string
	oldHead      string // the parent scope's head the verb CASes from
	resultCommit string // the commit the verb CASes the parent scope to
	want         map[string]snapshot.WorkspaceEntry
	fact         canonical.Value // the verb's complete settlement fact body
}

// materialize is the journaled tree materializer both verbs
// commit through (replacing Task 6/7's bare syncTree):
//
//   - preflight: extract the result commit's workspace into the
//     op's staging tree (the durable result artifact), compute
//     the unit list against the parent's current inventory, and
//     copy the preimage of every unit target that exists into
//     the staging preimage tree — all before the first side
//     effect;
//   - write the journal in state planned and fsync it;
//   - apply: per unit, write (temp file + rename in the target
//     directory, atomic per file) or delete, mark the unit done,
//     rewrite + fsync the journal;
//   - verify: re-snapshot the parent; its file map must equal
//     want, then journal state verified and fsync.
//
// On success the journal (verified) and staging are left in
// place: the verb completes the commit point (CAS + fact) and
// clears them. On any error the journal stays where it stopped,
// and Recover — at the next verb's entry, or called directly —
// resolves it.
func materialize(ctx *Context, req materializeRequest) error {
	if !validOpID(req.opID) {
		return fmt.Errorf("settle: %s %q: op ID %q is not a single path component", req.verb, req.runScope, req.opID)
	}
	// The journal records ParentDir, and Recover — whose fixed
	// signature receives no directory — acts on the recorded
	// value from whatever process and working directory comes
	// next. A directory spelled relatively ("." or "workspace")
	// would be recorded verbatim and then refused by
	// decodeJournal's absolute-path validation: the journal a
	// crashed verb most needs read back would be the one
	// recovery cannot read, and every later verb on that parent
	// would fail at its entry Recover. Normalize here — the one
	// path both materializing verbs take, before the journal is
	// written or the parent is touched; decodeJournal's
	// requirement stays as the backstop for journals written by
	// anything else.
	absDir, err := filepath.Abs(req.parentDir)
	if err != nil {
		return fmt.Errorf("settle: %s %q: resolve parent directory %q: %w", req.verb, req.runScope, req.parentDir, err)
	}
	req.parentDir = absDir
	stagingRoot := filepath.Join(ctx.Root, "staging")
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return fmt.Errorf("settle: create staging dir: %w", err)
	}
	stage := stagingDirFor(ctx.Root, req.opID)
	// The op's staging is disposable until the journal exists:
	// a remnant here belongs to an attempt that never reached
	// its planned write (a live journal's staging is resolved
	// by Recover at verb entry, before any new materialization).
	_ = os.RemoveAll(stage)
	treeDir := filepath.Join(stage, "tree")
	if err := ctx.Snaps.ExtractWorkspace(req.resultCommit, treeDir); err != nil {
		return err
	}

	current, curDirs, err := walkDirFiles(req.parentDir)
	if err != nil {
		return err
	}
	units, writeEntry, err := planUnits(req, treeDir, current)
	if err != nil {
		return err
	}

	// Preflight: capture the preimage of every unit target that
	// exists — the bytes (or link target) rollback restores.
	preDir := filepath.Join(stage, "pre")
	for _, u := range units {
		if err := capturePreimage(req.parentDir, preDir, u.Path); err != nil {
			return err
		}
	}

	j := &journal{
		Op:           req.opID,
		Verb:         req.verb,
		ParentScope:  req.parentScope,
		RunScope:     req.runScope,
		State:        journalStatePlanned,
		OldHead:      req.oldHead,
		ResultCommit: req.resultCommit,
		ParentDir:    req.parentDir,
		Fact:         req.fact,
		Units:        units,
	}
	if err := writeJournal(ctx.Root, j); err != nil {
		return err
	}
	callCrashHook(0)

	unitIndex := make(map[string]int, len(j.Units))
	for i, u := range j.Units {
		unitIndex[u.Path] = i
	}
	done := 0
	mark := func() error {
		if j.State == journalStatePlanned {
			j.State = journalStateApplying
		}
		if err := writeJournal(ctx.Root, j); err != nil {
			return err
		}
		callCrashHook(done)
		return nil
	}

	// Writes first (Task 6/7's order): a file standing where a
	// wanted path's ancestor directory belongs is removed as
	// part of the write that needs the way — it is an extra
	// whose own delete unit is marked done at the same journal
	// mark, since the file is genuinely gone.
	for i := range j.Units {
		u := &j.Units[i]
		if u.Kind != unitWrite {
			continue
		}
		for parent := pathParent(u.Path); parent != ""; parent = pathParent(parent) {
			if _, isEntry := current[parent]; !isEntry {
				continue
			}
			if err := os.Remove(filepath.Join(req.parentDir, filepath.FromSlash(parent))); err != nil {
				return fmt.Errorf("settle: remove blocking %s: %w", parent, err)
			}
			delete(current, parent)
			if di, ok := unitIndex[parent]; ok && !j.Units[di].Done {
				j.Units[di].Done = true
				done++
			}
		}
		if err := applyWriteUnit(req, treeDir, u.Path, writeEntry[u.Path]); err != nil {
			return err
		}
		u.Done = true
		done++
		// The symmetric accounting (Task 8 fix round): a write
		// that lands where a directory stood clears that
		// directory's whole subtree to take its place, so every
		// extra beneath the target is destroyed by this write —
		// its delete unit is done, and the journal says so at
		// the same mark. Without the mark, rollback would leave
		// those files unrestored: their destruction would be
		// recorded nowhere.
		for k := range j.Units {
			du := &j.Units[k]
			if du.Kind == unitDelete && !du.Done && strings.HasPrefix(du.Path, u.Path+"/") {
				du.Done = true
				done++
			}
		}
		if err := mark(); err != nil {
			return err
		}
	}
	// Deletes: extras the writes did not already consume. A
	// delete whose target is already gone is done — the file is
	// absent, which is all the unit asks.
	for i := range j.Units {
		u := &j.Units[i]
		if u.Kind != unitDelete || u.Done {
			continue
		}
		if err := os.Remove(filepath.Join(req.parentDir, filepath.FromSlash(u.Path))); err != nil && !targetGone(err) {
			return fmt.Errorf("settle: remove extra %s: %w", u.Path, err)
		}
		u.Done = true
		done++
		if err := mark(); err != nil {
			return err
		}
	}

	// Prune directories the result tree does not imply (deepest
	// first; a prune only succeeds on an empty directory, so
	// implied directories are never at risk). Directory shape is
	// outside the file-map contract — the snapshot verify below
	// is the arbiter — and mirrors the bare materializer's law.
	//
	// The inventory is pre-materialization: a path that was a
	// directory then may since have been replaced by a write
	// unit (a shape-changing delta). Only prune what is still a
	// directory — an unconditional remove would unlink the file
	// the write just landed (Task 8 fix round).
	implied := map[string]bool{}
	for rel := range req.want {
		for parent := pathParent(rel); parent != ""; parent = pathParent(parent) {
			implied[parent] = true
		}
	}
	// Directories beneath a path a write unit replaced are
	// excluded lexically, without resolving them: the write
	// destroyed the pre-materialization directory at its own
	// target (both writers remove what stood there), so a
	// curDirs entry beneath it no longer names a directory of
	// this tree — resolving it now would follow the
	// replacement, and for a directory→symlink swap the Lstat
	// below would find the LINK TARGET's directory at that
	// relative path and the prune would remove it, outside the
	// tree, invisible to the verify and unrestorable by the
	// journal. The same lexical decision the union builder
	// makes for its deletions (replacedAncestor, apply.go).
	replaced := map[string]bool{}
	for _, u := range j.Units {
		if u.Kind == unitWrite {
			replaced[u.Path] = true
		}
	}
	sort.Slice(curDirs, func(a, b int) bool {
		return strings.Count(curDirs[a], "/") > strings.Count(curDirs[b], "/")
	})
	for _, d := range curDirs {
		if implied[d] || replacedAncestor(d, replaced) {
			continue
		}
		dst := filepath.Join(req.parentDir, filepath.FromSlash(d))
		if info, err := os.Lstat(dst); err != nil || !info.IsDir() {
			continue
		}
		_ = os.Remove(dst)
	}

	// Verify: re-snapshot the parent; its file map must equal
	// the result artifact's before the journal may say verified.
	verifyCommit, err := ctx.Snaps.Snapshot(req.parentDir, map[string]string{"settle": req.verb + "-verify"})
	if err != nil {
		return fmt.Errorf("settle: %s %q: verify snapshot of parent directory: %w", req.verb, req.runScope, err)
	}
	got, err := ctx.Snaps.WorkspaceFiles(verifyCommit)
	if err != nil {
		return fmt.Errorf("settle: %s %q: %w", req.verb, req.runScope, err)
	}
	if !reflect.DeepEqual(got, req.want) {
		return fmt.Errorf("settle: %s %q: parent directory does not match the result tree after materialization", req.verb, req.runScope)
	}
	j.State = journalStateVerified
	if err := writeJournal(ctx.Root, j); err != nil {
		return err
	}
	callCrashHook(done)
	return nil
}

// planUnits computes the materialization unit list: writes
// (added and changed paths — a mode-only difference counts as
// changed, amendment F2) in sorted order, then deletes (extras)
// in sorted order. The order is the journal's record of the
// application order, and rollback respects its shape discipline
// (see rollbackJournal).
func planUnits(req materializeRequest, treeDir string, current map[string]dirFile) ([]journalUnit, map[string]snapshot.WorkspaceEntry, error) {
	var units []journalUnit
	writeEntry := map[string]snapshot.WorkspaceEntry{}
	rels := make([]string, 0, len(req.want))
	for rel := range req.want {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		entry := req.want[rel]
		src := filepath.Join(treeDir, filepath.FromSlash(rel))
		dst := filepath.Join(req.parentDir, filepath.FromSlash(rel))
		cur, exists := current[rel]
		write := false
		switch {
		case entry.Symlink:
			write = true
			if exists && cur.symlink {
				curTarget, err1 := os.Readlink(dst)
				srcTarget, err2 := os.Readlink(src)
				if err1 == nil && err2 == nil && curTarget == srcTarget {
					write = false
				}
			}
		case exists && cur.other:
			// A special file where a regular file belongs: the
			// probe snapshot has already refused unrecordable
			// trees, so this is a shape the target replaces.
			write = true
		case exists && !cur.symlink:
			same, err := filesEqual(src, dst)
			if err != nil {
				return nil, nil, err
			}
			write = !same || cur.mode != entry.Mode
		default:
			write = true
		}
		if write {
			units = append(units, journalUnit{Path: rel, Kind: unitWrite})
			writeEntry[rel] = entry
		}
	}
	var extras []string
	for rel := range current {
		if _, ok := req.want[rel]; !ok {
			extras = append(extras, rel)
		}
	}
	sort.Strings(extras)
	for _, rel := range extras {
		units = append(units, journalUnit{Path: rel, Kind: unitDelete})
	}
	return units, writeEntry, nil
}

// applyWriteUnit lands one write unit from the staged result
// tree: a symlink is recreated as a symlink; a regular file is
// written via temp file + rename in the target directory
// (atomic per file) with the tree's mode.
func applyWriteUnit(req materializeRequest, treeDir, rel string, entry snapshot.WorkspaceEntry) error {
	src := filepath.Join(treeDir, filepath.FromSlash(rel))
	dst := filepath.Join(req.parentDir, filepath.FromSlash(rel))
	if entry.Symlink {
		target, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("settle: read staged symlink %s: %w", src, err)
		}
		return replaceWithSymlink(dst, target)
	}
	return copyFileAtomic(src, dst, permForMode(entry.Mode))
}

// capturePreimage copies one unit target's current occupant
// into the preimage tree, preserving its kind: a regular file's
// bytes and mode, or a symlink's target. A target that does not
// exist has no preimage — rollback removes what the unit
// created. A directory or special file has no file preimage:
// a directory's contents are their own units with their own
// preimages, and a special file's content is not restorable
// (the probe snapshot refuses unrecordable trees before a verb
// writes, so a verb does not meet one at a unit target it did
// not place there itself).
//
// The occupant is the path's own inventory entry, lexically:
// if any ancestor component is a symlink or missing, the path
// has no occupant of its own — a path spelled through a
// symlinked ancestor names some other path's file (the alias
// shape of Task 7's census), and capturing through the alias
// would record the wrong file's bytes under this path.
func capturePreimage(parentDir, preDir, rel string) error {
	parts := strings.Split(rel, "/")
	ancestor := parentDir
	for _, part := range parts[:len(parts)-1] {
		ancestor = filepath.Join(ancestor, part)
		info, err := os.Lstat(ancestor)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("settle: stat preimage ancestor %s: %w", ancestor, err)
		}
		if !info.IsDir() {
			return nil
		}
	}
	src := filepath.Join(parentDir, filepath.FromSlash(rel))
	info, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("settle: stat preimage source %s: %w", src, err)
	}
	dst := filepath.Join(preDir, filepath.FromSlash(rel))
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("settle: read preimage symlink %s: %w", src, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("settle: create preimage dir: %w", err)
		}
		if err := os.Symlink(target, dst); err != nil {
			return fmt.Errorf("settle: capture preimage symlink %s: %w", dst, err)
		}
		return nil
	case info.Mode().IsRegular():
		return copyFileAtomic(src, dst, info.Mode().Perm())
	default:
		return nil
	}
}

// Recover resolves parentScope's journal, if one exists: the
// settlement a crashed (or failed) verb left behind is
// completed or rolled back before any new work is judged
// against the scope. No journal → nil. A journal that fails to
// decode or validate → a descriptive error, no writes, the
// journal left in place as evidence.
//
// State verified → the commit point is completed idempotently:
// the parent scope advances old_head → result_commit if it has
// not moved; if it already sits at result_commit the move is
// the crashed verb's own and recovery proceeds; anything else
// is a descriptive refusal — the line moved under a crashed
// verb, and recovery does not guess. The verb's fact is then
// minted from the journal's recorded fact body iff no fact
// carries the op, and journal + staging are cleared.
//
// State planned/applying → roll back: every done unit is
// undone from its preimage, and the parent directory is exactly
// pre-apply; journal + staging are cleared.
//
// Every verb (Select, Apply) calls Recover for its parent scope
// at entry; Recover is also exported so the next handle or
// process can resolve a crashed predecessor directly.
func Recover(ctx *Context, parentScope string) error {
	j, err := readJournal(ctx.Root, parentScope)
	if err != nil {
		return err
	}
	if j == nil {
		return nil
	}
	switch j.State {
	case journalStateVerified:
		head, err := ctx.Snaps.ScopeHead(j.ParentScope)
		if err != nil {
			return fmt.Errorf("settle: recover %q: %w", parentScope, err)
		}
		switch head {
		case j.OldHead:
			if err := ctx.Snaps.AdvanceScope(j.ParentScope, j.OldHead, j.ResultCommit); err != nil {
				return fmt.Errorf("settle: recover %q: %w", parentScope, err)
			}
		case j.ResultCommit:
			// The ref move landed before the crash; proceed.
		default:
			return fmt.Errorf("settle: recover %q: parent scope is at %s, journal expects %s or %s — refusing to guess", parentScope, head, j.OldHead, j.ResultCommit)
		}
		found, err := settlementFactExists(ctx, j)
		if err != nil {
			return err
		}
		if !found {
			if _, err := mintFromJournal(ctx, j); err != nil {
				return fmt.Errorf("settle: recover %q: %w", parentScope, err)
			}
		}
		return clearJournalState(ctx.Root, j.ParentScope, j.Op)
	case journalStatePlanned, journalStateApplying:
		return rollbackJournal(ctx, j)
	default:
		// Unreachable: readJournal validates the state.
		return fmt.Errorf("settle: recover %q: unknown journal state %q", parentScope, j.State)
	}
}

// rollbackJournal undoes a planned/applying journal's done
// units from their preimages. Write units are undone first, in
// reverse order, then delete units in reverse order: the two
// kinds interact only through path shape (a file where a
// directory belongs, or the reverse), and undoing writes first
// guarantees a restored delete never has to pass through a file
// a write unit created — the parent is then exactly pre-apply.
func rollbackJournal(ctx *Context, j *journal) error {
	preDir := filepath.Join(stagingDirFor(ctx.Root, j.Op), "pre")
	for i := len(j.Units) - 1; i >= 0; i-- {
		u := j.Units[i]
		if u.Kind != unitWrite || !u.Done {
			continue
		}
		if err := undoWriteUnit(j.ParentDir, preDir, u.Path); err != nil {
			return fmt.Errorf("settle: recover %q: roll back %s: %w", j.ParentScope, u.Path, err)
		}
	}
	for i := len(j.Units) - 1; i >= 0; i-- {
		u := j.Units[i]
		if u.Kind != unitDelete || !u.Done {
			continue
		}
		if err := undoDeleteUnit(j.ParentDir, preDir, u.Path); err != nil {
			return fmt.Errorf("settle: recover %q: roll back %s: %w", j.ParentScope, u.Path, err)
		}
	}
	return clearJournalState(ctx.Root, j.ParentScope, j.Op)
}

// targetGone reports whether a remove error means the target
// is absent: either it does not exist, or an ancestor component
// is no longer a directory (ENOTDIR) — a shape-changing write
// at the ancestor already destroyed it, so the path names
// nothing. Either way the absence a delete unit asks for holds.
func targetGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// statPreimage stats rel's preimage inside the preimage tree,
// lexically: the ancestor components are walked one by one
// with Lstat, and the walk stops — reporting no preimage — at
// the first ancestor that does not stand as a real directory
// there. The stop matters because the preimage tree stores a
// symlink occupant's preimage AS a symlink: a plain Lstat of
// the full path would resolve through that stored link and
// find the link target's live file, mistaking another tree's
// file for this unit's preimage (the symlink→directory swap:
// pre/old is the restored link, and pre/old/inner.txt reads as
// the outside target's unchanged inner.txt). Capture is
// lexical — it never records anything beneath a symlink or a
// file — so a path whose preimage-tree ancestors do not all
// stand as real directories has no preimage of its own,
// whatever a resolving stat would find there.
func statPreimage(preDir, rel string) (fs.FileInfo, bool, error) {
	parts := strings.Split(rel, "/")
	cur := preDir
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if targetGone(err) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("settle: stat preimage ancestor %s: %w", cur, err)
		}
		if !info.IsDir() {
			return nil, false, nil
		}
	}
	pre := filepath.Join(preDir, filepath.FromSlash(rel))
	info, err := os.Lstat(pre)
	if err != nil {
		if targetGone(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("settle: stat preimage %s: %w", pre, err)
	}
	return info, true, nil
}

// blockedByAncestor reports whether rel, read inside the live
// parentDir, fails to resolve through real directories of that
// tree: some proper ancestor component stands as a symlink
// (the path then names another tree's file — above all a
// symlink an earlier rollback pass restored from its
// preimage), a plain file (the path names nothing), or is
// missing (likewise). Rollback decides such paths lexically
// and never reaches through the replacement: the occupant a
// unit created beneath a since-restored ancestor is not
// reachable in this tree, and removing or restoring through
// the ancestor would delete and write in the tree the symlink
// names. The walk is component-wise from the top, stopping at
// the first non-directory, so no step ever resolves through a
// component a previous step has not cleared.
func blockedByAncestor(parentDir, rel string) (bool, error) {
	parts := strings.Split(rel, "/")
	cur := parentDir
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if targetGone(err) {
				return true, nil
			}
			return false, fmt.Errorf("settle: stat ancestor %s: %w", cur, err)
		}
		if !info.IsDir() {
			return true, nil
		}
	}
	return false, nil
}

// symlinkAncestor reports whether some proper ancestor
// component of rel inside parentDir stands as a symlink. The
// walk is component-wise from the top and stops at the first
// component that names nothing or is not a directory: nothing
// beneath such a component can be a symlink of this tree, and
// no step resolves through a component a previous step has
// not cleared.
func symlinkAncestor(parentDir, rel string) (bool, error) {
	parts := strings.Split(rel, "/")
	cur := parentDir
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if targetGone(err) {
				return false, nil
			}
			return false, fmt.Errorf("settle: stat ancestor %s: %w", cur, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return true, nil
		}
		if !info.IsDir() {
			return false, nil
		}
	}
	return false, nil
}

// removeCreatedOccupant removes the occupant a write unit
// created at dst — unless an ancestor of rel stands replaced
// in the live tree (blockedByAncestor), in which case the
// occupant is not reachable in this tree and the absence the
// undo asks for already holds: an earlier rollback pass
// restored the ancestor's pre-state occupant, destroying (or
// shadowing) whatever the unit created beneath it, and the
// only thing still naming the unit's path is the replacement
// itself, above all a restored symlink whose target settlement
// has no claim to.
func removeCreatedOccupant(parentDir, rel, dst string) error {
	blocked, err := blockedByAncestor(parentDir, rel)
	if err != nil {
		return err
	}
	if blocked {
		return nil
	}
	return removeOccupant(dst)
}

// undoWriteUnit reverses one applied write unit: if the target
// had a preimage, the preimage is restored; if it had none, the
// unit created the occupant and it is removed.
//
// Two shape-changing cases (Task 8 fix round) have no file
// preimage of their own, and both end at removeCreatedOccupant:
//
//   - no preimage beneath a non-directory ancestor of the
//     preimage path: another unit's preimage stands there (the
//     file→directory swap: the delete unit's file preimage sits
//     at pre/old.txt, shadowing everything beneath it; the
//     symlink→directory swap: the delete unit's SYMLINK preimage
//     sits at pre/old). Capture is lexical, so nothing was ever
//     recorded beneath either; there is no preimage at this
//     path — and statPreimage decides that lexically, never by
//     resolving through the stored preimage.
//   - the preimage path is a directory: it exists only to hold
//     the preimages of the units beneath the target (the
//     directory→file swap: pre/old.txt/ holds inner.txt's
//     preimage). A directory's contents are their own units,
//     restored by their own undo steps; the directory itself is
//     outside the file-map contract, and reading it as a file
//     would wedge recovery on a lawful shape.
func undoWriteUnit(parentDir, preDir, rel string) error {
	dst := filepath.Join(parentDir, filepath.FromSlash(rel))
	preInfo, hasPreimage, err := statPreimage(preDir, rel)
	if err != nil {
		return err
	}
	if !hasPreimage || preInfo.IsDir() {
		return removeCreatedOccupant(parentDir, rel, dst)
	}
	return restorePreimage(parentDir, preDir, rel)
}

// undoDeleteUnit reverses one applied delete unit by restoring
// its preimage. A delete unit's target existed by construction,
// so a missing preimage contradicts the journal: a descriptive
// refusal, never a guess.
func undoDeleteUnit(parentDir, preDir, rel string) error {
	if _, hasPreimage, err := statPreimage(preDir, rel); err != nil {
		return err
	} else if !hasPreimage {
		return fmt.Errorf("settle: delete unit %q is marked done but has no preimage — journal and staging disagree", rel)
	}
	return restorePreimage(parentDir, preDir, rel)
}

// removeOccupant removes whatever stands at dst — a file, a
// symlink, or a directory tree (a shape a later unit's writes
// may have left above a restored path). Absence is success —
// absence in targetGone's full sense: a stat that fails
// ENOTDIR means an ancestor is a file again, which is exactly
// the state a repeated rollback meets after an interrupted
// Recover restored a swapped path's file and died before
// clearing the journal. Recognizing only ErrNotExist there
// wedges recovery permanently — every later Recover repeats
// the same undo and fails the same way, and the verbs that
// call Recover at entry refuse with it.
func removeOccupant(dst string) error {
	info, err := os.Lstat(dst)
	if targetGone(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("settle: stat %s: %w", dst, err)
	}
	if info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("settle: remove directory at %s: %w", dst, err)
		}
		return nil
	}
	if err := os.Remove(dst); err != nil {
		return fmt.Errorf("settle: remove %s: %w", dst, err)
	}
	return nil
}

// restorePreimage puts rel's preimage back at its path in
// parentDir: the current occupant is removed, the parent
// directory recreated, and the preimage's kind restored — a
// symlink's target, or a regular file's bytes and mode.
//
// A unit that HAS a preimage had real directories for
// ancestors when the preimage was captured (capture is
// lexical), and no unit's undo can have restored a symlink at
// an ancestor of such a path — that unit's preimage would be
// the symlink, contradicting this unit's preimage beneath it.
// So a symlink standing at an ancestor here means the journal
// and the tree disagree, and restoration refuses descriptively
// rather than remove, create, or write through the link into
// whatever tree it names. A MISSING ancestor is lawful, not a
// disagreement: an earlier undo in the same rollback removed
// what stood there (the directory→file swap's landed file),
// and the restore recreates the directory itself.
func restorePreimage(parentDir, preDir, rel string) error {
	dst := filepath.Join(parentDir, filepath.FromSlash(rel))
	pre := filepath.Join(preDir, filepath.FromSlash(rel))
	throughLink, err := symlinkAncestor(parentDir, rel)
	if err != nil {
		return err
	}
	if throughLink {
		return fmt.Errorf("settle: restore %s: an ancestor stands as a symlink — journal and staging disagree", rel)
	}
	preInfo, err := os.Lstat(pre)
	if err != nil {
		return fmt.Errorf("settle: stat preimage %s: %w", pre, err)
	}
	if err := removeOccupant(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("settle: create %s: %w", filepath.Dir(dst), err)
	}
	if preInfo.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(pre)
		if err != nil {
			return fmt.Errorf("settle: read preimage symlink %s: %w", pre, err)
		}
		if err := os.Symlink(target, dst); err != nil {
			return fmt.Errorf("settle: restore symlink %s: %w", dst, err)
		}
		return nil
	}
	return copyFileAtomic(pre, dst, preInfo.Mode().Perm())
}

// settlementFactExists reports whether the run scope's trace
// path already carries the journal verb's settlement fact under
// the journal's op — the consume-once scan, keyed on
// (schema ref, run scope, op) per Decision 4.
func settlementFactExists(ctx *Context, j *journal) (bool, error) {
	wantRef := store.SettleSelectSchemaRef
	if j.Verb == verbApply {
		wantRef = store.SettleApplySchemaRef
	}
	recs, err := ctx.Trace.PathRecords(j.RunScope)
	if err != nil {
		return false, fmt.Errorf("settle: scan %q for op %q: %w", j.RunScope, j.Op, err)
	}
	for _, rec := range recs {
		if ref, _ := stringField(rec.Record, "schema_ref"); ref != wantRef {
			continue
		}
		for _, p := range rec.Record.Pairs {
			if p.Key != "body" {
				continue
			}
			if op, ok := stringField(p.Value, "op"); ok && op == j.Op {
				return true, nil
			}
		}
	}
	return false, nil
}

// mintFromJournal mints the journal verb's settlement fact from
// the journal's recorded fact body alone — the body was
// computed by the verb and recorded at planned time, so
// recovery never reconstructs it from live state (amendment
// F4). readJournal has already cross-checked the body against
// the journal's fields.
func mintFromJournal(ctx *Context, j *journal) (store.Receipt, error) {
	op, _ := stringField(j.Fact, "op")
	switch j.Verb {
	case verbSelect:
		parentHead, _ := stringField(j.Fact, "parent_head")
		runHead, _ := stringField(j.Fact, "run_head")
		return ctx.Trace.MintSettleSelect(j.RunScope, op, j.ParentScope, j.RunScope, parentHead, runHead)
	case verbApply:
		forkCommit, _ := stringField(j.Fact, "fork_commit")
		resultCommit, _ := stringField(j.Fact, "result_commit")
		return ctx.Trace.MintSettleApply(j.RunScope, op, j.ParentScope, j.RunScope, forkCommit, resultCommit, stringArrayField(j.Fact, "settled_paths"))
	default:
		return store.Receipt{}, fmt.Errorf("settle: cannot mint a fact for unknown verb %q", j.Verb)
	}
}
