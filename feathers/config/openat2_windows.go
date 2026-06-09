//go:build windows

package config

// detectOpenat2 always reports false on Windows: there is no openat2 syscall.
// The Windows ufs backend resolves paths eagerly and ignores this value.
func detectOpenat2() bool {
	return false
}
