// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2024 Matthew Penner

//go:build windows

package ufs

import (
	"syscall"
)

// errnoToPathError converts a Windows syscall error code into one of our
// canonical path errors so that callers can use errors.Is against the ufs
// sentinels (ErrExist, ErrNotExist, ...) regardless of platform.
func errnoToPathError(err syscall.Errno, op, path string) error {
	switch err {
	// File / directory already exists.
	case syscall.ERROR_FILE_EXISTS, syscall.ERROR_ALREADY_EXISTS:
		return &PathError{Op: op, Path: path, Err: ErrExist}
	// No such file or directory.
	case syscall.ERROR_FILE_NOT_FOUND, syscall.ERROR_PATH_NOT_FOUND:
		return &PathError{Op: op, Path: path, Err: ErrNotExist}
	// Permission denied.
	case syscall.ERROR_ACCESS_DENIED:
		return &PathError{Op: op, Path: path, Err: ErrPermission}
	default:
		return &PathError{Op: op, Path: path, Err: err}
	}
}
