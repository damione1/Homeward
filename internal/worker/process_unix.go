//go:build darwin || linux

package worker

import (
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func signalProcessGroup(pid int, signal syscall.Signal) error { return syscall.Kill(-pid, signal) }
