//go:build !windows

package proxy

import "syscall"

// wsaErrnoName is a no-op on non-Windows platforms where errno values already
// match the names used by normalizeErrno.
func wsaErrnoName(errno syscall.Errno) (string, bool) {
	return "", false
}
