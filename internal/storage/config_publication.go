package storage

import (
	"bytes"
	"context"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConfigPublicationGuard holds only the existing 151 exclusion across the
// final memory swap. Preparation, SQL writes, drivers and joins precede it.
// Release runs outside every runtime data mutex and writes no SQL facts.
type ConfigPublicationGuard struct {
	conn     *pgxpool.Conn
	tx       pgx.Tx
	released atomic.Bool
}

func (s *Store) GuardConfigPublication(ctx context.Context, target ConfigGenerationRevision, manifest [32]byte) (*ConfigPublicationGuard, error) {
	if s == nil || s.pool == nil || nilRegistryPort(ctx) || !target.Valid() || manifest == ([32]byte{}) {
		return nil, &Error{kind: ConfigInvalid}
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, &Error{kind: Unavailable, cause: err}
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		return nil, &Error{kind: Unavailable, cause: err}
	}
	g := &ConfigPublicationGuard{conn: conn, tx: tx}
	valid := false
	defer func() {
		if !valid {
			_ = g.Release()
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481316, 151)`); err != nil {
		return nil, &Error{kind: Unavailable, cause: err}
	}
	var revision int64
	var hash []byte
	if err := tx.QueryRow(ctx, `SELECT a.generation_revision,g.manifest_hash FROM public.config_active a JOIN public.config_generations g ON g.revision=a.generation_revision WHERE a.singleton AND g.sealed`).Scan(&revision, &hash); err != nil {
		return nil, &Error{kind: ConfigIntegrity, cause: err}
	}
	if revision != target.Number() || !bytes.Equal(hash, manifest[:]) {
		return nil, &Error{kind: ConfigConflict}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	valid = true
	return g, nil
}

func (g *ConfigPublicationGuard) Release() error {
	if g == nil || g.conn == nil || g.tx == nil || !g.released.CompareAndSwap(false, true) {
		return &Error{kind: ConfigInvalid}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := g.tx.Rollback(ctx)
	g.conn.Release()
	if err == pgx.ErrTxClosed {
		return nil
	}
	return err
}
