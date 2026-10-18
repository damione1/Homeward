// Package testpg starts a disposable PostgreSQL cluster for integration tests.
// It never connects to an ambient database or reads physical credentials.
package testpg

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const password = "NHA027_canary_password"

type Cluster struct{ Port int }

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
	cmd = exec.Command("pg_ctl", "-D", data, "-l", filepath.Join(root, "postgres.log"), "-o", fmt.Sprintf("-h 127.0.0.1 -p %d", port), "-w", "start")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v: %s", err, output)
	}
	t.Cleanup(func() {
		cmd := exec.Command("pg_ctl", "-D", data, "-m", "immediate", "-w", "stop")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("pg_ctl stop: %v: %s", err, output)
		}
	})
	return Cluster{Port: port}
}

func Password() string { return password }
