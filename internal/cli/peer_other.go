//go:build !darwin && !linux

package cli

import "net"

func peerUID(*net.UnixConn) (uint32, error) {
	return 0, fail("operator_peer_credentials_unsupported", ExitPermission)
}
