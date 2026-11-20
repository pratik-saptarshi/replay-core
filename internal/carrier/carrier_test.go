package carrier

import (
	"errors"
	"testing"
)

// fakeDriver is a registry substitute for the protocol-core tests:
// its probe verdict is fixed at construction, so the selection
// logic can be exercised against hostile drivers without touching
// a real filesystem mechanism.
type fakeDriver struct {
	result ProbeResult
}

func (f fakeDriver) probe(string) ProbeResult { return f.result }
func (f fakeDriver) materialize() error       { return errors.New("carrier: fake driver") }
func (f fakeDriver) remove() error            { return errors.New("carrier: fake driver") }

// TestSelectFallsThroughWithRecordedReasons is AC-F5-1 against a
// stubbed registry (input shape: every mechanism driver hostile):
// selection falls through the fixed order to the copy floor, and
// every failing probe's reason is recorded verbatim in the
// selection record — never silently replaced.
func TestSelectFallsThroughWithRecordedReasons(t *testing.T) {
	saved := drivers
	drivers = []driver{
		fakeDriver{result: ProbeResult{Driver: DriverKernelOverlay, Available: false, Reason: "overlay reason one"}},
		fakeDriver{result: ProbeResult{Driver: DriverFuseOverlay, Available: false, Reason: "fuse reason two"}},
		fakeDriver{result: ProbeResult{Driver: DriverReflink, Available: false, Reason: "reflink reason three"}},
		fakeDriver{result: ProbeResult{Driver: DriverCopy, Available: true}},
	}
	defer func() { drivers = saved }()

	sel, err := Select(t.TempDir())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Driver != DriverCopy {
		t.Errorf("selected driver = %q, want %q", sel.Driver, DriverCopy)
	}
	if len(sel.Probes) != 4 {
		t.Fatalf("recorded probes = %d, want 4", len(sel.Probes))
	}
	wantOrder := []DriverID{DriverKernelOverlay, DriverFuseOverlay, DriverReflink, DriverCopy}
	wantReasons := []string{"overlay reason one", "fuse reason two", "reflink reason three", ""}
	for i, pr := range sel.Probes {
		if pr.Driver != wantOrder[i] {
			t.Errorf("probe %d driver = %q, want %q (fixed selection order)", i, pr.Driver, wantOrder[i])
		}
		if pr.Reason != wantReasons[i] {
			t.Errorf("probe %d (%s) reason = %q, want %q verbatim", i, pr.Driver, pr.Reason, wantReasons[i])
		}
		if pr.Available && pr.Driver != DriverCopy {
			t.Errorf("probe %d (%s) reported available, want unavailable", i, pr.Driver)
		}
		if pr.Driver == sel.Driver && pr.Driver != DriverCopy {
			t.Errorf("failing driver %s was selected", pr.Driver)
		}
	}
	if !sel.Probes[3].Available {
		t.Errorf("copy probe reported unavailable, want available")
	}
}
