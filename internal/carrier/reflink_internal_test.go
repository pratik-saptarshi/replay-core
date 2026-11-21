//go:build linux

package carrier

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The stubbed-clone loop tests live in the internal package
// because the seam they drive — the package-level cloneFile
// variable — is unexported by design (the API guard pins it
// that way). FICLONE itself is refused on both of this
// sandbox's filesystems, so the loop's mechanics are exercised
// here against a stub that copies bytes, while the real ioctl
// path is covered by the external refusal tests. Nothing here
// discharges AC-F5-10's extent-sharing clause: it is
// host-deferred, NOT discharged (Decision 7).

// writeReflinkFixtureFile creates one file in a fixture tree
// with exact bytes and mode.
func writeReflinkFixtureFile(t *testing.T, path, content string, mode os.FileMode) {
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

// stubCloneFunc returns a cloneFile replacement that records
// every call in order and copies bytes, as the loop requires of
// the mechanism. When failOn names a source path, the call for
// that source fails with failErr instead — the loop must then
// abort the whole materialization, never fall back.
func stubCloneFunc(calls *[]string, failOn string, failErr error) func(string, string, os.FileMode) error {
	return func(src, dst string, perm os.FileMode) error {
		*calls = append(*calls, src)
		if src == failOn {
			return failErr
		}
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		return os.Chmod(dst, perm)
	}
}

// installCloneStub swaps cloneFile for the test's duration.
func installCloneStub(t *testing.T, fn func(string, string, os.FileMode) error) {
	t.Helper()
	saved := cloneFile
	cloneFile = fn
	t.Cleanup(func() { cloneFile = saved })
}

// assertClonedTreeEqual fails the test unless the layer tree at
// got serves exactly the base tree at want: every entry present
// with the same kind and mode, regular files byte-identical,
// symlinks pointing at the same targets, and nothing extra.
func assertClonedTreeEqual(t *testing.T, want, got string) {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir(want, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(want, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		seen[rel] = true
		target := filepath.Join(got, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		gotInfo, err := os.Lstat(target)
		if err != nil {
			t.Errorf("layer is missing %q: %v", rel, err)
			return nil
		}
		if gotInfo.Mode()&fs.ModeSymlink != info.Mode()&fs.ModeSymlink {
			t.Errorf("layer entry %q kind differs (symlink-ness): base %v, layer %v", rel, info.Mode(), gotInfo.Mode())
			return nil
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			wantTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			gotTarget, err := os.Readlink(target)
			if err != nil || gotTarget != wantTarget {
				t.Errorf("layer symlink %q target = %q (err %v), want %q", rel, gotTarget, err, wantTarget)
			}
		case info.Mode().IsRegular():
			wantBytes, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			gotBytes, err := os.ReadFile(target)
			if err != nil || string(gotBytes) != string(wantBytes) {
				t.Errorf("layer file %q bytes = %q (err %v), want %q byte-identical", rel, gotBytes, err, wantBytes)
			}
			if gotInfo.Mode().Perm() != info.Mode().Perm() {
				t.Errorf("layer file %q mode = %v, want %v", rel, gotInfo.Mode().Perm(), info.Mode().Perm())
			}
		case info.IsDir():
			if !gotInfo.IsDir() {
				t.Errorf("layer entry %q is not a directory", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk base %s: %v", want, err)
	}
	err = filepath.WalkDir(got, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(got, path)
		if err != nil {
			return err
		}
		if rel != "." && !seen[rel] {
			t.Errorf("layer has unexpected entry %q", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk layer %s: %v", got, err)
	}
}

// TestReflinkCloneLoopWithStubbedClones pins the clone loop's
// mechanics under a stubbed cloneFile (input/timing shape: the
// mechanism is driven file by file, and its failure handling is
// the law). Success leg: the layer is byte-identical to the
// base and the stub was called exactly once per regular file —
// directories and symlinks are rebuilt by the walk, never
// cloned. Failure leg: the stub fails on the second file, and
// the materialization aborts with an error naming that file —
// the third file is never attempted, and no layer (view,
// marker, staging) remains.
func TestReflinkCloneLoopWithStubbedClones(t *testing.T) {
	// Success leg: a nested base with three regular files
	// (one executable) and a symlink.
	base := t.TempDir()
	writeReflinkFixtureFile(t, filepath.Join(base, "top.txt"), "top level\n", 0o644)
	writeReflinkFixtureFile(t, filepath.Join(base, "sub", "run.sh"), "#!/bin/sh\necho run\n", 0o755)
	writeReflinkFixtureFile(t, filepath.Join(base, "sub", "deep", "notes.md"), "notes\n", 0o644)
	if err := os.Symlink("top.txt", filepath.Join(base, "top-link")); err != nil {
		t.Fatalf("symlink in base: %v", err)
	}

	var calls []string
	installCloneStub(t, stubCloneFunc(&calls, "", nil))

	root := filepath.Join(t.TempDir(), "layer")
	layer, err := Materialize(Selection{Driver: DriverReflink}, base, "tree-v1", root)
	if err != nil {
		t.Fatalf("Materialize with stubbed clones: %v", err)
	}
	if layer.Driver != DriverReflink {
		t.Errorf("layer driver = %q, want %q", layer.Driver, DriverReflink)
	}
	wantFiles := map[string]bool{
		filepath.Join(base, "top.txt"):                 true,
		filepath.Join(base, "sub", "run.sh"):           true,
		filepath.Join(base, "sub", "deep", "notes.md"): true,
	}
	if len(calls) != len(wantFiles) {
		t.Errorf("clone calls = %v, want exactly one per regular file (%d)", calls, len(wantFiles))
	}
	for _, src := range calls {
		if !wantFiles[src] {
			t.Errorf("clone called on %q, which is not a regular file of the base", src)
		}
		delete(wantFiles, src)
	}
	for src := range wantFiles {
		t.Errorf("regular file %q was never cloned", src)
	}
	assertClonedTreeEqual(t, base, layer.Dir)
	if err := layer.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("layer root still exists after Remove (stat err = %v)", err)
	}

	// Failure leg: a flat base of three files; the stub fails
	// on the second (b.txt, in the walk's lexical order).
	flat := t.TempDir()
	writeReflinkFixtureFile(t, filepath.Join(flat, "a.txt"), "a\n", 0o644)
	writeReflinkFixtureFile(t, filepath.Join(flat, "b.txt"), "b\n", 0o644)
	writeReflinkFixtureFile(t, filepath.Join(flat, "c.txt"), "c\n", 0o644)

	calls = nil
	installCloneStub(t, stubCloneFunc(&calls, filepath.Join(flat, "b.txt"), errors.New("stub clone failure")))

	root2 := filepath.Join(t.TempDir(), "layer")
	_, err = Materialize(Selection{Driver: DriverReflink}, flat, "tree-v1", root2)
	if err == nil {
		t.Fatal("Materialize with a failing second clone: err = nil, want the abort (no fallback)")
	}
	if !strings.Contains(err.Error(), filepath.Join(flat, "b.txt")) {
		t.Errorf("abort error = %q, want it to name the failed file %q", err, filepath.Join(flat, "b.txt"))
	}
	if len(calls) != 2 {
		t.Errorf("clone calls before the abort = %v, want exactly the first two files (the third is never attempted)", calls)
	}
	if _, serr := os.Stat(root2); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("partial layer left behind after the aborted materialization (stat err = %v): want no view, marker, or staging remnant", serr)
	}
}
