package helpers

import (
	"context"
	"errors"
	"math"
	"reflect"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Repository has no value cache or projection. Its caller supplies the admitted
// active definition; configuration activation and helper services remain separate.
type Repository struct{ store *storage.Store }

func NewRepository(store *storage.Store) (*Repository, error) {
	if store == nil {
		return nil, failure(InvalidInput)
	}
	return &Repository{store: store}, nil
}

// nilContext admits interface values before pgx can invoke their methods. A
// typed nil can implement context.Context while still panicking on every call.
func nilContext(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	value := reflect.ValueOf(ctx)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (r *Repository) transaction(ctx context.Context, fn func(pgx.Tx) error) error {
	if r == nil || r.store == nil || nilContext(ctx) || fn == nil {
		return failure(InvalidInput)
	}
	outcome, err := r.store.InTransactionOutcome(ctx, fn)
	if outcome == storage.TransactionIndeterminate {
		// Never retry: the primary must be read again before another CAS decision.
		return failure(CommitUnknown)
	}
	if err != nil {
		var expected *Error
		if errors.As(err, &expected) {
			return expected
		}
		return failure(Unavailable)
	}
	if outcome != storage.TransactionCommitted {
		return failure(Unavailable)
	}
	return nil
}

// Bind admits a stable EntityID using its configured source binding, rather than
// trusting ParseEntityID or resolving an alias to a new identity on every access.
func (r *Repository) Bind(ctx context.Context, entity domain.EntityID, definition Definition) (Handle, error) {
	if !definition.Valid() {
		return Handle{}, failure(InvalidDefinition)
	}
	handle := Handle{entity: entity, alias: definition.alias()}
	if !handle.valid() {
		return Handle{}, failure(InvalidInput)
	}
	if err := r.transaction(ctx, func(tx pgx.Tx) error { return lockIdentity(ctx, tx, handle) }); err != nil {
		return Handle{}, err
	}
	return handle, nil
}

func lockIdentity(ctx context.Context, tx pgx.Tx, handle Handle) error {
	entity, _ := handle.entity.Value()
	expectedAlias, _ := handle.alias.Value()
	var alias string
	var retired bool
	err := tx.QueryRow(ctx, `SELECT e.manifest_alias,e.tombstoned
		FROM public.registry_entities e
		JOIN public.registry_source_bindings b ON b.entity_id=e.id
		JOIN public.registry_device_sources d ON d.instance_id=b.instance_id
		    AND d.source_device=b.source_device AND d.device_id=e.device_id
		WHERE e.id=$1 FOR UPDATE OF e FOR SHARE OF b,d`, entity).Scan(&alias, &retired)
	if errors.Is(err, pgx.ErrNoRows) {
		return failure(Unregistered)
	}
	if err != nil {
		return err
	}
	if retired {
		return failure(Retired)
	}
	if alias != expectedAlias {
		return failure(BindingMismatch)
	}
	return nil
}

func validate(handle Handle, definition Definition) error {
	if !handle.valid() {
		return failure(InvalidInput)
	}
	if !definition.Valid() {
		return failure(InvalidDefinition)
	}
	if definition.alias() != handle.alias {
		return failure(BindingMismatch)
	}
	return nil
}

// Restore returns the last committed business value. Only a genuinely absent row
// may be initialized from YAML. An absent initial remains explicitly absent.
func (r *Repository) Restore(ctx context.Context, handle Handle, definition Definition) (Snapshot, error) {
	if err := validate(handle, definition); err != nil {
		return Snapshot{}, err
	}
	var result Snapshot
	err := r.transaction(ctx, func(tx pgx.Tx) error {
		if err := lockIdentity(ctx, tx, handle); err != nil {
			return err
		}
		current, err := read(ctx, tx, handle, definition)
		if err != nil {
			return err
		}
		if current.revision.state == revisionPresent {
			result = current
			return nil
		}
		initial, present := definition.initial()
		if !present {
			result = current
			return nil
		}
		result, err = write(ctx, tx, handle, definition, initial, current.revision)
		return err
	})
	if err != nil {
		return Snapshot{}, err
	}
	return result, nil
}

// CompareAndSwap compares and writes inside one SQL transaction. The registry
// row lock also serializes first insertion, when helper_values has no row to lock.
func (r *Repository) CompareAndSwap(ctx context.Context, handle Handle, definition Definition, expected Revision, value Value) (Snapshot, error) {
	if err := validate(handle, definition); err != nil {
		return Snapshot{}, err
	}
	if !expected.validFor(handle.entity) {
		return Snapshot{}, failure(InvalidInput)
	}
	if !definition.accepts(value) {
		return Snapshot{}, failure(InvalidValue)
	}
	var result Snapshot
	err := r.transaction(ctx, func(tx pgx.Tx) error {
		if err := lockIdentity(ctx, tx, handle); err != nil {
			return err
		}
		current, err := read(ctx, tx, handle, definition)
		if err != nil {
			return err
		}
		if current.revision != expected {
			return failure(Conflict)
		}
		result, err = write(ctx, tx, handle, definition, value, expected)
		return err
	})
	if err != nil {
		return Snapshot{}, err
	}
	return result, nil
}

// Reset is an explicit revisioned mutation, including when the initial is equal
// to the current value. It never deletes persistence or rewrites configuration.
func (r *Repository) Reset(ctx context.Context, handle Handle, definition Definition, expected Revision) (Snapshot, error) {
	if err := validate(handle, definition); err != nil {
		return Snapshot{}, err
	}
	initial, present := definition.initial()
	if !present {
		return Snapshot{}, failure(InitialAbsent)
	}
	return r.CompareAndSwap(ctx, handle, definition, expected, initial)
}

// sqlValue is a typed SQL boundary, not a second operational value model.
type sqlValue struct {
	boolean pgtype.Bool
	number  pgtype.Float8
	counter pgtype.Int8
}

func encode(value Value) (int16, sqlValue, error) {
	if !value.Valid() {
		return 0, sqlValue{}, failure(InvalidValue)
	}
	switch value.kind {
	case KindBoolean:
		return 1, sqlValue{boolean: pgtype.Bool{Bool: value.boolean, Valid: true}}, nil
	case KindNumber:
		return 2, sqlValue{number: pgtype.Float8{Float64: value.number, Valid: true}}, nil
	case KindCounter:
		return 3, sqlValue{counter: pgtype.Int8{Int64: value.counter, Valid: true}}, nil
	default:
		return 0, sqlValue{}, failure(InvalidValue)
	}
}

func decode(tag int16, value sqlValue) (Value, error) {
	switch tag {
	case 1:
		if value.boolean.Valid && !value.number.Valid && !value.counter.Valid {
			return Boolean(value.boolean.Bool), nil
		}
	case 2:
		if !value.boolean.Valid && value.number.Valid && !value.counter.Valid {
			number, err := Number(value.number.Float64)
			if err == nil {
				return number, nil
			}
		}
	case 3:
		if !value.boolean.Valid && !value.number.Valid && value.counter.Valid {
			return Counter(value.counter.Int64), nil
		}
	}
	return Value{}, failure(Corrupt)
}

func read(ctx context.Context, tx pgx.Tx, handle Handle, definition Definition) (Snapshot, error) {
	entity, _ := handle.entity.Value()
	var tag int16
	var payload sqlValue
	var revision int64
	var updated pgtype.Timestamptz
	err := tx.QueryRow(ctx, `SELECT kind,boolean_value,number_value,counter_value,revision,updated_at
		FROM public.helper_values WHERE entity_id=$1`, entity).Scan(&tag, &payload.boolean, &payload.number, &payload.counter, &revision, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{handle: handle, kind: definition.kind, revision: Revision{entity: handle.entity, state: revisionAbsent}}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	value, err := decode(tag, payload)
	if err != nil || revision <= 0 || !updated.Valid || updated.InfinityModifier != pgtype.Finite || updated.Time.IsZero() || updated.Time.Year() < 1 || updated.Time.Year() > 9999 {
		return Snapshot{}, failure(Corrupt)
	}
	if !definition.accepts(value) {
		return Snapshot{}, failure(Incompatible)
	}
	return Snapshot{handle: handle, kind: definition.kind, value: value,
		revision: Revision{entity: handle.entity, state: revisionPresent, number: revision}, updatedAt: updated.Time}, nil
}

func write(ctx context.Context, tx pgx.Tx, handle Handle, definition Definition, value Value, previous Revision) (Snapshot, error) {
	if !definition.accepts(value) {
		return Snapshot{}, failure(InvalidValue)
	}
	if previous.number == math.MaxInt64 {
		return Snapshot{}, failure(RevisionExhausted)
	}
	tag, payload, err := encode(value)
	if err != nil {
		return Snapshot{}, err
	}
	entity, _ := handle.entity.Value()
	revision := previous.number + 1
	var updated time.Time
	if previous.state == revisionAbsent {
		err = tx.QueryRow(ctx, `INSERT INTO public.helper_values(entity_id,kind,boolean_value,number_value,counter_value,revision,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,clock_timestamp()) RETURNING updated_at`, entity, tag, payload.boolean, payload.number, payload.counter, revision).Scan(&updated)
	} else {
		err = tx.QueryRow(ctx, `UPDATE public.helper_values SET kind=$2,boolean_value=$3,number_value=$4,counter_value=$5,revision=$6,updated_at=clock_timestamp()
			WHERE entity_id=$1 AND revision=$7 RETURNING updated_at`, entity, tag, payload.boolean, payload.number, payload.counter, revision, previous.number).Scan(&updated)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, failure(Conflict)
	}
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{handle: handle, kind: definition.kind, value: value,
		revision: Revision{entity: handle.entity, state: revisionPresent, number: revision}, updatedAt: updated}, nil
}
