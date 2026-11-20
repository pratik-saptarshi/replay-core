package carrier

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// reflinkDriver returns the registered reflink driver for this
// platform: the real FICLONE driver on Linux.
func reflinkDriver() driver { return ficloneDriver{} }

// ficloneDriver is the reflink carrier: it builds its view with
// the copy driver's staging and layout discipline, except every
// regular file is created by cloning its base file's extents
// with the FICLONE ioctl — never by copying bytes. Any clone
// failure fails the whole materialization and the staging tree
// is removed: there is no byte-copy fallback anywhere in this
// driver (AC-F5-10's no-silent-fallback clause, discharged
// here — the refusal path is exactly what this sandbox
// exercises).
//
// The extent-sharing clause of AC-F5-10 is recorded
// host-deferred, NOT discharged (Decision 7): FICLONE is
// refused (EPERM) on both of this sandbox's filesystems, so no
// test here can observe a shared extent.
type ficloneDriver struct{}

func (ficloneDriver) id() DriverID { return DriverReflink }

func (ficloneDriver) probe(rootDir string) ProbeResult { return probeReflink(rootDir) }

func (ficloneDriver) materialize(baseDir, viewDir string) error {
	return cloneTree(baseDir, viewDir)
}

// remove releases no driver state: a reflink layer is plain
// files, and the shared lifecycle deletes the layer root.
func (ficloneDriver) remove(*Layer) error { return nil }

// cloneFile creates dst as a clone of src with the given mode.
// It is a package-level variable so package-internal tests can
// stub the ioctl: the clone loop's mechanics are testable in
// this sandbox even though FICLONE itself is refused here.
var cloneFile = realCloneFile

// realCloneFile is cloneFile's production form: dst (which must
// not exist) is created with the given mode and src's extents
// are cloned into it with one FICLONE ioctl. A failed clone
// removes the partial destination, so a refused clone leaves no
// file behind for the walk to mistake for a built one. The
// errors are mechanism-only; the walk adds the file's name.
func realCloneFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return fmt.Errorf("create destination %q: %w", dst, err)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ficloneRequest, in.Fd()); errno != 0 {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("FICLONE ioctl: %w", errno)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close destination %q: %w", dst, err)
	}
	if err := os.Chmod(dst, perm); err != nil {
		return fmt.Errorf("set destination mode %q: %w", dst, err)
	}
	return nil
}

// cloneTree builds the view from the base with the copy
// driver's walk discipline — directories recreated with their
// modes, symlinks recreated as symlinks and never followed, any
// other kind (FIFO, socket, device) refused with an error
// naming the path — except regular files, which are cloned via
// cloneFile instead of byte-copied. The first failure of any
// kind aborts the build; the shared lifecycle removes the
// staging tree, so a failed clone leaves no partial layer.
func cloneTree(baseDir, viewDir string) error {
	type dirMode struct {
		path string
		mode os.FileMode
	}
	var dirs []dirMode
	err := filepath.WalkDir(baseDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(baseDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(viewDir, rel)
		switch {
		case info.IsDir():
			// Created permissive so children can be added
			// even under a read-only directory; the exact
			// mode is applied after the walk, deepest last
			// in walk order (parents precede children).
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("carrier: reflink base: create directory %q: %w", target, err)
			}
			dirs = append(dirs, dirMode{target, info.Mode().Perm()})
			return nil
		case info.Mode()&fs.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("carrier: reflink base: read symlink %q: %w", path, err)
			}
			if err := os.Symlink(linkTarget, target); err != nil {
				return fmt.Errorf("carrier: reflink base: recreate symlink %q: %w", target, err)
			}
			return nil
		case info.Mode().IsRegular():
			if err := cloneFile(path, target, info.Mode().Perm()); err != nil {
				return fmt.Errorf("carrier: reflink base: clone %q: %w", path, err)
			}
			return nil
		default:
			return fmt.Errorf("carrier: reflink base %q: unsupported file kind %s", path, info.Mode().Type())
		}
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return fmt.Errorf("carrier: reflink base: set directory mode %q: %w", dirs[i].path, err)
		}
	}
	return nil
}
