//go:build windows

package winproc

import (
	"context"
	"io"
	"sync"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/system"
)

// consoleBufferLines is how many console lines winproc retains in memory for
// Readlog(). The Panel only ever requests the tail of the console, so a bounded
// ring buffer is sufficient and keeps memory flat for long-running servers.
const consoleBufferLines = 2000

// ErrNotAttached is returned when a command is sent to a process that has no
// live stdin pipe (i.e. it is not running).
var ErrNotAttached = errors.Sentinel("winproc: not attached to a running process")

// consoleBuffer is a fixed-capacity ring of recent console lines.
type consoleBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newConsoleBuffer(max int) *consoleBuffer {
	return &consoleBuffer{max: max}
}

func (c *consoleBuffer) push(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
	if len(c.lines) > c.max {
		// Drop the oldest lines, keeping the most recent c.max.
		c.lines = append(c.lines[:0], c.lines[len(c.lines)-c.max:]...)
	}
}

func (c *consoleBuffer) tail(n int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= 0 || n > len(c.lines) {
		n = len(c.lines)
	}
	out := make([]string, n)
	copy(out, c.lines[len(c.lines)-n:])
	return out
}

func (c *consoleBuffer) reset() {
	c.mu.Lock()
	c.lines = c.lines[:0]
	c.mu.Unlock()
}

// Attach is a no-op on Windows. Unlike Docker — where Attach opens a stream to
// an already-created container — winproc has no process to attach to until it
// is launched, so console streaming and resource polling are wired up directly
// inside Start(). The method exists to satisfy the ProcessEnvironment contract.
func (e *Environment) Attach(ctx context.Context) error {
	return nil
}

// streamConsole consumes the process's merged stdout/stderr, forwarding every
// line both to the console ring buffer (for Readlog) and to the registered log
// callback (which the server layer uses to drive the websocket console and the
// "startup done" → running state transition). It owns the read end of the pipe
// and closes it once the process has exited and the pipe reaches EOF.
//
// IMPORTANT: this function — not wait() — closes the pipe. Closing it elsewhere
// would race with this read loop and truncate the output of fast-exiting
// processes (e.g. a server that prints an error and immediately quits).
func (e *Environment) streamConsole(rc io.ReadCloser) {
	defer rc.Close()
	err := system.ScanReader(rc, func(v []byte) {
		e.console.push(string(v))

		e.logCallbackMx.Lock()
		if e.logCallback != nil {
			e.logCallback(v)
		}
		e.logCallbackMx.Unlock()
	})
	if err != nil && err != io.EOF {
		e.log().WithField("error", err).Warn("winproc: error while scanning console output")
	}
}

// SendCommand writes a command to the running process's stdin. There is no
// confirmation the process acted on it, only that it was written to the pipe.
func (e *Environment) SendCommand(c string) error {
	e.mu.RLock()
	stdin := e.stdin
	stop := e.meta.Stop
	e.mu.RUnlock()

	if stdin == nil {
		return errors.WithStack(ErrNotAttached)
	}

	// If this command is the configured stop command, move to the stopping state
	// first so the process exiting is not misread as a crash.
	if stop.Type == "command" && c == stop.Value {
		e.SetState(environment.ProcessStoppingState)
	}

	// Game servers expect a trailing newline to submit a console line. We use
	// "\n"; some Windows-native servers may require "\r\n" — a known per-game
	// concern noted in the conformance spec (§2, SendCommand).
	if _, err := stdin.Write([]byte(c + "\n")); err != nil {
		return errors.Wrap(err, "winproc: could not write command to process stdin")
	}
	return nil
}

// Readlog returns the last `lines` lines of console output from the in-memory
// ring buffer. It does not depend on the process being currently running.
func (e *Environment) Readlog(lines int) ([]string, error) {
	return e.console.tail(lines), nil
}
