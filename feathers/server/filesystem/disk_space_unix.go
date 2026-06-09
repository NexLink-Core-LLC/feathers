//go:build !windows

package filesystem

import (
	"slices"
	"sync/atomic"

	"emperror.dev/errors"
	"golang.org/x/sys/unix"

	"github.com/pterodactyl/wings/internal/ufs"
)

// DirectorySize calculates the size of a directory and its descendants,
// deduplicating hard links via their inode number.
func (fs *Filesystem) DirectorySize(root string) (int64, error) {
	dirfd, name, closeFd, err := fs.unixFS.SafePath(root)
	defer closeFd()
	if err != nil {
		return 0, err
	}

	var hardLinks []uint64

	var size atomic.Int64
	err = fs.unixFS.WalkDirat(dirfd, name, func(dirfd int, name, _ string, d ufs.DirEntry, err error) error {
		if err != nil {
			return errors.Wrap(err, "walkdirat err")
		}

		// Only calculate the size of regular files.
		if !d.Type().IsRegular() {
			return nil
		}

		info, err := fs.unixFS.Lstatat(dirfd, name)
		if err != nil {
			return errors.Wrap(err, "lstatat err")
		}

		var sysFileInfo = info.Sys().(*unix.Stat_t)
		if sysFileInfo.Nlink > 1 {
			// Hard links have the same inode number
			if slices.Contains(hardLinks, sysFileInfo.Ino) {
				// Don't add hard links size twice
				return nil
			} else {
				hardLinks = append(hardLinks, sysFileInfo.Ino)
			}
		}

		size.Add(info.Size())
		return nil
	})
	return size.Load(), errors.WrapIf(err, "server/filesystem: directorysize: failed to walk directory")
}
