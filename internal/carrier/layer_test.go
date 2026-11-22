package carrier_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/carrier"
)

// TestCopyLayerStaleWipeOnBaseChange is AC-F5-3: when the base
// identity changes, the layer is wiped and rebuilt — never
// merged — so the view serves exactly the new base.
func TestCopyLayerStaleWipeOnBaseChange(t *testing.T) {
	base := makeBaseTree(t)
	root := filepath.Join(t.TempDir(), "layer")

	if _, err := carrier.Materialize(copySelection, base, "tree-v1", root); err != nil {
		t.Fatalf("Materialize v1: %v", err)
	}

	// Change the base: delete one file, add one, modify one.
	if err := os.Remove(filepath.Join(base, "sub", "deep", "notes.md")); err != nil {
		t.Fatalf("delete from base: %v", err)
	}
	writeBaseFile(t, filepath.Join(base, "added.txt"), "added\n", 0o644)
	writeBaseFile(t, filepath.Join(base, "top.txt"), "v2 bytes\n", 0o644)

	layer, err := carrier.Materialize(copySelection, base, "tree-v2", root)
	if err != nil {
		t.Fatalf("Materialize v2: %v", err)
	}
	got := treeDigest(t, layer.Dir)
	if _, ok := got[filepath.Join("sub", "deep", "notes.md")]; ok {
		t.Errorf("deleted base file still served after stale wipe (merged, not wiped)")
	}
	if _, ok := got["added.txt"]; !ok {
		t.Errorf("added base file not served after rebuild")
	}
	assertTreeEqual(t, "rebuilt layer vs v2 base", treeDigest(t, base), got)
}

// TestLayerLifecycleLeavesBaseByteIdentical is AC-F5-4: a full
// create → write → remove lifecycle leaves the base untouched
// and the layer root gone entirely, staging remnants included.
func TestLayerLifecycleLeavesBaseByteIdentical(t *testing.T) {
	base := makeBaseTree(t)
	before := treeDigest(t, base)
	parent := t.TempDir()
	root := filepath.Join(parent, "layer")

	layer, err := carrier.Materialize(copySelection, base, "tree-v1", root)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	writeBaseFile(t, filepath.Join(layer.Dir, "work.txt"), "work\n", 0o644)
	if err := layer.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	assertTreeEqual(t, "base after lifecycle", before, treeDigest(t, base))
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("layer root still exists after Remove (stat err = %v)", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read layer parent: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("layer parent holds %d entries after Remove, want 0 (no staging remnants)", len(entries))
	}
}

// TestMaterializeSameBaseIsIdempotent pins the reuse leg of the
// marker law (handle shape: a second handle on the same layer):
// re-materializing the same (base, baseID, driver) returns the
// layer as-is, contents preserved — no rebuild.
func TestMaterializeSameBaseIsIdempotent(t *testing.T) {
	base := makeBaseTree(t)
	root := filepath.Join(t.TempDir(), "layer")

	first, err := carrier.Materialize(copySelection, base, "tree-v1", root)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	writeBaseFile(t, filepath.Join(first.Dir, "kept.txt"), "kept\n", 0o644)

	second, err := carrier.Materialize(copySelection, base, "tree-v1", root)
	if err != nil {
		t.Fatalf("re-Materialize: %v", err)
	}
	if second.Dir != first.Dir {
		t.Errorf("second handle Dir = %q, want %q", second.Dir, first.Dir)
	}
	if _, err := os.Stat(filepath.Join(second.Dir, "kept.txt")); err != nil {
		t.Errorf("file written through the first handle lost on re-materialize: %v", err)
	}
}

// TestMaterializeRefusesForeignDirectory is the Review Focus
// leg: a markerless non-empty layer root is refused, never
// wiped; a corrupt marker is an error, never a guess — and in
// both cases the directory is left exactly as found.
func TestMaterializeRefusesForeignDirectory(t *testing.T) {
	base := makeBaseTree(t)

	// Foreign: a file and no marker.
	root := filepath.Join(t.TempDir(), "layer")
	writeBaseFile(t, filepath.Join(root, "precious.txt"), "precious\n", 0o644)
	if _, err := carrier.Materialize(copySelection, base, "tree-v1", root); !errors.Is(err, carrier.ErrForeignLayerDir) {
		t.Errorf("Materialize over foreign directory: err = %v, want ErrForeignLayerDir", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "precious.txt"))
	if err != nil || string(data) != "precious\n" {
		t.Errorf("foreign file disturbed: data = %q, err = %v", data, err)
	}

	// Corrupt marker: garbage bytes where layer.json belongs.
	root2 := filepath.Join(t.TempDir(), "layer")
	writeBaseFile(t, filepath.Join(root2, "layer.json"), "this is not json\n", 0o644)
	writeBaseFile(t, filepath.Join(root2, "other.txt"), "other\n", 0o644)
	if _, err := carrier.Materialize(copySelection, base, "tree-v1", root2); err == nil {
		t.Errorf("Materialize over corrupt marker: err = nil, want a descriptive error")
	}
	data, err = os.ReadFile(filepath.Join(root2, "other.txt"))
	if err != nil || string(data) != "other\n" {
		t.Errorf("directory with corrupt marker disturbed: data = %q, err = %v", data, err)
	}
	marker, err := os.ReadFile(filepath.Join(root2, "layer.json"))
	if err != nil || string(marker) != "this is not json\n" {
		t.Errorf("corrupt marker rewritten: data = %q, err = %v", marker, err)
	}
}
