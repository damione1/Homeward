//go:build !darwin && !linux

package server

import (
	"errors"
	"net"
)

const operatorSocketPathLimit = 103

func operatorPeerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("operator peer credentials unsupported")
}
