package settle

// Parent-chain identity tests (PR #11 Sourcery re-review
// round 2, panel ruling on F1/F2): the parent identity
// judges only the recorded path's FINAL component. An
// ANCESTOR of the parent directory replaced by a different
// REAL directory — the parent directory itself moved into
// it, inode intact — defeats both the symlink walk (no
// component is a link) and the final-component identity
// check (the directory at the end of the path is the very
// one the settlement wrote through), while the path's
// meaning has changed: rollback resolves through a
// directory object no record vouches for, and a later
// repointing of that ancestor's contents redirects every
// path beneath it.
//
// The journal therefore records the identity of EVERY
// component prefix of the recorded parent path — the
// ancestor chain, from the filesystem root down to the
// parent itself — and rollback verifies the whole chain at
// entry. Grandfathering follows the identity rule: a
// journal carrying no chain proceeds under the other
// guards; a journal carrying one is held to every element
// of it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// TestRecoverRefusesAncestorReplacedByRealDirectory: a
// crashed Select's journal stands; the fixture's scratch
// directory — a proper ancestor of the journaled parent —
// is then replaced by a NEW real directory, with the
// original parent directory (and the settle root and
// stores) moved back under it, inodes intact. Every older
// guard passes: no symlink anywhere, and the directory at
// the end of the recorded path is the very directory the
// settlement wrote through. Only the ancestor chain sees
// the substitution — the scratch component is a different
// directory object than the one the journal recorded.
// Rollback must refuse, naming the replaced component, and
// leave the tree and the journal untouched.
func TestRecoverRefusesAncestorReplacedByRealDirectory(t *testing.T) {
	f := newJournalFixture(t)
	crashSelectForAncestorTests(t, f, "op-chain")
	parentHash := hashTree(t, f.parentDir)

	origDir := f.scratch + "-orig"
	t.Cleanup(func() { _ = os.RemoveAll(origDir) })
	if err := os.Rename(f.scratch, origDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(origDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(origDir, e.Name()), filepath.Join(f.scratch, e.Name())); err != nil {
			t.Fatal(err)
		}
	}

	err = Recover(f.ctx, "parent")
	if err == nil {
		t.Fatal("Recover with an ancestor replaced by a different real directory succeeded; want a descriptive chain refusal")
	}
	if !strings.Contains(err.Error(), "identity") || !strings.Contains(err.Error(), f.scratch) {
		t.Fatalf("Recover refusal = %v, want it to name the replaced ancestor component %s and the identity mismatch", err, f.scratch)
	}
	if got := hashTree(t, f.parentDir); got != parentHash {
		t.Fatalf("parent walk hash after refused Recover = %s, want %s (rollback acted through an unvouched ancestor)", got, parentHash)
	}
	if _, serr := os.Stat(journalPath(f.root, "parent")); serr != nil {
		t.Fatalf("journal after refused Recover: %v, want still present as evidence", serr)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("refused Recover left %d facts, want 0", len(facts))
	}
}

// TestReadJournalRefusesMalformedParentChain: the ancestor
// chain is optional on read — a journal predating it
// carries none and is grandfathered — but a chain that IS
// present must be a full span of well-formed identities
// for the recorded path: a malformed entry, or a chain
// that does not span every prefix of parent_dir, is
// refused at decode, not guessed at by recovery.
func TestReadJournalRefusesMalformedParentChain(t *testing.T) {
	f := newJournalFixture(t)
	identity, err := parentDirIdentity(f.parentDir)
	if err != nil {
		t.Fatalf("parent identity: %v", err)
	}
	cases := []struct {
		name  string
		chain []string
	}{
		{"malformed entry", []string{"not-an-identity"}},
		{"short chain", []string{identity}},
	}
	for _, tc := range cases {
		plantRawJournal(t, journalPath(f.root, "parent"), rawJournalSpec{
			op: "op-chain-bad", verb: verbSelect, parentScope: "parent", runScope: "run-a",
			state: journalStateApplying, oldHead: f.parent0, resultCommit: f.parent0,
			parentDir:   f.parentDir,
			parentChain: tc.chain,
			fact:        store.SettleSelectBody("op-chain-bad", "parent", "run-a", f.parent0, f.parent0),
			units:       []canonical.Value{},
		})
		if _, err := readJournal(f.root, "parent"); err == nil {
			t.Fatalf("readJournal accepted a journal with a %s parent_chain, want a descriptive refusal", tc.name)
		}
		if err := os.Remove(journalPath(f.root, "parent")); err != nil {
			t.Fatal(err)
		}
	}
}
