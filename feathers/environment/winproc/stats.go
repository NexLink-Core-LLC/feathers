//go:build windows

package winproc

import (
	"context"
	"math"
	"time"

	"github.com/pterodactyl/wings/environment"
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
// Memory is the main process's working set (child-process summing is a Phase-2
// limitation). CPU is computed from the Job Object's cumulative user+kernel time
// deltas. Network counters are a known Windows parity gap (§8.1) and report 0.
func (e *Environment) pollResources(ctx context.Context) error {
	ticker := time.NewTicker(resourcePollInterval)
	defer ticker.Stop()

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

			var mem uint64
			if proc != 0 {
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

			uptime, _ := e.Uptime(ctx)

			e.Events().Publish(environment.ResourceEvent, environment.Stats{
				Uptime:      uptime,
				Memory:      mem,
				MemoryLimit: memLimit,
				CpuAbsolute: cpuAbs,
				Network:     environment.NetworkStats{},
			})
		}
	}
}
