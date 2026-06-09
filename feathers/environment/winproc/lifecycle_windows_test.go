//go:build windows

package winproc

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
)

// requireElevation skips a test unless the session is elevated. Phase 3
// isolation provisions local accounts and ACLs (admin-only); the daemon runs as
// LocalSystem in production, but a dev `go test` session usually is not elevated.
func requireElevation(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires an elevated session (account/ACL provisioning needs admin)")
	}
}

// newTestEnv wires up a winproc.Environment backed by a temp data directory and
// the given STARTUP invocation.
func newTestEnv(t *testing.T, startup string) *Environment {
	t.Helper()

	dir := t.TempDir()
	cfg, err := config.NewAtPath(filepath.Join(dir, "config.yml"))
	if err != nil {
		t.Fatalf("config.NewAtPath: %v", err)
	}
	cfg.System.Data = dir
	// config.Set builds a JWT signer from the token, which must be non-empty.
	cfg.Token = config.Token{ID: "test", Token: "test-secret"}
	config.Set(cfg)

	ec := environment.NewConfiguration(
		environment.Settings{Limits: environment.Limits{MemoryLimit: 256}},
		[]string{"STARTUP=" + startup},
	)
	e, err := New("test-"+strings.ReplaceAll(t.Name(), "/", "-"), &Metadata{}, ec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}

// TestConsoleCaptureAndExit launches a short-lived process and verifies that its
// stdout is captured into the console ring buffer, that it is reaped, and that
// the server transitions back to offline with a clean exit code.
func TestConsoleCaptureAndExit(t *testing.T) {
	requireElevation(t)
	e := newTestEnv(t, `cmd /c echo HELLO_FROM_WINPROC`)
	if err := e.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer e.Destroy()

	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitFor(t, 5*time.Second, func() bool {
		for _, l := range e.console.tail(0) {
			if strings.Contains(l, "HELLO_FROM_WINPROC") {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("did not capture expected console output; got: %#v", e.console.tail(0))
	}

	if !waitFor(t, 5*time.Second, func() bool {
		r, _ := e.IsRunning(context.Background())
		return !r && e.State() == environment.ProcessOfflineState
	}) {
		t.Fatalf("process did not reach offline state after exit (state=%s)", e.State())
	}

	code, oom, err := e.ExitState()
	if err != nil {
		t.Fatalf("ExitState: %v", err)
	}
	if code != 0 || oom {
		t.Fatalf("unexpected exit state: code=%d oom=%v", code, oom)
	}
}

// TestTerminateKillsProcessTree launches a long-running process and verifies
// that Terminate (via the Job Object) actually kills it.
func TestTerminateKillsProcessTree(t *testing.T) {
	requireElevation(t)
	e := newTestEnv(t, `ping -n 60 127.0.0.1`)
	if err := e.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer e.Destroy()

	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitFor(t, 5*time.Second, func() bool {
		r, _ := e.IsRunning(context.Background())
		return r
	}) {
		t.Fatal("process never reported running")
	}

	if err := e.Terminate(context.Background(), "SIGKILL"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	if !waitFor(t, 5*time.Second, func() bool {
		r, _ := e.IsRunning(context.Background())
		return !r && e.State() == environment.ProcessOfflineState
	}) {
		t.Fatalf("process was not terminated (state=%s)", e.State())
	}
}
