package storage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

const testPassword = "NHA027_canary_password"

type secretError string

func (e secretError) Error() string { return string(e) }

func testCluster(t *testing.T) int {
	return testpg.Start(t).Port
}

func testSettings(t *testing.T, port int) Settings {
	t.Helper()
	settings, err := ParseSettings(ConnectionInput{Host: "127.0.0.1", Port: port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(key string) (string, bool) {
		if key != "HOMEWARD_TEST_PASSWORD" {
			t.Fatalf("unexpected secret lookup %s", key)
		}
		return testpg.Password(), true
	})
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestNHA027MigrationsAndRollback(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base, err := store.migrate(ctx, migrations[:1])
	if err != nil || base.Number() != 1 {
		t.Fatalf("original migration: %v %v", base, err)
	}
	var applied time.Time
	if err := store.pool.QueryRow(ctx, `SELECT applied_at FROM public.homeward_schema_migrations WHERE version=1`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	first, err := store.Migrate(ctx)
	if err != nil || first != CurrentVersion() {
		t.Fatalf("upgrade from original migration: %v %v", first, err)
	}
	second, err := store.Migrate(ctx)
	if err != nil || second != first {
		t.Fatalf("second migration: %v %v", second, err)
	}
	var count int
	var appliedAgain time.Time
	if err := store.pool.QueryRow(ctx, `SELECT count(*), min(applied_at) FROM public.homeward_schema_migrations`).Scan(&count, &appliedAgain); err != nil {
		t.Fatal(err)
	}
	if count != int(CurrentVersion().Number()) || !applied.Equal(appliedAgain) {
		t.Fatal("second pass modified migration history")
	}
	if _, err := store.pool.Exec(ctx, `CREATE TABLE public.nha027_fixture(value integer NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.nha027_fixture VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	plan := append([]migration(nil), migrations...)
	plan = append(plan,
		migration{version: Version{CurrentVersion().Number() + 1}, sql: `INSERT INTO public.nha027_fixture VALUES (2)`},
		migration{version: Version{CurrentVersion().Number() + 2}, sql: `CREATE TABLE public.nha027_rolled_back(value integer)`},
		migration{version: Version{CurrentVersion().Number() + 3}, sql: `INSERT INTO public.missing_relation VALUES (1)`},
	)
	_, err = store.migrate(ctx, plan)
	var storageErr *Error
	if !errors.As(err, &storageErr) || storageErr.Kind() != MigrationFailed {
		t.Fatalf("failure classification: %v", err)
	}
	var fixtureCount, versionCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.nha027_fixture`).Scan(&fixtureCount); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.homeward_schema_migrations`).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	var rolledBack bool
	if err := store.pool.QueryRow(ctx, `SELECT to_regclass('public.nha027_rolled_back') IS NULL`).Scan(&rolledBack); err != nil {
		t.Fatal(err)
	}
	if fixtureCount != 1 || versionCount != int(CurrentVersion().Number()) || !rolledBack {
		t.Fatalf("rollback incomplete: data=%d versions=%d ddl=%t", fixtureCount, versionCount, rolledBack)
	}
	futureVersion := CurrentVersion().Number() + 1
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.homeward_schema_migrations(version) VALUES ($1)`, futureVersion); err != nil {
		t.Fatal(err)
	}
	_, err = store.Migrate(ctx)
	if !errors.As(err, &storageErr) || storageErr.Kind() != FutureSchema || storageErr.FoundVersion().Number() != futureVersion {
		t.Fatalf("future schema: %v", err)
	}
	if countErr := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.homeward_schema_migrations`).Scan(&versionCount); countErr != nil || versionCount != int(futureVersion) {
		t.Fatalf("future schema modified database: %d %v", versionCount, countErr)
	}
}

func TestNHA027SettingsAndRedaction(t *testing.T) {
	for _, input := range []ConnectionInput{
		{}, {Host: "/tmp/socket", Port: 5432, Database: "test", User: "test", PasswordEnv: "PASS", SSLMode: "disable"},
		{Host: "127.0.0.1", Port: 0, Database: "test", User: "test", PasswordEnv: "PASS", SSLMode: "disable"},
		{Host: "127.0.0.1", Port: 5432, Database: "test", User: "test", PasswordEnv: "PASS", SSLMode: "prefer"},
		{Host: "127.0.0.1", Port: 5432, Database: "test", User: "test", PasswordEnv: "PASS", SSLMode: "verify-full"},
		{Host: "127.0.0.1", Port: 5432, Database: "test", User: "test", PasswordEnv: "bad-name", SSLMode: "disable"},
	} {
		if _, err := ParseSettings(input, func(string) (string, bool) { return testPassword, true }); err == nil || strings.Contains(err.Error(), testPassword) {
			t.Fatalf("accepted or leaked invalid settings: %v", err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	settings := testSettings(t, listener.Addr().(*net.TCPAddr).Port)
	listener.Close()
	if err := OpenUnavailable(t, settings); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), Settings{}); err == nil || strings.Contains(err.Error(), testPassword) {
		t.Fatalf("zero settings: %v", err)
	}
	if _, err := Open(nil, settings); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestNHA027TransactionIdentityAndEnvironment(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings := testSettings(t, port)
	store, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(nil); err == nil {
		t.Fatal("nil migration context accepted")
	}
	if err := store.InTransaction(nil, func(pgx.Tx) error { return nil }); err == nil {
		t.Fatal("nil transaction context accepted")
	}
	canaryCause := secretError(testPassword)
	redacted := &Error{kind: TransactionFailed, cause: canaryCause}
	if !errors.Is(redacted, canaryCause) {
		t.Fatal("cause identity lost")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if rendered := fmt.Sprintf(format, redacted); strings.Contains(rendered, testPassword) {
			t.Fatalf("%s exposed callback secret", format)
		}
	}
	for _, rendered := range []string{fmt.Sprintf("%+v", store), fmt.Sprintf("%#v", store)} {
		if strings.Contains(rendered, testPassword) {
			t.Fatal("formatted store exposed secret")
		}
	}
	if _, err := store.pool.Exec(ctx, `CREATE TABLE public.nha027_tx(value integer)`); err != nil {
		t.Fatal(err)
	}
	marker := errors.New("business rejection")
	err = store.InTransaction(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO public.nha027_tx VALUES (1)`); err != nil {
			return err
		}
		return marker
	})
	if !errors.Is(err, marker) || strings.Contains(err.Error(), "business rejection") {
		t.Fatalf("callback identity or diagnostic: %v", err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.nha027_tx`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("callback transaction committed: %d %v", count, err)
	}
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `INSERT INTO public.nha027_tx VALUES (2)`); return err }); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.nha027_tx`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("transaction did not commit: %d %v", count, err)
	}
	// A hostile libpq service file must never retarget an explicit connection.
	serviceFile := filepath.Join(t.TempDir(), "rogue-service")
	if err := os.WriteFile(serviceFile, []byte("[rogue]\nhost=127.0.0.2\nport=1\ndbname=other\nuser=other\nsslmode=prefer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", serviceFile)
	t.Setenv("PGSERVICE", "rogue")
	hostile, err := Open(ctx, settings)
	if err != nil {
		t.Fatalf("explicit settings rejected by hostile environment: %v", err)
	}
	defer hostile.Close()
	var database, user string
	var actualPort int
	if err := hostile.pool.QueryRow(ctx, `SELECT current_database(), current_user, inet_server_port()`).Scan(&database, &user, &actualPort); err != nil {
		t.Fatal(err)
	}
	if database != "postgres" || user != "homeward_test" || actualPort != port {
		t.Fatalf("PGSERVICE retargeted connection: %s %s %d", database, user, actualPort)
	}
}

func OpenUnavailable(t *testing.T, settings Settings) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store, err := Open(ctx, settings)
	if store != nil {
		store.Close()
		return fmt.Errorf("unexpected connection")
	}
	var classified *Error
	if !errors.As(err, &classified) || classified.Kind() != Unavailable {
		return fmt.Errorf("unavailable classification: %v", err)
	}
	if strings.Contains(err.Error(), testPassword) || strings.Contains(err.Error(), "homeward_test") {
		return fmt.Errorf("secret or username in diagnostic")
	}
	return nil
}
