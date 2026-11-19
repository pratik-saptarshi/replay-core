package snapshot_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/snapshot"
)

// chainFromLatest returns the commits reachable from
// refs/vcscore/ops/latest, newest first (git rev-list order).
func chainFromLatest(t *testing.T, dir string) []string {
	t.Helper()
	out := git(t, dir, "rev-list", "refs/vcscore/ops/latest")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// assertChainContainsExactly checks that the chain reachable from
// latest is exactly the given snapshot commits, each exactly once:
// a linear chain with no snapshot orphaned and none duplicated.
func assertChainContainsExactly(t *testing.T, dir string, commits []string) {
	t.Helper()
	chain := chainFromLatest(t, dir)
	if len(chain) != len(commits) {
		t.Fatalf("chain from latest has %d commits, want %d (every snapshot exactly once):\nchain:    %v\nsnapshots: %v", len(chain), len(commits), chain, commits)
	}
	seen := make(map[string]bool, len(chain))
	for _, c := range chain {
		if seen[c] {
			t.Fatalf("commit %s appears twice in the chain from latest", c)
		}
		seen[c] = true
	}
	for _, c := range commits {
		if !seen[c] {
			t.Fatalf("snapshot commit %s is not reachable from latest (chain: %v)", c, chain)
		}
	}
}

// TestSnapshotTwoHandlesShareOneChain (ADV-1): two handles opened
// on the same repository alternate snapshots. Each handle used to
// parent its commits on its own cached head, so the second
// handle's first snapshot was parentless, latest rewound to it, and
// the first handle's snapshot dropped out of the chain. The chain
// from latest must contain every snapshot exactly once.
func TestSnapshotTwoHandlesShareOneChain(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(t.TempDir(), "snap.git")
	h1, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open h1: %v", err)
	}
	h2, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open h2: %v", err)
	}
	handles := []*snapshot.Store{h1, h2}
	var commits []string
	for i := 0; i < 4; i++ {
		writeFile(t, filepath.Join(ws, "f.txt"), fmt.Sprintf("v%d\n", i))
		c, err := handles[i%2].Snapshot(ws, map[string]string{"i": fmt.Sprint(i)})
		if err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
		commits = append(commits, c)
	}
	assertChainContainsExactly(t, dir, commits)
}

// TestSnapshotConcurrentOnOneHandle (ADV-1): concurrent Snapshot
// calls on ONE handle used to race on the cached chain head (a real
// data race under -race) and fork the chain. They must serialize
// and produce one linear chain containing every snapshot.
func TestSnapshotConcurrentOnOneHandle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snap.git")
	s, err := snapshot.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const n = 4
	workspaces := make([]string, n)
	for i := 0; i < n; i++ {
		workspaces[i] = t.TempDir()
		writeFile(t, filepath.Join(workspaces[i], "f.txt"), fmt.Sprintf("ws-%d\n", i))
	}
	var wg sync.WaitGroup
	commits := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			commits[i], errs[i] = s.Snapshot(workspaces[i], map[string]string{"i": fmt.Sprint(i)})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
	}
	assertChainContainsExactly(t, dir, commits)
}
