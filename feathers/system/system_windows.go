//go:build windows

package system

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GetSystemInformation reports host information on Windows. Unlike the Linux
// build it does NOT contact Docker: the winproc backend runs game servers as
// native Windows processes, so there is no Docker daemon to query. The Docker
// section of the response is therefore left zeroed. This keeps the /api/system
// endpoint — which the Panel polls to determine node health — returning 200
// instead of failing on the absent Docker named pipe.
func GetSystemInformation() (*Information, error) {
	major, minor, build := windows.RtlGetNtVersionNumbers()
	version := fmt.Sprintf("%d.%d.%d", major, minor, build)

	return &Information{
		Version: Version,
		// Docker is intentionally the zero value on Windows.
		System: System{
			Architecture:  runtime.GOARCH,
			CPUThreads:    runtime.NumCPU(),
			MemoryBytes:   totalPhysicalMemory(),
			KernelVersion: version,
			OS:            "Microsoft Windows " + version,
			OSType:        runtime.GOOS,
		},
	}, nil
}

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX structure used by
// GlobalMemoryStatusEx (see kernel32). Field order and sizes are fixed by the
// ABI; do not reorder.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// totalPhysicalMemory returns total physical RAM in bytes via
// GlobalMemoryStatusEx. The value is informational on the node page, so any
// failure degrades gracefully to 0 rather than failing the request.
func totalPhysicalMemory() int64 {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))

	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
	if r, _, _ := proc.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0
	}
	return int64(m.TotalPhys)
}
