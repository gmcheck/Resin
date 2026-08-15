//go:build windows

package proxy

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// wsaErrnoName maps Windows Winsock errnos onto the errno names used by
// normalizeErrno, so cross-platform error classification stays consistent
// (e.g. WSAECONNRESET → ECONNRESET).
func wsaErrnoName(errno syscall.Errno) (string, bool) {
	switch windows.Errno(errno) {
	case windows.WSAECONNREFUSED:
		return "ECONNREFUSED", true
	case windows.WSAECONNRESET:
		return "ECONNRESET", true
	case windows.WSAECONNABORTED:
		return "ECONNABORTED", true
	case windows.WSAENETUNREACH:
		return "ENETUNREACH", true
	case windows.WSAEHOSTUNREACH:
		return "EHOSTUNREACH", true
	case windows.WSAETIMEDOUT:
		return "ETIMEDOUT", true
	}
	return "", false
}
