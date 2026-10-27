package calendars

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Repository shares Store and registry identities. It has no YAML writer,
// source client, value cache, sequencer, or separate migration authority.
type Repository struct {
	store                      *storage.Store
	manifest                   registry.Manifest
	clock                      RemoteClock
	beforeRemoteFreshAdmission func()
	beforeRemoteWindowPage     func(int) error
	beforeImageLoad            func()
	beforeExpansion            func(context.Context) error
	remoteMu                   sync.Mutex
	remoteProcesses            map[CalendarID]*remoteProcess
	afterRemoteRead            func(remoteDecodeObservation)
	afterRemoteMutation        func(remoteDecodeObservation)
}

type RemoteClock interface {
	Now() time.Time
	MonoNow() time.Duration
}
type realRemoteClock struct{ origin time.Time }

func (c realRemoteClock) Now() time.Time         { return time.Now() }
func (c realRemoteClock) MonoNow() time.Duration { return time.Since(c.origin) }
func invalidRemoteClock(c RemoteClock) bool {
	if c == nil {
		return true
	}
	v := reflect.ValueOf(c)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Slice, reflect.Chan, reflect.Interface:
		return v.IsNil()
	}
	return false
}
func NewRepository(store *storage.Store, manifest registry.Manifest) (*Repository, error) {
	return NewRepositoryWithClock(store, manifest, realRemoteClock{origin: time.Now()})
}
func NewRepositoryWithClock(store *storage.Store, manifest registry.Manifest, clock RemoteClock) (*Repository, error) {
	_, valid := manifest.Digest()
	if store == nil || !valid || invalidRemoteClock(clock) || clock.Now().IsZero() || clock.MonoNow() < 0 {
		return nil, failure(InvalidInput)
	}
	return &Repository{store: store, manifest: manifest, clock: clock}, nil
}
func (r *Repository) transaction(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if r == nil || r.store == nil || fn == nil {
		return failure(InvalidInput)
	}
	if err := contextValid(ctx); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	outcome, err := r.store.InTransactionOutcome(bounded, func(tx pgx.Tx) error {
		if err := fenceNetwork(bounded, tx); err != nil {
			return err
		}
		return fn(bounded, tx)
	})
	if outcome == storage.TransactionIndeterminate {
		return failure(CommitUnknown)
	}
	if err != nil {
		if networkDenied(err) {
			return err
		}
		var expected *Error
		if errors.As(err, &expected) {
			return expected
		}
		if bounded.Err() != nil {
			return &Error{code: Canceled, cause: bounded.Err()}
		}
		return failure(Unavailable)
	}
	if outcome != storage.TransactionCommitted {
		return failure(Unavailable)
	}
	return nil
}
func originFor(manifest registry.Manifest, alias domain.HAAlias) (Origin, error) {
	source, present := manifest.SourceFor(alias)
	text, valid := source.Value()
	if !present || !valid {
		return OriginInvalid, failure(Unregistered)
	}
	switch text {
	case "local_calendar":
		return Local, nil
	case "remote_calendar":
		return RemoteICS, nil
	case "workday":
		return Calculated, nil
	default:
		return OriginInvalid, failure(Unregistered)
	}
}
func (r *Repository) identity(ctx context.Context, tx pgx.Tx, entity domain.EntityID, expected *CalendarID, write bool) (CalendarID, error) {
	value, err := entity.Value()
	if err != nil {
		return CalendarID{}, failure(InvalidInput)
	}
	var alias string
	var retired bool
	query := `SELECT e.manifest_alias,e.tombstoned
		FROM public.registry_entities e
		JOIN public.registry_source_bindings b ON b.entity_id=e.id
		JOIN public.registry_device_sources d ON d.instance_id=b.instance_id AND d.source_device=b.source_device AND d.device_id=e.device_id
		WHERE e.id=$1`
	if write {
		query += ` FOR UPDATE OF e FOR SHARE OF b,d`
	} else {
		query += ` FOR SHARE OF e,b,d`
	}
	err = tx.QueryRow(ctx, query, value).Scan(&alias, &retired)
	if errors.Is(err, pgx.ErrNoRows) {
		return CalendarID{}, failure(Unregistered)
	}
	if err != nil {
		return CalendarID{}, err
	}
	if retired {
		return CalendarID{}, failure(Retired)
	}
	parsed, err := domain.ParseHAAlias(alias)
	if err != nil {
		return CalendarID{}, failure(Corrupt)
	}
	origin, err := originFor(r.manifest, parsed)
	if err != nil {
		return CalendarID{}, err
	}
	id := CalendarID{entity: entity, alias: parsed, origin: origin}
	if expected != nil && *expected != id {
		return CalendarID{}, failure(BindingMismatch)
	}
	return id, nil
}
func (r *Repository) Bind(ctx context.Context, entity domain.EntityID) (CalendarID, error) {
	var id CalendarID
	err := r.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = r.identity(ctx, tx, entity, nil, true)
		if err == nil && id.origin == RemoteICS {
			err = r.ensureRemoteBound(ctx, tx, id)
		}
		return err
	})
	if err != nil {
		return CalendarID{}, err
	}
	return id, nil
}
func writable(id CalendarID) error {
	if !id.Valid() {
		return failure(InvalidInput)
	}
	if id.origin != Local {
		return failure(ReadOnly)
	}
	return nil
}
func nextRevision(previous int64) (int64, error) {
	if previous < 0 {
		return 0, failure(Corrupt)
	}
	if previous == math.MaxInt64 {
		return 0, failure(RevisionExhausted)
	}
	return previous + 1, nil
}

func readCalendar(ctx context.Context, tx pgx.Tx, id CalendarID, write bool) (CalendarSnapshot, error) {
	entity, _ := id.entity.Value()
	query := `SELECT origin,name,lifecycle,revision,updated_at FROM public.local_calendars WHERE entity_id=$1`
	if write {
		query += ` FOR UPDATE`
	} else {
		query += ` FOR SHARE`
	}
	var origin, life int16
	var name string
	var revision int64
	var updated pgtype.Timestamptz
	err := tx.QueryRow(ctx, query, entity).Scan(&origin, &name, &life, &revision, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return CalendarSnapshot{id: id, revision: Revision{calendar: id, presence: Absent}}, nil
	}
	if err != nil {
		return CalendarSnapshot{}, err
	}
	if origin != 1 || id.origin != Local {
		return CalendarSnapshot{}, failure(Incompatible)
	}
	presence, err := presenceFromSQL(life)
	if err != nil || revision <= 0 || !textValid(name, 512, true) {
		return CalendarSnapshot{}, failure(Corrupt)
	}
	stamp, err := validUpdated(updated)
	if err != nil {
		return CalendarSnapshot{}, err
	}
	return CalendarSnapshot{id: id, name: name, revision: Revision{calendar: id, presence: presence, number: revision}, updatedAt: stamp}, nil
}
func (r *Repository) GetCalendar(ctx context.Context, id CalendarID) (CalendarSnapshot, error) {
	if !id.Valid() {
		return CalendarSnapshot{}, failure(InvalidInput)
	}
	var snapshot CalendarSnapshot
	err := r.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := r.identity(ctx, tx, id.entity, &id, false); err != nil {
			return err
		}
		var err error
		snapshot, err = r.readCalendar(ctx, tx, id, false)
		return err
	})
	if err != nil {
		return CalendarSnapshot{}, err
	}
	return snapshot, nil
}
func (r *Repository) ListCalendars(ctx context.Context) ([]CalendarSnapshot, error) {
	var snapshots []CalendarSnapshot
	err := r.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT entity_id FROM public.local_calendars UNION SELECT entity_id FROM public.remote_calendar_cache ORDER BY entity_id LIMIT 129`)
		if err != nil {
			return err
		}
		var identities []domain.EntityID
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				rows.Close()
				return err
			}
			entity, err := domain.ParseEntityID(text)
			if err != nil {
				rows.Close()
				return failure(Corrupt)
			}
			identities = append(identities, entity)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(identities) > 128 {
			return failure(CapacityExceeded)
		}
		for _, entity := range identities {
			id, err := r.identity(ctx, tx, entity, nil, false)
			if err != nil {
				return err
			}
			snapshot, err := r.readCalendar(ctx, tx, id, false)
			if err != nil {
				return err
			}
			snapshots = append(snapshots, snapshot)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snapshots, nil
}

type calendarMutation uint8

const (
	calendarCreate calendarMutation = iota + 1
	calendarUpdate
	calendarDelete
)

func (r *Repository) mutateCalendar(ctx context.Context, id CalendarID, expected Revision, name string, kind calendarMutation) (CalendarSnapshot, error) {
	if err := writable(id); err != nil {
		return CalendarSnapshot{}, err
	}
	if kind != calendarCreate && kind != calendarUpdate && kind != calendarDelete {
		return CalendarSnapshot{}, failure(InvalidInput)
	}
	if !expected.validFor(id, EventUID{}) || kind != calendarDelete && !textValid(name, 512, true) {
		return CalendarSnapshot{}, failure(InvalidInput)
	}
	name = strings.Clone(name)
	var result CalendarSnapshot
	err := r.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := r.identity(ctx, tx, id.entity, &id, true); err != nil {
			return err
		}
		// Acquire the exclusive identity lock first, including absent-row inserts.
		entity, _ := id.entity.Value()
		current, err := readCalendar(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if current.revision.presence == Removed {
			return failure(Deleted)
		}
		if current.revision != expected {
			return failure(Conflict)
		}
		if kind == calendarCreate && current.revision.presence != Absent {
			return failure(Conflict)
		}
		if kind != calendarCreate && current.revision.presence != Present {
			return failure(NotFound)
		}
		number, err := nextRevision(current.revision.number)
		if err != nil {
			return err
		}
		life := int16(1)
		presence := Present
		if kind == calendarDelete {
			life = 2
			presence = Removed
			name = current.name
		}
		var updated pgtype.Timestamptz
		if kind == calendarCreate {
			err = tx.QueryRow(ctx, `INSERT INTO public.local_calendars(entity_id,origin,name,lifecycle,revision,updated_at) VALUES($1,1,$2,1,$3,clock_timestamp()) RETURNING updated_at`, entity, name, number).Scan(&updated)
		} else {
			err = tx.QueryRow(ctx, `UPDATE public.local_calendars SET name=$2,lifecycle=$3,revision=$4,updated_at=clock_timestamp() WHERE entity_id=$1 AND revision=$5 RETURNING updated_at`, entity, name, life, number, current.revision.number).Scan(&updated)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(Conflict)
		}
		if err != nil {
			return err
		}
		stamp, err := validUpdated(updated)
		if err != nil {
			return err
		}
		result = CalendarSnapshot{id: id, name: name, revision: Revision{calendar: id, presence: presence, number: number}, updatedAt: stamp}
		return nil
	})
	if err != nil {
		return CalendarSnapshot{}, err
	}
	return result, nil
}
func (r *Repository) CreateCalendar(ctx context.Context, id CalendarID, expected Revision, name string) (CalendarSnapshot, error) {
	return r.mutateCalendar(ctx, id, expected, name, calendarCreate)
}
func (r *Repository) UpdateCalendar(ctx context.Context, id CalendarID, expected Revision, name string) (CalendarSnapshot, error) {
	return r.mutateCalendar(ctx, id, expected, name, calendarUpdate)
}
func (r *Repository) DeleteCalendar(ctx context.Context, id CalendarID, expected Revision) (CalendarSnapshot, error) {
	return r.mutateCalendar(ctx, id, expected, "", calendarDelete)
}

func activeCalendar(current CalendarSnapshot) error {
	switch current.revision.presence {
	case Present:
		return nil
	case Absent:
		return failure(NotFound)
	case Removed:
		return failure(Deleted)
	default:
		return failure(Corrupt)
	}
}
func bumpCalendar(ctx context.Context, tx pgx.Tx, current CalendarSnapshot) (CalendarSnapshot, error) {
	number, err := nextRevision(current.revision.number)
	if err != nil {
		return CalendarSnapshot{}, err
	}
	entity, _ := current.id.entity.Value()
	var updated pgtype.Timestamptz
	err = tx.QueryRow(ctx, `UPDATE public.local_calendars SET revision=$2,updated_at=clock_timestamp() WHERE entity_id=$1 AND revision=$3 RETURNING updated_at`, entity, number, current.revision.number).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return CalendarSnapshot{}, failure(Conflict)
	}
	if err != nil {
		return CalendarSnapshot{}, err
	}
	stamp, err := validUpdated(updated)
	if err != nil {
		return CalendarSnapshot{}, err
	}
	current.revision.number = number
	current.updatedAt = stamp
	return current, nil
}
