package testpg

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestNHA114StoppedClusterRestartsTheSameCatalogAndPort(t *testing.T) {
	cluster := Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connect := func() *pgx.Conn {
		t.Helper()
		conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://homeward_test:%s@127.0.0.1:%d/postgres?sslmode=disable", Password(), cluster.Port))
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	first := connect()
	defer first.Close(context.Background())
	var before string
	if err := first.QueryRow(ctx, "SHOW data_directory").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(ctx, "CREATE TABLE nha114_restart(value integer NOT NULL); INSERT INTO nha114_restart VALUES (114)"); err != nil {
		t.Fatal(err)
	}
	if err := cluster.Restart(); err == nil {
		t.Fatal("restart accepted a running cluster")
	}
	if err := cluster.Stop(); err != nil {
		t.Fatal(err)
	}
	assertStopped(t, before)
	if _, err := first.Exec(ctx, "SELECT 1"); err == nil {
		t.Fatal("old database connection survived the outage")
	}
	if err := cluster.Stop(); err != nil {
		t.Fatal("stop is not idempotent", err)
	}
	if err := cluster.Restart(); err != nil {
		t.Fatal(err)
	}
	second := connect()
	defer second.Close(context.Background())
	var after string
	var port, value int
	if err := second.QueryRow(ctx, "SHOW data_directory").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := second.QueryRow(ctx, "SELECT inet_server_port(), value FROM nha114_restart").Scan(&port, &value); err != nil {
		t.Fatal(err)
	}
	if before != after || port != cluster.Port || value != 114 {
		t.Fatalf("restart replaced the cluster: directory %q -> %q, port %d, value %d", before, after, port, value)
	}
	// Leave the restarted process to the original test cleanup. Its stop must
	// still run after an earlier explicit Stop and Restart cycle.
}
