//go:build linux

package worker

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// A terminated orphan can remain a zombie until PID 1 reaps it. Zombies
// cannot execute, hold FDs, or make an RPC, so they do not keep a lease alive.
func processGroupAlive(pid int) bool {
	if err := syscall.Kill(-pid, 0); err == syscall.ESRCH {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		} // a process may exit during the scan
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return true
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 3 {
			return true
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return true
		}
		if group == pid && fields[0] != "Z" && fields[0] != "X" {
			return true
		}
	}
	return false
}
