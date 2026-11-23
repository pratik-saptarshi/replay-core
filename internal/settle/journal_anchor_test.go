package settle

// Parent-anchor tests (PR #7 judge consolidation, SEC-1): a
// journal's parent_dir is the field that selects WHICH tree
// its validated unit paths apply to, and it was the least
// validated field in the record — decodeJournal checked it
// only for absoluteness, and Recover's fixed signature
// carries no directory to check it against. A journal file
// planted under Root/journals (a corrupted write, a restored
// backup, a helper process sharing the root) therefore
// steered Recover's deletes and writes to any absolute path,
// and a forged verified journal minted its recorded fact with
// no materialization at all.
//
// The fix: the journal never vouches for its own scope.
// writeJournal records the parent directory in an independent
// anchor record (Root/journals/<scope>.anchor) before the
// journal itself lands, and Recover acts on a journal only
// when the anchor vouches for the directory it names.
// decodeJournal additionally enforces the scope-name laws and
// the object-ID shape on the journal's commit-point fields,
// and requires the recorded fact body to be exactly the body
// the store's constructors build from the journal's fields.
//
// The planted journals below are written as raw canonical
// bytes — a file placement, not a writeJournal call — because
// that is the attack: a journal that arrives without the
// system's writer must not acquire the writer's authority.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// rawJournalSpec is one planted journal's full contents.
type rawJournalSpec struct {
	op           string
	verb         string
	parentScope  string
	runScope     string
	state        string
	oldHead      string
	resultCommit string
	parentDir    string
	// parentIdentity, when non-empty, is planted as the
	// journal's parent_identity — the field the writer
	// records at a journal's creation. Plants that mean to
	// stand for a writer-produced journal set it (from
	// parentDirIdentity); plants of crafted journals leave
	// it empty, as they leave the anchor unrecorded.
	parentIdentity string
	// parentChain, when non-nil, is planted as the
	// journal's parent_chain — the ancestor-chain identities
	// the writer records at a journal's creation. Plants
	// that mean to stand for a writer-produced journal set
	// it (from parentChainIdentities); plants of crafted or
	// pre-chain journals leave it nil.
	parentChain []string
	fact           canonical.Value
	units          []canonical.Value
}

// plantRawJournal writes spec's journal in canonical form at
// path, bypassing writeJournal entirely, and returns the
// exact bytes planted.
func plantRawJournal(t *testing.T, path string, spec rawJournalSpec) []byte {
	t.Helper()
	pairs := []canonical.Pair{
		{Key: "fact", Value: spec.fact},
		{Key: "old_head", Value: canonical.StringOf(spec.oldHead)},
		{Key: "op", Value: canonical.StringOf(spec.op)},
	}
	if spec.parentChain != nil {
		elems := make([]canonical.Value, 0, len(spec.parentChain))
		for _, id := range spec.parentChain {
			elems = append(elems, canonical.StringOf(id))
		}
		pairs = append(pairs, canonical.Pair{Key: "parent_chain", Value: canonical.ArrayOf(elems...)})
	}
	pairs = append(pairs, canonical.Pair{Key: "parent_dir", Value: canonical.StringOf(spec.parentDir)})
	if spec.parentIdentity != "" {
		pairs = append(pairs, canonical.Pair{Key: "parent_identity", Value: canonical.StringOf(spec.parentIdentity)})
	}
	pairs = append(pairs,
		canonical.Pair{Key: "parent_scope", Value: canonical.StringOf(spec.parentScope)},
		canonical.Pair{Key: "result_commit", Value: canonical.StringOf(spec.resultCommit)},
		canonical.Pair{Key: "run_scope", Value: canonical.StringOf(spec.runScope)},
		canonical.Pair{Key: "state", Value: canonical.StringOf(spec.state)},
		canonical.Pair{Key: "units", Value: canonical.ArrayOf(spec.units...)},
		canonical.Pair{Key: "verb", Value: canonical.StringOf(spec.verb)},
	)
	v := canonical.ObjectOf(pairs...)
	b, err := canonical.MarshalKernel(v)
	if err != nil {
		t.Fatalf("marshal planted journal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

// anchorVictim plants a directory outside the settle root
// holding one sentinel file — the tree a crafted journal
// names as its parent_dir.
func anchorVictim(t *testing.T, f *journalFixture) string {
	t.Helper()
	dir := filepath.Join(f.scratch, "victim")
	journalWriteFile(t, filepath.Join(dir, "sentinel.txt"), "sentinel\n", 0o644)
	return dir
}

// assertAnchorRefusal checks the post-refusal invariants for
// the Recover-shaped tests: a descriptive anchor refusal, the
// victim tree byte-identical, the journal file left in place
// as evidence, no fact minted, the parent head unmoved.
func assertAnchorRefusal(t *testing.T, f *journalFixture, err error, victim, victimHash, jpath string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "anchor") {
		t.Fatalf("Recover err = %v, want a descriptive parent-anchor refusal", err)
	}
	if victim != "" {
		if got := hashTree(t, victim); got != victimHash {
			t.Fatalf("victim walk hash = %s, want %s (Recover acted on a directory no anchor vouches for)", got, victimHash)
		}
	}
	if _, serr := os.Stat(jpath); serr != nil {
		t.Fatalf("journal after refused Recover: %v, want it left in place as evidence", serr)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("refused Recover minted %d facts, want 0", len(facts))
	}
	if head, herr := f.snaps.ScopeHead("parent"); herr != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after refused Recover; want %s unmoved", head, herr, f.parent0)
	}
}

// TestRecoverRefusesUnanchoredJournalDeletePrimitive is
// SEC-1's delete primitive: an applying journal naming a
// victim directory outside every root, one done write unit,
// and no staging or preimage anywhere — rollback reads a done
// write unit with no preimage as "the unit created the
// occupant — remove it", so the journal file alone is a
// delete over any absolute path.
func TestRecoverRefusesUnanchoredJournalDeletePrimitive(t *testing.T) {
	f := newJournalFixture(t)
	victim := anchorVictim(t, f)
	victimHash := hashTree(t, victim)
	jpath := journalPath(f.root, "parent")
	plantRawJournal(t, jpath, rawJournalSpec{
		op: "op-a", verb: verbSelect, parentScope: "parent", runScope: "run-a",
		state: journalStateApplying, oldHead: f.parent0, resultCommit: f.parent0,
		parentDir: victim,
		fact:      store.SettleSelectBody("op-a", "parent", "run-a", f.parent0, f.parent0),
		units:     []canonical.Value{rawUnit("sentinel.txt", unitWrite, true, true)},
	})

	err := Recover(f.ctx, "parent")
	assertAnchorRefusal(t, f, err, victim, victimHash, jpath)
}

// TestRecoverRefusesUnanchoredJournalWritePrimitive is the
// write twin: a done delete unit whose preimage is planted in
// staging makes rollback restore the planted bytes into the
// victim directory.
func TestRecoverRefusesUnanchoredJournalWritePrimitive(t *testing.T) {
	f := newJournalFixture(t)
	victim := anchorVictim(t, f)
	victimHash := hashTree(t, victim)
	preDir := filepath.Join(f.root, "staging", "op-b", "pre")
	journalWriteFile(t, filepath.Join(preDir, "restored.txt"), "planted\n", 0o644)
	jpath := journalPath(f.root, "parent")
	plantRawJournal(t, jpath, rawJournalSpec{
		op: "op-b", verb: verbSelect, parentScope: "parent", runScope: "run-a",
		state: journalStateApplying, oldHead: f.parent0, resultCommit: f.parent0,
		parentDir: victim,
		fact:      store.SettleSelectBody("op-b", "parent", "run-a", f.parent0, f.parent0),
		units:     []canonical.Value{rawUnit("restored.txt", unitDelete, true, true)},
	})

	err := Recover(f.ctx, "parent")
	assertAnchorRefusal(t, f, err, victim, victimHash, jpath)
	if _, serr := os.Stat(filepath.Join(victim, "restored.txt")); !os.IsNotExist(serr) {
		t.Fatalf("victim/restored.txt after refused Recover: err = %v, want never written", serr)
	}
}

// TestRecoverRefusesUnanchoredVerifiedJournal is the
// integrity twin: a forged verified journal whose old_head
// matches the parent's current head makes Recover advance
// the scope and mint the recorded fact with no
// materialization ever having occurred. Its parent_dir names
// the REAL parent directory — the anchor's absence, not the
// directory, is what must refuse it.
func TestRecoverRefusesUnanchoredVerifiedJournal(t *testing.T) {
	f := newJournalFixture(t)
	resultDir := filepath.Join(f.scratch, "result")
	journalWriteFile(t, filepath.Join(resultDir, "keep.txt"), "keep\n", 0o644)
	journalWriteFile(t, filepath.Join(resultDir, "mod.txt"), "settled\n", 0o644)
	resultCommit, err := f.snaps.Snapshot(resultDir, map[string]string{"run": "settle-anchor-forge"})
	if err != nil {
		t.Fatalf("Snapshot result: %v", err)
	}
	jpath := journalPath(f.root, "parent")
	plantRawJournal(t, jpath, rawJournalSpec{
		op: "op-forge", verb: verbSelect, parentScope: "parent", runScope: "run-a",
		state: journalStateVerified, oldHead: f.parent0, resultCommit: resultCommit,
		parentDir: f.parentDir,
		fact:      store.SettleSelectBody("op-forge", "parent", "run-a", f.parent0, resultCommit),
		units: []canonical.Value{
			rawUnit("mod.txt", unitWrite, true, true),
			rawUnit("old.txt", unitDelete, true, true),
		},
	})

	err = Recover(f.ctx, "parent")
	assertAnchorRefusal(t, f, err, "", "", jpath)
}

// TestRecoverRefusesJournalDisagreeingWithAnchor: the anchor
// a genuine settlement recorded names the real parent
// directory; a journal file swapped in afterwards, naming a
// victim directory under the same scope and op, disagrees
// with it and is refused — the anchor, not the journal, is
// the authority on where recovery may act.
func TestRecoverRefusesJournalDisagreeingWithAnchor(t *testing.T) {
	f := newJournalFixture(t)
	// A genuine journal, written by the system's writer, which
	// records the parent anchor beside it naming the real
	// parent directory. The journal file is then swapped for
	// one naming the victim; the anchor stays.
	genuine := &journal{
		Op: "op-swap", Verb: verbSelect, ParentScope: "parent", RunScope: "run-a",
		State: journalStateApplying, OldHead: f.parent0, ResultCommit: f.parent0,
		ParentDir: f.parentDir,
		Fact:      store.SettleSelectBody("op-swap", "parent", "run-a", f.parent0, f.parent0),
		Units:     []journalUnit{{Path: "mod.txt", Kind: unitWrite, Done: true}},
	}
	if err := writeJournal(f.root, genuine); err != nil {
		t.Fatalf("writeJournal genuine: %v", err)
	}
	victim := anchorVictim(t, f)
	victimHash := hashTree(t, victim)
	jpath := journalPath(f.root, "parent")
	plantRawJournal(t, jpath, rawJournalSpec{
		op: "op-swap", verb: verbSelect, parentScope: "parent", runScope: "run-a",
		state: journalStateApplying, oldHead: f.parent0, resultCommit: f.parent0,
		parentDir: victim,
		fact:      store.SettleSelectBody("op-swap", "parent", "run-a", f.parent0, f.parent0),
		units:     []canonical.Value{rawUnit("sentinel.txt", unitWrite, true, true)},
	})

	err := Recover(f.ctx, "parent")
	assertAnchorRefusal(t, f, err, victim, victimHash, jpath)
}

// TestRecoverRefusesTransplantedJournal: a journal copied
// out of the root whose settlement produced it into a
// different root — where no anchor vouches for it — is
// refused there, and the tree it names is untouched: the
// journal file alone does not carry its authority with it.
func TestRecoverRefusesTransplantedJournal(t *testing.T) {
	f1 := newJournalFixture(t)
	// The genuine in-flight state in the origin root: mod.txt
	// already overwritten, its preimage durable in staging,
	// the journal recording the done write unit.
	journalWriteFile(t, filepath.Join(f1.parentDir, "mod.txt"), "changed\n", 0o644)
	preDir := filepath.Join(f1.root, "staging", "op-tx", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	j := &journal{
		Op: "op-tx", Verb: verbSelect, ParentScope: "parent", RunScope: "run-a",
		State: journalStateApplying, OldHead: f1.parent0, ResultCommit: f1.parent0,
		ParentDir: f1.parentDir,
		Fact:      store.SettleSelectBody("op-tx", "parent", "run-a", f1.parent0, f1.parent0),
		Units:     []journalUnit{{Path: "mod.txt", Kind: unitWrite, Done: true}},
	}
	if err := writeJournal(f1.root, j); err != nil {
		t.Fatalf("writeJournal origin: %v", err)
	}
	b, err := os.ReadFile(journalPath(f1.root, "parent"))
	if err != nil {
		t.Fatal(err)
	}

	// The transplant: the journal file alone, copied into a
	// second root (its own fixture, its own parent tree).
	f2 := newJournalFixture(t)
	jpath2 := journalPath(f2.root, "parent")
	if err := os.MkdirAll(filepath.Dir(jpath2), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jpath2, b, 0o644); err != nil {
		t.Fatal(err)
	}

	err = Recover(f2.ctx, "parent")
	if err == nil || !strings.Contains(err.Error(), "anchor") {
		t.Fatalf("Recover of transplanted journal err = %v, want a descriptive parent-anchor refusal", err)
	}
	if got, rerr := os.ReadFile(filepath.Join(f1.parentDir, "mod.txt")); rerr != nil || string(got) != "changed\n" {
		t.Fatalf("origin parent mod.txt = %q, %v; want its in-flight bytes untouched by the foreign Recover", got, rerr)
	}
	if _, serr := os.Stat(jpath2); serr != nil {
		t.Fatalf("transplanted journal after refused Recover: %v, want it left in place as evidence", serr)
	}
}

// TestRecoverActsOnWriterJournalWithAnchor is the control: a
// journal the system's writer produced — hand-built here
// through writeJournal, as the rollback tests build theirs —
// still recovers byte-exactly. The anchor refuses journals
// that arrive without the writer; it must not refuse the
// writer's own.
func TestRecoverActsOnWriterJournalWithAnchor(t *testing.T) {
	f := newJournalFixture(t)
	preHash := hashTree(t, f.parentDir)

	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "changed\n", 0o644)
	journalWriteFile(t, filepath.Join(f.parentDir, "added.txt"), "created\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-ctl", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)

	j := &journal{
		Op: "op-ctl", Verb: verbSelect, ParentScope: "parent", RunScope: "run-a",
		State: journalStatePlanned, OldHead: f.parent0, ResultCommit: f.parent0,
		ParentDir: f.parentDir,
		Fact:      store.SettleSelectBody("op-ctl", "parent", "run-a", f.parent0, f.parent0),
		Units: []journalUnit{
			{Path: "mod.txt", Kind: unitWrite, Done: true},
			{Path: "added.txt", Kind: unitWrite, Done: true},
		},
	}
	if err := writeJournal(f.root, j); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}

	if err := Recover(f.ctx, "parent"); err != nil {
		t.Fatalf("Recover of a writer-produced journal: %v", err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after rollback = %s, want pre-apply %s", got, preHash)
	}
	if _, err := os.Stat(journalPath(f.root, "parent")); !os.IsNotExist(err) {
		t.Fatalf("journal after rollback: err = %v, want gone", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "staging", "op-ctl")); !os.IsNotExist(err) {
		t.Fatalf("staging after rollback: err = %v, want gone", err)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("rollback minted %d facts, want 0", len(facts))
	}
}

// TestReadJournalRefusesTraversalRunScope: decodeJournal
// enforces the scope-name law on the journal's own fields —
// a journal naming a traversal run scope is refused at read
// time, whatever its other fields claim.
func TestReadJournalRefusesTraversalRunScope(t *testing.T) {
	f := newJournalFixture(t)
	plantRawJournal(t, journalPath(f.root, "parent"), rawJournalSpec{
		op: "op-d1", verb: verbSelect, parentScope: "parent", runScope: "../escape",
		state: journalStateApplying, oldHead: f.parent0, resultCommit: f.parent0,
		parentDir: f.parentDir,
		fact:      store.SettleSelectBody("op-d1", "parent", "../escape", f.parent0, f.parent0),
		units:     []canonical.Value{},
	})
	if _, err := readJournal(f.root, "parent"); err == nil {
		t.Fatal("readJournal accepted a journal whose run_scope is a traversal name, want a descriptive refusal")
	}
}

// TestReadJournalRefusesTraversalParentScope: the parent
// scope field gets the same law — readJournal's equality
// check against the argument is a self-consistency check a
// traversal string satisfies trivially, so decode itself
// must refuse the name.
func TestReadJournalRefusesTraversalParentScope(t *testing.T) {
	f := newJournalFixture(t)
	plantRawJournal(t, journalPath(f.root, "../escape"), rawJournalSpec{
		op: "op-d2", verb: verbSelect, parentScope: "../escape", runScope: "run-a",
		state: journalStateApplying, oldHead: f.parent0, resultCommit: f.parent0,
		parentDir: f.parentDir,
		fact:      store.SettleSelectBody("op-d2", "../escape", "run-a", f.parent0, f.parent0),
		units:     []canonical.Value{},
	})
	if _, err := readJournal(f.root, "../escape"); err == nil {
		t.Fatal("readJournal accepted a journal whose parent_scope is a traversal name, want a descriptive refusal")
	}
}

// TestReadJournalRefusesMalformedCommitFields: the journal's
// commit-point endpoints are object IDs or they are nothing
// — a journal whose old_head, result_commit, or (apply) fact
// fork_commit is a free-form string is refused at decode,
// not left for git to trip over downstream or, in the
// rollback branch, never checked at all.
func TestReadJournalRefusesMalformedCommitFields(t *testing.T) {
	f := newJournalFixture(t)
	cases := []struct {
		name string
		spec rawJournalSpec
	}{
		{"old_head", rawJournalSpec{
			op: "op-d3", verb: verbSelect, parentScope: "parent", runScope: "run-a",
			state: journalStateApplying, oldHead: "not-a-commit", resultCommit: f.parent0,
			parentDir: f.parentDir,
			fact:      store.SettleSelectBody("op-d3", "parent", "run-a", "not-a-commit", f.parent0),
			units:     []canonical.Value{},
		}},
		{"result_commit", rawJournalSpec{
			op: "op-d4", verb: verbSelect, parentScope: "parent", runScope: "run-a",
			state: journalStateApplying, oldHead: f.parent0, resultCommit: "not-a-commit",
			parentDir: f.parentDir,
			fact:      store.SettleSelectBody("op-d4", "parent", "run-a", f.parent0, "not-a-commit"),
			units:     []canonical.Value{},
		}},
		{"fork_commit", rawJournalSpec{
			op: "op-d5", verb: verbApply, parentScope: "parent", runScope: "run-a",
			state: journalStateApplying, oldHead: f.parent0, resultCommit: f.parent0,
			parentDir: f.parentDir,
			fact:      store.SettleApplyBody("op-d5", "parent", "run-a", "not-a-commit", f.parent0, []string{"a.txt"}),
			units:     []canonical.Value{},
		}},
	}
	for _, tc := range cases {
		plantRawJournal(t, journalPath(f.root, "parent"), tc.spec)
		if _, err := readJournal(f.root, "parent"); err == nil {
			t.Fatalf("readJournal accepted a journal with a malformed %s, want a descriptive refusal", tc.name)
		}
		if err := os.Remove(journalPath(f.root, "parent")); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReadJournalRefusesFactWithExtraField: the recorded
// fact body must be exactly the body the store's constructors
// build from the journal's fields — a body carrying a field
// no verb computed is refused at decode, so the body Recover
// mints from is one the system's one construction site could
// have produced.
func TestReadJournalRefusesFactWithExtraField(t *testing.T) {
	f := newJournalFixture(t)
	body := store.SettleSelectBody("op-d6", "parent", "run-a", f.parent0, f.parent0)
	pairs := append([]canonical.Pair{}, body.Pairs...)
	pairs = append(pairs, canonical.Pair{Key: "comment", Value: canonical.StringOf("planted")})
	plantRawJournal(t, journalPath(f.root, "parent"), rawJournalSpec{
		op: "op-d6", verb: verbSelect, parentScope: "parent", runScope: "run-a",
		state: journalStateApplying, oldHead: f.parent0, resultCommit: f.parent0,
		parentDir: f.parentDir,
		fact:      canonical.ObjectOf(pairs...),
		units:     []canonical.Value{},
	})
	if _, err := readJournal(f.root, "parent"); err == nil {
		t.Fatal("readJournal accepted a fact body with a field no constructor produces, want a descriptive refusal")
	}
}
