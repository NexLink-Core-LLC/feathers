package filesystem

import (
	"syscall"
	"time"
)

// CTime returns the creation time of the file/folder. On Windows the creation
// time is available directly from the file attributes, unlike on Unix where the
// "ctime" field is the inode change time.
func (s *Stat) CTime() time.Time {
	if st, ok := s.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, st.CreationTime.Nanoseconds())
	}
	return s.ModTime()
}
