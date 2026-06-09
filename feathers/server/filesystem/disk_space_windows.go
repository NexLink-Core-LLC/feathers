//go:build windows

package filesystem

import (
	"sync/atomic"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/internal/ufs"
)

// DirectorySize calculates the size of a directory and its descendants.
//
// NOTE: unlike the Unix implementation, this does not deduplicate hard links.
// Windows hard links are uncommon for game-server workloads and there is no
// inode/Nlink equivalent exposed through os.FileInfo; in the rare case a server
// uses hard links their size may be counted more than once. Tracked as a minor
// parity gap for a later phase.
func (fs *Filesystem) DirectorySize(root string) (int64, error) {
	dirfd, name, closeFd, err := fs.unixFS.SafePath(root)
	defer closeFd()
	if err != nil {
		return 0, err
	}

	var size atomic.Int64
	err = fs.unixFS.WalkDirat(dirfd, name, func(dirfd int, name, _ string, d ufs.DirEntry, err error) error {
		if err != nil {
			return errors.Wrap(err, "walkdirat err")
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := fs.unixFS.Lstatat(dirfd, name)
		if err != nil {
			return errors.Wrap(err, "lstatat err")
		}
		size.Add(info.Size())
		return nil
	})
	return size.Load(), errors.WrapIf(err, "server/filesystem: directorysize: failed to walk directory")
}
