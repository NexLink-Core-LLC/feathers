//go:build windows

package server

import (
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/environment/winproc"
)

// createEnvironment builds the execution backend for a server. On Windows this
// is the native Job Object process environment.
func createEnvironment(id, image string, c *environment.Configuration) (environment.ProcessEnvironment, error) {
	return winproc.New(id, &winproc.Metadata{Image: image}, c)
}
