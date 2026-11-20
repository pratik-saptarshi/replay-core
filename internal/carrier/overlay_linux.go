package carrier

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// The kernel-overlay driver's state layout in the layer root:
// the merged mountpoint and its two backing directories as
// siblings — upper/ and work/ are never inside the view (plan
// Task 3).
const (
	upperName = "upper"
	workName  = "work"
)

// kernelOverlayDriver returns the registered kernel-overlay
// driver for this platform: the real mount driver on Linux.
func kernelOverlayDriver() driver { return overlayDriver{} }

// overlayDriver is the kernel-overlay carrier: the view is an
// overlay mount with the base as its lowerdir, so the base is
// served read-only through the view and every write lands in
// upper/. The driver is filesystem-agnostic — it takes any
// lowerdir — so AC-F5-8 (real workspaces) needs no code change
// on a host whose workspace filesystem accepts overlay mounts;
// in this sandbox it is recorded not discharged (Decision 7).
type overlayDriver struct{}

func (overlayDriver) id() DriverID { return DriverKernelOverlay }

func (overlayDriver) probe(rootDir string) ProbeResult { return probeKernelOverlay(rootDir) }

// materialize is the staged-build form, and it never runs: the
// shared lifecycle routes view-managing drivers through
// materializeView, because a mountpoint cannot be built in a
// staging sibling and renamed into place. It refuses
// descriptively rather than pretending the staged form applies.
func (overlayDriver) materialize(string, string) error {
	return errors.New("carrier: kernel-overlay manages its view directly; staged materialization does not apply")
}

// materializeView builds the layer in place: upper/ and work/
// are created beside the view in the layer root, the view is
// created, and the overlay is mounted onto it. The mount is the
// last step, and a failure at any step removes the directories
// created here — the driver leaves either a mounted, ready
// layer or no trace of the attempt. A mount failure is the
// operation's error: a driver chosen by selection that fails at
// use is never silently replaced by another driver (carrier
// honesty).
func (overlayDriver) materializeView(baseDir, layerRoot, viewDir string) error {
	upper := filepath.Join(layerRoot, upperName)
	work := filepath.Join(layerRoot, workName)
	made := []string{upper, work, viewDir}
	for _, dir := range made {
		if err := os.Mkdir(dir, 0o755); err != nil {
			discardDirs(made)
			return fmt.Errorf("carrier: kernel-overlay: create layer directory %q: %w", dir, err)
		}
	}
	// The merged root presents the upper root's mode; match it
	// to the base root's mode so the view root looks like the
	// base root (parity with the staged path's view chmod).
	info, err := os.Stat(baseDir)
	if err != nil {
		discardDirs(made)
		return fmt.Errorf("carrier: kernel-overlay: stat base %q: %w", baseDir, err)
	}
	if err := os.Chmod(upper, info.Mode().Perm()); err != nil {
		discardDirs(made)
		return fmt.Errorf("carrier: kernel-overlay: set upper mode from base %q: %w", baseDir, err)
	}
	opts := "lowerdir=" + baseDir + ",upperdir=" + upper + ",workdir=" + work
	if err := syscall.Mount("overlay", viewDir, "overlay", 0, opts); err != nil {
		discardDirs(made)
		return fmt.Errorf("carrier: kernel-overlay: overlay mount failed: %w", err)
	}
	return nil
}

// remove unmounts the layer's view; the shared lifecycle then
// deletes the layer root. A view that is not mounted — already
// unmounted, or a hand-built remnant's plain directory — is not
// an error: remove's contract is that no mount remains.
func (overlayDriver) remove(l *Layer) error {
	view := filepath.Join(l.root, viewName)
	if err := unmount(view); err != nil {
		if errors.Is(err, syscall.EINVAL) {
			return nil
		}
		return fmt.Errorf("carrier: kernel-overlay: unmount view %q: %w", view, err)
	}
	return nil
}

// recoverRemnant recognizes the overlay driver's crash remnant
// in a markerless layer root (the F7 remnant law): the mount
// happens inside materializeView, before the shared lifecycle
// writes the marker, so a crash in that window leaves exactly
// upper/, work/, and view/ — all directories, nothing else —
// with the view possibly still mounted. A root in exactly that
// shape is the driver's own: the view is unmounted if a mount
// is still live, the root is wiped, and true is returned so the
// caller rebuilds. Any other shape — a missing directory, an
// extra entry, a non-directory — is not recognized, and false
// leaves it to the foreign-directory law, unwiped.
func (overlayDriver) recoverRemnant(layerRoot string) (bool, error) {
	entries, err := os.ReadDir(layerRoot)
	if err != nil {
		return false, fmt.Errorf("carrier: kernel-overlay: list layer root %q: %w", layerRoot, err)
	}
	if len(entries) != 3 {
		return false, nil
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			return false, nil
		}
		seen[e.Name()] = true
	}
	if !seen[upperName] || !seen[workName] || !seen[viewName] {
		return false, nil
	}
	// Recognized: release a still-live mount first — deleting
	// through it would corrupt the remnant's backing store —
	// then wipe the root for the rebuild.
	if err := (overlayDriver{}).remove(&Layer{Driver: DriverKernelOverlay, root: layerRoot}); err != nil {
		return false, err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(layerRoot, e.Name())); err != nil {
			return false, fmt.Errorf("carrier: kernel-overlay: wipe remnant entry %q in %q: %w", e.Name(), layerRoot, err)
		}
	}
	return true, nil
}

// discardDirs removes directories a failed build created; it is
// best-effort cleanup on an already-failing path.
func discardDirs(dirs []string) {
	for _, dir := range dirs {
		os.RemoveAll(dir)
	}
}
