package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// BackupSource is an immutable copy admitted against one SQL snapshot. Its
// representation is deliberately redacted; exporting bytes is explicit.
type BackupSource struct {
	path      string
	ref       ConfigVersionRef
	hash      [32]byte
	raw       []byte
	bootstrap bool
}

func (s BackupSource) Path() string                 { return s.path }
func (s BackupSource) Ref() ConfigVersionRef        { return s.ref }
func (s BackupSource) SHA256() [32]byte             { return s.hash }
func (s BackupSource) Bytes() []byte                { return append([]byte(nil), s.raw...) }
func (s BackupSource) Bootstrap() bool              { return s.bootstrap }
func (BackupSource) String() string                 { return "[REDACTED BACKUP SOURCE]" }
func (s BackupSource) GoString() string             { return s.String() }
func (s BackupSource) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, s.String()) }
func (s BackupSource) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// BackupTable is a witness, never an execution authority. It covers every
// public table, including optional Recorder history, using canonical UTC rows.
type BackupTable struct {
	Name   string `json:"name"`
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}
type BackupSnapshot struct {
	mu         sync.Mutex
	store      *Store
	tx         pgx.Tx
	token      string
	generation ConfigGenerationSnapshot
	sources    []BackupSource
	tables     []BackupTable
	cut        time.Time
	closed     bool
}

func (*BackupSnapshot) String() string                         { return "backup.SQLSnapshot(<private>)" }
func (s *BackupSnapshot) GoString() string                     { return s.String() }
func (s *BackupSnapshot) Format(f fmt.State, _ rune)           { _, _ = io.WriteString(f, s.String()) }
func (s *BackupSnapshot) MarshalJSON() ([]byte, error)         { return json.Marshal(s.String()) }
func (s *BackupSnapshot) Generation() ConfigGenerationSnapshot { return s.generation }
func (s *BackupSnapshot) Cut() time.Time                       { return s.cut }
func (s *BackupSnapshot) Sources() []BackupSource              { return append([]BackupSource(nil), s.sources...) }
func (s *BackupSnapshot) Tables() []BackupTable                { return append([]BackupTable(nil), s.tables...) }
func (s *BackupSnapshot) Close() error {
	if s == nil || nilRegistryPort(s.tx) {
		return &Error{kind: ConfigInvalid}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := s.tx.Rollback(ctx)
	if err == pgx.ErrTxClosed {
		return nil
	}
	return err
}

// BeginBackupSnapshot keeps the exporting transaction alive until Close. It
// never reads today's workspace and never blocks activation with a Go mutex.
func (s *Store) BeginBackupSnapshot(ctx context.Context, secrets *SecretRepository) (*BackupSnapshot, error) {
	if nilRegistryPort(ctx) || s == nil || s.pool == nil || !secrets.Valid() || secrets.store != s {
		return nil, &Error{kind: ConfigInvalid}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	ok := false
	defer func() {
		if !ok {
			rollback(tx)
		}
	}()
	if _, err = tx.Exec(ctx, `SELECT set_config('TimeZone','UTC',true),set_config('DateStyle','ISO, YMD',true),set_config('IntervalStyle','postgres',true),set_config('extra_float_digits','3',true),set_config('bytea_output','hex',true)`); err != nil {
		return nil, &Error{kind: Unavailable}
	}
	if err = validateBackupSchemaTx(ctx, tx); err != nil {
		return nil, err
	}
	var revision int64
	var cut time.Time
	// The clock and active pointer are selected at the same established snapshot.
	if err = tx.QueryRow(ctx, `SELECT generation_revision,clock_timestamp() FROM public.config_active WHERE singleton`).Scan(&revision, &cut); err != nil {
		return nil, &Error{kind: ConfigIntegrity}
	}
	generation, err := readConfigGenerationTx(ctx, tx, ConfigGenerationRevision{generation: revision})
	if err != nil {
		return nil, err
	}
	replay, err := NewConfigReplayRepository(s, secrets)
	if err != nil {
		return nil, err
	}
	sources := make([]BackupSource, 0, len(generation.versions)+1)
	for _, version := range generation.versions {
		raw := version.yaml
		if version.representation == ConfigSourceProtectedOperationalV1 {
			protected, e := replay.readProtectedSourceTx(ctx, tx, generation.revision, version.ref)
			if e != nil {
				return nil, e
			}
			raw = protected.Bytes()
		}
		if !version.present || sha256.Sum256(raw) != version.hash {
			return nil, &Error{kind: ConfigIntegrity}
		}
		sources = append(sources, BackupSource{path: version.path, ref: version.ref, hash: version.hash, raw: append([]byte(nil), raw...)})
	}
	bootstrap, present, err := readActiveBootstrapTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, &Error{kind: ConfigIncomplete}
	}
	sources = append(sources, BackupSource{path: bootstrap.path, ref: bootstrap.ref, hash: bootstrap.hash, raw: bootstrap.Bytes(), bootstrap: true})
	if err = validateBackupCredentials(ctx, tx, secrets); err != nil {
		return nil, err
	}
	tables, err := backupTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	var token string
	if err = tx.QueryRow(ctx, `SELECT pg_export_snapshot()`).Scan(&token); err != nil || token == "" || len(token) > 128 || strings.ContainsAny(token, "\x00\r\n") {
		return nil, &Error{kind: Unavailable}
	}
	ok = true
	return &BackupSnapshot{store: s, tx: tx, token: token, generation: generation, sources: sources, tables: tables, cut: cut.UTC()}, nil
}

func backupTables(ctx context.Context, tx pgx.Tx) ([]BackupTable, error) {
	rows, err := tx.Query(ctx, `SELECT tablename FROM pg_catalog.pg_tables WHERE schemaname='public' ORDER BY tablename COLLATE "C"`)
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) != nil || len(names) >= 512 {
			rows.Close()
			return nil, &Error{kind: ConfigIntegrity}
		}
		names = append(names, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	tables := make([]BackupTable, 0, len(names))
	var total int64
	for _, name := range names {
		query := `SELECT row_to_json(t)::text FROM ` + pgx.Identifier{"public", name}.Sanitize() + ` t ORDER BY row_to_json(t)::text COLLATE "C"`
		rows, err := tx.Query(ctx, query)
		if err != nil {
			return nil, &Error{kind: Unavailable}
		}
		h := sha256.New()
		var count int64
		for rows.Next() {
			var raw string
			if rows.Scan(&raw) != nil {
				rows.Close()
				return nil, &Error{kind: ConfigIntegrity}
			}
			total += int64(len(raw))
			if total > 512<<20 || count >= 2000000 {
				rows.Close()
				return nil, &Error{kind: ConfigInvalid}
			}
			_, _ = fmt.Fprintf(h, "%d:", len(raw))
			_, _ = io.WriteString(h, raw)
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, &Error{kind: Unavailable}
		}
		tables = append(tables, BackupTable{Name: name, Rows: count, SHA256: hex.EncodeToString(h.Sum(nil))})
	}
	if len(tables) == 0 {
		return nil, &Error{kind: ConfigIncomplete}
	}
	return tables, nil
}
