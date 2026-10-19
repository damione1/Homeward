package calendars

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Charges include pgx fields, private values, child slices, sorting/maps and
// returned copies. 2 MiB covers the bounded page heap and per-series work maps.
// The immutable embedded timezone index and shared cache have a separate fixed
// <1 MiB + 4 MiB ceiling; no zone location is copied per event.
const imageFixedCharge int64 = 2 * 1024 * 1024

type imageLedger struct {
	mu     sync.Mutex
	used   int64
	active int
}
type imageLease struct {
	owner  *imageLedger
	charge int64
	once   sync.Once
}

var images imageLedger

func (l *imageLedger) reserve(charge int64) (*imageLease, error) {
	if charge <= 0 || charge > MaxImageBytes {
		return nil, failure(CapacityExceeded)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= 2 || charge > MaxAggregateImageBytes-l.used {
		return nil, failure(Busy)
	}
	l.active++
	l.used += charge
	return &imageLease{owner: l, charge: charge}, nil
}
func (l *imageLease) release() {
	if l != nil {
		l.once.Do(func() { l.owner.mu.Lock(); defer l.owner.mu.Unlock(); l.owner.active--; l.owner.used -= l.charge })
	}
}

// imageCharge reads only SQL scalars BEFORE any parent/child payload is scanned
// into Go. Calendar's SHARE lock excludes every repository mutation throughout
// this preflight and loading; mutations take UPDATE on the same identity first.
func imageCharge(ctx context.Context, tx pgx.Tx, id CalendarID) (int64, error) {
	entity, _ := id.entity.Value()
	var count, adds, excludes, exceptions, charge int64
	err := tx.QueryRow(ctx, `WITH d AS (
 SELECT uid,count(*) FILTER(WHERE date_kind=1) a,count(*) FILTER(WHERE date_kind=2) b,
 count(*) n,sum(octet_length(point_key)) bytes FROM public.calendar_dates WHERE calendar_id=$1 GROUP BY uid
 ), x AS (
 SELECT uid,count(*) n,sum(octet_length(point_key)+coalesce(octet_length(timezone),0)+coalesce(octet_length(timezone_data),0)) bytes
 FROM public.calendar_exceptions WHERE calendar_id=$1 GROUP BY uid
 ) SELECT count(*),coalesce(max(d.a),0),coalesce(max(d.b),0),coalesce(max(x.n),0),
 coalesce(sum($2::bigint + 3::bigint*(octet_length(e.uid)+octet_length(e.summary)+octet_length(e.description)+octet_length(e.location)+coalesce(octet_length(e.recurrence_rule),0)+coalesce(octet_length(e.timezone),0)+coalesce(octet_length(e.timezone_data),0))
 +coalesce(d.n,0)*$3::bigint+coalesce(x.n,0)*$4::bigint+3::bigint*(coalesce(d.bytes,0)+coalesce(x.bytes,0))),0)::bigint
 FROM public.calendar_events e LEFT JOIN d ON d.uid=e.uid LEFT JOIN x ON x.uid=e.uid WHERE e.calendar_id=$1 AND e.lifecycle=1`, entity, eventBaseCharge, pointCharge, exceptionCharge).Scan(&count, &adds, &excludes, &exceptions, &charge)
	if err != nil {
		return 0, err
	}
	if count < 0 || count > MaxEvents || adds > MaxDates || excludes > MaxDates || exceptions > MaxExceptions || charge < 0 || charge > MaxImageBytes-imageFixedCharge {
		return 0, failure(CapacityExceeded)
	}
	return charge + imageFixedCharge, nil
}

type calendarImage struct {
	calendar CalendarSnapshot
	events   []EventSnapshot
	lookup   EventSnapshot
	lease    *imageLease
}

func (i *calendarImage) close() {
	if i != nil {
		i.lease.release()
	}
}
func (r *Repository) loadImage(ctx context.Context, id CalendarID, target *EventKey) (*calendarImage, error) {
	if !id.Valid() {
		return nil, failure(InvalidInput)
	}
	image := &calendarImage{}
	err := r.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := r.identity(ctx, tx, id.entity, &id, false); err != nil {
			return err
		}
		calendar, err := readCalendar(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err := activeCalendar(calendar); err != nil {
			return err
		}
		image.calendar = calendar
		if target != nil {
			image.lookup, err = readEventHead(ctx, tx, *target, calendar.revision, false)
			if err != nil {
				return err
			}
			if image.lookup.revision.presence != Present {
				return nil
			}
		}
		charge, err := imageCharge(ctx, tx, id)
		if err != nil {
			return err
		}
		image.lease, err = images.reserve(charge)
		if err != nil {
			return err
		}
		if r.beforeImageLoad != nil {
			r.beforeImageLoad()
		}
		image.calendar = calendar
		image.events, err = readImage(ctx, tx, calendar)
		if err != nil {
			return err
		}

		return err
	})
	if err != nil {
		image.close()
		return nil, err
	}
	return image, nil
}

func readImage(ctx context.Context, tx pgx.Tx, calendar CalendarSnapshot) ([]EventSnapshot, error) {
	entity, _ := calendar.id.entity.Value()
	rows, err := tx.Query(ctx, `SELECT uid,lifecycle,revision,updated_at,summary,description,location,temporal_kind,start_date,end_date,anchor_local,start_at,end_at,timezone,timezone_data,recurrence_profile,recurrence_rule FROM public.calendar_events WHERE calendar_id=$1 AND lifecycle=1 ORDER BY uid LIMIT 1025`, entity)
	if err != nil {
		return nil, err
	}
	var result []EventSnapshot
	inputs := make(map[EventUID]*EventInput)
	indexes := make(map[EventUID]int)
	for rows.Next() {
		if len(result) >= MaxEvents {
			rows.Close()
			return nil, failure(CapacityExceeded)
		}
		var uidText string
		var life int16
		var revision int64
		var updated pgtype.Timestamptz
		var input EventInput
		var raw storedSpan
		var profile, ruleText pgtype.Text
		targets := []any{&uidText, &life, &revision, &updated, &input.Summary, &input.Description, &input.Location}
		targets = append(targets, raw.destinations()...)
		targets = append(targets, &profile, &ruleText)
		if err := rows.Scan(targets...); err != nil {
			rows.Close()
			return nil, err
		}
		uid, err := ParseUID(uidText)
		if err != nil || revision <= 0 {
			rows.Close()
			return nil, failure(Corrupt)
		}
		presence, err := presenceFromSQL(life)
		if err != nil {
			rows.Close()
			return nil, err
		}
		stamp, err := validUpdated(updated)
		if err != nil {
			rows.Close()
			return nil, err
		}
		key := EventKey{calendar: calendar.id, uid: uid}
		if _, duplicate := indexes[uid]; duplicate {
			rows.Close()
			return nil, failure(Corrupt)
		}
		indexes[uid] = len(result)
		result = append(result, EventSnapshot{key: key, revision: Revision{calendar: calendar.id, uid: uid, presence: presence, number: revision}, calendarRevision: calendar.revision, updatedAt: stamp})
		if presence == Removed {
			continue
		}
		input.Span, err = decodeSpan(ctx, raw)
		if err != nil {
			rows.Close()
			return nil, err
		}
		input.Rule, err = decodeRule(profile, ruleText, input.Span)
		if err != nil {
			rows.Close()
			return nil, err
		}
		inputs[uid] = &input
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT uid,date_kind,temporal_kind,point_key,date_value,local_value FROM public.calendar_dates WHERE calendar_id=$1 ORDER BY uid,date_kind,point_key`, entity)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var uidText string
		var kind int16
		var raw storedPoint
		targets := []any{&uidText, &kind}
		targets = append(targets, raw.destinations()...)
		if err := rows.Scan(targets...); err != nil {
			rows.Close()
			return nil, err
		}
		uid, err := ParseUID(uidText)
		input := inputs[uid]
		if err != nil || input == nil {
			rows.Close()
			return nil, failure(Corrupt)
		}
		point, err := decodePoint(raw)
		if err != nil {
			rows.Close()
			return nil, err
		}
		switch kind {
		case 1:
			if len(input.Additions) >= MaxDates {
				rows.Close()
				return nil, failure(CapacityExceeded)
			}
			input.Additions = append(input.Additions, point)
		case 2:
			if len(input.Exclusions) >= MaxDates {
				rows.Close()
				return nil, failure(CapacityExceeded)
			}
			input.Exclusions = append(input.Exclusions, point)
		default:
			rows.Close()
			return nil, failure(Corrupt)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT uid,temporal_kind,point_key,date_value,local_value,action,start_date,end_date,anchor_local,start_at,end_at,timezone,timezone_data FROM public.calendar_exceptions WHERE calendar_id=$1 ORDER BY uid,point_key`, entity)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var uidText string
		var pointRaw storedPoint
		var action int16
		var spanRaw storedSpan
		targets := []any{&uidText}
		targets = append(targets, pointRaw.destinations()...)
		targets = append(targets, &action)
		targets = append(targets, spanRaw.destinations()[1:]...)
		if err := rows.Scan(targets...); err != nil {
			rows.Close()
			return nil, err
		}
		uid, err := ParseUID(uidText)
		input := inputs[uid]
		if err != nil || input == nil {
			rows.Close()
			return nil, failure(Corrupt)
		}
		point, err := decodePoint(pointRaw)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if len(input.Exceptions) >= MaxExceptions {
			rows.Close()
			return nil, failure(CapacityExceeded)
		}
		var exception Exception
		switch action {
		case 1:
			if spanRaw != (storedSpan{}) {
				rows.Close()
				return nil, failure(Corrupt)
			}
			exception, err = Cancel(point)
		case 2:
			spanRaw.kind = pointRaw.kind
			var span Span
			span, err = decodeSpan(ctx, spanRaw)
			if err == nil {
				exception, err = Move(point, span)
			}
		default:
			err = failure(Corrupt)
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		input.Exceptions = append(input.Exceptions, exception)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for uid, input := range inputs {
		event, err := admitEventStructure(*input)
		if err != nil {
			return nil, decodeFailure(err)
		}
		// Canonical SQL children must not silently collapse duplicates or change kind.
		if len(event.additions) != len(input.Additions) || len(event.exclusions) != len(input.Exclusions) {
			return nil, failure(Corrupt)
		}
		result[indexes[uid]].definition = event
	}
	return result, nil
}
func validateImage(ctx context.Context, image *calendarImage, budget *calculationBudget) error {
	for _, snapshot := range image.events {
		if err := budget.charge(1, false); err != nil {
			return err
		}
		if snapshot.revision.presence == Present {
			if err := validateEvent(ctx, snapshot.definition, budget); err != nil {
				return decodeFailure(err)
			}
		}
	}
	return nil
}
func (r *Repository) GetEvent(ctx context.Context, key EventKey) (EventSnapshot, error) {
	if !key.Valid() {
		return EventSnapshot{}, failure(InvalidInput)
	}
	image, err := r.loadImage(ctx, key.calendar, &key)
	if err != nil {
		return EventSnapshot{}, err
	}
	defer image.close()
	for _, snapshot := range image.events {
		if snapshot.key == key {
			if snapshot.revision.presence == Present {
				if err := validateEvent(ctx, snapshot.definition, newBudget(ctx)); err != nil {
					return EventSnapshot{}, decodeFailure(err)
				}
			}
			snapshot.definition = cloneEvent(snapshot.definition)
			return snapshot, nil
		}
	}
	if image.lookup.revision.presence == Present {
		return EventSnapshot{}, failure(Corrupt)
	}
	return image.lookup, nil
}

// ListEvents returns admitted definitions from one bounded committed image.
// Removed UID revisions remain available through GetEvent, without loading the
// historical tombstones into this active list.
func (r *Repository) ListEvents(ctx context.Context, id CalendarID) ([]EventSnapshot, error) {
	image, err := r.loadImage(ctx, id, nil)
	if err != nil {
		return nil, err
	}
	defer image.close()
	if err := validateImage(ctx, image, newBudget(ctx)); err != nil {
		return nil, err
	}
	result := make([]EventSnapshot, len(image.events))
	for i, snapshot := range image.events {
		snapshot.definition = cloneEvent(snapshot.definition)
		result[i] = snapshot
	}
	return result, nil
}
