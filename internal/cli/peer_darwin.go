//go:build darwin

package cli

import (
	"golang.org/x/sys/unix"
	"net"
)

func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credentials *unix.Xucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if socketErr != nil {
		return 0, socketErr
	}
	return credentials.Uid, nil
}
