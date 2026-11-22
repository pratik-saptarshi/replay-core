package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceEntry is one file of a snapshot commit's workspace/
// subtree: its blob ID, its git tree mode, and whether it is a
// symlink. Mode is the git tree mode (100644, 100755, 120000):
// a mode-only change is a real change in every delta, union, and
// verify computation the settlement layer performs over these
// maps — never invisible (plan Task 6 amendment F2).
type WorkspaceEntry struct {
	Blob    string
	Mode    string
	Symlink bool
}

// WorkspaceTreeID returns the tree ID of commitID's workspace/
// subtree — the content identity a carrier layer is keyed on
// (plan Decision 8(b)). A commit with no workspace subtree is a
// descriptive error: every snapshot commit carries one, so its
// absence names a foreign commit.
func (s *Store) WorkspaceTreeID(commitID string) (string, error) {
	treeID, err := s.git(nil, "rev-parse", "--verify", "--quiet", commitID+":workspace")
	if err != nil {
		return "", fmt.Errorf("snapshot: commit %s has no workspace subtree: %w", commitID, err)
	}
	return treeID, nil
}

// WorkspaceFiles returns every path under commitID's workspace/
// subtree, relative to it, with each file's blob ID, git tree
// mode, and symlink flag — read from the tree itself via
// ls-tree, so the map is git's own record of the commit. This
// map is the single currency of the settlement layer's delta,
// union, and verify computations (plan Task 6).
func (s *Store) WorkspaceFiles(commitID string) (map[string]WorkspaceEntry, error) {
	out, err := s.git(nil, "ls-tree", "-r", "-z", commitID+":workspace")
	if err != nil {
		return nil, fmt.Errorf("snapshot: list workspace of %s: %w", commitID, err)
	}
	files := map[string]WorkspaceEntry{}
	for _, line := range strings.Split(out, "\x00") {
		if line == "" {
			continue
		}
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("snapshot: list workspace of %s: malformed ls-tree line %q", commitID, line)
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("snapshot: list workspace of %s: malformed ls-tree line %q", commitID, line)
		}
		files[path] = WorkspaceEntry{
			Blob:    fields[2],
			Mode:    fields[0],
			Symlink: fields[0] == "120000",
		}
	}
	return files, nil
}

// ExtractWorkspace materializes commitID's workspace/ subtree
// into dir: files byte-for-byte, symlinks as symlinks, executable
// modes preserved. dir must be absent or empty — extraction is a
// birth, not a merge, so a non-empty target is refused rather
// than written over. The extraction runs through the package's
// plumbing discipline: the workspace tree is read into a
// temporary index and checked out from it, so no real index and
// no other tree is touched.
func (s *Store) ExtractWorkspace(commitID, dir string) error {
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("snapshot: extract workspace of %s: %s is not a directory", commitID, dir)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("snapshot: extract workspace of %s: read %s: %w", commitID, dir, err)
		}
		if len(entries) > 0 {
			return fmt.Errorf("snapshot: extract workspace of %s: %s is not empty", commitID, dir)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("snapshot: extract workspace of %s: stat %s: %w", commitID, dir, err)
	}
	treeID, err := s.WorkspaceTreeID(commitID)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("snapshot: extract workspace of %s: resolve %s: %w", commitID, dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("snapshot: extract workspace of %s: create %s: %w", commitID, abs, err)
	}

	idxFile, err := os.CreateTemp("", "replay-core-extract-index-*")
	if err != nil {
		return fmt.Errorf("snapshot: extract workspace of %s: create temp index: %w", commitID, err)
	}
	idxPath := idxFile.Name()
	_ = idxFile.Close()
	_ = os.Remove(idxPath) // git creates the index fresh at this path
	defer func() { _ = os.Remove(idxPath) }()
	idxEnv := []string{"GIT_INDEX_FILE=" + idxPath}

	if _, err := s.gitEnv(nil, idxEnv, "read-tree", treeID); err != nil {
		return fmt.Errorf("snapshot: extract workspace of %s: read-tree: %w", commitID, err)
	}
	// checkout-index is a work-tree operation and the store's
	// repository is bare, so the extraction target is named as
	// the command's work tree: the index's entries are then
	// written there directly, modes and symlinks intact.
	if _, err := s.gitEnv(nil, idxEnv, "--work-tree="+abs, "checkout-index", "-a", "-f"); err != nil {
		return fmt.Errorf("snapshot: extract workspace of %s: checkout-index: %w", commitID, err)
	}
	// checkout-index creates files under the process umask, so
	// an executable can land as 0770 under a restrictive umask —
	// its git mode (100755) is part of the tree's identity, so
	// regular files are chmodded to exactly the recorded mode.
	files, err := s.WorkspaceFiles(commitID)
	if err != nil {
		return err
	}
	for rel, entry := range files {
		if entry.Symlink {
			continue
		}
		want := os.FileMode(0o644)
		if entry.Mode == "100755" {
			want = 0o755
		}
		path := filepath.Join(abs, filepath.FromSlash(rel))
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("snapshot: extract workspace of %s: stat %s: %w", commitID, path, err)
		}
		if info.Mode().Perm() != want {
			if err := os.Chmod(path, want); err != nil {
				return fmt.Errorf("snapshot: extract workspace of %s: chmod %s: %w", commitID, path, err)
			}
		}
	}
	return nil
}
