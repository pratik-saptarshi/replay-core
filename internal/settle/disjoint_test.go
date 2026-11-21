package settle

import "testing"

// TestDisjointCensus (plan Task 7, Step 1; AC-F6-2/AC-F6-3's
// shapes, pure): the lexical half of the disjointness predicate
// over path pairs — no filesystem. Two relative paths overlap
// iff, after cleaning, they are equal or one is a
// directory-prefix of the other. The near-miss that matters is
// a/b vs a/b-c: a shared string prefix that is not a path
// prefix is DISJOINT.
func TestDisjointCensus(t *testing.T) {
	cases := []struct {
		a, b    string
		overlap bool
	}{
		// Equal paths overlap.
		{"a/b", "a/b", true},
		{"top.txt", "top.txt", true},
		// Directory-prefix overlaps, both directions.
		{"a/b", "a/b/c", true},
		{"a/b/c", "a/b", true},
		// The prefix near-miss: a shared string prefix that is
		// not a path prefix is disjoint.
		{"a/b", "a/b-c", false},
		{"a/b-c", "a/b", false},
		// Siblings and distinct top-level files are disjoint.
		{"a/b", "a/c", false},
		{"one.txt", "two.txt", false},
		// A "." segment cleans away: a/./b is a/b.
		{"a/./b", "a/b", true},
		{"a/b", "a/./b", true},
		// Deep nesting, both directions.
		{"a/b/c/d", "a/b", true},
		{"a/b", "a/b/c/d", true},
		{"a/b/c", "a/b/d", false},
		{"a/b/c/d", "a/b/c/e", false},
		{"x", "x/y/z", true},
		{"x/y/z", "x", true},
	}
	for _, tc := range cases {
		if got := pathsOverlap(tc.a, tc.b); got != tc.overlap {
			t.Errorf("pathsOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.overlap)
		}
	}
}
