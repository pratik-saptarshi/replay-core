package carrier_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/carrier"
)

// TestFuseMaterializeRefusesWithProbeReason is AC-F5-9's
// build-home pin (Decision 5): a forced FUSE-overlay selection
// fails honestly — the error carries the probe's own recorded
// reason, naming /dev/fuse — instead of pretending to serve,
// and no layer root is created. AC-F5-9 itself is
// host-deferred, not discharged: /dev/fuse is absent in this
// sandbox, so the mount cannot be exercised here at all.
func TestFuseMaterializeRefusesWithProbeReason(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base file: %v", err)
	}

	// The recorded reason is whatever the probe records in a
	// real selection record for this host.
	sel, err := carrier.Select(base)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	probe := probeOf(t, sel, carrier.DriverFuseOverlay)
	if probe.Available {
		t.Fatalf("fuse-overlay probe reported available, want unavailable (Decision 5)")
	}
	if probe.Reason == "" {
		t.Fatalf("fuse-overlay probe recorded no reason")
	}

	layerRoot := filepath.Join(t.TempDir(), "layer")
	_, err = carrier.Materialize(carrier.Selection{Driver: carrier.DriverFuseOverlay}, base, "tree-v1", layerRoot)
	if err == nil {
		t.Fatal("Materialize with a forced fuse-overlay selection: err = nil, want the probe-reason refusal")
	}
	if !strings.Contains(err.Error(), probe.Reason) {
		t.Errorf("Materialize error = %q, want it to carry the probe's recorded reason %q", err, probe.Reason)
	}
	if !strings.Contains(err.Error(), "/dev/fuse") {
		t.Errorf("Materialize error = %q, want it to name /dev/fuse", err)
	}
	if _, statErr := os.Stat(layerRoot); !os.IsNotExist(statErr) {
		t.Errorf("layer root exists after the refused materialize (stat err = %v), want no layer root created", statErr)
	}
}

// TestFuseNeverSelectedHere pins Decision 5's probe law on both
// of this sandbox's filesystems: the FUSE probe is recorded
// unavailable, with its reason, in every selection — the fixed
// order passes over the driver honestly, on tmpfs (where the
// kernel overlay serves) and on btrfs (where copy serves)
// alike.
func TestFuseNeverSelectedHere(t *testing.T) {
	tmpfsRoot, err := os.MkdirTemp("", ".carrier-fuse-tmpfs-")
	if err != nil {
		t.Fatalf("create tmpfs root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpfsRoot) })

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	btrfsRoot, err := os.MkdirTemp(wd, ".carrier-fuse-btrfs-")
	if err != nil {
		t.Fatalf("create btrfs root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(btrfsRoot) })

	for _, root := range []string{tmpfsRoot, btrfsRoot} {
		sel, err := carrier.Select(root)
		if err != nil {
			t.Fatalf("Select(%q): %v", root, err)
		}
		if sel.Driver == carrier.DriverFuseOverlay {
			t.Errorf("Select(%q) selected fuse-overlay, want it passed over (Decision 5)", root)
		}
		probe := probeOf(t, sel, carrier.DriverFuseOverlay)
		if probe.Available {
			t.Errorf("Select(%q): fuse-overlay probe reported available, want unavailable", root)
		}
		if !strings.Contains(probe.Reason, "/dev/fuse") {
			t.Errorf("Select(%q): fuse-overlay reason = %q, want it to name /dev/fuse", root, probe.Reason)
		}
	}
}
