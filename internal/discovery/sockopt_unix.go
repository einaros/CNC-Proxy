//go:build darwin || linux

package discovery

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reusePort enables SO_REUSEADDR and SO_REUSEPORT on the discovery socket so
// it can share UDP 3333 with other reuse-aware listeners on the same host.
func reusePort(network, address string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); serr != nil {
			return
		}
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
	if err != nil {
		return err
	}
	return serr
}

// broadcastable enables SO_BROADCAST so the advertiser may send to a directed
// broadcast address (required on Linux; macOS allows it regardless).
func broadcastable(network, address string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
