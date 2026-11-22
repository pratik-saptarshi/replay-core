package carrier

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// copyDriver is the floor carrier: it byte-copies the base into
// the view. It serves any writable root, which is why its probe
// is a scratch-file test.
type copyDriver struct{}

func (copyDriver) id() DriverID { return DriverCopy }

func (copyDriver) probe(rootDir string) ProbeResult { return probeCopy(rootDir) }

func (copyDriver) materialize(baseDir, viewDir string) error {
	return copyTree(baseDir, viewDir)
}

// remove releases no driver state: a copy layer is plain files,
// and the shared lifecycle deletes the layer root.
func (copyDriver) remove(*Layer) error { return nil }

// copyTree copies the base tree into viewDir: directories
// recreated with their modes, regular files byte-copied with
// modes preserved, symlinks recreated as symlinks and never
// followed. Any other kind (FIFO, socket, device) is refused
// with an error naming the path — a base the snapshot store
// could never have produced is hostile input, not a layer.
func copyTree(baseDir, viewDir string) error {
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
				return fmt.Errorf("carrier: copy base: create directory %q: %w", target, err)
			}
			dirs = append(dirs, dirMode{target, info.Mode().Perm()})
			return nil
		case info.Mode()&fs.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("carrier: copy base: read symlink %q: %w", path, err)
			}
			if err := os.Symlink(linkTarget, target); err != nil {
				return fmt.Errorf("carrier: copy base: recreate symlink %q: %w", target, err)
			}
			return nil
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("carrier: copy base %q: unsupported file kind %s", path, info.Mode().Type())
		}
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return fmt.Errorf("carrier: copy base: set directory mode %q: %w", dirs[i].path, err)
		}
	}
	return nil
}

// copyFile byte-copies one regular file, preserving its mode
// exactly (the create mode is umask-masked, so chmod follows).
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("carrier: copy base: open %q: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return fmt.Errorf("carrier: copy base: create %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("carrier: copy base: copy %q: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("carrier: copy base: close %q: %w", dst, err)
	}
	if err := os.Chmod(dst, perm); err != nil {
		return fmt.Errorf("carrier: copy base: set file mode %q: %w", dst, err)
	}
	return nil
}
