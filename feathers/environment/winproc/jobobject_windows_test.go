//go:build windows

package winproc

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestApplyJobLimits verifies the memory, CPU-rate, and active-process limits are
// actually written to a Job Object. Creating and querying your own job object is
// unprivileged, so this runs without elevation.
func TestApplyJobLimits(t *testing.T) {
	const memBytes = uint64(256 * 1024 * 1024)
	const cpuRate = uint32(5000) // 50% of total CPU
	const procCap = uint32(16)

	job, err := createJobObject(jobLimits{memoryBytes: memBytes, cpuRate: cpuRate, activeProcessLimit: procCap})
	if err != nil {
		t.Fatalf("createJobObject: %v", err)
	}
	defer windows.CloseHandle(job)

	// Extended limit info: memory cap, flags, active-process cap.
	var ext windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(
		job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext)), nil,
	); err != nil {
		t.Fatalf("query extended limit info: %v", err)
	}
	if ext.JobMemoryLimit != uintptr(memBytes) {
		t.Errorf("JobMemoryLimit = %d, want %d", ext.JobMemoryLimit, memBytes)
	}
	if ext.BasicLimitInformation.ActiveProcessLimit != procCap {
		t.Errorf("ActiveProcessLimit = %d, want %d", ext.BasicLimitInformation.ActiveProcessLimit, procCap)
	}
	flags := ext.BasicLimitInformation.LimitFlags
	for name, f := range map[string]uint32{
		"KILL_ON_JOB_CLOSE": windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		"JOB_MEMORY":        windows.JOB_OBJECT_LIMIT_JOB_MEMORY,
		"ACTIVE_PROCESS":    windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS,
	} {
		if flags&f == 0 {
			t.Errorf("missing limit flag %s (0x%x)", name, f)
		}
	}

	// CPU rate control (separate info class).
	var cpu jobCPURateControlInformation
	if err := windows.QueryInformationJobObject(
		job, windows.JobObjectCpuRateControlInformation,
		uintptr(unsafe.Pointer(&cpu)), uint32(unsafe.Sizeof(cpu)), nil,
	); err != nil {
		t.Fatalf("query cpu rate control info: %v", err)
	}
	if cpu.CPURate != cpuRate {
		t.Errorf("CPURate = %d, want %d", cpu.CPURate, cpuRate)
	}
	if cpu.ControlFlags&jobObjectCPURateControlEnable == 0 || cpu.ControlFlags&jobObjectCPURateControlHardCap == 0 {
		t.Errorf("CPU rate control flags = 0x%x, want ENABLE|HARD_CAP set", cpu.ControlFlags)
	}
}
