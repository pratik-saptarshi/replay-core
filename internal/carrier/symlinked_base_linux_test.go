//go:build linux

package carrier_test

// Symlinked base root, kernel-overlay half (PR #7 wave 6):
// the overlay driver never mishandled a symlinked base — the
// kernel resolves the lowerdir path itself at mount time, so
// the merged view served the real tree while the copy and
// reflink walks were building empty layers from the same
// input. That divergence is why the root law is enforced in
// Materialize, once, for every driver: after it, the overlay
// receives the resolved base like every other driver, and
// this fixture pins that the behavior it always had — the
// base served whole through the view — is the behavior that
// survives the unification.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/carrier"
)

// TestMaterializeOverlaySymlinkedBase: an overlay layer whose
// base is named through a symlink serves the base's files
// through the merged view, and a write through the view lands
// in the layer's upper directory, never in the base.
func TestMaterializeOverlaySymlinkedBase(t *testing.T) {
	base := makeOverlayBase(t)
	link := filepath.Join(tmpfsScratch(t), "base-link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatalf("symlink to base: %v", err)
	}
	root := filepath.Join(tmpfsScratch(t), "layer")
	guardViewMount(t, root)

	layer, err := carrier.Materialize(overlaySelection, link, "tree-v1", root)
	if err != nil {
		t.Fatalf("Materialize overlay through a symlinked base: %v", err)
	}
	assertTreeEqual(t, "overlay view vs base", treeDigest(t, base), treeDigest(t, layer.Dir))

	if err := os.WriteFile(filepath.Join(layer.Dir, "fresh.txt"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatalf("write through overlay view: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "fresh.txt")); !os.IsNotExist(err) {
		t.Errorf("write through the view reached the base (stat err = %v)", err)
	}
	if err := layer.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertNotMounted(t, layer.Dir)
}
