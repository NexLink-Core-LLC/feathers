// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2024 Matthew Penner

//go:build unix

package ufs

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// errnoToPathError converts an errno into a proper path error.
func errnoToPathError(err syscall.Errno, op, path string) error {
	switch err {
	// File exists
	case unix.EEXIST:
		return &PathError{Op: op, Path: path, Err: ErrExist}
	// Is a directory
	case unix.EISDIR:
		return &PathError{Op: op, Path: path, Err: ErrIsDirectory}
	// Not a directory
	case unix.ENOTDIR:
		return &PathError{Op: op, Path: path, Err: ErrNotDirectory}
	// No such file or directory
	case unix.ENOENT:
		return &PathError{Op: op, Path: path, Err: ErrNotExist}
	// Operation not permitted
	case unix.EPERM:
		return &PathError{Op: op, Path: path, Err: ErrPermission}
	// Invalid cross-device link
	case unix.EXDEV:
		return &PathError{Op: op, Path: path, Err: ErrBadPathResolution}
	// Too many levels of symbolic links
	case unix.ELOOP:
		return &PathError{Op: op, Path: path, Err: ErrBadPathResolution}
	default:
		return &PathError{Op: op, Path: path, Err: err}
	}
}
