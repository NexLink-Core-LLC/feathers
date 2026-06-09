//go:build windows

package server

import "emperror.dev/errors"

// assertEggPlatform refuses to install or start an egg that is not a Windows
// egg on this Windows (winproc) node, using the egg's Docker image as the
// platform marker (see platform.go). This prevents accidentally assigning a
// Linux egg to a Windows node.
func assertEggPlatform(image string) error {
	if !isWindowsImage(image) {
		return errors.Errorf(
			"egg platform mismatch: image %q targets Linux, but this is a Windows node. Assign a Windows egg (Docker image prefixed %q).",
			image, windowsImagePrefix,
		)
	}
	return nil
}
