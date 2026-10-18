// Package storage owns the PostgreSQL connection and the authoritative schema migrations.
package storage

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConnectionInput is untrusted configuration. ParseSettings turns it into Settings.
type ConnectionInput struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Database    string `yaml:"database"`
	User        string `yaml:"user"`
	PasswordEnv string `yaml:"password_env"`
	SSLMode     string `yaml:"sslmode"`
}

type Settings struct {
	host, database, user, passwordEnv string
	lookup                            func(string) (string, bool)
	port                              uint16
	sslMode                           sslMode
}

func (s Settings) String() string   { return "postgres: explicit settings" }
func (s Settings) GoString() string { return s.String() }

type sslMode uint8

const (
	sslUnknown sslMode = iota
	sslDisabled
)

var dnsLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// ParseSettings resolves exactly one named operator secret. The YAML never holds
// the bootstrap password, and lookup is injectable for isolated tests.
func ParseSettings(in ConnectionInput, lookup func(string) (string, bool)) (Settings, error) {
	if in.Host == "" || len(in.Host) > 253 || in.Port < 1 || in.Port > 65535 ||
		in.Database == "" || in.User == "" || !envName.MatchString(in.PasswordEnv) || in.SSLMode != "disable" || lookup == nil ||
		strings.ContainsAny(in.Database, "\x00\r\n") || strings.ContainsAny(in.User, "\x00\r\n") ||
		strings.ContainsAny(in.Host, "\x00\r\n") {
		return Settings{}, &Error{kind: InvalidSettings}
	}
	password, ok := lookup(in.PasswordEnv)
	if !ok || password == "" || strings.ContainsAny(password, "\x00\r\n") {
		return Settings{}, &Error{kind: InvalidSettings}
	}
	if _, err := netip.ParseAddr(in.Host); err != nil {
		for _, label := range strings.Split(in.Host, ".") {
			if !dnsLabel.MatchString(label) {
				return Settings{}, &Error{kind: InvalidSettings}
			}
		}
	}
	return Settings{host: in.Host, port: uint16(in.Port), database: in.Database, user: in.User, passwordEnv: in.PasswordEnv, lookup: lookup, sslMode: sslDisabled}, nil
}

func (s Settings) Valid() bool {
	return s.host != "" && s.port != 0 && s.database != "" && s.user != "" && s.passwordEnv != "" && s.lookup != nil && s.sslMode == sslDisabled
}

// Store is a bounded connection pool. Call Close on every successful Open.
type Store struct{ pool *pgxpool.Pool }

func (s *Store) String() string   { return "postgres: store" }
func (s *Store) GoString() string { return s.String() }

func Open(ctx context.Context, settings Settings) (*Store, error) {
	if ctx == nil {
		return nil, &Error{kind: InvalidContext}
	}
	if !settings.Valid() {
		return nil, &Error{kind: InvalidSettings}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	password, ok := settings.lookup(settings.passwordEnv)
	if !ok || password == "" || strings.ContainsAny(password, "\x00\r\n") {
		return nil, &Error{kind: InvalidSettings}
	}
	address := net.JoinHostPort(settings.host, fmt.Sprint(settings.port))
	u := url.URL{Scheme: "postgres", User: url.UserPassword(settings.user, password), Host: address, Path: "/" + settings.database}
	u.RawQuery = "sslmode=disable"
	config, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, &Error{kind: InvalidSettings}
	}
	// pgx follows libpq PG* variables and service files. No such value may retarget
	// this connection or add a fallback: the parsed settings are the sole authority.
	connection := config.ConnConfig
	if connection.Host != settings.host || connection.Port != settings.port ||
		connection.Database != settings.database || connection.User != settings.user ||
		connection.Password != password || connection.TLSConfig != nil || len(connection.Fallbacks) != 0 {
		return nil, &Error{kind: InvalidSettings}
	}
	connection.RuntimeParams = map[string]string{"application_name": "homeward"}
	connection.ConnectTimeout = 5 * time.Second
	config.MaxConns = 4
	config.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{kind: Unavailable}
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// InTransaction commits only after fn succeeds. A callback error rolls back.
func (s *Store) InTransaction(ctx context.Context, fn func(pgx.Tx) error) error {
	if ctx == nil {
		return &Error{kind: InvalidContext}
	}
	if s == nil || s.pool == nil || fn == nil {
		return &Error{kind: InvalidSettings}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{kind: Unavailable}
	}
	defer rollback(tx)
	if err := fn(tx); err != nil {
		return &Error{kind: TransactionFailed, cause: err}
	}
	if err := tx.Commit(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{kind: TransactionFailed}
	}
	return nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
