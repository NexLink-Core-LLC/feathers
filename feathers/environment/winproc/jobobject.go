//go:build windows

package winproc

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/environment"
)

// CPU rate control flags (not exported by x/sys/windows). HARD_CAP enforces an
// absolute ceiling rather than a relative weight.
const (
	jobObjectCPURateControlEnable  = 0x00000001
	jobObjectCPURateControlHardCap = 0x00000004
)

// jobCPURateControlInformation mirrors JOBOBJECT_CPU_RATE_CONTROL_INFORMATION
// using the CpuRate variant of its union. CpuRate is expressed in 1/100 of a
// percent of TOTAL system CPU capacity (10000 == all cores).
type jobCPURateControlInformation struct {
	ControlFlags uint32
	CPURate      uint32
}

// jobLimits is the set of resource limits enforced on a server's Job Object.
type jobLimits struct {
	memoryBytes        uint64 // hard committed-memory cap; 0 = unlimited
	cpuRate            uint32 // 1/100 percent of total CPU; 0 = unlimited
	activeProcessLimit uint32 // anti-forkbomb cap; 0 = unlimited
}

// jobLimitsFor translates a server's Panel-defined limits into Job Object terms.
func jobLimitsFor(l environment.Limits) jobLimits {
	var jl jobLimits

	// Memory: use the overhead-adjusted bound (matches the Docker backend) so
	// runtimes like the JVM that briefly exceed their nominal limit aren't killed.
	if b := l.BoundedMemoryLimit(); b > 0 {
		jl.memoryBytes = uint64(b)
	}

	// CPU: Pterodactyl's CpuLimit is a percentage where 100 == one full core.
	// Windows wants a fraction of TOTAL capacity in 1/100-percent units, so divide
	// by the core count: rate = CpuLimit*100 / numCPU, clamped to [1,10000].
	if l.CpuLimit > 0 {
		n := int64(runtime.NumCPU())
		if n < 1 {
			n = 1
		}
		rate := l.CpuLimit * 100 / n
		if rate < 1 {
			rate = 1
		} else if rate > 10000 {
			rate = 10000
		}
		jl.cpuRate = uint32(rate)
	}

	if p := l.ProcessLimit(); p > 0 {
		jl.activeProcessLimit = uint32(p)
	}
	return jl
}

// stillActive is the value GetExitCodeProcess returns while a process is still
// running (STILL_ACTIVE / STATUS_PENDING). It is not exported by x/sys/windows.
const stillActive = 259

// jobBasicAccountingInformation mirrors the Win32
// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION structure, which x/sys/windows does
// not define. The *Time fields are in 100-nanosecond units. Field order and
// sizes are fixed by the ABI — do not reorder.
type jobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	ActiveProcesses           uint32
	TotalProcesses            uint32
	TotalTerminatedProcesses  uint32
}

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS (psapi.h). The SIZE_T
// fields are uintptr (8 bytes on amd64).
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var (
	modpsapi                 = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = modpsapi.NewProc("GetProcessMemoryInfo")
)

// createJobObject creates an unnamed Job Object and applies the given limits.
// It always sets JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so that closing the handle
// (Destroy) or the daemon exiting reliably tears down the entire process tree,
// plus the configured memory/CPU/active-process caps (see applyJobLimits).
func createJobObject(limits jobLimits) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	if err := applyJobLimits(job, limits); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// applyJobLimits configures (or reconfigures) a Job Object's limits. It always
// sets JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE for reliable tree-kill, optionally a
// hard committed-memory cap and active-process cap, and a hard CPU rate cap.
// Safe to call on a running job to update limits in place (used by InSituUpdate).
func applyJobLimits(job windows.Handle, limits jobLimits) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if limits.memoryBytes > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(limits.memoryBytes)
	}
	if limits.activeProcessLimit > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		info.BasicLimitInformation.ActiveProcessLimit = limits.activeProcessLimit
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return err
	}

	// CPU rate control lives in a separate info class. Setting ControlFlags to 0
	// disables any previously applied cap (e.g. when a limit is removed).
	cpu := jobCPURateControlInformation{}
	if limits.cpuRate > 0 {
		cpu.ControlFlags = jobObjectCPURateControlEnable | jobObjectCPURateControlHardCap
		cpu.CPURate = limits.cpuRate
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectCpuRateControlInformation,
		uintptr(unsafe.Pointer(&cpu)),
		uint32(unsafe.Sizeof(cpu)),
	); err != nil {
		return err
	}
	return nil
}

// queryJobAccounting reads cumulative CPU accounting for every process that is
// (or was) a member of the job. Used to compute CPU usage deltas.
func queryJobAccounting(job windows.Handle) (jobBasicAccountingInformation, error) {
	var info jobBasicAccountingInformation
	err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	)
	return info, err
}

// processWorkingSet returns the resident memory (working set) of a single
// process in bytes, or 0 if it cannot be read. This reports one process only;
// jobWorkingSet sums it across an entire job for multi-process servers.
func processWorkingSet(h windows.Handle) uint64 {
	var pmc processMemoryCounters
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.CB))
	if r == 0 {
		return 0
	}
	return uint64(pmc.WorkingSetSize)
}

// jobProcessIDs returns the PIDs of every process currently assigned to the
// job. JOBOBJECT_BASIC_PROCESS_ID_LIST is a variable-length structure (a
// two-DWORD header followed by an inline ULONG_PTR array), so the buffer is
// grown and the query retried until the full list fits.
func jobProcessIDs(job windows.Handle) ([]uint32, error) {
	const headerBytes = 8 // NumberOfAssignedProcesses + NumberOfProcessIdsInList; the ULONG_PTR array is already 8-aligned at offset 8
	ptrSize := int(unsafe.Sizeof(uintptr(0)))

	capacity := 64
	for {
		buf := make([]byte, headerBytes+capacity*ptrSize)
		err := windows.QueryInformationJobObject(
			job,
			windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&buf[0])),
			uint32(len(buf)),
			nil,
		)
		if err != nil {
			// The buffer was too small to hold every PID; double it and retry.
			if err == windows.ERROR_MORE_DATA {
				capacity *= 2
				continue
			}
			return nil, err
		}
		count := *(*uint32)(unsafe.Pointer(&buf[4])) // NumberOfProcessIdsInList
		ids := make([]uint32, 0, count)
		for i := 0; i < int(count); i++ {
			pid := *(*uintptr)(unsafe.Pointer(&buf[headerBytes+i*ptrSize]))
			ids = append(ids, uint32(pid))
		}
		return ids, nil
	}
}

// jobWorkingSet sums the resident memory (working set) of every process in the
// job, giving an accurate figure for multi-process game servers — e.g. Unreal
// titles like The Isle, whose thin launcher spawns the real work in a child
// *-Win64-Shipping.exe. It returns 0 if the job's process list cannot be read,
// so callers can fall back to the main-process figure.
func jobWorkingSet(job windows.Handle) uint64 {
	pids, err := jobProcessIDs(job)
	if err != nil {
		return 0
	}
	var total uint64
	for _, pid := range pids {
		if pid == 0 {
			continue
		}
		// LocalSystem (the daemon) can open any process; QUERY_INFORMATION +
		// VM_READ are what GetProcessMemoryInfo requires.
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
		if err != nil {
			continue
		}
		total += processWorkingSet(h)
		_ = windows.CloseHandle(h)
	}
	return total
}

// processExitCode returns the exit code of the process referenced by h and
// whether it is still running.
func processExitCode(h windows.Handle) (code uint32, running bool, err error) {
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return 0, false, err
	}
	return code, code == stillActive, nil
}
