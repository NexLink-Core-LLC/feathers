//go:build windows

package server

import (
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/server/filesystem"
)

// impersonator is implemented by execution environments (winproc) that can run
// a function in the security context of the server's restricted account.
type impersonator interface {
	Impersonate(func() error) error
}

// applyFilesystemIsolation wires per-operation impersonation into the server's
// filesystem so the in-process file manager and SFTP server perform disk access
// as the server's restricted Windows account (subject to its NTFS ACLs) rather
// than as the daemon's LocalSystem identity. No-op if the environment does not
// support impersonation.
func applyFilesystemIsolation(fs *filesystem.Filesystem, env environment.ProcessEnvironment) {
	if imp, ok := env.(impersonator); ok {
		fs.SetImpersonator(imp.Impersonate)
	}
}
