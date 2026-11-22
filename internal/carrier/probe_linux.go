package carrier

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ficloneRequest is the FICLONE ioctl request number: clone a
// whole file's extents where the filesystem allows it.
const ficloneRequest = 0x40049409

// probeKernelOverlay is the kernel-overlay driver's probe: it
// builds a scratch overlay under the root (lower/upper/work/
// merged plus one lower file), mounts it, confirms the lower
// file is visible through the merged view, and unmounts. The
// verdict is about this root's filesystem, measured live on
// every call. Scratch is removed on every path.
func probeKernelOverlay(rootDir string) ProbeResult {
	pr := ProbeResult{Driver: DriverKernelOverlay}
	scratch, err := os.MkdirTemp(rootDir, ".carrier-overlay-probe-")
	if err != nil {
		pr.Reason = fmt.Sprintf("kernel-overlay: create probe scratch in root: %v", err)
		return pr
	}
	defer os.RemoveAll(scratch)
	lower := filepath.Join(scratch, "lower")
	upper := filepath.Join(scratch, "upper")
	work := filepath.Join(scratch, "work")
	merged := filepath.Join(scratch, "merged")
	for _, dir := range []string{lower, upper, work, merged} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			pr.Reason = fmt.Sprintf("kernel-overlay: create probe directory: %v", err)
			return pr
		}
	}
	if err := os.WriteFile(filepath.Join(lower, "probe.txt"), []byte("carrier probe\n"), 0o644); err != nil {
		pr.Reason = fmt.Sprintf("kernel-overlay: write probe file: %v", err)
		return pr
	}
	opts := "lowerdir=" + lower + ",upperdir=" + upper + ",workdir=" + work
	if err := syscall.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		pr.Reason = fmt.Sprintf("kernel-overlay: overlay mount failed: %v", err)
		return pr
	}
	_, statErr := os.Stat(filepath.Join(merged, "probe.txt"))
	unmountErr := unmount(merged)
	if statErr != nil {
		pr.Reason = fmt.Sprintf("kernel-overlay: lower file not visible through merged view: %v", statErr)
		return pr
	}
	if unmountErr != nil {
		pr.Reason = fmt.Sprintf("kernel-overlay: overlay unmount failed: %v", unmountErr)
		return pr
	}
	pr.Available = true
	return pr
}

// unmount detaches a probe mount, falling back to a lazy detach
// if the plain unmount is refused.
func unmount(target string) error {
	if err := syscall.Unmount(target, 0); err != nil {
		return syscall.Unmount(target, syscall.MNT_DETACH)
	}
	return nil
}

// probeReflink is the reflink driver's probe: it creates two
// scratch files under the root and attempts one FICLONE. A
// refusal (EPERM on both of this sandbox's filesystems) is the
// honest verdict, recorded with the mechanism and the error
// named. Scratch is removed on every path.
func probeReflink(rootDir string) ProbeResult {
	pr := ProbeResult{Driver: DriverReflink}
	scratch, err := os.MkdirTemp(rootDir, ".carrier-reflink-probe-")
	if err != nil {
		pr.Reason = fmt.Sprintf("reflink: create probe scratch in root: %v", err)
		return pr
	}
	defer os.RemoveAll(scratch)
	src, err := os.Create(filepath.Join(scratch, "src"))
	if err != nil {
		pr.Reason = fmt.Sprintf("reflink: create probe source: %v", err)
		return pr
	}
	defer src.Close()
	if _, err := src.WriteString("carrier probe\n"); err != nil {
		pr.Reason = fmt.Sprintf("reflink: write probe source: %v", err)
		return pr
	}
	dst, err := os.Create(filepath.Join(scratch, "dst"))
	if err != nil {
		pr.Reason = fmt.Sprintf("reflink: create probe destination: %v", err)
		return pr
	}
	defer dst.Close()
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficloneRequest, src.Fd()); errno != 0 {
		pr.Reason = fmt.Sprintf("reflink: FICLONE clone failed: %v", errno)
		return pr
	}
	pr.Available = true
	return pr
}
