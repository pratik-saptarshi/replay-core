package carrier

import "fmt"

// fuseDriver is the FUSE-overlay driver in its Phase 2 form
// (Decision 5): protocol-registered and probe-honest, with no
// mount behind it. Its probe (probeFuse, in driver.go) examines
// /dev/fuse and records what it finds, but reports the driver
// unavailable on every host until the mount exists to back it —
// the probe and the mount land together or not at all.
// materialize and remove therefore refuse with an error
// carrying the probe's recorded reason, so a forced selection
// fails honestly instead of pretending to serve.
//
// AC-F5-9 is owned by this driver as its build home and is
// recorded host-deferred, NOT discharged (Decision 7):
// /dev/fuse is absent in this sandbox, so the driver cannot be
// exercised here at all — no test in this environment could
// distinguish a correct FUSE mount from a plausible-looking
// one, and unexercisable code does not ship as if proven.
//
// Host follow-up: on a host with /dev/fuse present, the mount
// implementation lands in this driver against the same
// isolation contract as AC-F5-5, and only then does the probe
// start reporting availability — never before the mount exists
// to back the report.
type fuseDriver struct{}

func (fuseDriver) id() DriverID { return DriverFuseOverlay }

func (fuseDriver) probe(rootDir string) ProbeResult { return probeFuse(rootDir) }

func (fuseDriver) materialize(string, string) error { return fuseRefusal() }

func (fuseDriver) remove(*Layer) error { return fuseRefusal() }

// fuseRefusal is the refusal every FUSE-overlay operation
// returns in Phase 2: the probe's own recorded reason, verbatim,
// so the failure a forced selection meets is exactly the verdict
// selection recorded — "/dev/fuse absent" in this sandbox,
// "mount not implemented" on a host that has the device.
func fuseRefusal() error {
	return fmt.Errorf("carrier: fuse-overlay: %s", probeFuse("").Reason)
}
