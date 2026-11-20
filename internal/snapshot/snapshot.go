// Package snapshot is replay-core's Git-backed snapshot store (F4):
// each snapshot is one commit in a bare repository, holding a dual
// tree — workspace/ (the snapshotted files) and meta/ (the manifest
// and snapshot metadata) — chained by parentage and referenced
// under refs/vcscore/ops/. All Git access goes through the git
// binary's plumbing commands (plan Decision 2), never a shell.
package snapshot

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// zeroOID is the all-zero object ID: as an update-ref old value it
// means "the ref must not exist yet" — the compare-and-swap guard
// for the first snapshot in a chain.
const zeroOID = "0000000000000000000000000000000000000000"

// maxChainAttempts bounds Snapshot's compare-and-swap retries: if
// a peer handle or process keeps advancing the chain head first,
// Snapshot rebuilds on the new head this many times before
// failing.
const maxChainAttempts = 16

// Store is a snapshot store backed by one bare Git repository.
type Store struct {
	dir string // bare repository directory
	// mu serializes Snapshot calls on this handle: a snapshot
	// resolves the chain head, builds on it, and compare-and-swaps
	// refs/vcscore/ops/latest, and that sequence must not
	// interleave with itself. Contention across handles and
	// processes is handled by the compare-and-swap retry in
	// Snapshot, not by this mutex.
	mu sync.Mutex
	// last caches the chain head's commit ID, "" if none yet. It
	// is a cache only — refs/vcscore/ops/latest is the truth, and
	// Snapshot re-resolves it rather than trusting this field.
	last string
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
	// Recover the chain head, if any, and check it is actually a
	// snapshot commit before adopting it.
	head, err := s.resolveChainHead()
	if err != nil {
		return nil, err
	}
	if head != "" {
		if err := s.validateChainHead(head); err != nil {
			return nil, err
		}
	}
	s.last = head
	return s, nil
}

// validateChainHead checks that head — the commit
// refs/vcscore/ops/latest names — is a snapshot commit: its
// meta/snapshot.json must parse as the snapshot metadata the
// writer emits, and its meta/manifest.json must parse as a
// snapshot manifest, read through previousManifestOf — the same
// read Snapshot performs on its parent. A foreign commit
// (well-formed, but not a snapshot) adopted as chain head would
// otherwise pass Open and then fail every later Snapshot
// permanently on the manifest it lacks.
func (s *Store) validateChainHead(head string) error {
	rawMeta, err := s.git(nil, "cat-file", "-p", head+":meta/snapshot.json")
	if err != nil {
		return fmt.Errorf("snapshot: chain head %s names commit %s, which is not a snapshot commit (no meta/snapshot.json): %w", latestRef, head, err)
	}
	if err := parseSnapshotMeta([]byte(rawMeta)); err != nil {
		return fmt.Errorf("snapshot: chain head %s names commit %s, whose meta/snapshot.json does not parse as snapshot metadata: %w", latestRef, head, err)
	}
	if _, err := s.previousManifestOf(head); err != nil {
		return fmt.Errorf("snapshot: chain head %s names commit %s, whose meta/manifest.json does not parse as a snapshot manifest: %w", latestRef, head, err)
	}
	return nil
}

// parseSnapshotMeta validates raw meta/snapshot.json bytes
// against the shape snapshotMetaBytes emits: canonical-kernel
// bytes of an object whose "created" is a timestamp string and
// whose "meta" is an object of strings. The check is a content
// check, not the existence check it replaces: a garbage blob, a
// tree, a gitlink, or a symlink at that path cannot produce
// bytes of this shape, so none of them can be adopted as a chain
// head no real Snapshot could have written (panel W3-6).
func parseSnapshotMeta(raw []byte) error {
	v, err := canonical.DecodeKernel(raw)
	if err != nil {
		return err
	}
	if v.Kind != canonical.KindObject {
		return errors.New("snapshot metadata is not an object")
	}
	created, ok := metaField(v, "created")
	if !ok || created.Kind != canonical.KindString {
		return errors.New("snapshot metadata has no created string")
	}
	if _, err := time.Parse(time.RFC3339Nano, created.Str); err != nil {
		return fmt.Errorf("snapshot metadata created %q is not a timestamp: %w", created.Str, err)
	}
	meta, ok := metaField(v, "meta")
	if !ok || meta.Kind != canonical.KindObject {
		return errors.New("snapshot metadata has no meta object")
	}
	for _, p := range meta.Pairs {
		if p.Value.Kind != canonical.KindString {
			return fmt.Errorf("snapshot metadata value for %q is not a string", p.Key)
		}
	}
	return nil
}

// metaField returns the value of key in a decoded object value.
func metaField(obj canonical.Value, key string) (canonical.Value, bool) {
	for _, p := range obj.Pairs {
		if p.Key == key {
			return p.Value, true
		}
	}
	return canonical.Value{}, false
}

// resolveChainHead returns the commit refs/vcscore/ops/latest
// names, or "" when the ref is absent (no chain yet). A latest ref
// that exists but does not resolve to a commit is corruption, and
// resolveChainHead fails loudly rather than silently starting a
// fresh chain over the existing snapshots: the ref may name a
// non-commit object or a dangling object ID (the unpeeled
// rev-parse below still resolves it), or its contents may be
// unparseable — git reports that as a "broken ref" warning on the
// failing rev-parse's stderr while treating the ref as absent, so
// the warning text is the only signal separating a broken ref
// from a genuinely absent one.
func (s *Store) resolveChainHead() (string, error) {
	head, err := s.git(nil, "rev-parse", "--verify", "--quiet", latestRef+"^{commit}")
	if err == nil {
		return head, nil
	}
	if _, uerr := s.git(nil, "rev-parse", "--verify", "--quiet", latestRef); uerr == nil {
		return "", fmt.Errorf("snapshot: chain head %s does not resolve to a commit", latestRef)
	} else if strings.Contains(err.Error(), "broken ref") || strings.Contains(uerr.Error(), "broken ref") {
		return "", fmt.Errorf("snapshot: chain head %s is corrupt: %v", latestRef, err)
	}
	return "", nil
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

// walk lists the workspace's entries in lexical order. Entries
// named .git are skipped whatever their filesystem type;
// symlinks are recorded as links and never
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
		// An entry named .git is never workspace content,
		// whatever its filesystem type: a directory in an
		// ordinary checkout, a regular file in a linked worktree
		// or a submodule checkout (where it holds a "gitdir:"
		// pointer), a symlink in stranger layouts. Skipping only
		// the directory let the file reach staging, where
		// update-index refused it and the whole snapshot failed
		// (Codex P1 on PR #2). The workspace root itself is
		// exempt: it is the thing being snapshotted, not an
		// entry in it.
		if path != workspaceDir && d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
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
	Blob  string
	Kind  string
	Mtime int64
	Size  int64
}

// previousManifestOf loads the manifest of the given snapshot
// commit from that commit's meta/manifest.json, or nil when
// commit is "" (no parent snapshot). The manifest is read from
// the repository at Snapshot time — never from memory — so a
// reopened store stays incremental (plan Task 14, Step 2), and so
// the manifest always describes the parent the new commit is
// actually built on.
func (s *Store) previousManifestOf(commit string) (map[string]prevEntry, error) {
	if commit == "" {
		return nil, nil
	}
	raw, err := s.git(nil, "cat-file", "-p", commit+":meta/manifest.json")
	if err != nil {
		return nil, fmt.Errorf("snapshot: read previous manifest: %w", err)
	}
	manifest, err := parseManifest([]byte(raw))
	if err != nil {
		return nil, err
	}
	if err := s.checkManifestBlobs(manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

// parseManifest decodes a manifest's bytes and validates the
// shape of every entry. The decode is canonical.DecodeKernel —
// the same law the manifest writer encodes under — so the
// canonical prefix tag is required, not stripped and assumed,
// and a null or otherwise non-object document is refused rather
// than read as an empty manifest. Each entry must be an object
// carrying a well-formed blob ID, a kind of "file" or "symlink",
// an integer mtime, and a non-negative integer size (panel
// W3-7).
func parseManifest(raw []byte) (map[string]prevEntry, error) {
	v, err := canonical.DecodeKernel(raw)
	if err != nil {
		return nil, fmt.Errorf("snapshot: parse previous manifest: %w", err)
	}
	if v.Kind != canonical.KindObject {
		return nil, errors.New("snapshot: parse previous manifest: manifest is not an object")
	}
	manifest := make(map[string]prevEntry, len(v.Pairs))
	for _, p := range v.Pairs {
		entry, err := parsePrevEntry(p.Value)
		if err != nil {
			return nil, fmt.Errorf("snapshot: parse previous manifest: entry %q: %w", p.Key, err)
		}
		manifest[p.Key] = entry
	}
	return manifest, nil
}

// parsePrevEntry validates one manifest entry's shape and
// converts it to a prevEntry.
func parsePrevEntry(v canonical.Value) (prevEntry, error) {
	if v.Kind != canonical.KindObject {
		return prevEntry{}, errors.New("manifest entry is not an object")
	}
	var e prevEntry
	var haveBlob, haveKind, haveMtime, haveSize bool
	for _, p := range v.Pairs {
		switch p.Key {
		case "blob":
			if p.Value.Kind != canonical.KindString {
				return prevEntry{}, errors.New("blob is not a string")
			}
			e.Blob, haveBlob = p.Value.Str, true
		case "kind":
			if p.Value.Kind != canonical.KindString {
				return prevEntry{}, errors.New("kind is not a string")
			}
			e.Kind, haveKind = p.Value.Str, true
		case "mtime":
			if p.Value.Kind != canonical.KindInt {
				return prevEntry{}, errors.New("mtime is not an integer")
			}
			n, err := strconv.ParseInt(p.Value.Int, 10, 64)
			if err != nil {
				return prevEntry{}, fmt.Errorf("mtime: %w", err)
			}
			e.Mtime, haveMtime = n, true
		case "size":
			if p.Value.Kind != canonical.KindInt {
				return prevEntry{}, errors.New("size is not an integer")
			}
			n, err := strconv.ParseInt(p.Value.Int, 10, 64)
			if err != nil {
				return prevEntry{}, fmt.Errorf("size: %w", err)
			}
			if n < 0 {
				return prevEntry{}, errors.New("size is negative")
			}
			e.Size, haveSize = n, true
		}
	}
	if !haveBlob || !haveKind || !haveMtime || !haveSize {
		return prevEntry{}, errors.New("manifest entry lacks one of blob, kind, mtime, size")
	}
	if e.Kind != "file" && e.Kind != "symlink" {
		return prevEntry{}, fmt.Errorf("kind %q is neither \"file\" nor \"symlink\"", e.Kind)
	}
	if !isObjectID(e.Blob) {
		return prevEntry{}, fmt.Errorf("blob %q is not a well-formed object ID", e.Blob)
	}
	return e, nil
}

// isObjectID reports whether s is a full lowercase-hex SHA-1
// object ID, the only form git's plumbing emits.
func isObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// checkManifestBlobs verifies that every blob a manifest names
// exists in the repository as a blob object. snapshotOnce's
// reuse law trusts a manifest entry's blob whenever kind, size
// and mtime match the workspace file; an entry naming a missing
// or non-blob object would otherwise pass Open and then fail
// every later Snapshot permanently, deep in the plumbing,
// instead of being refused here as the corruption it is (panel
// W3-7). One batch-check call covers the whole manifest.
func (s *Store) checkManifestBlobs(manifest map[string]prevEntry) error {
	if len(manifest) == 0 {
		return nil
	}
	rels := make([]string, 0, len(manifest))
	for rel := range manifest {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	var in strings.Builder
	for _, rel := range rels {
		in.WriteString(manifest[rel].Blob)
		in.WriteByte('\n')
	}
	out, err := s.git([]byte(in.String()), "cat-file", "--batch-check")
	if err != nil {
		return fmt.Errorf("snapshot: check manifest blobs: %w", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != len(rels) {
		return fmt.Errorf("snapshot: check manifest blobs: batch-check returned %d lines for %d entries", len(lines), len(rels))
	}
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "blob" {
			return fmt.Errorf("snapshot: manifest entry %q names blob %s, which is not a blob in the repository (%s)", rels[i], manifest[rels[i]].Blob, line)
		}
	}
	return nil
}

// Snapshot captures workspaceDir as a new dual-tree commit: the
// workspace tree under workspace/, the manifest and snapshot
// metadata under meta/. The commit's parent is the chain head
// refs/vcscore/ops/latest names at snapshot time — re-resolved on
// every call, never the handle's cached head, so two handles (or
// processes) on one repository chain onto each other's snapshots
// instead of forking the chain. The commit is referenced as
// refs/vcscore/ops/snapshot-<commit-prefix> and
// refs/vcscore/ops/latest, the latter advanced by a
// compare-and-swap on the resolved head: if a peer advanced the
// chain first, the commit is rebuilt on the new head and the swap
// retried (bounded by maxChainAttempts). It returns the new
// commit ID. The build is incremental (plan Task 14): an entry
// whose (size, mtime) pair matches the parent snapshot's manifest
// reuses that entry's blob SHA without the file being read or
// re-hashed; only new or changed entries are read and hashed.
func (s *Store) Snapshot(workspaceDir string, meta map[string]string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The workspace root is resolved once, here, and the
	// resolved path is used for the walk and every file read:
	// WalkDir does not follow a symlinked root — it visits the
	// link itself as the workspace's single entry, and the tree
	// build then fails deep in the plumbing with a raw git error
	// naming a nonsense path (panel P3). Resolving ancestor
	// symlinks along the way changes nothing else: entries are
	// recorded relative to the root either way.
	resolved, err := resolveWorkspaceRoot(workspaceDir)
	if err != nil {
		return "", err
	}
	workspaceDir = resolved

	entries, err := walk(workspaceDir)
	if err != nil {
		return "", err
	}
	snapMeta, err := snapshotMetaBytes(meta)
	if err != nil {
		return "", err
	}
	// The generated metadata must round-trip through its own
	// reader before anything is built on it: MarshalKernel
	// preserves raw bytes, but DecodeKernel — the reader's law,
	// enforced by parseSnapshotMeta at Open — rejects invalid
	// UTF-8, so a caller meta key or value carrying such bytes
	// would publish a chain head no reopen could ever adopt
	// (Codex P2 on PR #2). Refuse here, before any object or
	// ref is written.
	if err := parseSnapshotMeta(snapMeta); err != nil {
		return "", fmt.Errorf("snapshot: generated meta/snapshot.json does not parse as snapshot metadata: %w", err)
	}
	var lastCAS error
	for attempt := 0; attempt < maxChainAttempts; attempt++ {
		commit, advanced, casErr, err := s.snapshotOnce(workspaceDir, entries, snapMeta)
		if err != nil {
			return "", err
		}
		if advanced {
			return commit, nil
		}
		lastCAS = casErr
	}
	return "", fmt.Errorf("snapshot: chain head %s kept moving under contention after %d attempts: %w", latestRef, maxChainAttempts, lastCAS)
}

// resolveWorkspaceRoot returns the absolute, symlink-free form
// of a workspace root directory, or a descriptive error naming
// the path as given when it cannot be resolved. The root must
// name a directory: an empty path is refused outright —
// filepath.Abs("") is the process working directory, so an
// empty root from a missing flag or config value would snapshot
// whatever directory the process runs in (panel W3-5) — and a
// root that resolves to a non-directory is refused here rather
// than deep in the git plumbing, where it died with a raw
// Invalid path error (panel W3-8).
func resolveWorkspaceRoot(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("snapshot: workspace root is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("snapshot: resolve workspace root %s: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("snapshot: resolve workspace root %s: %w", dir, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("snapshot: stat workspace root %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("snapshot: workspace root %s is not a directory", dir)
	}
	return resolved, nil
}

// attachEmptyWorkspaceTree returns root — a write-tree result
// holding only meta/, the empty-workspace case — with git's
// empty tree attached as its workspace/ subtree. The rebuilt
// tree's entries are meta plus the workspace line, already in
// tree order.
func (s *Store) attachEmptyWorkspaceTree(root string) (string, error) {
	emptyTree, err := s.git(nil, "mktree")
	if err != nil {
		return "", fmt.Errorf("snapshot: create empty tree: %w", err)
	}
	listing, err := s.git(nil, "ls-tree", root)
	if err != nil {
		return "", fmt.Errorf("snapshot: list root tree: %w", err)
	}
	input := listing + "\n040000 tree " + emptyTree + "\tworkspace\n"
	newRoot, err := s.git([]byte(input), "mktree")
	if err != nil {
		return "", fmt.Errorf("snapshot: attach empty workspace tree: %w", err)
	}
	return newRoot, nil
}

// snapshotOnce makes one attempt at extending the chain: it
// resolves the current head, builds a snapshot commit on it, and
// tries to advance refs/vcscore/ops/latest to the new commit with
// a compare-and-swap on the resolved head. advanced is false when
// the swap lost a race against a peer — the built commit is left
// orphaned (its per-snapshot ref remains, harmlessly) and the
// caller retries on the new head, so the manifest the retry
// reuses is always the manifest of the parent actually used. On
// a lost race casErr carries the compare-and-swap's failure (and
// err is nil): the caller keeps the last one so that exhausting
// the retry bound reports the underlying cause, not just the
// contention.
func (s *Store) snapshotOnce(workspaceDir string, entries []entry, snapMeta []byte) (commit string, advanced bool, casErr error, err error) {
	head, err := s.resolveChainHead()
	if err != nil {
		return "", false, nil, err
	}
	prev, err := s.previousManifestOf(head)
	if err != nil {
		return "", false, nil, err
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
				return "", false, nil, fmt.Errorf("snapshot: read %s: %w", path, err)
			}
			data = b
		}
		blob, err := s.git(data, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", false, nil, fmt.Errorf("snapshot: hash %s: %w", e.rel, err)
		}
		e.blob = blob
	}
	manifest, err := manifestBytes(entries)
	if err != nil {
		return "", false, nil, err
	}
	// The generated manifest must round-trip through its own
	// reader before the commit is created and any ref is
	// published: MarshalKernel preserves raw bytes in the entry
	// keys, but DecodeKernel — the reader's law, enforced by
	// parseManifest on every parent read — rejects invalid
	// UTF-8, and a workspace filename carrying such bytes is
	// legal on Unix. Publishing such a manifest would wedge
	// the chain permanently: Open and every later Snapshot
	// fail on the head even after the file is removed (Codex
	// P2 on PR #2). The manifest stays a faithful record —
	// the name is refused, never sanitized. This is the
	// earliest point the manifest bytes exist: they name the
	// blobs hashed above, and those orphaned blobs are as
	// harmless as a lost race's orphaned commit.
	if _, err := parseManifest(manifest); err != nil {
		return "", false, nil, fmt.Errorf("snapshot: generated meta/manifest.json does not parse as a snapshot manifest: %w", err)
	}

	// Assemble the dual tree in a temporary index so no real index
	// is touched.
	idxFile, err := os.CreateTemp("", "replay-core-index-*")
	if err != nil {
		return "", false, nil, fmt.Errorf("snapshot: create temp index: %w", err)
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
			return "", false, nil, err
		}
	}
	for _, m := range []struct {
		name string
		data []byte
	}{{"manifest.json", manifest}, {"snapshot.json", snapMeta}} {
		blob, err := s.git(m.data, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", false, nil, fmt.Errorf("snapshot: hash meta/%s: %w", m.name, err)
		}
		if err := add("100644", blob, "meta/"+m.name); err != nil {
			return "", false, nil, err
		}
	}
	tree, err := s.gitEnv(nil, idxEnv, "write-tree")
	if err != nil {
		return "", false, nil, fmt.Errorf("snapshot: write-tree: %w", err)
	}
	if len(entries) == 0 {
		// The root tree is always exactly {workspace, meta}
		// (plan Task 13): with no workspace entries staged, the
		// index holds only meta/, so write-tree produced a root
		// with no workspace subtree, and consumers resolving
		// that subtree cannot read the snapshot (Codex P2 on
		// PR #2). Attach git's empty tree as workspace/. The
		// walk law stands — empty directories leave no entries
		// under workspace/; only the subtree itself is
		// guaranteed.
		tree, err = s.attachEmptyWorkspaceTree(tree)
		if err != nil {
			return "", false, nil, err
		}
	}

	commitArgs := []string{"commit-tree", tree}
	if head != "" {
		commitArgs = append(commitArgs, "-p", head)
	}
	commitArgs = append(commitArgs, "-m", "snapshot")
	newCommit, err := s.git(nil, commitArgs...)
	if err != nil {
		return "", false, nil, fmt.Errorf("snapshot: commit-tree: %w", err)
	}
	if _, err := s.git(nil, "update-ref", opsRefPrefix+snapshotRefIn+newCommit[:commitPrefix], newCommit); err != nil {
		return "", false, nil, fmt.Errorf("snapshot: update-ref snapshot: %w", err)
	}
	// Advance the chain head only if it still names the head this
	// commit was built on (or still does not exist): the
	// compare-and-swap that keeps two handles from forking the
	// chain. Losing the race is not an error — the caller retries
	// on the new head.
	expectedOld := head
	if expectedOld == "" {
		expectedOld = zeroOID
	}
	if _, err := s.git(nil, "update-ref", latestRef, newCommit, expectedOld); err != nil {
		return "", false, err, nil
	}
	s.last = newCommit
	return newCommit, true, nil, nil
}
