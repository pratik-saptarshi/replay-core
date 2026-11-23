package settle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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
// after the verified write (done == total units) — and, since
// the in-flight fix, once per unit in the window between that
// unit's mutation landing and its completion mark being
// persisted (done == units completed including that unit; the
// journal on disk at that point says started, not done), so
// the crash suite exercises the window and not only the marks.
// Production code never sets it; the crash test's child process
// sets it from an environment variable and calls os.Exit.
var unitCrashHook func(done int)

func callCrashHook(done int) {
	if unitCrashHook != nil {
		unitCrashHook(done)
	}
}

// journalUnit is one materialization unit: a single path the
// materializer writes (from the result artifact) or deletes
// (an extra), with its completion mark and its in-flight mark.
// Started is recorded — and the journal rewritten and fsynced —
// BEFORE the unit's first mutation lands: a crash after the
// mutation but before the completion mark then leaves a unit
// rollback knows it may have to undo, instead of a mutation no
// durable record admits to. A unit a write's landing destroys
// as a side effect (a blocking ancestor it removes, an extra
// beneath its target it clears) is started by that write, for
// the same reason.
type journalUnit struct {
	Path    string
	Kind    string // unitWrite | unitDelete
	Done    bool
	Started bool
	// TempCreatedFile and TempCreatedLink are the ownership
	// records for the unit's derived-name temps, one per
	// temp kind: persisted — a journal rewrite in the
	// recordIntent style — after a temp's exclusive creation
	// succeeds and before its content is written, so a
	// durable record proves this settlement created that
	// kind's temp before any byte of it lands. The record
	// is per kind because the deletion is: a write unit
	// creates exactly one kind (a regular file lands through
	// copyFileAtomic, a symlink through replaceWithSymlink),
	// and a unit-wide record would vouch for a temp of the
	// other kind this settlement never created — the same
	// scope divergence as no record at all, one kind over.
	// The materializer's write loop sets the kind its writer
	// created; rollback's restore path sets the file kind
	// for a restore temp (restores create no other kind; see
	// rollbackJournal), so a later Recover may remove that
	// temp instead of wedging on it. The records are
	// rollback's only warrant for removing a derived-name
	// temp (see removeUnitTemps): a computable name is not
	// ownership, and a unit without a kind's record — its
	// creation was refused by a pre-existing occupant, or
	// its journal predates the record — has no temp of that
	// kind removed on its behalf.
	//
	// A record's lifetime is bounded by the temp's: it may
	// only ever vouch for a temp that may still stand. The
	// materializer clears a write unit's records in the
	// same journal write that first persists the unit as
	// done — a done unit's temp was renamed onto its
	// destination, so nothing of the unit's stands at the
	// derived name any more — and rollback clears a unit's
	// records, persisted by a journal write of its own,
	// when the unit's undo completes, since the restore
	// temp the undo may have recorded was renamed into
	// place by that completion. A record left set past
	// either point would vouch for whatever later comes to
	// occupy the derived name, and a later rollback would
	// delete a file the settlement does not own on a stale
	// warrant. In-flight units keep their records: a unit
	// started but not done, or mid-undo when a Recover
	// died, may genuinely still have a temp standing.
	TempCreatedFile bool
	TempCreatedLink bool
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
//
// Because ParentDir steers every write rollback makes, the
// journal never vouches for it alone: writeJournal records the
// directory in an independent parent anchor (see
// writeParentAnchor) before the journal itself lands, and
// Recover acts on a journal only when the anchor vouches for
// the directory it names — a journal file that arrives without
// the system's writer (planted, transplanted from another
// root, restored from a stray backup) carries no anchor and
// is refused, never acted on. The anchor vouches for the
// journal itself as well as its directory: it records a
// one-way digest of the journal's canonical bytes, refreshed
// on every writeJournal, so an anchor left behind with no
// journal beside it (an interrupted cleanup, a failed journal
// write) vouches for nothing a planter can construct — the
// digests name journals the writer produced, and a planted
// journal matches none of them.
type journal struct {
	Op           string
	Verb         string // verbSelect | verbApply
	ParentScope  string
	RunScope     string
	State        string // journalState*
	OldHead      string // the parent scope's head the verb CASes from
	ResultCommit string // the commit the verb CASes the parent scope to
	ParentDir    string // the real tree the units apply to
	// ParentIdentity is the recorded identity of ParentDir —
	// device and inode of the resolved directory, taken by
	// writeJournal at the journal's first write, while the
	// directory the verb resolved is the one standing there.
	// The path alone does not identify the tree: the
	// directory can be moved aside after a crash and a
	// different real directory placed at its path, and every
	// form check (not a symlink, ancestors real) passes on
	// the replacement while rollback's restores and removals
	// land in the wrong tree. Rollback therefore verifies
	// the identity before dereferencing the path at all (see
	// verifyParentIdentity). A journal written before
	// identities were recorded carries none, and an absent
	// identity is unverifiable: rollback refuses it, the
	// ownership record's rule one level up.
	ParentIdentity string
	Fact           canonical.Value
	Units          []journalUnit

	// digest is the digest of the exact bytes readJournal
	// loaded this journal from — its provenance mark, checked
	// against the parent anchor's vouch in Recover. Only
	// readJournal sets it; a journal built in memory has none.
	digest string
}

func journalPath(root, parentScope string) string {
	return filepath.Join(root, "journals", parentScope+".json")
}

func stagingDirFor(root, op string) string {
	return filepath.Join(root, "staging", op)
}

// journalAnchorPath returns the path of parentScope's parent
// anchor: Root/journals/<parentScope>.anchor. The name
// deliberately carries no .json suffix, so otherStagingOwner's
// census of the journals directory (which scans for *.json)
// never mistakes the anchor for a journal.
func journalAnchorPath(root, parentScope string) string {
	return filepath.Join(root, "journals", parentScope+".anchor")
}

// journalBytesDigest returns the digest of a journal's
// canonical bytes, in the codebase's digest form
// ("sha256:<lowercase hex>") — the value the parent anchor
// records as its vouch for the journal. The digest is
// one-way: the anchor names the journals the writer produced
// without letting anyone construct one from the anchor.
func journalBytesDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// validJournalDigest reports whether s is a well-formed
// journal digest — the only form journalBytesDigest emits.
func validJournalDigest(s string) bool {
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(h) != 64 {
		return false
	}
	for _, c := range h {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// parentAnchor is the independent record that vouches for a
// journal: the (scope, directory) pair the system's writer
// recorded under the settle root when the settlement was
// materialized, and the digests of the journal bytes that
// record covers. It is the anchor the stored-state rule
// requires — a durable record's field that steers filesystem
// effects (the journal's parent_dir, which selects the tree
// every validated unit path applies to) is checked against a
// record the journal itself is not, so a journal whose
// parent_dir no anchor vouches for is refused rather than
// acted on.
//
// The digest pair is a two-slot vouch. JournalDigest names
// the journal writeJournal was landing when it last
// refreshed the anchor; PrevJournalDigest names the journal
// those bytes replaced ("" when there were none). writeJournal
// refreshes the anchor BEFORE the journal's rename, so a
// crash between the two leaves the previous journal on disk —
// vouched by the second slot — while a completed rename
// leaves the new journal, vouched by the first. At every
// crash point the journal actually on disk is one of the
// two, and a journal the writer never produced is neither.
type parentAnchor struct {
	ParentScope       string
	ParentDir         string
	JournalDigest     string
	PrevJournalDigest string
}

// writeParentAnchor records a as parentScope's parent
// anchor: canonical JSON to a temp file in the journals
// directory, fsynced, renamed into place, and the directory
// fsynced — the writeJournal discipline, so the anchor is
// durable before the journal it vouches for lands.
func writeParentAnchor(root string, a parentAnchor) error {
	parentScope := a.ParentScope
	v := canonical.ObjectOf(
		canonical.Pair{Key: "journal_digest", Value: canonical.StringOf(a.JournalDigest)},
		canonical.Pair{Key: "parent_dir", Value: canonical.StringOf(a.ParentDir)},
		canonical.Pair{Key: "parent_scope", Value: canonical.StringOf(a.ParentScope)},
		canonical.Pair{Key: "prev_journal_digest", Value: canonical.StringOf(a.PrevJournalDigest)},
	)
	b, err := canonical.MarshalKernel(v)
	if err != nil {
		return fmt.Errorf("settle: marshal parent anchor for %q: %w", parentScope, err)
	}
	dir := filepath.Join(root, "journals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("settle: create journals dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".anchor-*")
	if err != nil {
		return fmt.Errorf("settle: create parent anchor temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("settle: write parent anchor temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("settle: fsync parent anchor temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("settle: close parent anchor temp: %w", err)
	}
	if err := os.Rename(tmpName, journalAnchorPath(root, parentScope)); err != nil {
		return fmt.Errorf("settle: rename parent anchor into place: %w", err)
	}
	return syncDir(dir)
}

// readParentAnchor loads parentScope's parent anchor. A
// missing anchor is (zero, false, nil) — no settlement's
// writer recorded one. An anchor that does not decode to the
// recorded shape is a descriptive error: the anchor is never
// guessed at either.
func readParentAnchor(root, parentScope string) (parentAnchor, bool, error) {
	b, err := os.ReadFile(journalAnchorPath(root, parentScope))
	if errors.Is(err, fs.ErrNotExist) {
		return parentAnchor{}, false, nil
	}
	if err != nil {
		return parentAnchor{}, false, fmt.Errorf("settle: read parent anchor for %q: %w", parentScope, err)
	}
	v, err := canonical.DecodeKernel(b)
	if err != nil {
		return parentAnchor{}, false, fmt.Errorf("settle: parent anchor for %q does not decode: %w", parentScope, err)
	}
	var a parentAnchor
	for key, dst := range map[string]*string{
		"parent_scope": &a.ParentScope,
		"parent_dir":   &a.ParentDir,
	} {
		s, ok := stringField(v, key)
		if !ok || s == "" {
			return parentAnchor{}, false, fmt.Errorf("settle: parent anchor for %q: field %q is missing or not a non-empty string", parentScope, key)
		}
		*dst = s
	}
	// The digest vouch is read leniently in form and strictly
	// in effect: an anchor recorded before the vouch existed
	// carries no digest fields and vouches for no journal (the
	// gate in Recover refuses any journal beside it, exactly
	// as it refuses a journal beside no anchor at all), and a
	// digest field that is present must be a well-formed
	// digest or the anchor does not decode.
	for key, dst := range map[string]*string{
		"journal_digest":      &a.JournalDigest,
		"prev_journal_digest": &a.PrevJournalDigest,
	} {
		s, ok := stringField(v, key)
		if !ok {
			continue
		}
		if s != "" && !validJournalDigest(s) {
			return parentAnchor{}, false, fmt.Errorf("settle: parent anchor for %q: field %q is not a journal digest", parentScope, key)
		}
		*dst = s
	}
	return a, true, nil
}

// ensureParentAnchor makes the anchor state match the
// journal writeJournal is about to land: the anchor records
// the journal's directory and the two-slot digest vouch for
// the transition — journalDigest, the journal being landed,
// and prevDigest, the journal it replaces. An anchor already
// recording exactly this vouch stands. An anchor naming a
// DIFFERENT directory while its journal is still live is a
// refusal — re-anchoring under a live journal would let a
// later writer redirect the earlier settlement's recovery —
// but with no journal live the anchor is debris (of an
// interrupted cleanup, or of a journal write that failed
// after the anchor landed) and the new settlement's writer
// replaces it.
func ensureParentAnchor(root string, j *journal, journalDigest, prevDigest string) error {
	a, found, err := readParentAnchor(root, j.ParentScope)
	if err != nil {
		return err
	}
	if found {
		if a.ParentScope == j.ParentScope && a.ParentDir == j.ParentDir &&
			a.JournalDigest == journalDigest && a.PrevJournalDigest == prevDigest {
			return nil
		}
		if a.ParentDir != j.ParentDir {
			if _, serr := os.Stat(journalPath(root, j.ParentScope)); serr == nil {
				return fmt.Errorf("settle: journal for %q is anchored to parent directory %q — refusing to record it under %q while that journal is live", j.ParentScope, a.ParentDir, j.ParentDir)
			} else if !errors.Is(serr, fs.ErrNotExist) {
				return fmt.Errorf("settle: stat journal for %q: %w", j.ParentScope, serr)
			}
		}
	}
	return writeParentAnchor(root, parentAnchor{
		ParentScope:       j.ParentScope,
		ParentDir:         j.ParentDir,
		JournalDigest:     journalDigest,
		PrevJournalDigest: prevDigest,
	})
}

// validOpID reports whether op is safe to name a staging
// directory: a single path component, never a traversal.
func validOpID(op string) bool {
	return op != "" && op != "." && op != ".." && !strings.ContainsAny(op, `/\`)
}

// validCommitID reports whether s is a full lowercase-hex
// SHA-1 object ID — the snapshot store's isObjectID law,
// mirrored: the only form git's plumbing emits, and the only
// form a journal's commit-point endpoints (old_head,
// result_commit, the apply fact's fork_commit) may take.
func validCommitID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
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

// parentDirIdentity returns dir's identity in the journal's
// recorded form: the device and inode of the directory the
// path names, decimal, colon-separated. Two directories at
// one path across a replacement never share it; one
// directory keeps it for its whole life.
func parentDirIdentity(dir string) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("settle: cannot determine the identity of directory %s on this platform", dir)
	}
	return strconv.FormatUint(st.Dev, 10) + ":" + strconv.FormatUint(uint64(st.Ino), 10), nil
}

// validParentIdentity reports whether s is a well-formed
// recorded parent identity — the only form parentDirIdentity
// emits.
func validParentIdentity(s string) bool {
	dev, ino, ok := strings.Cut(s, ":")
	if !ok || dev == "" || ino == "" || strings.Contains(ino, ":") {
		return false
	}
	if _, err := strconv.ParseUint(dev, 10, 64); err != nil {
		return false
	}
	if _, err := strconv.ParseUint(ino, 10, 64); err != nil {
		return false
	}
	return true
}

// writeJournal records j as parentScope's journal: canonical
// JSON to a temp file in the journals directory, fsynced, then
// renamed into place and the directory fsynced. A state change
// is therefore atomic and durable — a crash mid-write leaves
// the previous complete journal, never a torn one.
func writeJournal(root string, j *journal) error {
	// The parent identity is recorded once, at the journal's
	// first write, from the directory standing at the
	// recorded path in that moment — for the materializer,
	// the resolved directory the verb is about to write
	// through. Later writes preserve the recorded value (the
	// in-memory journal carries it; a journal read back from
	// disk carries it from there): re-recording at a later
	// write would let a directory replaced mid-settlement
	// re-anchor the journal to itself. A directory that
	// cannot be stat'ed leaves the field empty, and the
	// journal unverifiable — rollback refuses it rather than
	// act on an unproven claim (see verifyParentIdentity).
	if j.ParentIdentity == "" {
		if id, err := parentDirIdentity(j.ParentDir); err == nil {
			j.ParentIdentity = id
		}
	}
	units := make([]canonical.Value, 0, len(j.Units))
	for _, u := range j.Units {
		units = append(units, canonical.ObjectOf(
			canonical.Pair{Key: "done", Value: canonical.BoolOf(u.Done)},
			canonical.Pair{Key: "kind", Value: canonical.StringOf(u.Kind)},
			canonical.Pair{Key: "path", Value: canonical.StringOf(u.Path)},
			canonical.Pair{Key: "started", Value: canonical.BoolOf(u.Started)},
			canonical.Pair{Key: "temp_created_file", Value: canonical.BoolOf(u.TempCreatedFile)},
			canonical.Pair{Key: "temp_created_link", Value: canonical.BoolOf(u.TempCreatedLink)},
		))
	}
	pairs := []canonical.Pair{
		{Key: "fact", Value: j.Fact},
		{Key: "old_head", Value: canonical.StringOf(j.OldHead)},
		{Key: "op", Value: canonical.StringOf(j.Op)},
		{Key: "parent_dir", Value: canonical.StringOf(j.ParentDir)},
	}
	if j.ParentIdentity != "" {
		pairs = append(pairs, canonical.Pair{Key: "parent_identity", Value: canonical.StringOf(j.ParentIdentity)})
	}
	pairs = append(pairs,
		canonical.Pair{Key: "parent_scope", Value: canonical.StringOf(j.ParentScope)},
		canonical.Pair{Key: "result_commit", Value: canonical.StringOf(j.ResultCommit)},
		canonical.Pair{Key: "run_scope", Value: canonical.StringOf(j.RunScope)},
		canonical.Pair{Key: "state", Value: canonical.StringOf(j.State)},
		canonical.Pair{Key: "units", Value: canonical.ArrayOf(units...)},
		canonical.Pair{Key: "verb", Value: canonical.StringOf(j.Verb)},
	)
	v := canonical.ObjectOf(pairs...)
	b, err := canonical.MarshalKernel(v)
	if err != nil {
		return fmt.Errorf("settle: marshal journal for %q: %w", j.ParentScope, err)
	}
	dir := filepath.Join(root, "journals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("settle: create journals dir: %w", err)
	}
	// The parent anchor lands before the journal it vouches
	// for: at no moment does a journal exist on disk whose
	// parent directory no anchor records. The anchor's digest
	// vouch names this journal and the journal it replaces —
	// read back now, before the rename — so a crash between
	// the anchor's landing and the journal's leaves the
	// on-disk journal vouched by the second slot, and a
	// journal write that fails after the anchor landed leaves
	// the same recoverable state.
	journalDigest := journalBytesDigest(b)
	prevDigest := ""
	if prev, rerr := os.ReadFile(journalPath(root, j.ParentScope)); rerr == nil {
		prevDigest = journalBytesDigest(prev)
	} else if !errors.Is(rerr, fs.ErrNotExist) {
		return fmt.Errorf("settle: read journal being replaced for %q: %w", j.ParentScope, rerr)
	}
	if err := ensureParentAnchor(root, j, journalDigest, prevDigest); err != nil {
		return err
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
	j.digest = journalBytesDigest(b)
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
	// The journal's own names obey the same laws the verbs
	// enforce at entry (checkScopeName): readJournal's
	// parent_scope equality check is a self-consistency check
	// a traversal string satisfies trivially, so decode itself
	// refuses an unlawful name in either scope field.
	if !validScopeName(j.ParentScope) {
		return nil, fmt.Errorf("parent_scope %q is not a lawful scope name", j.ParentScope)
	}
	if !validScopeName(j.RunScope) {
		return nil, fmt.Errorf("run_scope %q is not a lawful scope name", j.RunScope)
	}
	// The commit-point endpoints are object IDs or they are
	// nothing: a free-form string here is refused at decode,
	// not left for git to trip over downstream — or, in the
	// rollback branch, never checked at all.
	if !validCommitID(j.OldHead) {
		return nil, fmt.Errorf("old_head %q is not a well-formed object ID", j.OldHead)
	}
	if !validCommitID(j.ResultCommit) {
		return nil, fmt.Errorf("result_commit %q is not a well-formed object ID", j.ResultCommit)
	}
	if !filepath.IsAbs(j.ParentDir) {
		return nil, fmt.Errorf("parent_dir %q is not an absolute path", j.ParentDir)
	}
	// The parent identity is optional on read, on the
	// started flag's precedent: journals written before
	// identities were recorded carry no such field, and
	// their claim on the parent directory is simply
	// unverifiable — rollback refuses them for it (see
	// verifyParentIdentity). When present it must be a
	// well-formed identity: one that cannot be read is not
	// one recovery may guess at.
	for _, p := range v.Pairs {
		if p.Key != "parent_identity" {
			continue
		}
		if p.Value.Kind != canonical.KindString || !validParentIdentity(p.Value.Str) {
			return nil, fmt.Errorf("parent_identity %q is not a well-formed directory identity", p.Value.Str)
		}
		j.ParentIdentity = p.Value.Str
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
		// The started flag is optional on read: journals
		// written before the in-flight discipline existed carry
		// no such field, and their units are simply not started.
		// When present it must be a bool — a started flag that
		// cannot be read is not a flag recovery may guess at.
		started := false
		for _, pr := range uv.Pairs {
			if pr.Key != "started" {
				continue
			}
			if pr.Value.Kind != canonical.KindBool {
				return nil, fmt.Errorf("unit %d has an invalid started flag", i)
			}
			started = pr.Value.Bool
		}
		// The temp_created_file / temp_created_link flags are
		// optional on read, on the started flag's precedent:
		// journals written before temp ownership was journaled
		// per kind carry neither field, and their units are
		// simply unproven for both kinds — rollback removes
		// no temp for them. When present each must be a bool:
		// an ownership record that cannot be read is not a
		// record recovery may guess at.
		//
		// The predecessor form — a single unit-wide
		// temp_created flag, written only by an intermediate
		// version of this code that was never merged — is
		// validated the same way when present but grants
		// neither kind: it cannot say WHICH kind the unit
		// created, and crediting it with either would let
		// rollback delete a temp of the other kind on a
		// record that never vouched for it. A journal
		// carrying only the old flag decodes with both kinds
		// unrecorded; its genuine temps stand as inert
		// debris, the same disposition as a journal predating
		// ownership entirely.
		tempCreatedFile, tempCreatedLink := false, false
		for _, pr := range uv.Pairs {
			switch pr.Key {
			case "temp_created_file":
				if pr.Value.Kind != canonical.KindBool {
					return nil, fmt.Errorf("unit %d has an invalid temp_created_file flag", i)
				}
				tempCreatedFile = pr.Value.Bool
			case "temp_created_link":
				if pr.Value.Kind != canonical.KindBool {
					return nil, fmt.Errorf("unit %d has an invalid temp_created_link flag", i)
				}
				tempCreatedLink = pr.Value.Bool
			case "temp_created":
				if pr.Value.Kind != canonical.KindBool {
					return nil, fmt.Errorf("unit %d has an invalid temp_created flag", i)
				}
				// Validated, and deliberately uncredited:
				// see the comment above.
			}
		}
		j.Units = append(j.Units, journalUnit{Path: p, Kind: kind, Done: done, Started: started, TempCreatedFile: tempCreatedFile, TempCreatedLink: tempCreatedLink})
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
		forkCommit, ok := stringField(fact, "fork_commit")
		if !ok {
			return nil, errors.New("apply fact has no fork_commit")
		}
		if !validCommitID(forkCommit) {
			return nil, fmt.Errorf("apply fact's fork_commit %q is not a well-formed object ID", forkCommit)
		}
		if got, ok := stringField(fact, "result_commit"); !ok || got != j.ResultCommit {
			return nil, errors.New("apply fact's result_commit does not match result_commit")
		}
		if _, ok := arrayField(fact, "settled_paths"); !ok {
			return nil, errors.New("apply fact has no settled_paths array")
		}
	}
	// The recorded fact body must be EXACTLY the body the
	// store's constructors build from the journal's own
	// fields — no extra field, no missing one. The cross-
	// checks above say the parts agree; this says the whole
	// is one the system's one construction site could have
	// produced, so the body Recover mints from carries
	// nothing no verb ever computed.
	var wantBody canonical.Value
	switch j.Verb {
	case verbSelect:
		wantBody = store.SettleSelectBody(j.Op, j.ParentScope, j.RunScope, j.OldHead, j.ResultCommit)
	case verbApply:
		forkCommit, _ := stringField(fact, "fork_commit")
		wantBody = store.SettleApplyBody(j.Op, j.ParentScope, j.RunScope, forkCommit, j.ResultCommit, stringArrayField(fact, "settled_paths"))
	}
	wantBytes, err := canonical.MarshalKernel(wantBody)
	if err != nil {
		return nil, fmt.Errorf("rebuild fact body from the journal's fields: %w", err)
	}
	gotBytes, err := canonical.MarshalKernel(j.Fact)
	if err != nil {
		return nil, fmt.Errorf("marshal recorded fact body: %w", err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		return nil, errors.New("fact body is not the body the settlement constructors build from the journal's fields")
	}
	return &j, nil
}

// otherStagingOwner reports whether a journal on a DIFFERENT
// parent scope names op: staging is keyed only by operation
// ID (stagingDirFor), so such a journal owns the staging
// directory this op would use — its preimages and result tree
// live there, and removing or reusing the directory destroys
// the only record that journal's recovery can restore from.
// Operation IDs are caller-supplied and are not enforced as
// globally unique; settlement identity is (parent scope, op),
// so reuse across parent scopes must be refused, never shared.
// unreadable is true when another scope's journal exists but
// cannot be read back: its op cannot be known, so it is
// treated as a potential owner — the fail-closed direction.
func otherStagingOwner(root, parentScope, op string) (owner string, unreadable bool) {
	entries, err := os.ReadDir(filepath.Join(root, "journals"))
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		other := strings.TrimSuffix(name, ".json")
		if other == parentScope {
			continue
		}
		j, err := readJournal(root, other)
		if err != nil {
			return "", true
		}
		if j != nil && j.Op == op {
			return other, false
		}
	}
	return "", false
}

// clearJournalState removes parentScope's journal, the parent
// anchor that vouched for it, and the op's staging directory
// (preimages and the result-artifact tree): the settlement is
// complete or undone, and its operational state goes with it. The staging directory is removed only
// when no other parent scope's journal still owns it under
// the same op ID: one scope's cleanup must never destroy
// another scope's durable preimages.
func clearJournalState(root, parentScope, op string) error {
	if err := os.Remove(journalPath(root, parentScope)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("settle: remove journal for %q: %w", parentScope, err)
	}
	// The parent anchor goes with the journal it vouched for:
	// the settlement is complete or undone, and no record of
	// its directory outlives it. (A crash between the two
	// removals leaves an orphan anchor: it vouches only for
	// the digests of the journal just removed and the one
	// that journal replaced, so no planted journal can act
	// beside it, and the next settlement's writer replaces
	// it — see ensureParentAnchor.)
	if err := os.Remove(journalAnchorPath(root, parentScope)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("settle: remove parent anchor for %q: %w", parentScope, err)
	}
	if owner, unreadable := otherStagingOwner(root, parentScope, op); owner != "" || unreadable {
		return nil
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
//     the unit list against the parent's current inventory,
//     refuse any replacement whose wholesale directory removal
//     would destroy inventory-excluded content no preimage
//     captures, and copy the preimage of every unit target
//     that exists into the staging preimage tree — all before
//     the first side effect;
//   - write the journal in state planned and fsync it;
//   - apply: per unit, first record the unit's in-flight intent
//     (its started mark, plus the started marks of every unit
//     its landing destroys as a side effect) and rewrite +
//     fsync the journal, then write (temp file + rename in the
//     target directory, atomic per file — the temp under the
//     name derived from the destination and this operation's
//     ID, syncTempName, so a crash before the rename leaves a
//     temp rollback can name and remove) or delete, mark the
//     unit done, rewrite + fsync the journal;
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
	// would fail at its entry Recover. A directory spelled
	// through a symlink is worse: the inventory walk does not
	// follow a symlinked root, so the settlement would plan
	// against the link itself and fail in preflight (see
	// resolveParentDir). Normalize here — the one path both
	// materializing verbs take, before the journal is written
	// or the parent is touched — to the absolute, symlink-free
	// path the verbs already resolved at their entries;
	// decodeJournal's absolute-path requirement stays as the
	// backstop for journals written by anything else.
	resolvedDir, err := resolveParentDir(req.parentDir)
	if err != nil {
		return fmt.Errorf("settle: %s %q: %w", req.verb, req.runScope, err)
	}
	req.parentDir = resolvedDir
	// Staging is keyed only by op ID, so a pending journal on
	// another parent scope with the same op ID owns the staging
	// directory below: removing it here (or clearing it when
	// this verb completes) would destroy that settlement's
	// durable preimages while its journal survives, and its
	// recovery would then treat modified files as creations.
	// Refuse the reuse before any side effect — the other
	// settlement must be recovered or completed first. An
	// unreadable journal on another scope is treated the same
	// way: its op cannot be known, so reuse cannot be proven
	// safe.
	if owner, unreadable := otherStagingOwner(ctx.Root, req.parentScope, req.opID); owner != "" || unreadable {
		if unreadable {
			return fmt.Errorf("settle: %s %q: a journal on another parent scope cannot be read, so op ID %q cannot be proven free — refusing to reuse its staging directory", req.verb, req.runScope, req.opID)
		}
		return fmt.Errorf("settle: %s %q: op ID %q is already owned by a pending settlement on parent scope %q — refusing to reuse its staging directory", req.verb, req.runScope, req.opID, owner)
	}
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
	if err := refuseUncapturedReplacements(req, units); err != nil {
		return err
	}

	// Preflight: capture the preimage of every unit target that
	// exists — the bytes (or link target) rollback restores.
	preDir := filepath.Join(stage, "pre")
	for _, u := range units {
		if err := capturePreimage(req.opID, req.parentDir, preDir, u.Path); err != nil {
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
	// recordIntent persists the journal with the started marks
	// the caller just set, before the mutation they announce
	// lands. It is mark without the completion count and
	// without the crash hook: no unit has completed, and the
	// hook's window invocation belongs after the mutation.
	recordIntent := func() error {
		if j.State == journalStatePlanned {
			j.State = journalStateApplying
		}
		return writeJournal(ctx.Root, j)
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
		// In-flight intent, recorded before the first mutation
		// this unit performs: the unit itself, plus every unit
		// whose target this unit's landing destroys as a side
		// effect — the blocking ancestors the write removes to
		// clear its way, and the extras beneath its target that
		// its landing clears (exactly the set the completion
		// accounting below marks done at this unit's mark). A
		// crash after any of those mutations but before the
		// completion mark persists then leaves a journal that
		// names every unit the crash may have touched, and
		// rollback undoes started units as it undoes done ones.
		// Without the record, rollback skipped the unit, the
		// journal and preimages were cleared, and the mutation
		// stood permanently with no settlement fact.
		u.Started = true
		for parent := pathParent(u.Path); parent != ""; parent = pathParent(parent) {
			if _, isEntry := current[parent]; !isEntry {
				continue
			}
			if di, ok := unitIndex[parent]; ok {
				j.Units[di].Started = true
			}
		}
		for k := range j.Units {
			du := &j.Units[k]
			if du.Kind == unitDelete && strings.HasPrefix(du.Path, u.Path+"/") {
				du.Started = true
			}
		}
		if err := recordIntent(); err != nil {
			return err
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
		// Temp ownership, recorded at the one moment the
		// proof exists: the writers invoke this between the
		// temp's exclusive creation and its content write,
		// reporting the kind they created, and it persists
		// the unit's per-kind temp-created record —
		// recordIntent's discipline, a journal rewrite with
		// no completion count and no crash hook — so
		// rollback may later remove that kind's temp as this
		// settlement's own. A unit whose writer never
		// invokes it (its creation was refused by a
		// pre-existing occupant) carries no record for the
		// kind, and rollback removes no temp of that kind
		// for it.
		recordTemp := func(link bool) error {
			if link {
				if u.TempCreatedLink {
					return nil
				}
				u.TempCreatedLink = true
			} else {
				if u.TempCreatedFile {
					return nil
				}
				u.TempCreatedFile = true
			}
			return recordIntent()
		}
		if err := applyWriteUnit(req, treeDir, u.Path, writeEntry[u.Path], recordTemp); err != nil {
			return err
		}
		u.Done = true
		// The unit's temp-created records end here: the
		// write landed, so the temp its writer created was
		// renamed onto the destination and no temp of this
		// unit may still stand. Clearing them in memory now
		// puts the clearing in the mark() below — the first
		// journal write to persist this unit as done — so a
		// durable journal never shows a completed unit still
		// vouching for a derived name. A crash before that
		// mark (the window invocation below is one) leaves
		// the on-disk unit started with its record intact:
		// from durable state the unit is still in flight,
		// its temp may still stand, and rollback must still
		// be able to remove it.
		u.TempCreatedFile = false
		u.TempCreatedLink = false
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
		// The window invocation (see unitCrashHook): the
		// mutation has landed and the completion count includes
		// this unit, but the journal on disk still records it
		// as started, not done — the state a crash here leaves.
		callCrashHook(done)
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
		// In-flight intent before the removal, as for writes:
		// a crash after the removal but before the completion
		// mark must leave the unit started, so rollback restores
		// the file instead of skipping the unit and clearing
		// the journal and preimage that could have restored it.
		u.Started = true
		if err := recordIntent(); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(req.parentDir, filepath.FromSlash(u.Path))); err != nil && !targetGone(err) {
			return fmt.Errorf("settle: remove extra %s: %w", u.Path, err)
		}
		u.Done = true
		done++
		callCrashHook(done)
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

// refuseUncapturedReplacements is materialize's preflight
// guard against destroying content the settlement never
// captured (PR #7 wave 7, Codex P1 on b96ae2c). A write unit
// whose target is a pre-existing directory removes that
// directory wholesale when it lands — copyFileAtomic's
// clearBlockingDirs and replaceWithSymlink both RemoveAll
// it — but the inventory the unit list was planned against
// excludes every entry inventoryExcludedName names, at any
// depth: no snapshot records such content, no unit names it,
// no preimage captures it, and verification, which compares
// the inventory's file maps, cannot see its loss. A nested
// repository's metadata beneath a replaced directory would
// be deleted silently and unrestorably. Refuse instead,
// here in preflight — after the unit list is known, before
// preimage capture, the journal write, and any mutation —
// so the refusal leaves no partial state and is retry-safe:
// with the excluded content moved out of the tree, the same
// operation proceeds.
func refuseUncapturedReplacements(req materializeRequest, units []journalUnit) error {
	for _, u := range units {
		if u.Kind != unitWrite {
			continue
		}
		dst := filepath.Join(req.parentDir, filepath.FromSlash(u.Path))
		info, err := os.Lstat(dst)
		if targetGone(err) {
			continue // nothing stands there: nothing is removed wholesale
		}
		if err != nil {
			return fmt.Errorf("settle: %s %q: stat replacement target %s: %w", req.verb, req.runScope, dst, err)
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			continue // a file or symlink target is replaced in place, never removed wholesale
		}
		excluded, err := excludedEntryBeneath(dst)
		if err != nil {
			return err
		}
		if excluded != "" {
			return fmt.Errorf("settle: %s %q: replacing the directory at %q would delete %q, which the workspace inventory excludes — no snapshot records it, no journal unit names it, and no preimage captures it, so its loss could neither be verified nor rolled back; refusing before any mutation (move the excluded content out of the tree and retry)", req.verb, req.runScope, u.Path, u.Path+"/"+excluded)
		}
	}
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
// (atomic per file) with the tree's mode. onTempCreated is
// the writers' ownership hook, passed through to them: it
// fires between the temp's exclusive creation and its
// content write, reporting the kind created (the writer this
// function chose is the unit's only temp source, so the kind
// is exact), and the materializer uses it to persist the
// unit's per-kind temp-created record in the journal.
func applyWriteUnit(req materializeRequest, treeDir, rel string, entry snapshot.WorkspaceEntry, onTempCreated func(link bool) error) error {
	src := filepath.Join(treeDir, filepath.FromSlash(rel))
	dst := filepath.Join(req.parentDir, filepath.FromSlash(rel))
	if entry.Symlink {
		target, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("settle: read staged symlink %s: %w", src, err)
		}
		return replaceWithSymlink(dst, target, req.opID, onTempCreated)
	}
	return copyFileAtomic(src, dst, permForMode(entry.Mode), req.opID, onTempCreated)
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
func capturePreimage(op, parentDir, preDir, rel string) error {
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
		return copyFileAtomic(src, dst, info.Mode().Perm(), op, nil)
	default:
		return nil
	}
}

// Recover resolves parentScope's journal, if one exists: the
// settlement a crashed (or failed) verb left behind is
// completed or rolled back before any new work is judged
// against the scope. Recovery presumes the single-caller
// contract (spec §9): it cannot tell a live operation's
// journal from a crashed one's, so no second caller — in this
// process or any other — may act on the scope while one may
// still be running. No journal → nil. A journal that fails to
// decode or validate → a descriptive error, no writes, the
// journal left in place as evidence. A journal the scope's
// parent anchor does not vouch for — no anchor recorded, a
// different directory anchored, or a digest matching neither
// of the anchor's vouched journals — is refused the same way,
// before any state is judged: the journal never vouches for
// its own scope.
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
// State planned/applying → roll back: every done unit — and
// every started unit, whose mutation may have landed before
// its completion mark — is undone from its preimage, and the
// parent directory is exactly pre-apply; journal + staging are
// cleared.
//
// Every verb (Select, Apply) calls Recover for its parent scope
// at entry; Recover is also exported so the next handle or
// process can resolve a crashed predecessor directly.
func Recover(ctx *Context, parentScope string) error {
	// The journal path is constructed from the parent scope
	// name, so the scope-name law is enforced before it is
	// built (see checkScopeName in settle.go).
	if err := checkScopeName("parent", parentScope); err != nil {
		return err
	}
	j, err := readJournal(ctx.Root, parentScope)
	if err != nil {
		return err
	}
	if j == nil {
		return nil
	}
	// The anchor gate: the journal's parent_dir selects the
	// tree every effect below applies to — rollback's deletes
	// and restores, and (in the verified branch) the act the
	// minted fact attests — so it is re-validated against the
	// parent anchor, the independent record the settlement's
	// writer left under the settle root, before any state is
	// judged. A journal with no anchor, or one naming a
	// different directory than the anchor records, is refused
	// with the journal left in place. The anchor must also
	// vouch for the journal itself: its digest must match one
	// of the anchor's two slots — the journal the writer last
	// landed, or the one that journal replaced (the journal
	// on disk when a crash interrupted the anchor's refresh
	// ahead of the journal's own rename). An anchor left
	// behind with no journal beside it therefore vouches for
	// nothing a planter can construct: the slots hold one-way
	// digests of journals the writer produced, and a planted
	// journal is neither.
	a, found, err := readParentAnchor(ctx.Root, parentScope)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("settle: recover %q: journal names parent directory %q but no parent anchor is recorded for the scope — refusing to act on an unanchored journal", parentScope, j.ParentDir)
	}
	if a.ParentScope != parentScope || a.ParentDir != j.ParentDir {
		return fmt.Errorf("settle: recover %q: journal's parent directory %q disagrees with the recorded parent anchor %q — refusing to act on it", parentScope, j.ParentDir, a.ParentDir)
	}
	if j.digest != a.JournalDigest && j.digest != a.PrevJournalDigest {
		return fmt.Errorf("settle: recover %q: the parent anchor does not vouch for this journal — its digest matches neither the journal the anchor last recorded nor the journal that one replaced — refusing to act on it", parentScope)
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
// and started units from their preimages. A started unit's
// mutation may or may not have landed — the crash that left
// the journal struck somewhere inside the unit — and undoing
// it is safe in both states: a write with a preimage restores
// the same bytes or link target the parent still serves if
// the write never landed, a write without one removes an
// occupant that is either the unit's creation or (for the
// directory it replaces) a directory whose contents are their
// own started units, restored by their own undo steps, and a
// delete restores its preimage over a file that is either
// gone or still identical to it. Units neither done nor
// started were never touched and are never undone. Write
// units are undone first, in reverse order, then delete units
// in reverse order: the two kinds interact only through path
// shape (a file where a directory belongs, or the reverse),
// and undoing writes first guarantees a restored delete never
// has to pass through a file a write unit created — the
// parent is then exactly pre-apply.
//
// A write unit's undo begins by removing the unit's temp
// files (removeUnitTemps): a write lands through a temp in
// the destination directory, and a crash between the temp's
// creation and its rename leaves the temp standing in the
// parent tree, outside every unit's destination — restoring
// destinations alone would leave the parent holding a file
// the pre-operation tree never had, for a later verb to
// snapshot as a parent change. The temp's name is derived
// from the destination the journal records (syncTempName),
// so rollback can compute it — but a computable name is
// not ownership, so the removal is gated on the unit's
// journaled temp-created record and confined by the same
// lexical ancestor discipline the other rollback removals
// carry: a temp no record vouches for (a foreign occupant
// of the derived name, or the operation's own debris from
// a kill between creation and record) is left standing,
// and a later verb refuses descriptively at its name
// rather than adopt or delete it.
func rollbackJournal(ctx *Context, j *journal) error {
	// The parent root itself is judged before any undo
	// work: the journal's parent directory was resolved
	// when written, so a symlink standing at that path now
	// means the root was replaced after the crash, and
	// every undo below — temp cleanup first of all — would
	// resolve through it into another tree. Refuse, and
	// leave the journal in place as evidence.
	if parentRootReplaced(j.ParentDir) {
		return fmt.Errorf("settle: recover %q: parent directory %q stands as a symlink, not the directory the settlement wrote through — the parent root has been replaced since the journal was written, and rolling back would resolve into the tree the link names; refusing", j.ParentScope, j.ParentDir)
	}
	// The root's identity is judged with its form, before
	// any undo work dereferences the recorded path: the
	// form checks cannot tell the directory the settlement
	// wrote through from a different real directory placed
	// at its path after the crash, and every undo below
	// would land in whichever tree the path now names.
	if err := verifyParentIdentity(j); err != nil {
		return err
	}
	preDir := filepath.Join(stagingDirFor(ctx.Root, j.Op), "pre")
	// recordRestoreTemp is the restore path's ownership hook,
	// the materializer's recordTemp discipline pointed at
	// rollback's own temps: a restore writes its preimage back
	// through a derived-name temp, and a Recover killed between
	// that temp's creation and its rename would otherwise leave
	// a temp no cleanup pass visits — the next Recover's restore
	// retries into the same name and wedges on the collision.
	// The hook persists the unit's file-kind temp-created
	// record (a journal rewrite, no crash hook — rollback has
	// none) at the one moment the proof exists, so the next
	// restore may remove the recorded temp and proceed (see
	// restorePreimage).
	recordRestoreTemp := func(idx int) func(bool) error {
		return func(link bool) error {
			// The restore path's only hooked writer is
			// copyFileAtomic, so link is always false here:
			// a restore temp is a file-kind temp, and the
			// file kind is the kind the record vouches for.
			if j.Units[idx].TempCreatedFile {
				return nil
			}
			j.Units[idx].TempCreatedFile = true
			return writeJournal(ctx.Root, j)
		}
	}
	// completeUnit closes a unit's record lifetime when its
	// undo finishes, the mirror of the materializer clearing
	// a write unit's records at its done-mark. By the time a
	// unit's undo returns, no temp of that unit may still
	// stand: its write temp was renamed by the verb or
	// removed by this rollback's own removeUnitTemps pass,
	// and its restore temp — the one recordRestoreTemp may
	// have set during this very undo — was renamed onto the
	// destination by the restore it served. The clearing is
	// persisted by a journal write of its own, before the
	// next unit is touched, because the kill points demand
	// it: a Recover killed after this undo but before the
	// journal is cleared leaves the on-disk journal behind,
	// and a record still set in it would vouch, for the next
	// Recover, for whatever then occupies the derived name —
	// the temp it once named is gone, so the warrant would
	// be stale and the deletion a foreign one. A kill
	// between the restore's rename and this write leaves the
	// record set, which is correct from durable state: the
	// journal cannot distinguish that kill from one between
	// the temp's creation and its rename, where the temp
	// genuinely stands and the record must still vouch.
	// Units carrying no record need no write — there is
	// nothing to clear, and the journal's disposal at
	// clearJournalState takes the rest.
	completeUnit := func(idx int) error {
		u := &j.Units[idx]
		if !u.TempCreatedFile && !u.TempCreatedLink {
			return nil
		}
		u.TempCreatedFile = false
		u.TempCreatedLink = false
		if err := writeJournal(ctx.Root, j); err != nil {
			return fmt.Errorf("settle: recover %q: record completed undo of %s: %w", j.ParentScope, u.Path, err)
		}
		return nil
	}
	for i := len(j.Units) - 1; i >= 0; i-- {
		u := j.Units[i]
		if u.Kind != unitWrite || (!u.Done && !u.Started) {
			continue
		}
		if err := removeUnitTemps(j.ParentDir, j.Op, u); err != nil {
			return fmt.Errorf("settle: recover %q: roll back %s: %w", j.ParentScope, u.Path, err)
		}
		if err := undoWriteUnit(j.ParentDir, preDir, u, j.Op, recordRestoreTemp(i)); err != nil {
			return fmt.Errorf("settle: recover %q: roll back %s: %w", j.ParentScope, u.Path, err)
		}
		if err := completeUnit(i); err != nil {
			return err
		}
	}
	for i := len(j.Units) - 1; i >= 0; i-- {
		u := j.Units[i]
		if u.Kind != unitDelete || (!u.Done && !u.Started) {
			continue
		}
		if err := undoDeleteUnit(j.ParentDir, preDir, u, j.Op, recordRestoreTemp(i)); err != nil {
			return fmt.Errorf("settle: recover %q: roll back %s: %w", j.ParentScope, u.Path, err)
		}
		if err := completeUnit(i); err != nil {
			return err
		}
	}
	return clearJournalState(ctx.Root, j.ParentScope, j.Op)
}

// removeUnitTemps removes the temp files a write unit may
// have left in the parent tree — the file temp and the
// symlink temp, each under its derived name (syncTempName).
// Two disciplines gate every removal, each answering a
// defect the derived-name cleanup shipped with:
//
//   - Ownership: the derived name is a pure function of the
//     destination and the operation, computable by anyone
//     who can read the journal — it does not prove this
//     settlement created the file standing at it. Each
//     kind's name is removed only under that kind's
//     journaled temp-created record: the record was
//     persisted between that temp's exclusive creation and
//     its content write, so it names a temp this settlement
//     made. The gate is per kind because the creation is —
//     a write unit creates exactly one kind, and its other
//     kind's record says nothing about the file standing at
//     that name. A kind without its record — its creation
//     was refused by a pre-existing occupant, the kill
//     struck between creation and record, or the journal
//     predates the record — has no temp of that kind
//     removed on the unit's behalf: the occupant may be a
//     genuine file, and it has no unit and no preimage
//     rollback could restore it from, so deleting it would
//     be unrecoverable loss on a guess.
//   - Confinement: the temp path is decided lexically,
//     through the unit's ancestors in the live tree
//     (blockedByAncestor) — the decision removeCreatedOccupant
//     makes for its removal. Both temp names share the
//     unit's directory, so one guard covers both passes.
//     If an ancestor stands replaced — above all a symlink
//     an earlier rollback pass restored from its preimage —
//     the temp is not reachable in this tree, and resolving
//     the path would delete in whatever tree the
//     replacement names.
//
// A unit that was never started created no temp and is not
// visited here; a unit whose rename landed has no temp
// standing, and absence (in targetGone's full sense) is
// success. A real directory at a derived name is left
// standing: no temp this writer makes is a directory, so
// the name's occupant is not the settlement's to remove.
// Temps left by earlier writers under superseded derivations
// — random names, or the destination-only derivation this
// one replaced — cannot be derived from anything the
// journal records now; they are left as visible debris
// rather than swept by prefix — a sweep could not tell
// debris from a genuine file of the same shape, and this
// branch has no production deployment carrying such debris.
func removeUnitTemps(parentDir, op string, u journalUnit) error {
	if !u.TempCreatedFile && !u.TempCreatedLink {
		return nil
	}
	blocked, err := blockedByAncestor(parentDir, u.Path)
	if err != nil {
		return err
	}
	if blocked {
		return nil
	}
	dst := filepath.Join(parentDir, filepath.FromSlash(u.Path))
	for _, kind := range []struct {
		link     bool
		recorded bool
	}{{false, u.TempCreatedFile}, {true, u.TempCreatedLink}} {
		if !kind.recorded {
			continue
		}
		tmp := filepath.Join(filepath.Dir(dst), syncTempName(dst, op, kind.link))
		info, err := os.Lstat(tmp)
		if err != nil {
			if targetGone(err) {
				continue
			}
			return fmt.Errorf("settle: stat temp %s: %w", tmp, err)
		}
		if info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		if err := removeOccupant(tmp); err != nil {
			return err
		}
	}
	return nil
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

// parentRootReplaced reports whether dir's final component
// stands as a symlink. The journal's parent directory was
// resolved — absolute and symlink-free — when the settlement
// wrote it (resolveParentDir at the materializer's entry), so
// a symlink standing at that exact path when rollback runs
// means the directory itself was replaced after the crash:
// moved aside and linked is a filesystem state, needing no
// concurrent actor. Every path rollback computes then
// resolves through the link into whatever tree it names, and
// the component guards (blockedByAncestor, symlinkAncestor)
// cannot see it — they judge components INSIDE the parent,
// never the parent itself. A directory that cannot be
// stat'ed is not judged here: its absence is met by the undo
// paths in their own terms, as before this check existed.
// Only the final component is judged — the ancestors above
// it were the platform's business at resolution time and
// remain so.
func parentRootReplaced(dir string) bool {
	info, err := os.Lstat(dir)
	return err == nil && info.Mode()&fs.ModeSymlink != 0
}

// verifyParentIdentity judges the journal's claim on its
// parent directory by identity, not by path: the directory
// standing at the recorded path now must be the very
// directory the journal recorded at its creation — same
// device, same inode. The path's form checks
// (parentRootReplaced and the ancestor guards) cannot see
// the replacement this one exists for: the journaled
// directory moved aside and a different REAL directory
// placed at its path, a filesystem state needing no
// concurrent actor, under which rollback's restores would
// overwrite the replacement's files and its removals delete
// them while the settled tree sits aside unrecovered. A
// journal carrying no identity — written before identities
// were recorded — is refused rather than acted on: the
// claim is unverifiable, and recovery does not guess. A
// directory that cannot be stat'ed at all is refused the
// same way: its identity cannot be verified either.
func verifyParentIdentity(j *journal) error {
	if j.ParentIdentity == "" {
		return fmt.Errorf("settle: recover %q: the journal records no parent identity for %q — it predates identity recording, so the directory standing there cannot be verified as the one the settlement wrote through; refusing to roll back on an unverifiable journal", j.ParentScope, j.ParentDir)
	}
	id, err := parentDirIdentity(j.ParentDir)
	if err != nil {
		return fmt.Errorf("settle: recover %q: cannot verify the identity of parent directory %q: %v — refusing to roll back", j.ParentScope, j.ParentDir, err)
	}
	if id != j.ParentIdentity {
		return fmt.Errorf("settle: recover %q: parent directory %q has identity %s, but the journal recorded identity %s — the directory has been replaced since the journal was written, and rolling back would restore and delete in a different tree; refusing", j.ParentScope, j.ParentDir, id, j.ParentIdentity)
	}
	return nil
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
func undoWriteUnit(parentDir, preDir string, u journalUnit, op string, recordTemp func(bool) error) error {
	rel := u.Path
	dst := filepath.Join(parentDir, filepath.FromSlash(rel))
	preInfo, hasPreimage, err := statPreimage(preDir, rel)
	if err != nil {
		return err
	}
	if !hasPreimage || preInfo.IsDir() {
		return removeCreatedOccupant(parentDir, rel, dst)
	}
	return restorePreimage(parentDir, preDir, u, op, recordTemp)
}

// undoDeleteUnit reverses one applied delete unit by restoring
// its preimage. A delete unit's target existed by construction,
// so a missing preimage contradicts the journal: a descriptive
// refusal, never a guess.
func undoDeleteUnit(parentDir, preDir string, u journalUnit, op string, recordTemp func(bool) error) error {
	rel := u.Path
	if _, hasPreimage, err := statPreimage(preDir, rel); err != nil {
		return err
	} else if !hasPreimage {
		return fmt.Errorf("settle: delete unit %q is marked done but has no preimage — journal and staging disagree", rel)
	}
	return restorePreimage(parentDir, preDir, u, op, recordTemp)
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

// restorePreimage puts the unit's preimage back at its path
// in parentDir: the current occupant is removed, the parent
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
//
// The restore writes through the derived-name temp under the
// journal's op, and recordTemp — rollback's ownership hook —
// persists the unit's temp-created record between the temp's
// creation and its content write, exactly as the write path
// does. That record is what lets a repeated restore proceed
// past its own predecessor: if this restore's exclusive
// creation collides with a temp already standing at the
// derived name AND the unit carries the record, the standing
// temp is the recorded one's — an earlier Recover was killed
// between creating it and renaming it — so it is removed and
// creation retried once. A collision with no record is a
// foreign occupant: the refusal stands, byte-identical,
// because a computable name is not ownership.
func restorePreimage(parentDir, preDir string, u journalUnit, op string, recordTemp func(bool) error) error {
	rel := u.Path
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
	// A destination already serving its preimage — same
	// regular file, same bytes, same permissions — is
	// restored by definition, and the restore completes
	// without writing. The case is not an optimization: it
	// is the shape a refused or never-landed write leaves
	// (the verb's exclusive temp creation was refused by a
	// pre-existing occupant, or the kill struck before the
	// rename), and rewriting identical bytes through the
	// derived temp name would let that occupant — a file
	// rollback must not delete, or the operation's own
	// unrecorded debris — wedge a rollback whose substance
	// is already satisfied.
	if preInfo.Mode().IsRegular() {
		if dstInfo, lerr := os.Lstat(dst); lerr == nil && dstInfo.Mode().IsRegular() && dstInfo.Mode().Perm() == preInfo.Mode().Perm() {
			same, ferr := filesEqual(pre, dst)
			if ferr != nil {
				return ferr
			}
			if same {
				return nil
			}
		}
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
	err = copyFileAtomic(pre, dst, preInfo.Mode().Perm(), op, recordTemp)
	if err == nil || !u.TempCreatedFile || !errors.Is(err, fs.ErrExist) {
		return err
	}
	// The recorded-temp collision above: the unit's file-kind
	// record proves this settlement created the temp standing
	// at the derived name (a restore temp is always the file
	// kind — the only kind this path creates), so removing it
	// removes the settlement's own — never a foreign file,
	// which carries no record and keeps the refusal.
	tmp := filepath.Join(filepath.Dir(dst), syncTempName(dst, op, false))
	if rerr := os.Remove(tmp); rerr != nil && !targetGone(rerr) {
		return fmt.Errorf("settle: remove recorded restore temp %s: %w", tmp, rerr)
	}
	return copyFileAtomic(pre, dst, preInfo.Mode().Perm(), op, recordTemp)
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
