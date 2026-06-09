//go:build windows

package winproc

import (
	"os/exec"
	"syscall"

	"emperror.dev/errors"
	"golang.org/x/sys/windows"
)

// applyServerACL locks a server's data directory down to its per-server
// account. It grants explicit full control to SYSTEM, the Administrators group,
// and the account, then strips inherited ACEs so that ordinary host users (and
// other servers' accounts) have no access. Child files/dirs inherit these ACEs
// via the (OI)(CI) flags, so new server files are covered automatically.
//
// Well-known SIDs are used for SYSTEM (S-1-5-18) and Administrators
// (S-1-5-32-544) so this is locale-independent.
func applyServerACL(dir, account string) error {
	// Grant first (so we never leave an empty DACL), then remove inheritance.
	if err := runIcacls(dir,
		"/grant:r",
		"*S-1-5-18:(OI)(CI)F",     // NT AUTHORITY\SYSTEM
		"*S-1-5-32-544:(OI)(CI)F", // BUILTIN\Administrators
		account+":(OI)(CI)F",      // the per-server account
	); err != nil {
		return err
	}
	if err := runIcacls(dir, "/inheritance:r"); err != nil {
		return err
	}
	return nil
}

// removeServerACL drops the per-server account's grant from the directory. The
// account is also deleted on Destroy, which invalidates its ACEs regardless.
func removeServerACL(dir, account string) error {
	return runIcacls(dir, "/remove:g", account, "/T", "/C")
}

func runIcacls(dir string, args ...string) error {
	full := append([]string{dir}, args...)
	cmd := exec.Command("icacls", full...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "winproc: icacls %v failed: %s", args, string(out))
	}
	return nil
}
