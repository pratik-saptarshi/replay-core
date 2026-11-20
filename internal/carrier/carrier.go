// Package carrier is the F5 carrier protocol: it materializes a
// workspace tree as a writable layer over a base directory,
// through one of four probed drivers tried in a fixed order. The
// carrier is directory-level only — it never sees Git or the
// trace (plan Decision 1); settlement (internal/settle)
// orchestrates it against the snapshot store.
package carrier

import (
	"fmt"
	"os"
)

// DriverID names one carrier driver.
type DriverID string

const (
	DriverKernelOverlay DriverID = "kernel-overlay"
	DriverFuseOverlay   DriverID = "fuse-overlay"
	DriverReflink       DriverID = "reflink"
	DriverCopy          DriverID = "copy"
)

// ProbeResult is one driver's probed verdict for one root.
type ProbeResult struct {
	Driver    DriverID
	Available bool
	Reason    string // why not, when Available is false; "" when available
}

// Selection is the selection record (AC-F5-1): the chosen driver
// and every probe that led to it, in selection order.
type Selection struct {
	Driver DriverID
	Probes []ProbeResult
}

// Select probes every registered driver against rootDir in the
// fixed order (kernel overlay, FUSE overlay, reflink, copy) and
// selects the first available. Copy is the floor: it is available
// on any writable root. An error is returned only when rootDir
// itself is unusable (missing, not a directory, unwritable) —
// never as a substitute for a recorded probe failure.
//
// Selection is computed fresh on every call — no caching: a
// verdict about one filesystem says nothing about another
// (Global Constraints, carrier honesty).
func Select(rootDir string) (Selection, error) {
	info, err := os.Stat(rootDir)
	if err != nil {
		return Selection{}, fmt.Errorf("carrier: select root %q: %w", rootDir, err)
	}
	if !info.IsDir() {
		return Selection{}, fmt.Errorf("carrier: select root %q: not a directory", rootDir)
	}
	sel := Selection{}
	var copyResult ProbeResult
	for _, d := range drivers {
		pr := d.probe(rootDir)
		sel.Probes = append(sel.Probes, pr)
		if pr.Driver == DriverCopy {
			copyResult = pr
		}
		if pr.Available && sel.Driver == "" {
			sel.Driver = pr.Driver
		}
	}
	if sel.Driver == "" {
		return Selection{}, fmt.Errorf("carrier: root %q is unusable: no driver can serve it (copy probe: %s)", rootDir, copyResult.Reason)
	}
	return sel, nil
}
