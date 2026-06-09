package server

import "strings"

// windowsImagePrefix is the marker that identifies a Windows-native egg.
//
// Pterodactyl eggs have no platform field, so the egg's Docker image is reused
// as a platform tag (the winproc backend ignores the image otherwise). A
// Windows egg declares an image beginning with "windows" — e.g. "windows/java".
// A leading "~" (Pterodactyl's "local image, do not pull" marker) is ignored
// for the purpose of this check.
const windowsImagePrefix = "windows"

// isWindowsImage reports whether an egg's Docker image marks it as a
// Windows-native egg.
func isWindowsImage(image string) bool {
	image = strings.ToLower(strings.TrimSpace(image))
	image = strings.TrimPrefix(image, "~")
	return strings.HasPrefix(image, windowsImagePrefix)
}
