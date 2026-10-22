// Package testpg starts a disposable PostgreSQL cluster for integration tests.
// It never connects to an ambient database or reads physical credentials.
package testpg

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

const password = "NHA027_canary_password"
const maxStartupLogBytes = 8 * 1024

type Cluster struct {
	Port    int
	stop    func() error
	restart func() error
}

// Stop ends only this disposable cluster. Cleanup remains idempotent.
func (c Cluster) Stop() error {
	if c.stop == nil {
		return fmt.Errorf("temporary postgres cluster is not initialized")
	}
	return c.stop()
}

// Restart starts the same stopped disposable cluster on its original port.
// It preserves the catalog and refuses a running or already cleaned-up cluster.
func (c Cluster) Restart() error {
	if c.restart == nil {
		return fmt.Errorf("temporary postgres cluster is not initialized")
	}
	return c.restart()
}

func Start(t *testing.T) Cluster {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	passwordFile := filepath.Join(root, "password")
	if err := os.WriteFile(passwordFile, []byte(password+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	cmd := exec.Command("initdb", "-D", data, "--username=homeward_test", "--auth-host=scram-sha-256", "--auth-local=trust", "--pwfile="+passwordFile, "--no-instructions")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v: %s", err, output)
	}
	log := filepath.Join(root, "postgres.log")
	if err := startPostgres(data, log, fmt.Sprintf("-h 127.0.0.1 -p %d -c unix_socket_directories=''", port)); err != nil {
		if stopErr := stopIfRunning(data); stopErr != nil {
			t.Fatalf("%v; cleanup: %v", err, stopErr)
		}
		t.Fatal(err)
	}
	var mu sync.Mutex
	running, closed := true, false
	stopLocked := func() error {
		if !running {
			return nil
		}
		cmd := exec.Command("pg_ctl", "-D", data, "-m", "immediate", "-w", "stop")
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("pg_ctl stop: %w: %s", err, output)
		}
		running = false
		return nil
	}
	stop := func() error {
		mu.Lock()
		defer mu.Unlock()
		return stopLocked()
	}
	restart := func() error {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return fmt.Errorf("temporary postgres cluster has been cleaned up")
		}
		if running {
			return fmt.Errorf("temporary postgres cluster must be stopped before restart")
		}
		if err := startPostgres(data, log, fmt.Sprintf("-h 127.0.0.1 -p %d -c unix_socket_directories=''", port)); err != nil {
			cleanupErr := stopIfRunning(data)
			// If cleanup is uncertain, retain ownership so the final cleanup
			// still attempts to stop this cluster instead of abandoning it.
			running = cleanupErr != nil
			return errors.Join(err, cleanupErr)
		}
		running = true
		return nil
	}
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		closed = true
		if err := stopLocked(); err != nil {
			t.Error(err)
		}
	})
	return Cluster{Port: port, stop: stop, restart: restart}
}

func Password() string { return password }

func startPostgres(data, log, options string) error {
	cmd := exec.Command("pg_ctl", "-D", data, "-l", log, "-o", options, "-w", "start")
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	startErr := fmt.Errorf("pg_ctl start: %w: %s", err, output)
	serverLog, logErr := startupLogTail(log)
	if errors.Is(logErr, os.ErrNotExist) || (logErr == nil && len(serverLog) == 0) {
		return startErr
	}
	if logErr != nil {
		return fmt.Errorf("%w; postgres.log read: %v", startErr, logErr)
	}
	return fmt.Errorf("%w; postgres.log (last %d bytes): %s", startErr, len(serverLog), serverLog)
}

func startupLogTail(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := info.Size() - maxStartupLogBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(file, maxStartupLogBytes))
}

func stopIfRunning(data string) error {
	output, err := exec.Command("pg_ctl", "-D", data, "status").CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 3 {
			return nil
		}
		return fmt.Errorf("pg_ctl status after failed start: %w: %s", err, output)
	}
	output, err = exec.Command("pg_ctl", "-D", data, "-m", "immediate", "-w", "stop").CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_ctl stop after failed start: %w: %s", err, output)
	}
	return nil
}
