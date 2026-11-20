package carrier

import (
	"fmt"
	"os"
)

// driver is one carrier mechanism. The shared lifecycle
// (layer.go) owns the marker law, staging, and layer-root
// deletion; a driver owns only its mechanism: materialize
// populates an existing empty view directory from a base
// directory, and remove releases any driver state (mounts) a
// layer holds before the lifecycle deletes its root.
type driver interface {
	id() DriverID
	probe(rootDir string) ProbeResult
	materialize(baseDir, viewDir string) error
	remove(l *Layer) error
}

// viewDriver is a driver that manages the view directory
// itself: a mountpoint cannot be built in a staging sibling and
// renamed into place, so the shared lifecycle creates the view
// at its final path in the layer root and the driver populates
// it there, keeping its own state as the view's siblings. The
// lifecycle still owns the marker law and the layer root's
// deletion.
type viewDriver interface {
	driver
	materializeView(baseDir, layerRoot, viewDir string) error
}

// remnantRecoverer is a driver that can recognize and recover
// its own crash remnant in a markerless layer root: the exact
// layout a crash between the driver's state creation and the
// marker write leaves behind. recoverRemnant reports whether
// the root held exactly that layout; a recognized remnant has
// its driver state released and its contents wiped, and true is
// returned so the caller proceeds to build. Any other shape
// returns false and stays subject to the foreign-directory law
// — recognition is by exact layout, never by emptiness or name
// similarity.
type remnantRecoverer interface {
	recoverRemnant(layerRoot string) (bool, error)
}

// driverByID returns the registered driver with the given ID.
func driverByID(id DriverID) (driver, bool) {
	for _, d := range drivers {
		if d.id() == id {
			return d, true
		}
	}
	return nil, false
}

// stubDriver is a registered driver whose probe is real but whose
// materialize/remove are not yet implemented; it reports a
// descriptive "driver not yet implemented" error rather than
// pretending to serve.
type stubDriver struct {
	driverID DriverID
	probeFn  func(rootDir string) ProbeResult
}

func (d stubDriver) id() DriverID { return d.driverID }

func (d stubDriver) probe(rootDir string) ProbeResult { return d.probeFn(rootDir) }

func (d stubDriver) materialize(string, string) error {
	return fmt.Errorf("carrier: %s driver not yet implemented", d.driverID)
}

func (d stubDriver) remove(*Layer) error {
	return fmt.Errorf("carrier: %s driver not yet implemented", d.driverID)
}

// drivers is the carrier registry, in the fixed selection order
// (AC-F5-1: kernel overlay → FUSE overlay → reflink → copy).
// Package-internal tests may substitute it; the public Select
// always reads this list.
var drivers = []driver{
	kernelOverlayDriver(),
	stubDriver{driverID: DriverFuseOverlay, probeFn: probeFuse},
	stubDriver{driverID: DriverReflink, probeFn: probeReflink},
	copyDriver{},
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
