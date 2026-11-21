package settle

// Scope-traversal probes (PR #7 judge consolidation, SEC-2):
// the entry-validation census leg for the scope-name law.
// Wave 3 (0aa5fd5) enforced the law at every verb before any
// filesystem path is constructed from a scope name; these
// probes verify that claim entry by entry rather than trusting
// it — for each public entry and each scope name it is handed,
// a traversal name must be refused as invalid BEFORE the path
// it would steer is read, acted on, or removed, and a victim
// tree standing where the traversal resolves must be untouched.
//
// The entry census (the package's pinned API surface):
// Fork(parent, run), Select(parent, run), Apply(parent, run),
// Discard(run), Recover(parent) — eight name slots. The
// planted-file geometry is SEC-2's own: journalPath(root,
// "../escape") and sidecarPath(root, "../escape") both resolve
// to <root>/escape.json, one level above the directory the
// name was supposed to stay inside.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// plantTraversalVictim creates a directory outside the settle
// root holding one sentinel file, and returns its path.
func plantTraversalVictim(t *testing.T, f *journalFixture) string {
	t.Helper()
	dir := filepath.Join(f.scratch, "victim")
	journalWriteFile(t, filepath.Join(dir, "sentinel.txt"), "sentinel\n", 0o644)
	return dir
}

// plantEscapeJournal writes a journal file at path in
// canonical form — state applying, one done write unit for
// sentinel.txt — naming parentScope as its parent scope and
// victim as its parent directory: the file a traversal scope
// would have Recover (or a verb's entry Recover) read and act
// on if the name were ever resolved into a path unchecked.
// The journal is internally consistent (its fact body is the
// store constructor's, cross-checked fields agree), so only
// the scope-name law stands between it and the victim.
func plantEscapeJournal(t *testing.T, path, parentScope, victim string, f *journalFixture) []byte {
	t.Helper()
	unit := canonical.ObjectOf(
		canonical.Pair{Key: "done", Value: canonical.BoolOf(true)},
		canonical.Pair{Key: "kind", Value: canonical.StringOf(unitWrite)},
		canonical.Pair{Key: "path", Value: canonical.StringOf("sentinel.txt")},
		canonical.Pair{Key: "started", Value: canonical.BoolOf(true)},
	)
	v := canonical.ObjectOf(
		canonical.Pair{Key: "fact", Value: store.SettleSelectBody("op-esc", parentScope, "run-a", f.parent0, f.parent0)},
		canonical.Pair{Key: "old_head", Value: canonical.StringOf(f.parent0)},
		canonical.Pair{Key: "op", Value: canonical.StringOf("op-esc")},
		canonical.Pair{Key: "parent_dir", Value: canonical.StringOf(victim)},
		canonical.Pair{Key: "parent_scope", Value: canonical.StringOf(parentScope)},
		canonical.Pair{Key: "result_commit", Value: canonical.StringOf(f.parent0)},
		canonical.Pair{Key: "run_scope", Value: canonical.StringOf("run-a")},
		canonical.Pair{Key: "state", Value: canonical.StringOf(journalStateApplying)},
		canonical.Pair{Key: "units", Value: canonical.ArrayOf(unit)},
		canonical.Pair{Key: "verb", Value: canonical.StringOf(verbSelect)},
	)
	b, err := canonical.MarshalKernel(v)
	if err != nil {
		t.Fatalf("marshal escape journal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

// assertTraversalRefused checks the probe's invariants after a
// refused call: the refusal names the name as invalid, the
// victim tree's walk hash is unchanged, the planted file is
// byte-identical and still present, no settlement fact was
// minted, and the parent scope's head never moved.
func assertTraversalRefused(t *testing.T, f *journalFixture, err error, victim, victimHash, plantedPath string, planted []byte) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("err = %v, want an invalid-scope refusal before any path is built from the name", err)
	}
	if got := hashTree(t, victim); got != victimHash {
		t.Fatalf("victim walk hash = %s, want %s (the traversal reached a filesystem effect outside the settle root)", got, victimHash)
	}
	got, rerr := os.ReadFile(plantedPath)
	if rerr != nil || string(got) != string(planted) {
		t.Fatalf("planted file after refusal = %q, %v; want it byte-identical and in place", got, rerr)
	}
	if facts := f.journalFacts(t, "run-a"); len(facts) != 0 {
		t.Fatalf("refused call minted %d facts, want 0", len(facts))
	}
	if head, herr := f.snaps.ScopeHead("parent"); herr != nil || head != f.parent0 {
		t.Fatalf("parent head = %s, %v after refusal; want %s unmoved", head, herr, f.parent0)
	}
}

// TestRecoverRefusesTraversalScopePlantedJournal is SEC-2's
// exact shape: Recover is exported, performs its effects
// before any snapshot call, and nothing downstream of it
// validates the name — the law must refuse the name itself,
// before the journal path is built.
func TestRecoverRefusesTraversalScopePlantedJournal(t *testing.T) {
	f := newJournalFixture(t)
	victim := plantTraversalVictim(t, f)
	victimHash := hashTree(t, victim)
	escapePath := filepath.Join(f.root, "escape.json")
	planted := plantEscapeJournal(t, escapePath, "../escape", victim, f)

	err := Recover(f.ctx, "../escape")
	assertTraversalRefused(t, f, err, victim, victimHash, escapePath, planted)
}

// TestSelectRefusesTraversalParentScope: Select hands its
// parent scope to Recover as its first step; the refusal must
// land before that journal path is built.
func TestSelectRefusesTraversalParentScope(t *testing.T) {
	f := newJournalFixture(t)
	victim := plantTraversalVictim(t, f)
	victimHash := hashTree(t, victim)
	escapePath := filepath.Join(f.root, "escape.json")
	planted := plantEscapeJournal(t, escapePath, "../escape", victim, f)

	_, err := Select(f.ctx, "../escape", f.parentDir, "run-a", "op-probe")
	assertTraversalRefused(t, f, err, victim, victimHash, escapePath, planted)
}

// TestApplyRefusesTraversalParentScope: Apply has the same
// entry Recover shape as Select.
func TestApplyRefusesTraversalParentScope(t *testing.T) {
	f := newJournalFixture(t)
	victim := plantTraversalVictim(t, f)
	victimHash := hashTree(t, victim)
	escapePath := filepath.Join(f.root, "escape.json")
	planted := plantEscapeJournal(t, escapePath, "../escape", victim, f)

	_, err := Apply(f.ctx, "../escape", f.parentDir, "run-a", "op-probe")
	assertTraversalRefused(t, f, err, victim, victimHash, escapePath, planted)
}

// TestSelectRefusesTraversalRunScope: the run scope builds
// the fork sidecar path Select reads; a planted file where
// the traversal resolves must never even be read as
// provenance, let alone acted on.
func TestSelectRefusesTraversalRunScope(t *testing.T) {
	f := newJournalFixture(t)
	victim := plantTraversalVictim(t, f)
	victimHash := hashTree(t, victim)
	escapePath := filepath.Join(f.root, "escape.json")
	planted := []byte("planted sidecar bytes\n")
	if err := os.MkdirAll(filepath.Dir(escapePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(escapePath, planted, 0o644); err != nil {
		t.Fatal(err)
	}
	parentHash := hashTree(t, f.parentDir)

	_, err := Select(f.ctx, "parent", f.parentDir, "../escape", "op-probe")
	assertTraversalRefused(t, f, err, victim, victimHash, escapePath, planted)
	if got := hashTree(t, f.parentDir); got != parentHash {
		t.Fatalf("parent walk hash = %s, want %s (a refused Select touched the parent tree)", got, parentHash)
	}
}

// TestApplyRefusesTraversalRunScope: same sidecar path, Apply.
func TestApplyRefusesTraversalRunScope(t *testing.T) {
	f := newJournalFixture(t)
	victim := plantTraversalVictim(t, f)
	victimHash := hashTree(t, victim)
	escapePath := filepath.Join(f.root, "escape.json")
	planted := []byte("planted sidecar bytes\n")
	if err := os.MkdirAll(filepath.Dir(escapePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(escapePath, planted, 0o644); err != nil {
		t.Fatal(err)
	}
	parentHash := hashTree(t, f.parentDir)

	_, err := Apply(f.ctx, "parent", f.parentDir, "../escape", "op-probe")
	assertTraversalRefused(t, f, err, victim, victimHash, escapePath, planted)
	if got := hashTree(t, f.parentDir); got != parentHash {
		t.Fatalf("parent walk hash = %s, want %s (a refused Apply touched the parent tree)", got, parentHash)
	}
}

// TestDiscardRefusesTraversalRunScope: Discard reads the
// sidecar at a path built from the run scope and removes that
// path after its mint — the planted file must survive both
// the read and the removal it would have reached.
func TestDiscardRefusesTraversalRunScope(t *testing.T) {
	f := newJournalFixture(t)
	victim := plantTraversalVictim(t, f)
	victimHash := hashTree(t, victim)
	escapePath := filepath.Join(f.root, "escape.json")
	planted := []byte("planted sidecar bytes\n")
	if err := os.MkdirAll(filepath.Dir(escapePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(escapePath, planted, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Discard(f.ctx, "../escape", "op-probe")
	assertTraversalRefused(t, f, err, victim, victimHash, escapePath, planted)
}

// TestForkRefusesTraversalParentScope: Fork's parent scope
// reaches the snapshot store's ref machinery; the refusal
// must land in settle, before the settle root is even
// created. (The run-scope slot's victim geometry is covered
// by TestForkRejectsTraversalRunScopeBeforeFilesystemEffects.)
func TestForkRefusesTraversalParentScope(t *testing.T) {
	f := newJournalFixture(t)
	if _, err := Fork(f.ctx, "../escape", "run-a"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("Fork with traversal parent scope: err = %v, want an invalid-scope refusal", err)
	}
	if _, err := os.Stat(f.root); !os.IsNotExist(err) {
		t.Fatalf("settle root after refused Fork: err = %v, want never created (validation must precede every filesystem effect)", err)
	}
}
