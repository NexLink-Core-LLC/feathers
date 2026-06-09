//go:build !windows

package server

import "emperror.dev/errors"

// assertEggPlatform refuses to install or start a Windows egg on a Linux/Docker
// node, using the egg's Docker image as the platform marker (see platform.go).
// This prevents accidentally assigning a Windows egg to a Linux node, where its
// (non-existent) image would otherwise fail to pull with a confusing error.
func assertEggPlatform(image string) error {
	if isWindowsImage(image) {
		return errors.Errorf(
			"egg platform mismatch: image %q targets Windows, but this is a Linux node. Assign a Linux egg.",
			image,
		)
	}
	return nil
}
