package carrier_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/carrier"
)

// copySelection is the selection record the lifecycle tests run
// against: the copy driver, as Select would record it on a root
// where only the floor mechanism can serve.
var copySelection = carrier.Selection{Driver: carrier.DriverCopy}

// writeBaseFile creates one file in a fixture tree with exact
// bytes and mode.
func writeBaseFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// makeBaseTree builds the standard fixture base: nested
// directories, two regular files (one executable), and a symlink
// pointing at one of them.
func makeBaseTree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	writeBaseFile(t, filepath.Join(base, "top.txt"), "top level\n", 0o644)
	writeBaseFile(t, filepath.Join(base, "sub", "run.sh"), "#!/bin/sh\necho run\n", 0o755)
	writeBaseFile(t, filepath.Join(base, "sub", "deep", "notes.md"), "notes\n", 0o644)
	if err := os.Symlink("top.txt", filepath.Join(base, "top-link")); err != nil {
		t.Fatalf("symlink in base: %v", err)
	}
	return base
}

// treeEntry is one node's identity in a recursive tree digest:
// its kind, its permission bits, and either its content hash
// (regular file) or its target (symlink).
type treeEntry struct {
	Kind    string
	Mode    os.FileMode
	Content string
	Target  string
}

// treeDigest walks root and records every entry below it, so two
// trees can be compared exactly — paths, kinds, modes, bytes, and
// link targets. Symlinks are recorded, never followed.
func treeDigest(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	digest := map[string]treeEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
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
		entry := treeEntry{Mode: info.Mode().Perm()}
		switch {
		case info.IsDir():
			entry.Kind = "dir"
		case info.Mode()&fs.ModeSymlink != 0:
			entry.Kind = "symlink"
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entry.Target = target
		case info.Mode().IsRegular():
			entry.Kind = "file"
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry.Content = fmt.Sprintf("%x", sha256.Sum256(data))
		default:
			entry.Kind = "other:" + info.Mode().Type().String()
		}
		digest[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return digest
}

// assertTreeEqual fails the test unless the two digests are
// identical entry for entry.
func assertTreeEqual(t *testing.T, what string, want, got map[string]treeEntry) {
	t.Helper()
	for rel, w := range want {
		g, ok := got[rel]
		if !ok {
			t.Errorf("%s: %s missing", what, rel)
			continue
		}
		if g != w {
			t.Errorf("%s: %s = %+v, want %+v", what, rel, g, w)
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("%s: unexpected entry %s", what, rel)
		}
	}
}

// TestCopyLayerMaterializesBaseByteIdentical is AC-F5-5 through
// the copy driver: the layer serves exactly the base — paths,
// bytes, modes, the symlink as a symlink — and writes inside the
// layer never reach the base.
func TestCopyLayerMaterializesBaseByteIdentical(t *testing.T) {
	base := makeBaseTree(t)
	before := treeDigest(t, base)
	root := filepath.Join(t.TempDir(), "layer")

	layer, err := carrier.Materialize(copySelection, base, "tree-v1", root)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if layer.Driver != carrier.DriverCopy {
		t.Errorf("layer driver = %q, want %q", layer.Driver, carrier.DriverCopy)
	}
	if layer.BaseID != "tree-v1" {
		t.Errorf("layer base ID = %q, want %q", layer.BaseID, "tree-v1")
	}
	assertTreeEqual(t, "layer view vs base", before, treeDigest(t, layer.Dir))

	// Writes in the layer — a new file and an edit of an
	// existing one — must leave the base byte-identical.
	writeBaseFile(t, filepath.Join(layer.Dir, "fresh.txt"), "fresh\n", 0o644)
	writeBaseFile(t, filepath.Join(layer.Dir, "top.txt"), "edited\n", 0o644)
	assertTreeEqual(t, "base after layer writes", before, treeDigest(t, base))
}

// TestCopyLayerRejectsSpecialFiles pins the hostile-input leg of
// the copy walk: a base containing a FIFO — a tree the snapshot
// store could never have produced — fails the materialization
// with an error naming the path, and no layer root is left
// behind.
func TestCopyLayerRejectsSpecialFiles(t *testing.T) {
	base := makeBaseTree(t)
	fifo := filepath.Join(base, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	root := filepath.Join(t.TempDir(), "layer")

	_, err := carrier.Materialize(copySelection, base, "tree-v1", root)
	if err == nil {
		t.Fatal("Materialize over a base containing a FIFO: err = nil, want refusal")
	}
	if !strings.Contains(err.Error(), fifo) {
		t.Errorf("refusal error = %q, want it to name the FIFO path %q", err, fifo)
	}
	if _, serr := os.Stat(root); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("layer root left behind after refused materialization (stat err = %v)", serr)
	}
}
