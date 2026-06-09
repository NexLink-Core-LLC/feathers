//go:build !windows

package config

// applyPlatformDefaults is a no-op on Unix; the struct-tag defaults
// (/var/lib/pterodactyl, ...) are already correct.
func applyPlatformDefaults(_ *Configuration) {}
