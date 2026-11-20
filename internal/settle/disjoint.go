package settle

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// This file holds the disjointness predicate at the heart of
// path-disjoint apply (plan Task 7; AC-F6-2/AC-F6-3): the run's
// delta and the parent's post-fork delta must not touch the
// same file, where "the same" is judged alias-aware — equal
// paths, prefix paths, and aliases all overlap, and no content
// merge is ever synthesized in place of a refusal.
//
// Two relative paths overlap iff, after cleaning, they are
// equal or one is a directory-prefix of the other — or their
// canonical forms in the parent's current tree do. The lexical
// half is pathsOverlap; the canonical half is pathsOverlapIn
// (canonicalPath), which resolves symlinks over a path's
// longest existing ancestor inside the parent directory: the
// run's paths are judged against the parent's tree because that
// is where they would land.

// pathsOverlap is the lexical half of the predicate: cleaned
// equality, or a directory-prefix relation in either direction.
// A shared string prefix that is not a path prefix (a/b vs
// a/b-c) is disjoint — the prefix near-miss.
func pathsOverlap(a, b string) bool {
	a = path.Clean(a)
	b = path.Clean(b)
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// pathsOverlapIn is the census predicate: two delta paths
// overlap iff they overlap lexically or their canonical forms
// in the parent's current tree do. The parent's tree is the
// directory itself — the tree the probe snapshot recorded — so
// canonicalization resolves against parentDir on disk.
func pathsOverlapIn(parentDir, a, b string) bool {
	if pathsOverlap(a, b) {
		return true
	}
	root, err := filepath.EvalSymlinks(parentDir)
	if err != nil {
		root = parentDir
	}
	return pathsOverlap(canonicalPath(root, a), canonicalPath(root, b))
}

// canonicalPath returns rel's canonical form inside root (an
// already-resolved absolute directory): the path cleaned, then
// symlinks resolved over its longest existing ancestor inside
// root, with the remainder re-appended. A path whose every
// ancestor exists resolves in full; a path to a not-yet-existing
// file resolves as far as its deepest existing ancestor — which
// is exactly where an alias can hide, since an alias is an
// existing symlinked component. A prefix that exists but does
// not resolve (a dangling symlink) is stepped over for a
// shorter resolvable ancestor, and a path with no existing
// ancestor is its own canonical form.
func canonicalPath(root, rel string) string {
	clean := path.Clean(rel)
	parts := strings.Split(clean, "/")
	for n := len(parts); n >= 1; n-- {
		cand := filepath.Join(root, filepath.FromSlash(strings.Join(parts[:n], "/")))
		if _, err := os.Lstat(cand); err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(cand)
		if err != nil {
			continue
		}
		relResolved, err := filepath.Rel(root, resolved)
		if err != nil {
			continue
		}
		out := filepath.ToSlash(relResolved)
		if n < len(parts) {
			out += "/" + strings.Join(parts[n:], "/")
		}
		return path.Clean(out)
	}
	return clean
}
