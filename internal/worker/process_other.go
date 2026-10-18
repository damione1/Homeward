//go:build !darwin && !linux

package worker

import (
	"os/exec"
	"syscall"
)

func configureProcess(*exec.Cmd) error             { return ErrInvalidConfig }
func signalProcessGroup(int, syscall.Signal) error { return ErrInvalidConfig }
func processGroupAlive(int) bool                   { return false }
