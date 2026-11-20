//go:build !linux

package carrier

import (
	"fmt"
	"runtime"
)

// Non-Linux fallbacks: the kernel-overlay mount and the FICLONE
// ioctl are Linux mechanisms (Decision 2), so on any other
// platform both drivers report unavailable with the platform
// named as the reason.

func probeKernelOverlay(string) ProbeResult {
	return ProbeResult{Driver: DriverKernelOverlay, Reason: fmt.Sprintf("kernel-overlay: overlay mounts require Linux, this platform is %s", runtime.GOOS)}
}

func probeReflink(string) ProbeResult {
	return ProbeResult{Driver: DriverReflink, Reason: fmt.Sprintf("reflink: FICLONE requires Linux, this platform is %s", runtime.GOOS)}
}
