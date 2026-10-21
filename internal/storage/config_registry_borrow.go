package storage

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RegistryBorrow lends only statement execution to the registry during one
// ConfigTx callback. It cannot commit, roll back, change stores or outlive the
// callback. The owner has already acquired config lock 151; this borrow takes
// registry lock 15 on the same transaction before any registry statement.
type RegistryBorrow struct {
	owner *ConfigTx
	live  bool
	open  *registryBorrowRow
}

func (c *ConfigTx) WithRegistryBorrow(store *Store, fn func(*RegistryBorrow) error) (err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return err
	}
	if store == nil || store != c.store || fn == nil {
		return &Error{kind: ConfigInvalid}
	}
	if _, err := c.tx.Exec(c.ctx, `SELECT pg_advisory_xact_lock(481316,15)`); err != nil {
		return err
	}
	borrow := &RegistryBorrow{owner: c, live: true}
	defer func() {
		borrow.closeOpen()
		borrow.live = false
	}()
	return fn(borrow)
}

func (b *RegistryBorrow) closeOpen() {
	if b == nil || b.open == nil {
		return
	}
	row := b.open
	b.open = nil
	row.finished = true
	// Leaving a result unexamined is an invalid borrowed operation. Consume it
	// before the next ConfigTx statement using only locally owned destinations.
	b.owner.remember(&Error{kind: ConfigInvalid})
	if err := row.statement.drainRegistryRow(row.row); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		b.owner.remember(err)
	}
}

func (b *RegistryBorrow) valid() error {
	if b == nil || !b.live || b.owner == nil {
		return &Error{kind: ConfigTransactionClosed}
	}
	return b.owner.valid()
}

// Exec and QueryRow use only the closed registry vocabulary and the owning
// ConfigTx context. Neither SQL text nor another transaction can be supplied.
func (b *RegistryBorrow) Exec(statement RegistryStatement, args ...string) (pgconn.CommandTag, error) {
	if err := b.valid(); err != nil {
		return pgconn.CommandTag{}, err
	}
	if b.open != nil {
		err := &Error{kind: ConfigInvalid}
		b.owner.remember(err)
		return pgconn.CommandTag{}, err
	}
	tag, err := ExecuteRegistryCommand(b.owner.ctx, b.owner.tx, statement, args...)
	b.owner.remember(err)
	return tag, err
}

type registryBorrowRow struct {
	borrow    *RegistryBorrow
	row       pgx.Row
	statement RegistryStatement
	finished  bool
}

func (r *registryBorrowRow) Scan(dest ...any) error {
	if r == nil || r.borrow == nil {
		return &Error{kind: ConfigTransactionClosed}
	}
	if err := r.borrow.valid(); err != nil {
		return err
	}
	if r.finished || r.borrow.open != r {
		err := &Error{kind: ConfigInvalid}
		r.borrow.owner.remember(err)
		return err
	}
	r.finished = true
	r.borrow.open = nil
	if err := r.statement.admitScan(dest); err != nil {
		r.borrow.owner.remember(err)
		if drainErr := r.statement.drainRegistryRow(r.row); drainErr != nil && !errors.Is(drainErr, pgx.ErrNoRows) {
			r.borrow.owner.remember(drainErr)
		}
		return err
	}
	err := r.row.Scan(dest...)
	// A missing row is an expected branch in identity creation; all other SQL
	// failures poison ConfigTx even if a callback discards its returned error.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		r.borrow.owner.remember(err)
	}
	return err
}

func (b *RegistryBorrow) QueryRow(statement RegistryStatement, args ...string) pgx.Row {
	if err := b.valid(); err != nil {
		return closedRegistryRow{err}
	}
	if b.open != nil {
		err := &Error{kind: ConfigInvalid}
		b.owner.remember(err)
		return closedRegistryRow{err}
	}
	row, err := QueryRegistryRow(b.owner.ctx, b.owner.tx, statement, args...)
	if err != nil {
		b.owner.remember(err)
		return closedRegistryRow{err}
	}
	borrowed := &registryBorrowRow{borrow: b, row: row, statement: statement}
	b.open = borrowed
	return borrowed
}

type closedRegistryRow struct{ err error }

func (r closedRegistryRow) Scan(...any) error { return r.err }
