//go:build windows

package winproc

import (
	"runtime"

	"emperror.dev/errors"
	"golang.org/x/sys/windows"
)

// This file implements per-operation impersonation so the in-process file
// manager and SFTP server can perform filesystem work AS the server's restricted
// account (subject to its NTFS ACLs) instead of the daemon's LocalSystem
// identity. See docs/phase0-conformance-spec.md §5/§7.
//
// Windows impersonation is per-OS-thread (ImpersonateLoggedOnUser). Because Go
// multiplexes goroutines across threads, every impersonated operation must pin
// its thread for the duration, or the impersonation could leak to another
// goroutine (a privilege hole) or be lost mid-operation. withThreadImpersonation
// enforces that discipline.

var procImpersonateLoggedOnUser = modadvapi32.NewProc("ImpersonateLoggedOnUser")

func impersonateLoggedOnUser(token windows.Token) error {
	r, _, e := procImpersonateLoggedOnUser.Call(uintptr(token))
	if r == 0 {
		return errors.Wrap(e, "winproc: ImpersonateLoggedOnUser failed")
	}
	return nil
}

// withThreadImpersonation runs fn while the current OS thread impersonates
// token, pinning the thread so the impersonation cannot leak to or be lost by
// other goroutines. If the impersonation cannot be reverted afterwards the
// thread is intentionally left pinned (and NOT returned to the scheduler) so it
// can never run later work as the wrong user — fail-closed.
func withThreadImpersonation(token windows.Token, fn func() error) (err error) {
	runtime.LockOSThread()
	if e := impersonateLoggedOnUser(token); e != nil {
		// Never impersonated — safe to release the thread.
		runtime.UnlockOSThread()
		return e
	}
	defer func() {
		if e := windows.RevertToSelf(); e != nil {
			if err == nil {
				err = errors.Wrap(e, "winproc: RevertToSelf failed; thread retained for safety")
			}
			// Deliberately do NOT UnlockOSThread here.
			return
		}
		runtime.UnlockOSThread()
	}()
	return fn()
}

// ensureAccountLocked makes sure the per-server account exists with a known
// in-memory password. The caller MUST hold acctMu. The password is generated
// once per daemon run and kept only in memory (never persisted); the account
// itself persists, and its ACLs/SID survive password rotation.
func (e *Environment) ensureAccountLocked() error {
	if e.acctPass == "" {
		pass, err := randomPassword()
		if err != nil {
			return err
		}
		e.acctPass = pass
	}
	return ensureAccount(e.account, e.acctPass)
}

// ensureAccountProvisioned ensures the account exists (used by Create before
// applying ACLs).
func (e *Environment) ensureAccountProvisioned() error {
	e.acctMu.Lock()
	defer e.acctMu.Unlock()
	return e.ensureAccountLocked()
}

// logonToken returns a fresh primary token for the server account, provisioning
// the account on first use. The caller owns the token and must Close it. Used to
// launch the server process.
func (e *Environment) logonToken() (windows.Token, error) {
	e.acctMu.Lock()
	defer e.acctMu.Unlock()
	if err := e.ensureAccountLocked(); err != nil {
		return 0, err
	}
	return logonAccount(e.account, e.acctPass)
}

// Impersonate runs fn while impersonating the server's restricted account, so
// any filesystem operations fn performs are access-checked (NTFS ACLs) as that
// account rather than as the daemon. A single impersonation token is minted on
// first use and cached for the environment's lifetime (it survives password
// rotation, since tokens are independent of the password once issued).
func (e *Environment) Impersonate(fn func() error) error {
	e.acctMu.Lock()
	if e.impToken == 0 {
		if err := e.ensureAccountLocked(); err != nil {
			e.acctMu.Unlock()
			e.log().WithField("error", err).Error("winproc: impersonation setup failed (ensure account)")
			return err
		}
		tok, err := logonAccount(e.account, e.acctPass)
		if err != nil {
			e.acctMu.Unlock()
			e.log().WithField("error", err).Error("winproc: impersonation setup failed (logon)")
			return err
		}
		e.impToken = tok
	}
	tok := e.impToken
	e.acctMu.Unlock()

	if err := withThreadImpersonation(tok, fn); err != nil {
		// Log via the daemon logger (writes to wings.log) so impersonation
		// failures are visible regardless of how the calling route reports them.
		e.log().WithField("error", err).WithField("account", e.account).Error("winproc: impersonated filesystem operation failed")
		return err
	}
	return nil
}

// closeImpersonationToken releases the cached impersonation token, if any.
func (e *Environment) closeImpersonationToken() {
	e.acctMu.Lock()
	tok := e.impToken
	e.impToken = 0
	e.acctMu.Unlock()
	if tok != 0 {
		_ = tok.Close()
	}
}
