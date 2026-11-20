//go:build linux

package carrier_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/carrier"
)

// overlaySelection is the selection record the overlay tests run
// against: a forced kernel-overlay selection, as Select records
// it on a root where the overlay probe passes.
var overlaySelection = carrier.Selection{Driver: carrier.DriverKernelOverlay}

// assertNotMounted fails the test if path is still a mountpoint:
// a leaked mount outlives the test process in the shared mount
// namespace and accumulates across the suite.
func assertNotMounted(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Fatalf("read /proc/self/mounts: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			t.Errorf("%s is still mounted after the test (leaked mount): %s", path, line)
		}
	}
}

// TestOverlayMaterializeOnBtrfsRefusedNotReplaced pins the
// carrier-honesty law at use time: a forced overlay selection
// on a btrfs root (the working directory's filesystem —
// baseline §2, where overlay mounts fail) is refused with the
// mount failure as the error and NO partial layer left behind;
// it is never silently replaced by another driver. Selection
// on the same root still lands on copy, which is where
// fall-through legitimately happens — at selection time.
func TestOverlayMaterializeOnBtrfsRefusedNotReplaced(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	parent, err := os.MkdirTemp(wd, ".carrier-overlay-btrfs-")
	if err != nil {
		t.Fatalf("create btrfs scratch: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(parent) })
	root := filepath.Join(parent, "layer")
	guardViewMount(t, root)
	base := makeOverlayBase(t)

	_, err = carrier.Materialize(overlaySelection, base, "tree-v1", root)
	if err == nil {
		t.Fatal("Materialize with a forced overlay selection on btrfs: err = nil, want the mount refusal")
	}
	if !strings.Contains(err.Error(), "mount") {
		t.Errorf("refusal error = %q, want it to name the mount failure", err)
	}
	if _, serr := os.Stat(root); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("partial layer root left behind after refused materialization (stat err = %v)", serr)
	}

	sel, err := carrier.Select(parent)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Driver != carrier.DriverCopy {
		t.Errorf("Select on the same root = %q, want %q (fall-through happens at selection time)", sel.Driver, carrier.DriverCopy)
	}
}

// TestOverlayMaterializeRecoversCrashRemnant is the F7
// amendment's law (timing shape): a crash between the overlay
// mount and the marker write leaves a markerless root in the
// driver's own remnant shape — upper/, work/, view/, nothing
// else. Materialize recognizes exactly that shape as its own,
// unmounts the view if a mount is still live, wipes the root,
// and rebuilds. A markerless root in any other shape stays
// foreign.
func TestOverlayMaterializeRecoversCrashRemnant(t *testing.T) {
	base := makeOverlayBase(t)

	// Leg 1: the plain remnant. Debris in upper/ proves the
	// wipe: crash leftovers must not survive into the rebuilt
	// layer.
	root := filepath.Join(tmpfsScratch(t), "layer")
	guardViewMount(t, root)
	for _, dir := range []string{"upper", "work", "view"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("build remnant: %v", err)
		}
	}
	writeBaseFile(t, filepath.Join(root, "upper", "debris.txt"), "debris\n", 0o644)

	layer, err := carrier.Materialize(overlaySelection, base, "tree-v1", root)
	if err != nil {
		t.Fatalf("Materialize over crash remnant: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(layer.Dir, "top.txt"))
	if err != nil || string(data) != "top level\n" {
		t.Errorf("rebuilt layer does not serve the base: data = %q, err = %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "upper", "debris.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("remnant debris survived recovery (stat err = %v)", err)
	}
	if err := layer.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertNotMounted(t, filepath.Join(root, "view"))

	// Leg 2: the remnant's view is still a live mount (the
	// crash did not take the mount down). Recovery unmounts
	// it, wipes, and rebuilds — the stale mount's content is
	// never served.
	root2 := filepath.Join(tmpfsScratch(t), "layer")
	guardViewMount(t, root2)
	for _, dir := range []string{"upper", "work", "view"} {
		if err := os.MkdirAll(filepath.Join(root2, dir), 0o755); err != nil {
			t.Fatalf("build mounted remnant: %v", err)
		}
	}
	lower := tmpfsScratch(t)
	writeBaseFile(t, filepath.Join(lower, "stale.txt"), "stale\n", 0o644)
	opts := "lowerdir=" + lower + ",upperdir=" + filepath.Join(root2, "upper") + ",workdir=" + filepath.Join(root2, "work")
	if err := syscall.Mount("overlay", filepath.Join(root2, "view"), "overlay", 0, opts); err != nil {
		t.Fatalf("stage the remnant's stale mount: %v", err)
	}
	layer2, err := carrier.Materialize(overlaySelection, base, "tree-v1", root2)
	if err != nil {
		t.Fatalf("Materialize over mounted remnant: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layer2.Dir, "stale.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale mount's content served after recovery (stat err = %v)", err)
	}
	data, err = os.ReadFile(filepath.Join(layer2.Dir, "top.txt"))
	if err != nil || string(data) != "top level\n" {
		t.Errorf("rebuilt layer does not serve the base: data = %q, err = %v", data, err)
	}
	if err := layer2.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertNotMounted(t, filepath.Join(root2, "view"))

	// Leg 3: an UNRECOGNIZED markerless shape — upper/ plus a
	// stray file — is not the driver's remnant: refused as
	// foreign, and left exactly as found.
	root3 := filepath.Join(tmpfsScratch(t), "layer")
	if err := os.MkdirAll(filepath.Join(root3, "upper"), 0o755); err != nil {
		t.Fatalf("build unrecognized root: %v", err)
	}
	writeBaseFile(t, filepath.Join(root3, "stray.txt"), "stray\n", 0o644)
	if _, err := carrier.Materialize(overlaySelection, base, "tree-v1", root3); !errors.Is(err, carrier.ErrForeignLayerDir) {
		t.Errorf("Materialize over unrecognized markerless root: err = %v, want ErrForeignLayerDir", err)
	}
	data, err = os.ReadFile(filepath.Join(root3, "stray.txt"))
	if err != nil || string(data) != "stray\n" {
		t.Errorf("unrecognized root disturbed: data = %q, err = %v", data, err)
	}
}

// TestOverlayLayerStaleWipeOnBaseChange is AC-F5-3 through the
// overlay driver, with the first layer still mounted when the
// rebuild begins: the previous mount is released and the root
// wiped before the new mount — never merged, never deleted
// through — so the rebuilt view serves exactly the new base
// and nothing written through the old view survives.
func TestOverlayLayerStaleWipeOnBaseChange(t *testing.T) {
	base := makeOverlayBase(t)
	root := filepath.Join(tmpfsScratch(t), "layer")
	guardViewMount(t, root)

	first, err := carrier.Materialize(overlaySelection, base, "tree-v1", root)
	if err != nil {
		t.Fatalf("Materialize v1: %v", err)
	}
	writeBaseFile(t, filepath.Join(first.Dir, "work.txt"), "work\n", 0o644)

	// Change the base: delete one file, add one, modify one.
	if err := os.Remove(filepath.Join(base, "sub", "deep", "notes.md")); err != nil {
		t.Fatalf("delete from base: %v", err)
	}
	writeBaseFile(t, filepath.Join(base, "added.txt"), "added\n", 0o644)
	writeBaseFile(t, filepath.Join(base, "top.txt"), "v2 bytes\n", 0o644)

	layer, err := carrier.Materialize(overlaySelection, base, "tree-v2", root)
	if err != nil {
		t.Fatalf("Materialize v2 over a live overlay layer: %v", err)
	}
	assertTreeEqual(t, "rebuilt overlay layer vs v2 base", treeDigest(t, base), treeDigest(t, layer.Dir))
	if _, err := os.Stat(filepath.Join(layer.Dir, "work.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file written through the v1 view survived the stale wipe (stat err = %v)", err)
	}
	if err := layer.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertNotMounted(t, filepath.Join(root, "view"))
}

// TestCopyRebuildReleasesOverlayMount pins the staged path's
// half of the release law: rebuilding a root that holds a live
// overlay layer with a different driver releases the overlay
// mount before the wipe, so the copy build never deletes
// through a mount.
func TestCopyRebuildReleasesOverlayMount(t *testing.T) {
	base := makeOverlayBase(t)
	root := filepath.Join(tmpfsScratch(t), "layer")
	guardViewMount(t, root)

	if _, err := carrier.Materialize(overlaySelection, base, "tree-v1", root); err != nil {
		t.Fatalf("Materialize overlay: %v", err)
	}
	layer, err := carrier.Materialize(copySelection, base, "tree-v2", root)
	if err != nil {
		t.Fatalf("Materialize copy over a live overlay layer: %v", err)
	}
	if layer.Driver != carrier.DriverCopy {
		t.Errorf("layer driver = %q, want %q", layer.Driver, carrier.DriverCopy)
	}
	assertTreeEqual(t, "copy layer vs base", treeDigest(t, base), treeDigest(t, layer.Dir))
	assertNotMounted(t, filepath.Join(root, "view"))
	if err := layer.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

// guardViewMount registers a best-effort lazy unmount of the
// layer's view, so a test that fails before Remove cannot leak
// its mount into the rest of the suite.
func guardViewMount(t *testing.T, root string) {
	t.Helper()
	view := filepath.Join(root, "view")
	t.Cleanup(func() { syscall.Unmount(view, syscall.MNT_DETACH) })
}

// tmpfsScratch returns a scratch directory on the tmpfs
// (os.TempDir() — baseline §1). Filesystem placement is
// explicit here, per the plan's Global Constraints: in this
// sandbox t.TempDir() resolves under the go work tree on the
// btrfs workspace filesystem, where overlay mounts are refused,
// so a fixture that means tmpfs must build it from
// os.MkdirTemp("", ...) exactly as Task 1's probe tests do.
func tmpfsScratch(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", ".carrier-overlay-")
	if err != nil {
		t.Fatalf("create tmpfs scratch: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// makeOverlayBase builds the standard fixture base (the
// makeBaseTree shape) on the tmpfs, so the whole overlay
// fixture — lower, upper, work, view — sits on the filesystem
// whose overlay verdict the baseline verified.
func makeOverlayBase(t *testing.T) string {
	t.Helper()
	base := tmpfsScratch(t)
	writeBaseFile(t, filepath.Join(base, "top.txt"), "top level\n", 0o644)
	writeBaseFile(t, filepath.Join(base, "sub", "run.sh"), "#!/bin/sh\necho run\n", 0o755)
	writeBaseFile(t, filepath.Join(base, "sub", "deep", "notes.md"), "notes\n", 0o644)
	if err := os.Symlink("top.txt", filepath.Join(base, "top-link")); err != nil {
		t.Fatalf("symlink in base: %v", err)
	}
	return base
}

// TestOverlayLayerOnTmpfs is AC-F5-7 at scratch grade: a real
// overlay mount on the sandbox's tmpfs serves the base through
// the merged view, writes land in the upper directory (never in
// the base), edits copy up, and Remove unmounts and deletes the
// layer root. There is deliberately NO t.Skip on mount failure:
// the baseline verified overlay works on this sandbox's tmpfs,
// so a mount failure here is a finding, not an excuse.
func TestOverlayLayerOnTmpfs(t *testing.T) {
	base := makeOverlayBase(t)
	before := treeDigest(t, base)
	root := filepath.Join(tmpfsScratch(t), "layer")
	guardViewMount(t, root)

	layer, err := carrier.Materialize(overlaySelection, base, "tree-v1", root)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if layer.Driver != carrier.DriverKernelOverlay {
		t.Errorf("layer driver = %q, want %q", layer.Driver, carrier.DriverKernelOverlay)
	}
	if layer.BaseID != "tree-v1" {
		t.Errorf("layer base ID = %q, want %q", layer.BaseID, "tree-v1")
	}

	// The base is visible through the merged view: a regular
	// file's bytes and a symlink presented as a symlink.
	data, err := os.ReadFile(filepath.Join(layer.Dir, "top.txt"))
	if err != nil || string(data) != "top level\n" {
		t.Errorf("base file through overlay view: data = %q, err = %v", data, err)
	}
	target, err := os.Readlink(filepath.Join(layer.Dir, "top-link"))
	if err != nil || target != "top.txt" {
		t.Errorf("base symlink through overlay view: target = %q, err = %v", target, err)
	}

	// A new file written through the view lands in upper/ and
	// never in the base.
	writeBaseFile(t, filepath.Join(layer.Dir, "fresh.txt"), "fresh\n", 0o644)
	if _, err := os.Stat(filepath.Join(root, "upper", "fresh.txt")); err != nil {
		t.Errorf("file written through the view not found in upper/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "fresh.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("write through the overlay reached the base (stat err = %v)", err)
	}

	// Editing a base file through the view copies it up: the
	// view serves the edit, upper/ holds it, the base bytes are
	// unchanged.
	writeBaseFile(t, filepath.Join(layer.Dir, "top.txt"), "edited\n", 0o644)
	if _, err := os.Stat(filepath.Join(root, "upper", "top.txt")); err != nil {
		t.Errorf("edited file not copied up into upper/: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(base, "top.txt"))
	if err != nil || string(data) != "top level\n" {
		t.Errorf("base file changed through the overlay: data = %q, err = %v", data, err)
	}

	// Remove unmounts the view and deletes the layer root; the
	// base is byte-identical and no mount leaks.
	if err := layer.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("layer root still exists after Remove (stat err = %v)", err)
	}
	assertTreeEqual(t, "base after overlay lifecycle", before, treeDigest(t, base))
	assertNotMounted(t, filepath.Join(root, "view"))
}
