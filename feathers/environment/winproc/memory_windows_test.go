//go:build windows

package winproc

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// TestJobWorkingSetSumsProcesses verifies that jobProcessIDs enumerates every
// process in a job and that jobWorkingSet sums memory across all of them (not
// just one) — the multi-process case that Unreal game servers hit. Creating and
// querying your own job object is unprivileged, so this runs without elevation.
func TestJobWorkingSetSumsProcesses(t *testing.T) {
	job, err := createJobObject(jobLimits{})
	if err != nil {
		t.Fatalf("createJobObject: %v", err)
	}
	defer windows.CloseHandle(job) // KILL_ON_JOB_CLOSE reaps any stragglers

	const want = 2
	var started []*exec.Cmd
	defer func() {
		for _, c := range started {
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
		}
	}()

	for i := 0; i < want; i++ {
		// ping loops long enough to stay alive for the whole test and holds a
		// small but non-zero working set.
		cmd := exec.Command("ping", "-n", "30", "127.0.0.1")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child %d: %v", i, err)
		}
		started = append(started, cmd)

		h, err := windows.OpenProcess(windows.PROCESS_ALL_ACCESS, false, uint32(cmd.Process.Pid))
		if err != nil {
			t.Fatalf("open child %d: %v", i, err)
		}
		if err := windows.AssignProcessToJobObject(job, h); err != nil {
			_ = windows.CloseHandle(h)
			t.Fatalf("assign child %d to job: %v", i, err)
		}
		_ = windows.CloseHandle(h)
	}

	ids, err := jobProcessIDs(job)
	if err != nil {
		t.Fatalf("jobProcessIDs: %v", err)
	}
	// At least the processes we started must be present. The OS may auto-assign
	// helper processes (e.g. a hidden conhost.exe per console app) into the job
	// too — which is exactly why summing across the whole job matters.
	if len(ids) < want {
		t.Errorf("jobProcessIDs returned %d pids, want at least %d", len(ids), want)
	}

	// The sum across the job must be strictly larger than any single process's
	// working set — proving it actually summed rather than reporting one.
	sum := jobWorkingSet(job)
	if sum == 0 {
		t.Fatalf("jobWorkingSet = 0, want > 0")
	}
	var single uint64
	if len(ids) > 0 {
		if h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, ids[0]); err == nil {
			single = processWorkingSet(h)
			_ = windows.CloseHandle(h)
		}
	}
	if single > 0 && sum <= single {
		t.Errorf("jobWorkingSet = %d, expected greater than a single process (%d)", sum, single)
	}
	t.Logf("summed working set across %d procs = %d bytes (single = %d)", len(ids), sum, single)
}
