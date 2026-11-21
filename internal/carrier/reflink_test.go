//go:build linux

package carrier_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/carrier"
)

// reflinkSelection is the selection record the reflink tests
// run against: a forced reflink selection, as a caller holding
// a selection record from a host where the probe passed would
// present it.
var reflinkSelection = carrier.Selection{Driver: carrier.DriverReflink}

// TestReflinkProbeReportsCloneRefusalHere is AC-F5-10's probe
// leg in this sandbox: on both filesystems — tmpfs (os.TempDir(),
// baseline §1) and btrfs (a scratch dir under the working
// directory, baseline §2) — FICLONE is refused (EPERM, baseline
// §4), so the reflink probe is recorded unavailable with a
// reason naming the FICLONE refusal, and selection passes over
// the driver honestly. The probe's verdict is the honest
// discharge of the probe's own contract; AC-F5-10's
// extent-sharing clause itself is host-deferred, NOT discharged
// (Decision 7) — no test here can observe a shared extent.
func TestReflinkProbeReportsCloneRefusalHere(t *testing.T) {
	tmpfsRoot, err := os.MkdirTemp("", ".carrier-reflink-tmpfs-")
	if err != nil {
		t.Fatalf("create tmpfs root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpfsRoot) })

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	btrfsRoot, err := os.MkdirTemp(wd, ".carrier-reflink-btrfs-")
	if err != nil {
		t.Fatalf("create btrfs root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(btrfsRoot) })

	for _, root := range []string{tmpfsRoot, btrfsRoot} {
		sel, err := carrier.Select(root)
		if err != nil {
			t.Fatalf("Select(%q): %v", root, err)
		}
		if sel.Driver == carrier.DriverReflink {
			t.Errorf("Select(%q) selected reflink, want it passed over (FICLONE is refused here)", root)
		}
		probe := probeOf(t, sel, carrier.DriverReflink)
		if probe.Available {
			t.Errorf("Select(%q): reflink probe reported available, want unavailable (FICLONE refused here)", root)
		}
		if !strings.Contains(probe.Reason, "FICLONE") {
			t.Errorf("Select(%q): reflink reason = %q, want it to name the FICLONE refusal", root, probe.Reason)
		}
		assertNoScratchRemains(t, root)
	}
}

// TestReflinkMaterializeFailureLeavesNoPartialLayer is
// AC-F5-10's no-silent-fallback clause exercised end to end
// (Decision 7: the refusal path is exactly what this sandbox
// can prove): a forced reflink selection over a three-file base
// fails at the first file's clone — the error names that file
// and the clone failure, it is never degraded to a byte copy —
// and no partial layer remains: the shared lifecycle removes
// the staging tree and the failed first build's root, so no
// view, no marker, and no staging remnant is left behind.
func TestReflinkMaterializeFailureLeavesNoPartialLayer(t *testing.T) {
	base := t.TempDir()
	writeBaseFile(t, filepath.Join(base, "a.txt"), "a\n", 0o644)
	writeBaseFile(t, filepath.Join(base, "b.txt"), "b\n", 0o644)
	writeBaseFile(t, filepath.Join(base, "c.txt"), "c\n", 0o644)
	root := filepath.Join(t.TempDir(), "layer")

	_, err := carrier.Materialize(reflinkSelection, base, "tree-v1", root)
	if err == nil {
		t.Fatal("Materialize with a forced reflink selection: err = nil, want the clone refusal (no byte-copy fallback)")
	}
	// The walk clones in lexical order, so the first file is
	// a.txt; the error names it and the FICLONE failure.
	if !strings.Contains(err.Error(), filepath.Join(base, "a.txt")) {
		t.Errorf("refusal error = %q, want it to name the first file %q", err, filepath.Join(base, "a.txt"))
	}
	if !strings.Contains(err.Error(), "FICLONE") {
		t.Errorf("refusal error = %q, want it to name the clone failure (FICLONE)", err)
	}
	if _, serr := os.Stat(root); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("partial layer left behind after refused materialization (stat err = %v): want no view, marker, or staging remnant", serr)
	}
}
