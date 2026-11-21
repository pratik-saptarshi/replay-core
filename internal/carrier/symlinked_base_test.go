package carrier_test

// Symlinked base root tests (PR #7 wave 6, Codex P2 on
// internal/carrier/layer.go): Materialize's existence check
// follows a symlinked base root (os.Stat), but the copy and
// reflink drivers build the view with filepath.WalkDir, which
// does not follow a symlinked root — it visits the link itself
// as the walk's only entry, and the root callback skips it.
// Materialization therefore succeeded with an EMPTY view and
// wrote the layer marker claiming the base's identity, and the
// marker law handed that empty layer out on every later call
// as if it held the base's content. The kernel-overlay driver
// never showed it (the kernel resolves a lowerdir path
// itself), so one input produced two behaviors across drivers.
// The carrier now resolves the base root once, in Materialize,
// before any driver is dispatched — the root law stated at the
// resolution point in layer.go, the same law the snapshot
// store states for its workspace root. These fixtures pin the
// copy driver's half; the reflink and overlay halves live in
// the linux-tagged files beside this one.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/carrier"
)

// symlinkToBase builds the standard fixture base and returns a
// symlink naming it, alongside the real directory.
func symlinkToBase(t *testing.T) (link, real string) {
	t.Helper()
	real = makeBaseTree(t)
	link = filepath.Join(t.TempDir(), "base-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink to base: %v", err)
	}
	return link, real
}

// TestMaterializeCopySymlinkedBase: a layer materialized from
// a symlinked base root serves the base's content — never an
// empty view under the base's marker — and the idempotent
// re-materialization serves the same content.
func TestMaterializeCopySymlinkedBase(t *testing.T) {
	link, real := symlinkToBase(t)
	root := filepath.Join(t.TempDir(), "layer")

	layer, err := carrier.Materialize(copySelection, link, "tree-v1", root)
	if err != nil {
		t.Fatalf("Materialize through a symlinked base: %v", err)
	}
	// The marker claims the base's identity; the view must
	// hold what the marker claims.
	marker, err := os.ReadFile(filepath.Join(root, "layer.json"))
	if err != nil {
		t.Fatalf("read layer marker: %v", err)
	}
	if !strings.Contains(string(marker), `"tree-v1"`) {
		t.Fatalf("layer marker = %s, want it to name base tree-v1", marker)
	}
	assertTreeEqual(t, "layer view vs base", treeDigest(t, real), treeDigest(t, layer.Dir))

	again, err := carrier.Materialize(copySelection, link, "tree-v1", root)
	if err != nil {
		t.Fatalf("re-Materialize through a symlinked base: %v", err)
	}
	assertTreeEqual(t, "re-materialized layer view vs base", treeDigest(t, real), treeDigest(t, again.Dir))
}

// TestMaterializeCopySymlinkedBaseRelativeLink: the link
// itself may be spelled relatively (a relative path to the
// link, whose target the resolution must still find) —
// resolution, not the caller's spelling, decides what is
// materialized.
func TestMaterializeCopySymlinkedBaseRelativeLink(t *testing.T) {
	link, real := symlinkToBase(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	rel, err := filepath.Rel(wd, link)
	if err != nil {
		t.Fatalf("relativize link path: %v", err)
	}
	root := filepath.Join(t.TempDir(), "layer")
	layer, err := carrier.Materialize(copySelection, rel, "tree-v1", root)
	if err != nil {
		t.Fatalf("Materialize through a relatively-spelled symlinked base: %v", err)
	}
	assertTreeEqual(t, "layer view vs base", treeDigest(t, real), treeDigest(t, layer.Dir))
}
