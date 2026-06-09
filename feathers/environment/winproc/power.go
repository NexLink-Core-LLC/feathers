//go:build windows

package winproc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"emperror.dev/errors"
	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/remote"
)

// mergeChildEnv builds the environment for a spawned process: the daemon's base
// Windows environment (so SystemRoot, SystemDrive, PATH, ProgramData, etc. are
// present — native Windows binaries and SteamCMD require them, and without them
// unexpanded %VAR% references create junk like a "%SystemDrive%" folder) overlaid
// with the server's egg variables, then TEMP/TMP pointed at a writable per-server
// directory. Keys are merged case-insensitively, since Windows environment
// variable names are case-insensitive (avoids duplicate Path/PATH entries).
func mergeChildEnv(eggVars []string, tmpDir string) []string {
	byKey := make(map[string]string)
	var order []string
	put := func(kv string) {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			return
		}
		key := strings.ToUpper(kv[:i])
		if _, ok := byKey[key]; !ok {
			order = append(order, key)
		}
		byKey[key] = kv
	}
	for _, kv := range os.Environ() {
		put(kv)
	}
	for _, kv := range eggVars {
		put(kv)
	}
	put("TEMP=" + tmpDir)
	put("TMP=" + tmpDir)

	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

// Start launches the server process, assigns it to the server's Job Object, and
// begins streaming its console and resource usage. It mirrors the Docker
// backend's contract: it does NOT block until the server is "running" — the
// transition to the running state is driven by the server layer once it sees
// the egg's start-completion output on the console.
func (e *Environment) Start(ctx context.Context) error {
	sawError := false
	defer func() {
		if sawError {
			// Move through stopping first so crash detection is not triggered for
			// a failure that happened during boot.
			e.SetState(environment.ProcessStoppingState)
			e.SetState(environment.ProcessOfflineState)
		}
	}()

	// If a process is already running just make sure our state reflects that.
	if running, _ := e.IsRunning(ctx); running {
		e.SetState(environment.ProcessRunningState)
		return nil
	}

	e.SetState(environment.ProcessStartingState)
	sawError = true

	// Recreate the Job Object with the latest configuration before booting.
	if err := e.OnBeforeStart(ctx); err != nil {
		return errors.WrapIf(err, "winproc: failed to run pre-boot process")
	}

	// Resolve the startup invocation into an argv slice (placeholder
	// substitution + tokenize). See docs/phase0-conformance-spec.md §1.
	vars := e.Configuration.EnvironmentVariables()
	argv, err := buildCommand(vars)
	if err != nil {
		return errors.WrapIf(err, "winproc: failed to build startup command")
	}

	// Clear the previous boot's console so we don't replay stale output.
	e.console.reset()

	// Obtain a primary token for the per-server restricted account. The server
	// process runs under this account (NOT the daemon's LocalSystem), sandboxed by
	// the NTFS ACLs applied in Create(). The token can be closed as soon as the
	// process is created — the new process holds its own reference.
	token, err := e.logonToken()
	if err != nil {
		return errors.Wrap(err, "winproc: failed to log on as server account")
	}
	defer token.Close()

	// NB: deliberately NOT exec.CommandContext — the process must outlive the
	// (often short-lived) power-action context. Lifecycle is owned by the Job
	// Object, not ctx.
	// Give the sandboxed process a writable temp directory inside its own volume.
	// As a restricted account it cannot write the system temp dir, so without this
	// anything using temp files (e.g. JNA extracting native libs, SteamCMD) fails
	// with "access denied". The dir inherits the volume's ACLs, so the account
	// owns it.
	tmpDir := filepath.Join(e.directory(), "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return errors.Wrap(err, "winproc: failed to create server temp directory")
	}
	env := mergeChildEnv(vars, tmpDir)

	// Resolve the executable. A non-absolute argv[0] is preferentially resolved
	// against the server directory — where SteamCMD-installed game binaries live
	// (e.g. TheIsleServer.exe) — before falling back to PATH lookup (e.g. "java").
	// Without this, exec.Command would only search PATH and miss in-tree binaries.
	exe := argv[0]
	if !filepath.IsAbs(exe) {
		if cand := filepath.Join(e.directory(), exe); fileExists(cand) {
			exe = cand
		}
	}

	cmd := exec.Command(exe, argv[1:]...)
	cmd.Dir = e.directory()
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Run as the restricted per-server account.
		Token: syscall.Token(token),
		// CREATE_NO_WINDOW prevents console game servers from popping a window on
		// the host — part of the headless service model.
		CreationFlags: windows.CREATE_NO_WINDOW,
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return errors.Wrap(err, "winproc: failed to open stdin pipe")
	}

	// Merge stdout+stderr into a single pipe so console ordering is preserved.
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return errors.Wrap(err, "winproc: failed to open output pipe")
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		_ = stdin.Close()
		return errors.Wrap(err, "winproc: failed to start server process")
	}
	// The child has its own copy of the write end now; the parent must close its
	// copy so the read end sees EOF when the child exits.
	_ = pw.Close()

	// Open a handle for stats/termination and bind the process to the Job Object
	// so it (and any children it spawns) are killed together.
	procHandle, herr := windows.OpenProcess(windows.PROCESS_ALL_ACCESS, false, uint32(cmd.Process.Pid))
	if herr != nil {
		e.log().WithField("error", herr).Warn("winproc: could not open process handle (stats/terminate may be degraded)")
	}
	e.mu.RLock()
	job := e.job
	e.mu.RUnlock()
	if job != 0 && procHandle != 0 {
		if err := windows.AssignProcessToJobObject(job, procHandle); err != nil {
			e.log().WithField("error", err).Warn("winproc: failed to assign process to job object")
		}
	}

	pollCtx, cancel := context.WithCancel(context.Background())

	e.mu.Lock()
	e.cmd = cmd
	e.stdin = stdin
	e.procHandle = procHandle
	e.startedAt = time.Now()
	e.pollCancel = cancel
	e.mu.Unlock()

	// streamConsole owns pr and closes it on EOF — do not close pr elsewhere.
	go e.streamConsole(pr)
	go func() {
		if err := e.pollResources(pollCtx); err != nil && !errors.Is(err, context.Canceled) {
			e.log().WithField("error", err).Warn("winproc: resource polling stopped with error")
		}
	}()
	go e.wait(cmd)

	sawError = false
	return nil
}

// wait reaps the process, records its exit code, tears down the per-process
// resources, and marks the server offline. It does NOT touch the console pipe;
// streamConsole owns and closes that (see the note there).
func (e *Environment) wait(cmd *exec.Cmd) {
	werr := cmd.Wait()

	var code uint32
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code = uint32(ee.ExitCode())
		} else {
			code = 1
		}
	}

	e.mu.Lock()
	e.lastExit = code
	stdin := e.stdin
	procHandle := e.procHandle
	cancel := e.pollCancel
	e.cmd = nil
	e.stdin = nil
	e.procHandle = 0
	e.pollCancel = nil
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if procHandle != 0 {
		_ = windows.CloseHandle(procHandle)
	}

	e.SetState(environment.ProcessOfflineState)
}

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// IsRunning reports whether the server process is currently alive.
func (e *Environment) IsRunning(ctx context.Context) (bool, error) {
	e.mu.RLock()
	h := e.procHandle
	hasCmd := e.cmd != nil
	e.mu.RUnlock()

	if !hasCmd || h == 0 {
		return false, nil
	}
	_, running, err := processExitCode(h)
	if err != nil {
		// If we can't query the handle, treat the process as gone rather than
		// surfacing an error into the boot/power path.
		return false, nil
	}
	return running, nil
}

// Stop attempts to stop the server using its configured stop method. Command
// stops are written to stdin; everything else (signal stop, or no
// configuration) falls back to terminating the Job Object, since Windows has no
// POSIX signal delivery for arbitrary processes.
func (e *Environment) Stop(ctx context.Context) error {
	e.mu.RLock()
	s := e.meta.Stop
	e.mu.RUnlock()

	if e.st.Load() != environment.ProcessOfflineState {
		e.SetState(environment.ProcessStoppingState)
	}

	if s.Type == remote.ProcessStopSignal {
		return e.Terminate(ctx, "SIGKILL")
	}

	if e.IsAttached() && s.Type == remote.ProcessStopCommand {
		return e.SendCommand(s.Value)
	}

	if s.Type == "" {
		e.log().Warn("winproc: no stop configuration defined for environment, terminating process tree")
	}
	return e.Terminate(ctx, "SIGKILL")
}

// WaitForStop issues a stop and then waits up to `duration` for the process to
// actually exit, polling its handle. If it has not exited in time it is either
// terminated (terminate=true) or an error is returned.
func (e *Environment) WaitForStop(ctx context.Context, duration time.Duration, terminate bool) error {
	tctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	if err := e.Stop(tctx); err != nil {
		if terminate && errors.Is(err, context.DeadlineExceeded) {
			return e.Terminate(ctx, "SIGKILL")
		}
		return err
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-tctx.Done():
			if terminate {
				return e.Terminate(ctx, "SIGKILL")
			}
			return tctx.Err()
		case <-ticker.C:
			running, err := e.IsRunning(ctx)
			if err != nil {
				return err
			}
			if !running {
				return nil
			}
		}
	}
}

// Terminate forcibly kills the entire process tree by terminating the Job
// Object. The wait() goroutine observes the exit and transitions the server to
// the offline state.
func (e *Environment) Terminate(ctx context.Context, signal string) error {
	e.mu.RLock()
	job := e.job
	running := e.cmd != nil
	e.mu.RUnlock()

	if !running {
		// Already stopped; make sure the state reflects that without tripping
		// crash detection.
		if e.st.Load() != environment.ProcessOfflineState {
			e.SetState(environment.ProcessStoppingState)
			e.SetState(environment.ProcessOfflineState)
		}
		return nil
	}

	e.SetState(environment.ProcessStoppingState)
	if job != 0 {
		if err := windows.TerminateJobObject(job, 1); err != nil {
			return errors.Wrap(err, "winproc: failed to terminate job object")
		}
	}
	return nil
}

// ExitState returns the exit code of the most recently finished process. OOM
// detection is a documented Phase-2 parity gap (§8.5), so the OOM flag is always
// false for now.
func (e *Environment) ExitState() (uint32, bool, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastExit, false, nil
}

// Uptime returns how long the current process has been running, in
// milliseconds, or 0 if nothing is running.
func (e *Environment) Uptime(ctx context.Context) (int64, error) {
	e.mu.RLock()
	started := e.startedAt
	running := e.cmd != nil
	e.mu.RUnlock()

	if !running || started.IsZero() {
		return 0, nil
	}
	return time.Since(started).Milliseconds(), nil
}
