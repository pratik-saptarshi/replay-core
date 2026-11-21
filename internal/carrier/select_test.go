package carrier_test

import (
	"os"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/carrier"
)

// probeOf returns the recorded probe for one driver, failing the
// test if the selection record does not carry it.
func probeOf(t *testing.T, sel carrier.Selection, id carrier.DriverID) carrier.ProbeResult {
	t.Helper()
	for _, pr := range sel.Probes {
		if pr.Driver == id {
			return pr
		}
	}
	t.Fatalf("selection record has no probe for %s", id)
	return carrier.ProbeResult{}
}

// assertNoScratchRemains pins the probe law: scratch is removed
// on every path, so a probed root is left exactly as found.
func assertNoScratchRemains(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read probe root: %v", err)
	}
	for _, e := range entries {
		t.Errorf("probe scratch remains under root: %s", e.Name())
	}
}

// TestSelectProbeTruthOnBtrfsRoot is AC-F5-2 on the workspace
// filesystem (path shape: the verdict depends on the filesystem).
// The root is a scratch dir under the working directory — the
// repo tree lives on the btrfs workspace filesystem (baseline
// §2), where overlay mounts fail and FICLONE is refused.
func TestSelectProbeTruthOnBtrfsRoot(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	root, err := os.MkdirTemp(wd, ".carrier-probe-btrfs-")
	if err != nil {
		t.Fatalf("create btrfs probe root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	sel, err := carrier.Select(root)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Driver != carrier.DriverCopy {
		t.Errorf("selected driver = %q, want %q", sel.Driver, carrier.DriverCopy)
	}
	overlay := probeOf(t, sel, carrier.DriverKernelOverlay)
	if overlay.Available {
		t.Errorf("kernel-overlay probe reported available on btrfs, want unavailable")
	}
	if !strings.Contains(strings.ToLower(overlay.Reason), "mount") {
		t.Errorf("kernel-overlay reason = %q, want a mount-failure reason", overlay.Reason)
	}
	fuse := probeOf(t, sel, carrier.DriverFuseOverlay)
	if fuse.Available {
		t.Errorf("fuse-overlay probe reported available, want unavailable (Decision 5)")
	}
	if !strings.Contains(fuse.Reason, "/dev/fuse") {
		t.Errorf("fuse-overlay reason = %q, want it to name /dev/fuse", fuse.Reason)
	}
	reflink := probeOf(t, sel, carrier.DriverReflink)
	if reflink.Available {
		t.Errorf("reflink probe reported available on btrfs, want unavailable")
	}
	if !strings.Contains(reflink.Reason, "FICLONE") {
		t.Errorf("reflink reason = %q, want it to name the FICLONE refusal", reflink.Reason)
	}
	if copy := probeOf(t, sel, carrier.DriverCopy); !copy.Available {
		t.Errorf("copy probe reported unavailable on a writable root: %q", copy.Reason)
	}
	assertNoScratchRemains(t, root)
}

// TestSelectProbeTruthOnTmpfsRoot is AC-F5-2 on tmpfs. The root
// is a scratch dir under os.TempDir() — /tmp, a tmpfs in this
// sandbox (baseline §1), where overlay mounts succeed.
func TestSelectProbeTruthOnTmpfsRoot(t *testing.T) {
	root, err := os.MkdirTemp("", ".carrier-probe-tmpfs-")
	if err != nil {
		t.Fatalf("create tmpfs probe root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	sel, err := carrier.Select(root)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	overlay := probeOf(t, sel, carrier.DriverKernelOverlay)
	if !overlay.Available {
		t.Errorf("kernel-overlay probe reported unavailable on tmpfs (%q), want available", overlay.Reason)
	}
	if overlay.Reason != "" {
		t.Errorf("kernel-overlay reason = %q, want empty for an available driver", overlay.Reason)
	}
	if sel.Driver != carrier.DriverKernelOverlay {
		t.Errorf("selected driver = %q, want %q (the fixed order's head)", sel.Driver, carrier.DriverKernelOverlay)
	}
	assertNoScratchRemains(t, root)
}
