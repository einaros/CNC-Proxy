//go:build !darwin && !linux

package discovery

import "syscall"

// reusePort is a no-op where SO_REUSEPORT isn't portable (e.g. Windows, whose
// SO_REUSEADDR semantics differ and are unsafe to enable blindly).
func reusePort(network, address string, c syscall.RawConn) error { return nil }

// broadcastable is a no-op on platforms where we don't manage SO_BROADCAST.
func broadcastable(network, address string, c syscall.RawConn) error { return nil }
