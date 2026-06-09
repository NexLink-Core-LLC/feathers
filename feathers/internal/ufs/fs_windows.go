// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2024 Matthew Penner

//go:build windows

// This file is the Windows counterpart to fs_unix.go. The upstream Wings
// filesystem relies on Linux file-descriptor semantics (openat2/RESOLVE_BENEATH,
// *at syscalls and numeric dirfds) that have no direct Windows equivalent.
//
// Rather than emulate kernel file descriptors, the Windows backend keeps the
// same public API but resolves every operation to an absolute path under the
// sandbox base and executes it through the standard os package. The numeric
// "dirfd" used throughout the upstream code is modeled here as a token into a
// small registry of absolute directory paths (see dirRegistry), so the dirfd
// based *at methods and WalkDirat continue to work unchanged for callers in
// server/filesystem.
//
// Sandbox jailing is enforced with a lexical clean + base-prefix check
// (unsafePath / unsafeIsPathInsideOfBase, case-insensitive to match NTFS) plus
// a symlink/reparse-point resolution check on resolved paths. NOTE: this is the
// Phase 1 implementation aimed at functional parity and boot; the race-free
// hardening of the jail (TOCTOU, reparse-point attacks) is tracked for Phase 3.

package ufs

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// sentinelFlagMask are the ufs open flags that have no os-package equivalent on
// Windows and must be stripped before calling os.OpenFile.
const sentinelFlagMask = O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC | O_LARGEFILE

// UnixFS is the Windows implementation of the sandboxed filesystem. The name is
// retained (rather than e.g. WindowsFS) so that the shared, OS-neutral code in
// this package and in server/filesystem can refer to a single concrete type.
type UnixFS struct {
	// basePath is the absolute, cleaned base path that all operations are
	// sandboxed within.
	basePath string

	// useOpenat2 is accepted for API compatibility with the Unix backend but is
	// unused on Windows (there is no openat2).
	useOpenat2 bool

	// reg maps synthetic dirfd tokens to absolute directory paths.
	reg dirRegistry

	// imp, when non-nil, runs each terminal disk syscall under the per-server
	// account's security context so access is checked against the directory's
	// NTFS ACLs rather than the daemon's (LocalSystem) identity. nil = run
	// directly (the default). Set via SetImpersonator by the server layer.
	imp func(func() error) error
}

// SetImpersonator installs a wrapper that executes each filesystem syscall as
// the per-server restricted account. Passing nil disables impersonation.
func (fs *UnixFS) SetImpersonator(fn func(func() error) error) {
	fs.imp = fn
}

// do runs a single terminal filesystem syscall, impersonating the per-server
// account when an impersonator is configured. It must wrap ONLY a leaf os.*
// call — never a method that fans out to goroutines or calls back into other
// UnixFS methods, or the (thread-local) impersonation would leak across threads
// or nest incorrectly.
func (fs *UnixFS) do(fn func() error) error {
	if fs.imp != nil {
		return fs.imp(fn)
	}
	return fn()
}

// dirRegistry hands out integer tokens that stand in for Unix directory file
// descriptors. Each token maps to an absolute directory path on disk.
type dirRegistry struct {
	mu   sync.Mutex
	next int
	dirs map[int]string
}

func (r *dirRegistry) add(absDir string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dirs == nil {
		r.dirs = make(map[int]string)
	}
	r.next++
	// Avoid 0 so callers can treat 0 as "no fd" like the Unix code does.
	if r.next == 0 {
		r.next++
	}
	fd := r.next
	r.dirs[fd] = absDir
	return fd
}

func (r *dirRegistry) get(fd int) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.dirs[fd]
	return d, ok
}

func (r *dirRegistry) release(fd int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.dirs, fd)
}

// NewUnixFS creates a new sandboxed filesystem rooted at basePath. Operations
// resolving outside basePath are rejected.
func NewUnixFS(basePath string, useOpenat2 bool) (*UnixFS, error) {
	abs, err := filepath.Abs(basePath)
	if err != nil {
		return nil, err
	}
	fs := &UnixFS{
		basePath:   filepath.Clean(abs),
		useOpenat2: useOpenat2,
	}
	return fs, nil
}

// BasePath returns the base path of the sandbox.
func (fs *UnixFS) BasePath() string {
	return fs.basePath
}

// Close is a no-op on Windows; there is no persistent base file descriptor.
func (fs *UnixFS) Close() error {
	return nil
}

// unsafeIsPathInsideOfBase reports whether path is within the sandbox base path.
// The comparison is case-insensitive to match NTFS semantics.
func (fs *UnixFS) unsafeIsPathInsideOfBase(p string) bool {
	sep := string(filepath.Separator)
	base := strings.ToLower(fs.basePath)
	candidate := strings.ToLower(strings.TrimSuffix(p, sep))
	return strings.HasPrefix(candidate+sep, base+sep)
}

// unsafePath cleans path, joins it onto the base path, and verifies it does not
// lexically escape the sandbox. It returns the path relative to the base path
// (or "." for the base path itself). This mirrors the Unix implementation but
// is separator-aware for Windows.
func (fs *UnixFS) unsafePath(p string) (string, error) {
	r := filepath.Clean(filepath.Join(fs.basePath, strings.TrimPrefix(p, fs.basePath)))
	if fs.unsafeIsPathInsideOfBase(r) {
		r = strings.TrimPrefix(strings.TrimPrefix(r, fs.basePath), string(filepath.Separator))
		if r == "" {
			return ".", nil
		}
		return r, nil
	}
	return "", &PathError{Op: "safePath", Path: p, Err: ErrBadPathResolution}
}

// resolveAbs returns the absolute, jailed path for a base-relative or absolute
// input path.
func (fs *UnixFS) resolveAbs(p string) (string, error) {
	rel, err := fs.unsafePath(p)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return fs.basePath, nil
	}
	return filepath.Join(fs.basePath, rel), nil
}

// symlinkJailCheck resolves any symlinks/reparse points in abs (if it exists)
// and ensures the real path is still within the sandbox.
func (fs *UnixFS) symlinkJailCheck(abs string) error {
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// If the path (or a parent) does not exist yet, the lexical check in
		// unsafePath already applied; allow it.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return convertErrorType(err)
	}
	if !fs.unsafeIsPathInsideOfBase(resolved) {
		return &PathError{Op: "resolve", Path: abs, Err: ErrBadPathResolution}
	}
	return nil
}

// atPath resolves a synthetic dirfd + name into a jailed absolute path.
func (fs *UnixFS) atPath(dirfd int, name string) (string, error) {
	dir, ok := fs.reg.get(dirfd)
	if !ok {
		return "", &PathError{Op: "at", Path: name, Err: ErrInvalid}
	}
	abs := filepath.Clean(filepath.Join(dir, name))
	if !fs.unsafeIsPathInsideOfBase(abs) {
		return "", &PathError{Op: "at", Path: name, Err: ErrBadPathResolution}
	}
	// Also reject reparse-point escapes (junction/symlink) at the resolved path,
	// matching the hardening in safePath.
	if err := fs.symlinkJailCheck(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// SafePath resolves path to its parent directory (returned as a dirfd token)
// and the leaf name, mirroring the Unix safePath contract. closeFd releases the
// token and must always be called.
func (fs *UnixFS) SafePath(path string) (int, string, func(), error) {
	return fs.safePath(path)
}

func (fs *UnixFS) safePath(p string) (dirfd int, file string, closeFd func(), err error) {
	closeFd = func() {}

	var name string
	name, err = fs.unsafePath(p)
	if err != nil {
		return
	}

	dir, leaf := filepath.Split(name)
	var absDir string
	if dir == "" {
		absDir = fs.basePath
		file = name // may be "." for the base path itself
	} else {
		absDir = filepath.Join(fs.basePath, strings.TrimSuffix(dir, string(filepath.Separator)))
		file = leaf
	}

	// Resolve the REAL location of the full target (parent + leaf), following any
	// junctions/symlinks/reparse points, and confirm it is still inside the
	// sandbox. Checking the full path (not just the parent) closes the gap where
	// the leaf itself is a reparse point escaping the jail.
	fullPath := absDir
	if file != "" && file != "." {
		fullPath = filepath.Join(absDir, file)
	}
	if err = fs.symlinkJailCheck(fullPath); err != nil {
		return
	}

	dirfd = fs.reg.add(absDir)
	closeFd = func() { fs.reg.release(dirfd) }
	return
}

// TouchPath is like SafePath but creates any missing parent directories.
func (fs *UnixFS) TouchPath(p string) (int, string, func(), error, bool) {
	dirfd, name, closeFd, err := fs.safePath(p)
	switch {
	case err == nil:
		return dirfd, name, closeFd, nil, true
	case !errors.Is(err, ErrNotExist):
		return dirfd, name, closeFd, err, false
	}

	var pathErr *PathError
	if !errors.As(err, &pathErr) {
		return dirfd, name, closeFd, err, false
	}
	if err := fs.MkdirAll(pathErr.Path, 0o755); err != nil {
		return dirfd, name, closeFd, err, false
	}
	closeFd()
	dirfd, name, closeFd, err = fs.safePath(p)
	return dirfd, name, closeFd, err, false
}

// --- mode changes -----------------------------------------------------------

func (fs *UnixFS) Chmod(name string, mode FileMode) error {
	abs, err := fs.resolveAbs(name)
	if err != nil {
		return err
	}
	return ensurePathError(fs.do(func() error { return os.Chmod(abs, mode) }), "chmod", name)
}

func (fs *UnixFS) Chmodat(dirfd int, name string, mode FileMode) error {
	abs, err := fs.atPath(dirfd, name)
	if err != nil {
		return err
	}
	return ensurePathError(fs.do(func() error { return os.Chmod(abs, mode) }), "chmodat", name)
}

// Chown, Lchown and their *at variants are no-ops on Windows: the platform has
// no POSIX uid/gid ownership model. File access is controlled via NTFS ACLs,
// which are applied out-of-band by the daemon (per-server user accounts).
func (fs *UnixFS) Chown(name string, uid, gid int) error              { return nil }
func (fs *UnixFS) Lchown(name string, uid, gid int) error             { return nil }
func (fs *UnixFS) Chownat(dirfd int, name string, uid, gid int) error { return nil }
func (fs *UnixFS) Lchownat(dirfd int, name string, uid, gid int) error {
	return nil
}

func (fs *UnixFS) Chtimes(name string, atime, mtime time.Time) error {
	abs, err := fs.resolveAbs(name)
	if err != nil {
		return err
	}
	return ensurePathError(fs.do(func() error { return os.Chtimes(abs, atime, mtime) }), "chtimes", name)
}

func (fs *UnixFS) Chtimesat(dirfd int, name string, atime, mtime time.Time) error {
	abs, err := fs.atPath(dirfd, name)
	if err != nil {
		return err
	}
	return ensurePathError(fs.do(func() error { return os.Chtimes(abs, atime, mtime) }), "chtimes", name)
}

// --- open / create ----------------------------------------------------------

func (fs *UnixFS) Create(name string) (File, error) {
	return fs.OpenFile(name, O_CREATE|O_WRONLY|O_TRUNC, 0o644)
}

func (fs *UnixFS) Open(name string) (File, error) {
	return fs.OpenFile(name, O_RDONLY, 0)
}

func (fs *UnixFS) OpenFile(name string, flag int, mode FileMode) (File, error) {
	abs, err := fs.resolveAbs(name)
	if err != nil {
		return nil, err
	}
	return fs.openAbs(abs, name, flag, mode)
}

func (fs *UnixFS) OpenFileat(dirfd int, name string, flag int, mode FileMode) (File, error) {
	abs, err := fs.atPath(dirfd, name)
	if err != nil {
		return nil, err
	}
	return fs.openAbs(abs, name, flag, mode)
}

func (fs *UnixFS) openAbs(abs, name string, flag int, mode FileMode) (File, error) {
	// Reject symlink escapes for existing paths before opening.
	if flag&O_NOFOLLOW != 0 || flag&O_CREATE == 0 {
		if err := fs.symlinkJailCheck(abs); err != nil {
			return nil, err
		}
	}
	var f *os.File
	if err := fs.do(func() error {
		var e error
		f, e = os.OpenFile(abs, flag&^sentinelFlagMask, mode)
		return e
	}); err != nil {
		return nil, ensurePathError(err, "openat", name)
	}
	if flag&O_DIRECTORY != 0 {
		if st, serr := f.Stat(); serr == nil && !st.IsDir() {
			_ = f.Close()
			return nil, &PathError{Op: "openat", Path: name, Err: ErrNotDirectory}
		}
	}
	return f, nil
}

// --- directories ------------------------------------------------------------

func (fs *UnixFS) Mkdir(name string, mode FileMode) error {
	abs, err := fs.resolveAbs(name)
	if err != nil {
		return err
	}
	return ensurePathError(fs.do(func() error { return os.Mkdir(abs, mode) }), "mkdir", name)
}

func (fs *UnixFS) Mkdirat(dirfd int, name string, mode FileMode) error {
	abs, err := fs.atPath(dirfd, name)
	if err != nil {
		return err
	}
	return ensurePathError(fs.do(func() error { return os.Mkdir(abs, mode) }), "mkdirat", name)
}

func (fs *UnixFS) MkdirAll(name string, mode FileMode) error {
	abs, err := fs.resolveAbs(name)
	if err != nil {
		return err
	}
	return ensurePathError(fs.do(func() error { return os.MkdirAll(abs, mode) }), "mkdirall", name)
}

func (fs *UnixFS) ReadDir(p string) ([]DirEntry, error) {
	abs, err := fs.resolveAbs(p)
	if err != nil {
		return nil, err
	}
	var entries []os.DirEntry
	if err := fs.do(func() error {
		var e error
		entries, e = os.ReadDir(abs)
		return e
	}); err != nil {
		return nil, ensurePathError(err, "readdir", p)
	}
	out := make([]DirEntry, len(entries))
	for i, e := range entries {
		// Wrap each entry so it also exposes Open() — the file manager
		// (ListDirectory) type-asserts to that to sniff mimetypes, mirroring the
		// Unix backend's dirent. A bare os.DirEntry lacks Open() and would panic
		// the assertion. Open() routes through the jailed (and impersonated) fs.
		out[i] = winDirEntry{DirEntry: e, fs: fs, relPath: path.Join(p, e.Name())}
	}
	return out, nil
}

// winDirEntry adapts an os.DirEntry (Name/IsDir/Type/Info) and adds an Open()
// method that opens the entry through the sandboxed filesystem, matching the
// Unix backend's dirent contract that the file manager relies on.
type winDirEntry struct {
	os.DirEntry
	fs      *UnixFS
	relPath string
}

func (d winDirEntry) Open() (File, error) {
	return d.fs.OpenFile(d.relPath, O_RDONLY, 0)
}

// --- remove -----------------------------------------------------------------

func (fs *UnixFS) RemoveStat(name string) (FileInfo, error) {
	abs, err := fs.resolveAbs(name)
	if err != nil {
		return nil, err
	}
	var s os.FileInfo
	if err := fs.do(func() error {
		var e error
		s, e = os.Lstat(abs)
		return e
	}); err != nil {
		return nil, ensurePathError(err, "lstat", name)
	}
	if err := fs.do(func() error { return os.Remove(abs) }); err != nil {
		return s, ensurePathError(err, "remove", name)
	}
	return s, nil
}

func (fs *UnixFS) Remove(name string) error {
	abs, err := fs.resolveAbs(name)
	if err != nil {
		return err
	}
	if abs == fs.basePath {
		return &PathError{Op: "remove", Path: name, Err: ErrBadPathResolution}
	}
	return ensurePathError(fs.do(func() error { return os.Remove(abs) }), "remove", name)
}

func (fs *UnixFS) RemoveAll(name string) error {
	abs, err := fs.resolveAbs(name)
	if err != nil {
		return err
	}
	if abs == fs.basePath {
		return &PathError{Op: "removeall", Path: name, Err: ErrBadPathResolution}
	}
	return fs.removeAll(name)
}

func (fs *UnixFS) RemoveContents(name string) error {
	abs, err := fs.resolveAbs(name)
	if err != nil {
		return err
	}
	var entries []os.DirEntry
	if err := fs.do(func() error {
		var e error
		entries, e = os.ReadDir(abs)
		return e
	}); err != nil {
		return ensurePathError(err, "readdir", name)
	}
	for _, e := range entries {
		if err := fs.removeAll(path.Join(name, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (fs *UnixFS) removeAll(p string) error {
	return removeAll(fs, p)
}

func (fs *UnixFS) unlinkat(dirfd int, name string, flags int) error {
	abs, err := fs.atPath(dirfd, name)
	if err != nil {
		return err
	}
	// os.Remove handles both files and (empty) directories, covering the
	// AT_REMOVEDIR distinction the Unix backend needs to make explicitly.
	return fs.do(func() error { return os.Remove(abs) })
}

// --- rename / symlink -------------------------------------------------------

func (fs *UnixFS) Rename(oldpath, newpath string) error {
	if oldpath == newpath {
		return nil
	}
	oldAbs, err := fs.resolveAbs(oldpath)
	if err != nil {
		return err
	}
	if oldAbs == fs.basePath {
		return &PathError{Op: "rename", Path: oldpath, Err: ErrBadPathResolution}
	}
	newAbs, err := fs.resolveAbs(newpath)
	if err != nil {
		return err
	}
	if newAbs == fs.basePath {
		return &PathError{Op: "rename", Path: newpath, Err: ErrBadPathResolution}
	}
	// Ensure the destination parent exists, matching the Unix backend which
	// MkdirAll's the parent when it is missing.
	if err := fs.do(func() error { return os.MkdirAll(filepath.Dir(newAbs), 0o755) }); err != nil {
		return ensurePathError(err, "rename", newpath)
	}
	if err := fs.do(func() error { return os.Rename(oldAbs, newAbs) }); err != nil {
		return &LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

func (fs *UnixFS) Symlink(oldpath, newpath string) error {
	newAbs, err := fs.resolveAbs(newpath)
	if err != nil {
		return err
	}
	if err := fs.do(func() error { return os.Symlink(oldpath, newAbs) }); err != nil {
		return &LinkError{Op: "symlink", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

// --- stat -------------------------------------------------------------------

func (fs *UnixFS) Stat(name string) (FileInfo, error) {
	abs, err := fs.resolveAbs(name)
	if err != nil {
		return nil, err
	}
	var fi os.FileInfo
	if err := fs.do(func() error {
		var e error
		fi, e = os.Stat(abs)
		return e
	}); err != nil {
		return nil, ensurePathError(err, "stat", name)
	}
	return fi, nil
}

func (fs *UnixFS) Statat(dirfd int, name string) (FileInfo, error) {
	abs, err := fs.atPath(dirfd, name)
	if err != nil {
		return nil, err
	}
	var fi os.FileInfo
	if err := fs.do(func() error {
		var e error
		fi, e = os.Stat(abs)
		return e
	}); err != nil {
		return nil, ensurePathError(err, "statat", name)
	}
	return fi, nil
}

func (fs *UnixFS) Lstat(name string) (FileInfo, error) {
	abs, err := fs.resolveAbs(name)
	if err != nil {
		return nil, err
	}
	var fi os.FileInfo
	if err := fs.do(func() error {
		var e error
		fi, e = os.Lstat(abs)
		return e
	}); err != nil {
		return nil, ensurePathError(err, "lstat", name)
	}
	return fi, nil
}

func (fs *UnixFS) Lstatat(dirfd int, name string) (FileInfo, error) {
	abs, err := fs.atPath(dirfd, name)
	if err != nil {
		return nil, err
	}
	var fi os.FileInfo
	if err := fs.do(func() error {
		var e error
		fi, e = os.Lstat(abs)
		return e
	}); err != nil {
		return nil, ensurePathError(err, "lstatat", name)
	}
	return fi, nil
}

// --- touch ------------------------------------------------------------------

func (fs *UnixFS) Touch(p string, flag int, mode FileMode) (File, error) {
	if flag&O_CREATE == 0 {
		flag |= O_CREATE
	}
	dirfd, name, closeFd, err, _ := fs.TouchPath(p)
	defer closeFd()
	if err != nil {
		return nil, err
	}
	return fs.OpenFileat(dirfd, name, flag, mode)
}

// --- walk -------------------------------------------------------------------

func (fs *UnixFS) WalkDir(root string, fn WalkDirFunc) error {
	return WalkDir(fs, root, fn)
}

// WalkDiratFunc mirrors the Unix signature: it receives the synthetic dirfd of
// the directory containing name.
type WalkDiratFunc func(dirfd int, name, relative string, d DirEntry, err error) error

func (fs *UnixFS) WalkDirat(dirfd int, name string, fn WalkDiratFunc) error {
	info, err := fs.Lstatat(dirfd, name)
	if err != nil {
		err = fn(dirfd, name, ".", nil, err)
	} else {
		err = fs.walkDir(dirfd, name, ".", info, fn)
	}
	if err == SkipDir || err == SkipAll {
		return nil
	}
	return err
}

func (fs *UnixFS) walkDir(parentfd int, name, relative string, d FileInfo, walkDirFn WalkDiratFunc) error {
	entry := dirEntryFromInfo(d)
	if err := walkDirFn(parentfd, name, relative, entry, nil); err != nil || !d.IsDir() {
		if err == SkipDir && d.IsDir() {
			err = nil
		}
		return err
	}

	// Resolve the directory we are descending into and register a token for it.
	dirAbs, err := fs.atPath(parentfd, name)
	if err != nil {
		return err
	}
	dirfd := fs.reg.add(dirAbs)
	defer fs.reg.release(dirfd)

	entries, err := os.ReadDir(dirAbs)
	if err != nil {
		if e := walkDirFn(dirfd, name, relative, entry, ensurePathError(err, "readdir", name)); e != nil {
			if e == SkipDir {
				return nil
			}
			return e
		}
	}

	for _, e := range entries {
		childName := e.Name()
		var rel string
		if relative == "." {
			rel = childName
		} else {
			rel = path.Join(relative, childName)
		}
		ci, cerr := os.Lstat(filepath.Join(dirAbs, childName))
		if cerr != nil {
			if werr := walkDirFn(dirfd, childName, rel, e, ensurePathError(cerr, "lstat", childName)); werr != nil && werr != SkipDir {
				return werr
			}
			continue
		}
		if err := fs.walkDir(dirfd, childName, rel, ci, walkDirFn); err != nil {
			if err == SkipDir {
				break
			}
			return err
		}
	}
	return nil
}

// dirEntryFromInfo adapts a FileInfo into a DirEntry.
func dirEntryFromInfo(fi FileInfo) DirEntry {
	return fileInfoDirEntry{fi}
}

type fileInfoDirEntry struct{ fi FileInfo }

func (d fileInfoDirEntry) Name() string               { return d.fi.Name() }
func (d fileInfoDirEntry) IsDir() bool                { return d.fi.IsDir() }
func (d fileInfoDirEntry) Type() FileMode             { return d.fi.Mode().Type() }
func (d fileInfoDirEntry) Info() (FileInfo, error)    { return d.fi, nil }

// ReadDirMap reads the entries of path and applies fn to each, returning the
// mapped results.
func ReadDirMap[T any](fs *UnixFS, p string, fn func(DirEntry) (T, error)) ([]T, error) {
	entries, err := fs.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]T, len(entries))
	for i, e := range entries {
		v, err := fn(e)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// --- removeAll helper (Windows) ---------------------------------------------

// unixFS is the minimal interface removeAll needs; it is satisfied by both
// *UnixFS and *Quota so that Quota's size accounting (via its overridden
// Remove/unlinkat) is preserved during recursive deletes.
type unixFS interface {
	Open(name string) (File, error)
	Remove(name string) error
	unlinkat(dirfd int, path string, flags int) error
}

func removeAll(fs unixFS, p string) error {
	if p == "" {
		return nil
	}
	// Fast path: works for regular files and empty directories.
	err := fs.Remove(p)
	if err == nil || errors.Is(err, ErrNotExist) {
		return nil
	}

	// Otherwise it may be a non-empty directory; recurse into its children.
	d, derr := fs.Open(p)
	if derr != nil {
		if errors.Is(derr, ErrNotExist) {
			return nil
		}
		// Return the original Remove error which is more meaningful.
		return err
	}
	for {
		names, rerr := d.Readdirnames(512)
		for _, name := range names {
			if e := removeAll(fs, path.Join(p, name)); e != nil && err == nil {
				err = e
			}
		}
		if rerr == io.EOF || len(names) == 0 {
			break
		}
		if rerr != nil {
			_ = d.Close()
			return rerr
		}
	}
	_ = d.Close()

	if e := fs.Remove(p); e != nil {
		return e
	}
	return err
}
