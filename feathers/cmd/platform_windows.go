//go:build windows

package cmd

import (
	"context"
)

// configurePlatformUser is a no-op on Windows. There is no global pterodactyl
// user; each server runs under its own restricted local account that winproc
// provisions when the server is created/started.
func configurePlatformUser() error {
	return nil
}

// configureExecutionBackend is a no-op on Windows. Unlike Docker (which needs a
// client connection and a shared bridge network), the winproc backend needs no
// global initialization — Job Objects and per-server accounts are created
// lazily at server start.
func configureExecutionBackend(ctx context.Context) error {
	return nil
}
