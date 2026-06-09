//go:build windows

// Package winproc implements the Wings environment.ProcessEnvironment interface
// for Windows hosts. Where the Docker environment runs each server inside a
// Linux container, winproc runs the server as a native Windows process tree
// managed by a Job Object (for reliable tree-kill) with its console piped back
// to Wings exactly like the Docker stream.
//
// PHASE 2 SCOPE: process power (start/stop/restart/kill), console attach +
// stdin, a console ring buffer, and resource *reporting*. The process currently
// runs under the daemon's own service account; the per-server restricted user
// and NTFS ACLs are Phase 3, and memory/CPU *enforcement* is Phase 4. See
// docs/phase0-conformance-spec.md.
package winproc

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/system"
)

// Metadata mirrors docker.Metadata so the rest of Wings can treat the two
// backends uniformly.
type Metadata struct {
	// Image is received from the Panel for every server. On Windows there is no
	// container image; it is retained only as an informational label / version
	// hint and is otherwise ignored.
	Image string
	// Stop is the configuration used to gracefully stop the server process
	// (a stop command written to stdin, or a signal).
	Stop remote.ProcessStopConfiguration
}

// Ensure winproc.Environment satisfies the full ProcessEnvironment interface at
// compile time, exactly like the Docker backend does.
var _ environment.ProcessEnvironment = (*Environment)(nil)

type Environment struct {
	mu sync.RWMutex

	// Id is the public identifier for this environment (the server UUID).
	Id string

	// account is the per-server restricted local Windows account the server
	// process runs as (derived from the UUID, e.g. "pt_1a2b3c4d").
	account string

	// Configuration holds the environment configuration (limits, allocations,
	// mounts, environment variables).
	Configuration *environment.Configuration

	meta *Metadata

	emitter *events.Bus

	logCallbackMx sync.Mutex
	logCallback   func([]byte)

	// st tracks the environment state (offline/starting/running/stopping).
	st *system.AtomicString

	// acctMu guards the account password and cached impersonation token. It is
	// independent of mu and must never be held together with mu in the reverse
	// order (Create may take mu then acctMu; nothing takes acctMu then mu).
	acctMu   sync.Mutex
	acctPass string         // in-memory only; never persisted
	impToken windows.Token  // cached token for file-manager/SFTP impersonation

	// job is the Job Object that owns the server process tree. Created by
	// Create(); closing it (Destroy) kills every member process.
	job windows.Handle

	// cmd is the running server process, nil when offline. procHandle is a
	// queryable/terminable handle to that process. stdin is the pipe used by
	// SendCommand. These are all guarded by mu.
	cmd        *exec.Cmd
	procHandle windows.Handle
	stdin      io.WriteCloser

	// startedAt is when the current process was launched; lastExit is the exit
	// code of the most recently finished process.
	startedAt time.Time
	lastExit  uint32

	// console is the in-memory ring buffer backing Readlog().
	console *consoleBuffer

	// pollCancel stops the resource-polling goroutine when the process exits.
	pollCancel context.CancelFunc
}

// New creates a new Windows process environment. The id should be unique
// per-server (the UUID). No process is created at this point.
func New(id string, m *Metadata, c *environment.Configuration) (*Environment, error) {
	e := &Environment{
		Id:            id,
		account:       accountName(id),
		Configuration: c,
		meta:          m,
		st:            system.NewAtomicString(environment.ProcessOfflineState),
		emitter:       events.NewBus(),
		console:       newConsoleBuffer(consoleBufferLines),
	}
	return e, nil
}

func (e *Environment) log() *log.Entry {
	return log.WithField("environment", e.Type()).WithField("server", e.Id)
}

// Type returns the environment identifier. This is distinct from "docker" so
// logs and diagnostics make the backend obvious; the Panel never sees it.
func (e *Environment) Type() string {
	return "winproc"
}

// Config returns the environment configuration.
func (e *Environment) Config() *environment.Configuration {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Configuration
}

// Events returns the environment's event bus.
func (e *Environment) Events() *events.Bus {
	return e.emitter
}

// SetStopConfiguration updates the stop configuration for the server.
func (e *Environment) SetStopConfiguration(c remote.ProcessStopConfiguration) {
	e.mu.Lock()
	e.meta.Stop = c
	e.mu.Unlock()
}

// SetImage stores the (informational) image label. It has no functional effect
// on Windows.
func (e *Environment) SetImage(i string) {
	e.mu.Lock()
	e.meta.Image = i
	e.mu.Unlock()
}

// IsAttached reports whether the console is attached to a running process,
// determined by the presence of a live stdin pipe.
func (e *Environment) IsAttached() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.stdin != nil
}

// directory returns the server's working directory on disk
// (<system.data>\<uuid>), which is where the process is launched.
func (e *Environment) directory() string {
	return filepath.Join(config.Get().System.Data, e.Id)
}

// Exists reports whether the server "instance" exists. On Windows a server's
// existence is defined by its data directory (there is no persistent container
// object). This is what OnBeforeStart relies on to know a boot can proceed.
func (e *Environment) Exists() (bool, error) {
	if _, err := os.Stat(e.directory()); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// InSituUpdate updates resource limits on a running server without restarting
// it, by re-applying the current Panel-defined limits to the live Job Object.
// If no job exists yet the limits will be applied at the next Create()/Start().
func (e *Environment) InSituUpdate() error {
	e.mu.RLock()
	job := e.job
	e.mu.RUnlock()

	if job == 0 {
		return nil
	}
	if err := applyJobLimits(job, jobLimitsFor(e.Configuration.Limits())); err != nil {
		return errors.Wrap(err, "winproc: failed to update job object limits")
	}
	return nil
}

// OnBeforeStart runs before a server is started. It ensures the server has a
// freshly configured Job Object, mirroring the Docker backend, which destroys
// and recreates the container so the latest Panel-synced configuration is used.
func (e *Environment) OnBeforeStart(ctx context.Context) error {
	// Drop any previous job (and, with KILL_ON_JOB_CLOSE, any stragglers) so we
	// always start from a clean, freshly limited job.
	e.closeJob()
	return e.Create()
}

// Create prepares the environment for running the server process. On Windows
// this creates the per-server Job Object. It is idempotent: if the job already
// exists this is a no-op (matching the Docker Create semantics).
func (e *Environment) Create() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	dir := e.directory()

	// Ensure the server data directory exists before a process is launched into
	// it. The filesystem layer also manages this, but Create() must be safe to
	// call standalone (e.g. during install).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.Wrap(err, "winproc: failed to create server data directory")
	}

	// Provision the per-server restricted account and lock the data directory to
	// it. Idempotent — safe to run on every boot. The account must exist before
	// the ACL can reference it by name.
	if err := e.ensureAccountProvisioned(); err != nil {
		return errors.Wrap(err, "winproc: failed to provision server account")
	}
	if err := applyServerACL(dir, e.account); err != nil {
		return errors.Wrap(err, "winproc: failed to apply server directory ACLs")
	}

	if e.job == 0 {
		job, err := createJobObject(jobLimitsFor(e.Configuration.Limits()))
		if err != nil {
			return errors.Wrap(err, "winproc: failed to create job object")
		}
		e.job = job
	}
	return nil
}

// closeJob closes the Job Object handle, which (via KILL_ON_JOB_CLOSE) kills
// every process still running inside it. Safe to call when no job exists.
func (e *Environment) closeJob() {
	e.mu.Lock()
	job := e.job
	e.job = 0
	e.mu.Unlock()

	if job != 0 {
		if err := windows.CloseHandle(job); err != nil {
			e.log().WithField("error", err).Warn("winproc: failed to close job object handle")
		}
	}
}

// Destroy tears down the environment, killing the process tree (closing the job
// handle) and marking the server offline. Server files are intentionally left
// on disk.
func (e *Environment) Destroy() error {
	e.SetState(environment.ProcessStoppingState)
	e.closeJob()
	e.closeImpersonationToken()

	// Best-effort teardown of the per-server account. Deleting the account also
	// invalidates its ACEs on the (now-orphaned) data directory. Server files
	// themselves are left on disk; the Panel manages directory removal.
	if e.account != "" {
		if err := deleteAccount(e.account); err != nil {
			e.log().WithField("error", err).Warn("winproc: failed to delete per-server account")
		}
	}

	e.SetState(environment.ProcessOfflineState)
	return nil
}

func (e *Environment) State() string {
	return e.st.Load()
}

// SetState sets the environment state and emits a state-change event, matching
// the Docker backend's semantics exactly.
func (e *Environment) SetState(state string) {
	if state != environment.ProcessOfflineState &&
		state != environment.ProcessStartingState &&
		state != environment.ProcessRunningState &&
		state != environment.ProcessStoppingState {
		panic(errors.New(fmt.Sprintf("invalid server state received: %s", state)))
	}

	if e.State() != state {
		e.st.Store(state)
		e.Events().Publish(environment.StateChangeEvent, state)
	}
}

func (e *Environment) SetLogCallback(f func([]byte)) {
	e.logCallbackMx.Lock()
	defer e.logCallbackMx.Unlock()
	e.logCallback = f
}
