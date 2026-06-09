//go:build unix

package config

import (
	"github.com/apex/log"
	"golang.org/x/sys/unix"
)

// detectOpenat2 probes whether the running kernel supports the openat2(2)
// syscall (Linux 5.6+). It is only used when the configured OpenatMode is left
// at its default ("auto").
func detectOpenat2() bool {
	fd, err := unix.Openat2(unix.AT_FDCWD, "/", &unix.OpenHow{})
	if err != nil {
		log.WithError(err).Warn("error occurred while checking for openat2 support, falling back to openat")
		return false
	}
	_ = unix.Close(fd)
	return true
}
