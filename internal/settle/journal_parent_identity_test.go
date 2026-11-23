package settle

// Parent-identity tests (PR #11 Sourcery re-review, finding
// F1): rollback's entry checks judge the journaled parent
// path's FORM — the final component is not a symlink — but
// never its IDENTITY. If the journaled directory is moved
// aside after the crash and a different REAL directory is
// placed at its path, the form checks pass and rollback
// proceeds into the replacement tree: its restores overwrite
// the replacement's files and its removals delete them,
// while the tree the settlement actually wrote through sits
// moved aside, unrecovered. A filesystem state, needing no
// concurrent actor: directories are replaced by hand, by
// restore tools, by provisioning scripts.
//
// The journal must therefore record the parent directory's
// identity — device and inode of the resolved directory,
// taken when the journal is created — and rollback must
// verify it before any path under the recorded directory is
// dereferenced. A journal carrying no identity (written
// before identities were recorded) is unverifiable, and an
// unverifiable journal is refused, never acted on: the
// ownership record's rule, one level up — absence of the
// record means the claim is unproven.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// TestRecoverRefusesReplacedParentDirectory: a Select is
// crashed with its mutation landed, and the journaled
// parent directory is then moved aside and replaced by a
// different real directory at the same path. Rollback must
// refuse at its entry — naming the directory and the
// identity mismatch — and touch neither the replacement
// tree nor the moved-aside original; the journal stays in
// place as evidence.
func TestRecoverRefusesReplacedParentDirectory(t *testing.T) {
	f := newJournalFixture(t)

	layer, err := Fork(f.ctx, "parent", "run-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	journalWriteFile(t, filepath.Join(layer.Dir, "mod.txt"), "run mod\n", 0o644)
	advanceRun(t, f, "run-a", layer.Dir)

	// Crash in the first unit's window: the new bytes have
	// landed in the real parent, the completion mark has
	// not persisted.
	runCrashed(t, 1, func() {
		_, _ = Select(f.ctx, "parent", f.parentDir, "run-a", "op-ident")
	})
	if b, err := os.ReadFile(filepath.Join(f.parentDir, "mod.txt")); err != nil || string(b) != "run mod\n" {
		t.Fatalf("parent mod.txt after crash = %q, %v; want the landed bytes", b, err)
	}

	// The replacement: the journaled directory moved aside,
	// a different real directory standing at its path, with
	// content of its own rollback would overwrite.
	realDir := filepath.Join(f.scratch, "parent-real")
	if err := os.Rename(f.parentDir, realDir); err != nil {
		t.Fatal(err)
	}
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "replacement tree\n", 0o644)
	journalWriteFile(t, filepath.Join(f.parentDir, "own.txt"), "replacement own\n", 0o644)
	replacementHash := hashTree(t, f.parentDir)

	err = Recover(f.ctx, "parent")
	if err == nil {
		t.Fatal("Recover into a replaced parent directory succeeded; want a descriptive identity refusal")
	}
	if !strings.Contains(err.Error(), f.parentDir) || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("Recover refusal = %v, want it to name the parent directory %s and the identity mismatch", err, f.parentDir)
	}
	if got := hashTree(t, f.parentDir); got != replacementHash {
		t.Fatalf("replacement tree walk hash after refused Recover = %s, want %s (rollback wrote into the replacement tree)", got, replacementHash)
	}
	if b, rerr := os.ReadFile(filepath.Join(realDir, "mod.txt")); rerr != nil || string(b) != "run mod\n" {
		t.Fatalf("moved-aside parent mod.txt = %q, %v; want its landed bytes untouched", b, rerr)
	}
	if _, serr := os.Stat(journalPath(f.root, "parent")); serr != nil {
		t.Fatalf("journal after refused Recover: %v, want still present as evidence", serr)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("refused Recover left %d facts, want 0", len(facts))
	}
}

// TestRecoverRefusesJournalWithoutParentIdentity: a journal
// in the form written before parent identities were
// recorded — the full writer state of its day (anchor
// vouching for its exact bytes, preimage durable, unit
// marked) minus the identity field. Its claim on the parent
// directory is unverifiable, so rollback refuses it
// descriptively and leaves the tree and the journal
// untouched, exactly as it refuses a unit whose temp record
// is absent.
func TestRecoverRefusesJournalWithoutParentIdentity(t *testing.T) {
	f := newJournalFixture(t)

	// The interrupted verb's landed state and its preimage.
	journalWriteFile(t, filepath.Join(f.parentDir, "mod.txt"), "changed\n", 0o644)
	preDir := filepath.Join(f.root, "staging", "op-noident", "pre")
	journalWriteFile(t, filepath.Join(preDir, "mod.txt"), "original\n", 0o644)
	preHash := hashTree(t, f.parentDir)

	// The old-form journal: every field the writer of its
	// day recorded, and no parent_identity among them.
	jpath := journalPath(f.root, "parent")
	b := plantRawJournal(t, jpath, rawJournalSpec{
		op: "op-noident", verb: verbSelect, parentScope: "parent", runScope: "run-a",
		state: journalStateApplying, oldHead: f.parent0, resultCommit: f.parent0,
		parentDir: f.parentDir,
		fact:      store.SettleSelectBody("op-noident", "parent", "run-a", f.parent0, f.parent0),
		units: []canonical.Value{
			rawUnit("mod.txt", unitWrite, true, true),
		},
	})
	if err := writeParentAnchor(f.root, parentAnchor{
		ParentScope:   "parent",
		ParentDir:     f.parentDir,
		JournalDigest: journalBytesDigest(b),
	}); err != nil {
		t.Fatalf("write parent anchor: %v", err)
	}

	err := Recover(f.ctx, "parent")
	if err == nil {
		t.Fatal("Recover acted on a journal carrying no parent identity; want a descriptive refusal (absent identity is unverifiable)")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Fatalf("Recover refusal = %v, want it to name the missing parent identity", err)
	}
	if got := hashTree(t, f.parentDir); got != preHash {
		t.Fatalf("parent walk hash after refused Recover = %s, want %s (rollback acted on an unverifiable journal)", got, preHash)
	}
	if _, serr := os.Stat(jpath); serr != nil {
		t.Fatalf("journal after refused Recover: %v, want still present as evidence", serr)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("refused Recover left %d facts, want 0", len(facts))
	}
}
