//go:build !windows

package watcher

import "github.com/jun9100/savevault/internal/fsx"

// isFileLocked on POSIX: a plain open() has no universal "busy" errno
// (files can always be read while another process writes), so a readable
// file is never considered locked. This matches the practical behavior of
// the JS app on Linux, where EBUSY never fires for regular file opens.
func isFileLocked(path string) bool {
	f, err := fsx.OpenShared(path)
	if err == nil {
		f.Close()
	}
	return false
}
