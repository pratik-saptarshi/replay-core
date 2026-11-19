// Package snapshot is replay-core's Git-backed snapshot store (F4):
// each snapshot is one commit in a bare repository, holding a dual
// tree — workspace/ (the snapshotted files) and meta/ (the manifest
// and snapshot metadata) — chained by parentage and referenced
// under refs/vcscore/ops/. All Git access goes through the git
// binary's plumbing commands (plan Decision 2), never a shell.
package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// ErrGitUnavailable reports that the git plumbing probe in Open
// failed: the git binary is missing, or it did not reproduce the
// empty-blob hash in this repository. Snapshot stores depend on the
// plumbing for every operation, so Open fails loudly with this
// named error before any caller depends on it (plan Decision 2).
var ErrGitUnavailable = errors.New("snapshot: git plumbing unavailable")

// emptyBlobID is the object ID git assigns the empty blob; the Open
// probe hashes empty input and must get exactly this back.
const emptyBlobID = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"

// Refs layout (plan Task 15 states the layout law; Task 13 writes
// the ops class): one ref per snapshot commit plus the chain head.
const (
	opsRefPrefix  = "refs/vcscore/ops/"
	latestRef     = "refs/vcscore/ops/latest"
	snapshotRefIn = "snapshot-" // ref name: ops/snapshot-<commit-prefix>
	commitPrefix  = 12          // hex chars of the commit ID used in the ref name
)

// Store is a snapshot store backed by one bare Git repository.
type Store struct {
	dir  string // bare repository directory
	last string // commit ID of the latest snapshot, "" if none yet
}

// Open opens the snapshot store at dir, creating a bare repository
// there if absent, probes the Git plumbing, and recovers the
// snapshot chain head from refs/vcscore/ops/latest so a reopened
// store continues the chain.
func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("snapshot: resolve %s: %w", dir, err)
	}
	s := &Store{dir: abs}
	if _, err := os.Stat(filepath.Join(abs, "HEAD")); errors.Is(err, fs.ErrNotExist) {
		// The repository does not exist yet, so this one call runs
		// without -C (git cannot chdir into a directory it is about
		// to create).
		cmd := exec.Command("git", "init", "--bare", abs)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("%w: init bare repo: %v: %s", ErrGitUnavailable, err, strings.TrimSpace(string(out)))
		}
	} else if err != nil {
		return nil, fmt.Errorf("snapshot: stat %s: %w", abs, err)
	}
	// Capability probe (Decision 2): hash-object over empty input
	// must reproduce the empty blob's ID in this repository.
	got, err := s.git([]byte{}, "hash-object", "-w", "--stdin")
	if err != nil {
		return nil, fmt.Errorf("%w: probe hash-object: %v", ErrGitUnavailable, err)
	}
	if got != emptyBlobID {
		return nil, fmt.Errorf("%w: probe hash-object returned %q, want %q", ErrGitUnavailable, got, emptyBlobID)
	}
	// Recover the chain head, if any.
	if head, err := s.git(nil, "rev-parse", "--verify", "--quiet", latestRef+"^{commit}"); err == nil {
		s.last = head
	}
	return s, nil
}

// git runs one plumbing command against the store's repository:
// -C <dir> on every call and the committer identity pinned per
// invocation, so snapshot commits never depend on host Git
// configuration. Argument vectors only — never a shell. It returns
// the trimmed stdout; extraEnv entries (e.g. GIT_INDEX_FILE) are
// appended to the process environment.
func (s *Store) git(stdin []byte, args ...string) (string, error) {
	return s.gitEnv(stdin, nil, args...)
}

func (s *Store) gitEnv(stdin []byte, extraEnv []string, args ...string) (string, error) {
	full := []string{"-C", s.dir,
		"-c", "user.name=replay-core",
		"-c", "user.email=replay-core@localhost"}
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// entry is one snapshotted workspace entry: a regular file or a
// symlink. Directories are not entries — Git trees cannot represent
// empty directories, so a directory contributes only its children.
type entry struct {
	rel   string // slash-separated path relative to the workspace root
	kind  string // "file" or "symlink"
	mode  string // git tree mode: 100644, 100755, or 120000
	size  int64
	mtime int64  // Unix nanoseconds
	data  []byte // symlink target string; nil for regular files
	blob  string // git blob ID, filled by Snapshot
}

// walk lists the workspace's entries in lexical order. Directories
// named .git are skipped; symlinks are recorded as links and never
// followed; any entry that is not a regular file, directory, or
// symlink fails the walk with an error naming the path. Regular
// files are only statted here, never read: Snapshot reads a file's
// bytes only when the entry must actually be hashed (the Task 14
// reuse law reuses the previous blob for unchanged entries without
// the file being read at all).
func walk(workspaceDir string) ([]entry, error) {
	var entries []entry
	err := filepath.WalkDir(workspaceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("snapshot: walk %s: %w", path, err)
		}
		if d.IsDir() {
			if d.Name() == ".git" && path != workspaceDir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(workspaceDir, path)
		if err != nil {
			return fmt.Errorf("snapshot: relativize %s: %w", path, err)
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("snapshot: readlink %s: %w", path, err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("snapshot: lstat %s: %w", path, err)
			}
			entries = append(entries, entry{
				rel: rel, kind: "symlink", mode: "120000",
				size: info.Size(), mtime: info.ModTime().UnixNano(),
				data: []byte(target),
			})
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("snapshot: stat %s: %w", path, err)
			}
			mode := "100644"
			if info.Mode().Perm()&0o111 != 0 {
				mode = "100755"
			}
			entries = append(entries, entry{
				rel: rel, kind: "file", mode: mode,
				size: info.Size(), mtime: info.ModTime().UnixNano(),
			})
		default:
			return fmt.Errorf("snapshot: unsupported workspace entry %s (type %s)", path, d.Type())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	return entries, nil
}

func intValue(n int64) canonical.Value {
	v, err := canonical.IntOf(strconv.FormatInt(n, 10))
	if err != nil {
		panic(err) // FormatInt output is always valid decimal text
	}
	return v
}

// manifestBytes renders meta/manifest.json deterministically (via
// canonical.MarshalKernel): an object keyed by workspace-relative
// path, each entry carrying its kind, size, mtime, and blob ID.
func manifestBytes(entries []entry) ([]byte, error) {
	pairs := make([]canonical.Pair, 0, len(entries))
	for _, e := range entries {
		obj := canonical.ObjectOf(
			canonical.Pair{Key: "blob", Value: canonical.StringOf(e.blob)},
			canonical.Pair{Key: "kind", Value: canonical.StringOf(e.kind)},
			canonical.Pair{Key: "mtime", Value: intValue(e.mtime)},
			canonical.Pair{Key: "size", Value: intValue(e.size)},
		)
		pairs = append(pairs, canonical.Pair{Key: e.rel, Value: obj})
	}
	b, err := canonical.MarshalKernel(canonical.ObjectOf(pairs...))
	if err != nil {
		return nil, fmt.Errorf("snapshot: marshal manifest: %w", err)
	}
	return b, nil
}

// snapshotMetaBytes renders meta/snapshot.json deterministically:
// the creation time plus the caller's meta map verbatim (nested
// under "meta" so caller keys can never collide with the store's
// own fields).
func snapshotMetaBytes(meta map[string]string) ([]byte, error) {
	metaPairs := make([]canonical.Pair, 0, len(meta))
	for k, v := range meta {
		metaPairs = append(metaPairs, canonical.Pair{Key: k, Value: canonical.StringOf(v)})
	}
	v := canonical.ObjectOf(
		canonical.Pair{Key: "created", Value: canonical.StringOf(time.Now().UTC().Format(time.RFC3339Nano))},
		canonical.Pair{Key: "meta", Value: canonical.ObjectOf(metaPairs...)},
	)
	b, err := canonical.MarshalKernel(v)
	if err != nil {
		return nil, fmt.Errorf("snapshot: marshal snapshot meta: %w", err)
	}
	return b, nil
}

// prevEntry is one entry of a previous snapshot's manifest, as
// recorded in meta/manifest.json.
type prevEntry struct {
	Blob  string `json:"blob"`
	Kind  string `json:"kind"`
	Mtime int64  `json:"mtime"`
	Size  int64  `json:"size"`
}

// previousManifest loads the manifest of the store's latest
// snapshot commit from that commit's meta/manifest.json, or nil
// when the store has no snapshot yet. The commit is the chain head
// recovered from refs/vcscore/ops/latest (tracked as s.last), and
// the manifest is read from the repository at Snapshot time —
// never from memory — so a reopened store stays incremental
// (plan Task 14, Step 2).
func (s *Store) previousManifest() (map[string]prevEntry, error) {
	if s.last == "" {
		return nil, nil
	}
	raw, err := s.git(nil, "cat-file", "-p", s.last+":meta/manifest.json")
	if err != nil {
		return nil, fmt.Errorf("snapshot: read previous manifest: %w", err)
	}
	// The manifest is canonical-kernel bytes: the law's prefix
	// tag on the first line, then the canonical JSON document.
	doc := raw
	if i := strings.Index(doc, "\n"); i >= 0 {
		doc = doc[i+1:]
	}
	var manifest map[string]prevEntry
	if err := json.Unmarshal([]byte(doc), &manifest); err != nil {
		return nil, fmt.Errorf("snapshot: parse previous manifest: %w", err)
	}
	return manifest, nil
}

// Snapshot captures workspaceDir as a new dual-tree commit: the
// workspace tree under workspace/, the manifest and snapshot
// metadata under meta/. The commit's parent is the store's previous
// snapshot commit; it is referenced as refs/vcscore/ops/snapshot-
// <commit-prefix> and refs/vcscore/ops/latest. It returns the new
// commit ID. The build is incremental (plan Task 14): an entry
// whose (size, mtime) pair matches the previous snapshot's manifest
// reuses that entry's blob SHA without the file being read or
// re-hashed; only new or changed entries are read and hashed.
func (s *Store) Snapshot(workspaceDir string, meta map[string]string) (string, error) {
	entries, err := walk(workspaceDir)
	if err != nil {
		return "", err
	}
	prev, err := s.previousManifest()
	if err != nil {
		return "", err
	}
	for i := range entries {
		e := &entries[i]
		if p, ok := prev[e.rel]; ok && p.Kind == e.kind && p.Size == e.size && p.Mtime == e.mtime {
			e.blob = p.Blob // reuse law: no read, no re-hash
			continue
		}
		data := e.data
		if e.kind == "file" {
			path := filepath.Join(workspaceDir, filepath.FromSlash(e.rel))
			b, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("snapshot: read %s: %w", path, err)
			}
			data = b
		}
		blob, err := s.git(data, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", fmt.Errorf("snapshot: hash %s: %w", e.rel, err)
		}
		e.blob = blob
	}
	manifest, err := manifestBytes(entries)
	if err != nil {
		return "", err
	}
	snapMeta, err := snapshotMetaBytes(meta)
	if err != nil {
		return "", err
	}

	// Assemble the dual tree in a temporary index so no real index
	// is touched.
	idxFile, err := os.CreateTemp("", "replay-core-index-*")
	if err != nil {
		return "", fmt.Errorf("snapshot: create temp index: %w", err)
	}
	idxPath := idxFile.Name()
	_ = idxFile.Close()
	_ = os.Remove(idxPath) // git creates the index fresh at this path
	defer func() { _ = os.Remove(idxPath) }()
	idxEnv := []string{"GIT_INDEX_FILE=" + idxPath}

	add := func(mode, blob, path string) error {
		_, err := s.gitEnv(nil, idxEnv, "update-index", "--add", "--cacheinfo", mode+","+blob+","+path)
		if err != nil {
			return fmt.Errorf("snapshot: stage %s: %w", path, err)
		}
		return nil
	}
	for _, e := range entries {
		if err := add(e.mode, e.blob, "workspace/"+e.rel); err != nil {
			return "", err
		}
	}
	for _, m := range []struct {
		name string
		data []byte
	}{{"manifest.json", manifest}, {"snapshot.json", snapMeta}} {
		blob, err := s.git(m.data, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", fmt.Errorf("snapshot: hash meta/%s: %w", m.name, err)
		}
		if err := add("100644", blob, "meta/"+m.name); err != nil {
			return "", err
		}
	}
	tree, err := s.gitEnv(nil, idxEnv, "write-tree")
	if err != nil {
		return "", fmt.Errorf("snapshot: write-tree: %w", err)
	}

	commitArgs := []string{"commit-tree", tree}
	if s.last != "" {
		commitArgs = append(commitArgs, "-p", s.last)
	}
	commitArgs = append(commitArgs, "-m", "snapshot")
	commit, err := s.git(nil, commitArgs...)
	if err != nil {
		return "", fmt.Errorf("snapshot: commit-tree: %w", err)
	}
	if _, err := s.git(nil, "update-ref", opsRefPrefix+snapshotRefIn+commit[:commitPrefix], commit); err != nil {
		return "", fmt.Errorf("snapshot: update-ref snapshot: %w", err)
	}
	if _, err := s.git(nil, "update-ref", latestRef, commit); err != nil {
		return "", fmt.Errorf("snapshot: update-ref latest: %w", err)
	}
	s.last = commit
	return commit, nil
}
