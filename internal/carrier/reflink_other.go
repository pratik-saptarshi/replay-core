//go:build !linux

package carrier

// reflinkDriver returns the registered reflink driver for this
// platform: off Linux there is no FICLONE to build with, so the
// driver keeps its stub form — a real probe (the platform
// refusal) and a materialize that returns that refusal
// descriptively (plan Task 5).
func reflinkDriver() driver {
	return stubDriver{driverID: DriverReflink, probeFn: probeReflink}
}
