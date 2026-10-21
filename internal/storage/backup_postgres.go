package storage

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"

	"github.com/jackc/pgx/v5"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// BackupPGTools admits operator-selected native binaries. No shell, automatic
// install, inherited PG configuration, or password command-line is involved.
type BackupPGTools struct{ dump, restore string }

func ParseBackupPGTools(directory string) (BackupPGTools, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return BackupPGTools{}, &Error{kind: InvalidSettings}
	}
	result := BackupPGTools{filepath.Join(directory, "pg_dump"), filepath.Join(directory, "pg_restore")}
	for _, path := range []string{result.dump, result.restore} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return BackupPGTools{}, &Error{kind: InvalidSettings}
		}
	}
	return result, nil
}
func (t BackupPGTools) Valid() bool  { return t.dump != "" && t.restore != "" }
func (BackupPGTools) String() string { return "backup.PostgreSQLTools(<operator>)" }

// pgVersion rejects a different major; these programs are an explicitly trusted
// operator tool boundary, not arbitrary commands from a request or YAML source.
func (s *Store) backupCommand(ctx context.Context, tool string, args ...string) (*exec.Cmd, error) {
	if nilRegistryPort(ctx) || s == nil || s.pool == nil || tool == "" {
		return nil, &Error{kind: InvalidSettings}
	}
	connection := s.pool.Config().ConnConfig
	version := exec.CommandContext(ctx, tool, "--version")
	version.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	var output boundedBackupOutput
	version.Stdout = &output
	version.Stderr = io.Discard
	if version.Run() != nil {
		return nil, &Error{kind: Unavailable}
	}
	fields := strings.Fields(output.text)
	if len(fields) < 3 {
		return nil, &Error{kind: InvalidSettings}
	}
	major, err := strconv.Atoi(strings.Split(fields[2], ".")[0])
	if err != nil {
		return nil, &Error{kind: InvalidSettings}
	}
	var server int
	if s.pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&server) != nil || major != server/10000 {
		return nil, &Error{kind: InvalidSettings}
	}
	flags := []string{"--host=" + connection.Host, "--port=" + strconv.Itoa(int(connection.Port)), "--username=" + connection.User, "--dbname=" + backupConnectionURI(connection), "--no-password"}
	command := exec.CommandContext(ctx, tool, append(flags, args...)...)
	command.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "PGPASSWORD=" + connection.Password, "PGPASSFILE=/dev/null", "PGSSLMODE=disable", "PGCONNECT_TIMEOUT=5", "PGOPTIONS=-c timezone=UTC -c datestyle=ISO"}
	command.Stderr = io.Discard
	return command, nil
}

type boundedBackupOutput struct{ text string }

func (b *boundedBackupOutput) Write(p []byte) (int, error) {
	if len(b.text)+len(p) > 256 {
		return 0, &Error{kind: ConfigInvalid}
	}
	b.text += string(p)
	return len(p), nil
}
func (s *Store) DumpBackupSnapshot(ctx context.Context, snapshot *BackupSnapshot, tools BackupPGTools, destination io.Writer) error {
	if nilRegistryPort(ctx) || snapshot == nil || snapshot.store != s || !tools.Valid() || nilRegistryPort(destination) {
		return &Error{kind: ConfigInvalid}
	}
	snapshot.mu.Lock()
	defer snapshot.mu.Unlock()
	if snapshot.closed {
		return &Error{kind: ConfigInvalid}
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	command, err := s.backupCommand(bounded, tools.dump, "--format=custom", "--no-owner", "--no-privileges", "--snapshot="+snapshot.token)
	if err != nil {
		return err
	}
	command.Stdout = destination
	if command.Run() != nil {
		return &Error{kind: Unavailable}
	}
	return nil
}

// ImportBackupIntoEmpty refuses every existing user relation, not only known
// Homeward tables. After a process starts, an error is conservatively reported
// as indeterminate; a transport error does not prove rollback.
func (s *Store) ImportBackupIntoEmpty(ctx context.Context, tools BackupPGTools, dump string) (TransactionOutcome, error) {
	if nilRegistryPort(ctx) || s == nil || s.pool == nil || !tools.Valid() || !filepath.IsAbs(dump) {
		return TransactionNotCommitted, &Error{kind: InvalidSettings}
	}
	var count int
	if s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp_%' AND c.relkind IN ('r','p','v','m','S','f')`).Scan(&count) != nil {
		return TransactionNotCommitted, &Error{kind: Unavailable}
	}
	if count != 0 {
		return TransactionNotCommitted, &Error{kind: ConfigConflict}
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	command, err := s.backupCommand(bounded, tools.restore, "--single-transaction", "--exit-on-error", "--no-owner", "--no-privileges", dump)
	if err != nil {
		return TransactionNotCommitted, err
	}
	if err = command.Start(); err != nil {
		return TransactionNotCommitted, &Error{kind: Unavailable}
	}
	if err = command.Wait(); err != nil {
		return TransactionIndeterminate, &Error{kind: TransactionFailed}
	}
	return TransactionCommitted, nil
}

var _ fmt.Stringer = BackupPGTools{}

// Serialize the literal database as a URI path component. Passing a raw name
// to --dbname would let libpq reinterpret names containing '=' or URI prefixes
// as connection strings and override the already admitted host/user.
func backupConnectionURI(connection *pgx.ConnConfig) string {
	target := url.URL{Scheme: "postgresql", User: url.User(connection.User), Host: net.JoinHostPort(connection.Host, strconv.Itoa(int(connection.Port))), Path: "/" + connection.Database, RawPath: "/" + url.PathEscape(connection.Database)}
	return target.String()
}
