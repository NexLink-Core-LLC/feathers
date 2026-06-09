// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2024 Matthew Penner

//go:build unix

package ufs

import (
	"golang.org/x/sys/unix"
)

// Re-using the same names as Go's official `unix` and `os` package do.
const (
	// O_RDONLY opens the file read-only.
	O_RDONLY = unix.O_RDONLY
	// O_WRONLY opens the file write-only.
	O_WRONLY = unix.O_WRONLY
	// O_RDWR opens the file read-write.
	O_RDWR = unix.O_RDWR
	// O_APPEND appends data to the file when writing.
	O_APPEND = unix.O_APPEND
	// O_CREATE creates a new file if it doesn't exist.
	O_CREATE = unix.O_CREAT
	// O_EXCL is used with O_CREATE, file must not exist.
	O_EXCL = unix.O_EXCL
	// O_SYNC open for synchronous I/O.
	O_SYNC = unix.O_SYNC
	// O_TRUNC truncates regular writable file when opened.
	O_TRUNC = unix.O_TRUNC
	// O_DIRECTORY opens a directory only. If the entry is not a directory an
	// error will be returned.
	O_DIRECTORY = unix.O_DIRECTORY
	// O_NOFOLLOW opens the exact path given without following symlinks.
	O_NOFOLLOW  = unix.O_NOFOLLOW
	O_CLOEXEC   = unix.O_CLOEXEC
	O_LARGEFILE = unix.O_LARGEFILE
)

const (
	AT_SYMLINK_NOFOLLOW = unix.AT_SYMLINK_NOFOLLOW
	AT_REMOVEDIR        = unix.AT_REMOVEDIR
	AT_EMPTY_PATH       = unix.AT_EMPTY_PATH
)
