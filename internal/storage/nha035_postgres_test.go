package storage

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func TestNHA035PostgresLiteralDatabaseCannotRetarget(t *testing.T) {
	cluster := testpg.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	settings := func(name string) Settings {
		s, e := ParseSettings(ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: name, User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(string) (string, bool) { return testpg.Password(), true })
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	admin, e := Open(ctx, settings("postgres"))
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	binary, e := exec.LookPath("pg_dump")
	if e != nil {
		t.Fatal(e)
	}
	binary, e = filepath.Abs(binary)
	if e != nil {
		t.Fatal(e)
	}
	tools, e := ParseBackupPGTools(filepath.Dir(binary))
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"host=127.0.0.2", "space name", "quote'name", "postgresql://evil/db", "postgres://evil/db", "reserved/?#%&=:@"} {
		t.Run(name, func(t *testing.T) {
			if _, e := admin.pool.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); e != nil {
				t.Fatal(e)
			}
			store, e := Open(ctx, settings(name))
			if e != nil {
				t.Fatal(e)
			}
			defer store.Close()
			connection := store.pool.Config().ConnConfig
			uri := backupConnectionURI(connection)
			// A round-trip checks escaping independently from the later native libpq
			// pg_dump; the actual dump below must reach this literal named database.
			parsed, e := pgx.ParseConfig(uri)
			if e != nil || parsed.Database != name || parsed.Host != "127.0.0.1" || parsed.Port != uint16(cluster.Port) || parsed.User != "homeward_test" {
				t.Fatalf("retargeted URI %q: %v", uri, e)
			}
			command, e := store.backupCommand(ctx, tools.dump, "--schema-only", "--no-owner", "--no-privileges")
			if e != nil {
				t.Fatal(e)
			}
			for _, arg := range command.Args {
				if strings.Contains(arg, testpg.Password()) {
					t.Fatal("password in process arguments")
				}
			}
			var output bytes.Buffer
			command.Stdout = &output
			command.Stderr = io.Discard
			if e = command.Run(); e != nil {
				t.Fatalf("literal database %q was not reached through native libpq: %v", name, e)
			}
			if !strings.Contains(output.String(), "PostgreSQL database dump") {
				t.Fatal("native dump missing")
			}
			if strings.Contains(uri, testpg.Password()) || strings.Contains(uri, "@evil") {
				t.Fatal("URI leaked or retargeted")
			}
		})
	}
	connection := &pgx.ConnConfig{}
	connection.Host = "::1"
	connection.Port = uint16(cluster.Port)
	connection.User = "quoted'user@host"
	connection.Database = "literal=postgresql://elsewhere/a b"
	parsed, e := pgx.ParseConfig(backupConnectionURI(connection))
	if e != nil || parsed.Host != "::1" || strconv.Itoa(int(parsed.Port)) != strconv.Itoa(cluster.Port) || parsed.User != connection.User || parsed.Database != connection.Database {
		t.Fatal("IPv6/user URI round-trip failed", e)
	}
}
