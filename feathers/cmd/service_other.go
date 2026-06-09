//go:build !windows

package cmd

// runUnderServiceManager is a no-op on non-Windows platforms. Linux nodes are
// managed by systemd (or similar) which runs Wings as an ordinary foreground
// process, so the command is always executed directly.
func runUnderServiceManager() bool {
	return false
}
