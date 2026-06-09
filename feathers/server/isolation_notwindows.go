//go:build !windows

package server

import (
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/server/filesystem"
)

// applyFilesystemIsolation is a no-op on non-Windows platforms: filesystem
// isolation there is provided by the Docker environment (containers), not by
// impersonating an account in-process.
func applyFilesystemIsolation(fs *filesystem.Filesystem, env environment.ProcessEnvironment) {}
