//go:build !windows

package server

import (
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/environment/docker"
)

// createEnvironment builds the execution backend for a server. On non-Windows
// hosts this is the Docker container environment.
func createEnvironment(id, image string, c *environment.Configuration) (environment.ProcessEnvironment, error) {
	return docker.New(id, &docker.Metadata{Image: image}, c)
}
