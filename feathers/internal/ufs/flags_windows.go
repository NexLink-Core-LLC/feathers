// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2024 Matthew Penner

//go:build windows

package ufs

import (
	"os"
)

// Standard open flags map to the cross-platform os package values. The Windows
// filesystem backend implements OpenFile in terms of os.OpenFile, so these are
// exactly the flags that package expects.
const (
	// O_RDONLY opens the file read-only.
	O_RDONLY = os.O_RDONLY
	// O_WRONLY opens the file write-only.
	O_WRONLY = os.O_WRONLY
	// O_RDWR opens the file read-write.
	O_RDWR = os.O_RDWR
	// O_APPEND appends data to the file when writing.
	O_APPEND = os.O_APPEND
	// O_CREATE creates a new file if it doesn't exist.
	O_CREATE = os.O_CREATE
	// O_EXCL is used with O_CREATE, file must not exist.
	O_EXCL = os.O_EXCL
	// O_SYNC open for synchronous I/O.
	O_SYNC = os.O_SYNC
	// O_TRUNC truncates regular writable file when opened.
	O_TRUNC = os.O_TRUNC

	// The following flags have no direct os-package equivalent on Windows. They
	// are defined as distinct, non-overlapping bits above the standard flag
	// range so that code can still set/test them; the Windows backend handles
	// the associated semantics (directory-only opens, symlink rejection)
	// explicitly rather than via open(2) flags.
	O_DIRECTORY = 0x10000
	O_NOFOLLOW  = 0x20000
	O_CLOEXEC   = 0x40000 // files are not inherited by child processes on Windows by default
	O_LARGEFILE = 0x0     // Windows file offsets are already 64-bit
)

// AT_* resolution flags. On Unix these come from the kernel; on Windows they
// are internal sentinels consumed by the Windows backend's *at shims to choose
// between follow/no-follow and unlink/rmdir behavior.
const (
	AT_SYMLINK_NOFOLLOW = 0x100
	AT_REMOVEDIR        = 0x200
	AT_EMPTY_PATH       = 0x1000
)
