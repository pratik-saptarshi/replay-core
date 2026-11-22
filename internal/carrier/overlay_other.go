//go:build !linux

package carrier

// kernelOverlayDriver returns the registered kernel-overlay
// driver for this platform: off Linux there is no mount to
// build, so the driver keeps its stub form — a real probe (the
// platform refusal) and a materialize that returns that refusal
// descriptively (plan Task 3).
func kernelOverlayDriver() driver {
	return stubDriver{driverID: DriverKernelOverlay, probeFn: probeKernelOverlay}
}
