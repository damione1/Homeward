package testpg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestNHA169TwoIsolatedClusters(t *testing.T) {
	first := Start(t)
	second := Start(t)
	if first.Port == second.Port {
		t.Fatal("clusters share a port")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connect := func(port int, password string) *pgx.Conn {
		t.Helper()
		url := fmt.Sprintf("postgres://homeward_test:%s@127.0.0.1:%d/postgres?sslmode=disable", password, port)
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	if _, err := pgx.Connect(ctx, fmt.Sprintf("postgres://homeward_test:wrong@127.0.0.1:%d/postgres?sslmode=disable", first.Port)); err == nil {
		t.Fatal("TCP connection accepted a wrong password")
	}
	firstConn := connect(first.Port, Password())
	defer firstConn.Close(context.Background())
	secondConn := connect(second.Port, Password())
	defer secondConn.Close(context.Background())
	dataDirs := make([]string, 2)
	for index, pair := range []struct {
		cluster Cluster
		conn    *pgx.Conn
	}{
		{first, firstConn},
		{second, secondConn},
	} {
		var address, socketDirs, scramPrefix string
		var port int
		err := pair.conn.QueryRow(ctx, `SELECT host(inet_server_addr()), inet_server_port(), current_setting('unix_socket_directories'),
			(SELECT left(rolpassword, 14) FROM pg_authid WHERE rolname = current_user)`).Scan(&address, &port, &socketDirs, &scramPrefix)
		if err != nil {
			t.Fatal(err)
		}
		if address != "127.0.0.1" || port != pair.cluster.Port || socketDirs != "" || scramPrefix != "SCRAM-SHA-256$" {
			t.Fatalf("cluster %d transport/authentication: address=%q port=%d sockets=%q hash prefix=%q", index, address, port, socketDirs, scramPrefix)
		}
		if err := pair.conn.QueryRow(ctx, "SHOW data_directory").Scan(&dataDirs[index]); err != nil {
			t.Fatal(err)
		}
		if _, err := pair.conn.Exec(ctx, `CREATE TABLE nha169_isolation(value integer NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		if _, err := pair.conn.Exec(ctx, `INSERT INTO nha169_isolation(value) VALUES ($1)`, index+1); err != nil {
			t.Fatal(err)
		}
	}
	for index, conn := range []*pgx.Conn{firstConn, secondConn} {
		var value int
		if err := conn.QueryRow(ctx, `SELECT value FROM nha169_isolation`).Scan(&value); err != nil || value != index+1 {
			t.Fatalf("cluster %d data isolation: value=%d error=%v", index, value, err)
		}
	}
	if err := first.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := first.Stop(); err != nil {
		t.Fatalf("second stop of first cluster: %v", err)
	}
	assertStopped(t, dataDirs[0])
	var secondValue int
	if err := secondConn.QueryRow(ctx, `SELECT value FROM nha169_isolation`).Scan(&secondValue); err != nil || secondValue != 2 {
		t.Fatalf("second cluster did not survive first stop: value=%d error=%v", secondValue, err)
	}
	if err := second.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := second.Stop(); err != nil {
		t.Fatalf("second stop of second cluster: %v", err)
	}
	assertStopped(t, dataDirs[1])
}

func assertStopped(t *testing.T, data string) {
	t.Helper()
	output, err := exec.Command("pg_ctl", "-D", data, "status").CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("server did not reach stopped status 3: %v: %s", err, output)
	}
}

func TestNHA169StartupDiagnostic(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	passwordFile := filepath.Join(root, "password")
	if err := os.WriteFile(passwordFile, []byte(password+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("initdb", "-D", data, "--username=homeward_test", "--auth-host=scram-sha-256", "--auth-local=trust", "--pwfile="+passwordFile, "--no-instructions").CombinedOutput()
	if err != nil {
		t.Fatalf("initdb: %v: %s", err, output)
	}
	log := filepath.Join(root, "postgres.log")
	if err := os.WriteFile(log, []byte(strings.Repeat("x", 64*1024)), 0600); err != nil {
		t.Fatal(err)
	}
	err = startPostgres(data, log, "-h 127.0.0.1 -p 49542 -c unix_socket_directories='' -c nha169_unknown_setting=1")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || !strings.Contains(err.Error(), "nha169_unknown_setting") || !strings.Contains(err.Error(), "postgres.log") {
		t.Fatalf("missing original pg_ctl error or real server diagnostic: %v", err)
	}
	if len(err.Error()) > maxStartupLogBytes+2048 {
		t.Fatalf("startup diagnostic is unbounded: %d bytes", len(err.Error()))
	}
	if err := stopIfRunning(data); err != nil {
		t.Fatal(err)
	}
	missingLog := filepath.Join(root, "missing", "postgres.log")
	err = startPostgres(data, missingLog, "-h 127.0.0.1 -p 49542 -c unix_socket_directories=''")
	if !errors.As(err, &exitErr) || !strings.Contains(err.Error(), "pg_ctl start:") {
		t.Fatalf("missing log replaced original startup error: %v", err)
	}
	if err := stopIfRunning(data); err != nil {
		t.Fatal(err)
	}
	if err := stopIfRunning(filepath.Join(root, "missing-data")); err == nil {
		t.Fatal("inaccessible data directory was treated as a stopped server")
	}
}
