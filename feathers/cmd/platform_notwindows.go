//go:build !windows

package cmd

import (
	"context"

	"github.com/apex/log"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
)

// configurePlatformUser creates the pterodactyl system user and the container
// passwd/group files used by the Docker environment.
func configurePlatformUser() error {
	if err := config.EnsurePterodactylUser(); err != nil {
		return err
	}
	if err := config.ConfigurePasswd(); err != nil {
		return err
	}
	log.WithFields(log.Fields{
		"username": config.Get().System.Username,
		"uid":      config.Get().System.User.Uid,
		"gid":      config.Get().System.User.Gid,
	}).Info("configured system user successfully")
	return nil
}

// configureExecutionBackend initializes the Docker client and network.
func configureExecutionBackend(ctx context.Context) error {
	return environment.ConfigureDocker(ctx)
}
