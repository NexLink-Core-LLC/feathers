//go:build !windows

package filesystem

// volumeHasHeadroom is a no-op on non-Windows platforms. The Docker backend and
// Linux disk quotas provide physical-volume protection there, so behavior is
// unchanged from upstream Wings; it always reports headroom.
func volumeHasHeadroom(path string) bool { return true }
