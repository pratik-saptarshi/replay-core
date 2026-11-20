package carrier

import (
	"fmt"
	"os"
)

// driver is one carrier mechanism. The materialize/remove
// signatures arrive with the Layer type in Task 2 (T-F5-2); in
// this task every driver's materialize/remove returns a
// descriptive "driver not yet implemented" error.
type driver interface {
	probe(rootDir string) ProbeResult
	materialize() error
	remove() error
}

// stubDriver is a registered driver whose probe is real but whose
// materialize/remove are not yet implemented.
type stubDriver struct {
	id      DriverID
	probeFn func(rootDir string) ProbeResult
}

func (d stubDriver) probe(rootDir string) ProbeResult { return d.probeFn(rootDir) }

func (d stubDriver) materialize() error {
	return fmt.Errorf("carrier: %s driver not yet implemented", d.id)
}

func (d stubDriver) remove() error {
	return fmt.Errorf("carrier: %s driver not yet implemented", d.id)
}

// drivers is the carrier registry, in the fixed selection order
// (AC-F5-1: kernel overlay → FUSE overlay → reflink → copy).
// Package-internal tests may substitute it; the public Select
// always reads this list.
var drivers = []driver{
	stubDriver{id: DriverKernelOverlay, probeFn: probeKernelOverlay},
	stubDriver{id: DriverFuseOverlay, probeFn: probeFuse},
	stubDriver{id: DriverReflink, probeFn: probeReflink},
	stubDriver{id: DriverCopy, probeFn: probeCopy},
}

// probeCopy is the copy driver's probe: the floor mechanism needs
// nothing but a writable root, proven by creating and removing a
// scratch file in it.
func probeCopy(rootDir string) ProbeResult {
	pr := ProbeResult{Driver: DriverCopy}
	f, err := os.CreateTemp(rootDir, ".carrier-copy-probe-*")
	if err != nil {
		pr.Reason = fmt.Sprintf("copy: create scratch file in root: %v", err)
		return pr
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(name)
		pr.Reason = fmt.Sprintf("copy: close scratch file: %v", err)
		return pr
	}
	if err := os.Remove(name); err != nil {
		pr.Reason = fmt.Sprintf("copy: remove scratch file: %v", err)
		return pr
	}
	pr.Available = true
	return pr
}

// probeFuse is the FUSE-overlay driver's probe. It examines
// /dev/fuse and records what it finds, but reports the driver
// unavailable on every host in Phase 2 (Decision 5's law:
// availability is reported only when the mount exists to back
// it, and the mount is not implemented). The reason
// distinguishes the two cases: device absent vs. device present
// but mount not implemented.
func probeFuse(string) ProbeResult {
	pr := ProbeResult{Driver: DriverFuseOverlay}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		pr.Reason = "/dev/fuse absent: fuse-overlay mount not implemented"
		return pr
	}
	pr.Reason = "/dev/fuse present, mount not implemented: fuse-overlay unavailable"
	return pr
}
