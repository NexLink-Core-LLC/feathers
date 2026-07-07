//go:build windows

package winproc

import (
	"context"
	"math"
	"time"

	"github.com/pterodactyl/wings/environment"
	"golang.org/x/sys/windows"
)

// resourcePollInterval is how often resource usage is sampled and published.
// It matches the cadence the Panel expects from the Docker backend's stats
// stream closely enough for smooth graphs.
const resourcePollInterval = 2 * time.Second

// pollResources samples the server process's resource usage on a fixed cadence
// and publishes a ResourceEvent, mirroring the Docker backend. It runs until the
// provided context is canceled (by wait() when the process exits) or the server
// enters the offline state.
//
// Memory is the working set summed across every process in the Job Object (so
// multi-process servers like Unreal titles report their true usage), falling
// back to the main process if the job's process list can't be read. CPU is
// computed from the Job Object's cumulative user+kernel time deltas. Network
// rx/tx are summed across the server's whole process tree from an ETW monitor on
// the Microsoft-Windows-Kernel-Network provider (TCP + UDP); if ETW is
// unavailable the monitor degrades to reporting 0 without ever failing the poll.
func (e *Environment) pollResources(ctx context.Context) error {
	ticker := time.NewTicker(resourcePollInterval)
	defer ticker.Stop()

	// Lazily start the ETW network monitor. Idempotent and non-fatal: any
	// failure is logged once inside and leaves network reporting 0.
	monitor.ensureStarted()

	memLimit := uint64(e.Configuration.Limits().MemoryLimit) * 1024 * 1024

	var (
		prevCPU  int64
		prevTime time.Time
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if e.st.Load() == environment.ProcessOfflineState {
				return nil
			}

			e.mu.RLock()
			job := e.job
			proc := e.procHandle
			e.mu.RUnlock()

			// Sum the working set across the whole job so multi-process servers
			// (e.g. an Unreal launcher that spawns a child *-Shipping.exe) report
			// their true memory. Fall back to the main process if the job's
			// process list can't be read.
			var mem uint64
			if job != 0 {
				mem = jobWorkingSet(job)
			}
			if mem == 0 && proc != 0 {
				mem = processWorkingSet(proc)
			}

			var cpuAbs float64
			if job != 0 {
				if acct, err := queryJobAccounting(job); err == nil {
					// TotalUserTime/TotalKernelTime are cumulative, in 100ns units.
					totalCPU := acct.TotalUserTime + acct.TotalKernelTime
					now := time.Now()
					if !prevTime.IsZero() {
						if wall := now.Sub(prevTime).Seconds(); wall > 0 {
							cpuSeconds := float64(totalCPU-prevCPU) / 1e7
							// (cpu-seconds used / wall-seconds) * 100 yields an
							// absolute percentage where 200 == two full cores, matching
							// the Docker backend's cpu_absolute semantics.
							cpuAbs = math.Round((cpuSeconds/wall)*100*1000) / 1000
						}
					}
					prevCPU = totalCPU
					prevTime = now
				}
			}

			// Enumerate the server's whole process tree from the job so network
			// is summed across the main process and any children it spawned. Fall
			// back to just the main process's PID if enumeration fails.
			var pids []uint32
			if job != 0 {
				if list, err := jobProcessIDs(job); err == nil {
					pids = list
				}
			}
			if len(pids) == 0 && proc != 0 {
				if mainPID, err := windows.GetProcessId(proc); err == nil && mainPID != 0 {
					pids = []uint32{mainPID}
				}
			}

			var rx, tx uint64
			if len(pids) > 0 {
				monitor.registerPIDs(pids)
				rx, tx = monitor.networkBytesFor(pids)
			}

			uptime, _ := e.Uptime(ctx)

			e.Events().Publish(environment.ResourceEvent, environment.Stats{
				Uptime:      uptime,
				Memory:      mem,
				MemoryLimit: memLimit,
				CpuAbsolute: cpuAbs,
				Network:     environment.NetworkStats{RxBytes: rx, TxBytes: tx},
			})
		}
	}
}
