//go:build windows

package filesystem

import (
	"golang.org/x/sys/windows"
)

// Physical-volume free-space reserve bounds. The reserve is 1% of the volume,
// clamped to this range — so a 2TB disk keeps ~25GiB in hand and a small disk
// keeps at least 2GiB. Tunable; a config knob is a reasonable follow-up.
const (
	volumeReserveMin = 2 * 1024 * 1024 * 1024  // 2 GiB
	volumeReserveMax = 25 * 1024 * 1024 * 1024 // 25 GiB
)

// volumeHasHeadroom reports whether the physical volume backing path has more
// than the safety reserve of free space left.
//
// This is the last line of defense against a single runaway game server filling
// the disk and taking down the ENTIRE node — the wings SQLite DB, panel
// communication, and every other customer server — something the per-server,
// cache-lagged usage limit fundamentally cannot prevent (a server writes
// directly to disk between 150s usage walks). When the volume runs critically
// low, HasSpaceAvailable denies writes for every server, which trips the disk
// limiter and stops them gracefully rather than letting the OS volume fill.
//
// It fails OPEN: if the free space cannot be queried, it returns true so a
// transient API error never blocks all writes across the node.
func volumeHasHeadroom(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return true
	}
	var freeToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &totalBytes, &totalFree); err != nil {
		return true
	}

	reserve := uint64(totalBytes) / 100 // 1% of the volume
	if reserve < volumeReserveMin {
		reserve = volumeReserveMin
	} else if reserve > volumeReserveMax {
		reserve = volumeReserveMax
	}
	return freeToCaller > reserve
}
