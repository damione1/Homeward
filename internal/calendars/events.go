package calendars

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func storedRule(rule Rule) (pgtype.Text, pgtype.Text) {
	if rule.state == ruleNone {
		return pgtype.Text{}, pgtype.Text{}
	}
	return pgtype.Text{String: RecurrenceProfile, Valid: true}, pgtype.Text{String: rule.canonical, Valid: true}
}
func decodeRule(profile, raw pgtype.Text, span Span) (Rule, error) {
	if !profile.Valid && !raw.Valid {
		return NoRecurrence(), nil
	}
	if !profile.Valid || !raw.Valid {
		return Rule{}, failure(Corrupt)
	}
	if profile.String != RecurrenceProfile {
		return Rule{}, failure(Incompatible)
	}
	rule, err := ParseRule(raw.String, span)
	if err != nil {
		return Rule{}, decodeFailure(err)
	}
	if rule.canonical != raw.String {
		return Rule{}, failure(Corrupt)
	}
	return rule, nil
}

func readEventHead(ctx context.Context, tx pgx.Tx, key EventKey, calendar Revision, write bool) (EventSnapshot, error) {
	entity, _ := key.calendar.entity.Value()
	var life int16
	var revision int64
	var updated pgtype.Timestamptz
	query := `SELECT lifecycle,revision,updated_at FROM public.calendar_events WHERE calendar_id=$1 AND uid=$2`
	if write {
		query += ` FOR UPDATE`
	} else {
		query += ` FOR SHARE`
	}
	err := tx.QueryRow(ctx, query, entity, key.uid.value).Scan(&life, &revision, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return EventSnapshot{key: key, revision: Revision{calendar: key.calendar, uid: key.uid, presence: Absent}, calendarRevision: calendar}, nil
	}
	if err != nil {
		return EventSnapshot{}, err
	}
	presence, err := presenceFromSQL(life)
	if err != nil || revision <= 0 {
		return EventSnapshot{}, failure(Corrupt)
	}
	stamp, err := validUpdated(updated)
	if err != nil {
		return EventSnapshot{}, err
	}
	return EventSnapshot{key: key, revision: Revision{calendar: key.calendar, uid: key.uid, presence: presence, number: revision}, calendarRevision: calendar, updatedAt: stamp}, nil
}

type eventMutation uint8

const (
	eventCreate eventMutation = iota + 1
	eventUpdate
	eventDelete
)

func (r *Repository) mutateEvent(ctx context.Context, key EventKey, expected Revision, event Event, kind eventMutation) (EventSnapshot, error) {
	if !key.Valid() {
		return EventSnapshot{}, failure(InvalidInput)
	}
	if err := writable(key.calendar); err != nil {
		return EventSnapshot{}, err
	}
	if kind != eventCreate && kind != eventUpdate && kind != eventDelete || !expected.validFor(key.calendar, key.uid) || kind != eventDelete && !event.Valid() {
		return EventSnapshot{}, failure(InvalidInput)
	}
	if kind != eventDelete && event.contentKind != contentLocal {
		return EventSnapshot{}, failure(Incompatible)
	}
	// This reservation includes the admitted input, returned copy and SQL codecs.
	charge := eventBaseCharge
	if kind != eventDelete {
		charge = event.ramCharge()
	}
	lease, err := images.reserve(charge)
	if err != nil {
		return EventSnapshot{}, err
	}
	defer lease.release()
	var result EventSnapshot
	err = r.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := r.identity(ctx, tx, key.calendar.entity, &key.calendar, true); err != nil {
			return err
		}
		calendar, err := readCalendar(ctx, tx, key.calendar, true)
		if err != nil {
			return err
		}
		if err := activeCalendar(calendar); err != nil {
			return err
		}
		current, err := readEventHead(ctx, tx, key, calendar.revision, true)
		if err != nil {
			return err
		}
		if current.revision.presence == Removed {
			return failure(Deleted)
		}
		if current.revision != expected {
			return failure(Conflict)
		}
		if kind == eventCreate && current.revision.presence != Absent {
			return failure(Conflict)
		}
		if kind != eventCreate && current.revision.presence != Present {
			return failure(NotFound)
		}
		number, err := nextRevision(current.revision.number)
		if err != nil {
			return err
		}
		entity, _ := key.calendar.entity.Value()
		var updated pgtype.Timestamptz
		if kind != eventCreate {
			if _, err := tx.Exec(ctx, `DELETE FROM public.calendar_dates WHERE calendar_id=$1 AND uid=$2`, entity, key.uid.value); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM public.calendar_exceptions WHERE calendar_id=$1 AND uid=$2`, entity, key.uid.value); err != nil {
				return err
			}
		}
		if kind == eventDelete {
			err = tx.QueryRow(ctx, `UPDATE public.calendar_events SET lifecycle=2,revision=$3,updated_at=clock_timestamp(),summary=NULL,description=NULL,location=NULL,temporal_kind=NULL,start_date=NULL,end_date=NULL,anchor_local=NULL,start_at=NULL,end_at=NULL,timezone=NULL,timezone_data=NULL,recurrence_profile=NULL,recurrence_rule=NULL WHERE calendar_id=$1 AND uid=$2 AND revision=$4 RETURNING updated_at`, entity, key.uid.value, number, current.revision.number).Scan(&updated)
		} else {
			profile, rule := storedRule(event.rule)
			args := []any{entity, key.uid.value, number, event.summary, event.description, event.location}
			args = append(args, encodeSpan(event.span).arguments()...)
			args = append(args, profile, rule)
			query := `INSERT INTO public.calendar_events(calendar_id,uid,lifecycle,revision,updated_at,summary,description,location,temporal_kind,start_date,end_date,anchor_local,start_at,end_at,timezone,timezone_data,recurrence_profile,recurrence_rule) VALUES($1,$2,1,$3,clock_timestamp(),$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING updated_at`
			if kind == eventUpdate {
				query = `UPDATE public.calendar_events SET revision=$3,updated_at=clock_timestamp(),summary=$4,description=$5,location=$6,temporal_kind=$7,start_date=$8,end_date=$9,anchor_local=$10,start_at=$11,end_at=$12,timezone=$13,timezone_data=$14,recurrence_profile=$15,recurrence_rule=$16 WHERE calendar_id=$1 AND uid=$2 AND lifecycle=1 AND revision=$17 RETURNING updated_at`
				args = append(args, current.revision.number)
			}
			err = tx.QueryRow(ctx, query, args...).Scan(&updated)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(Conflict)
		}
		if err != nil {
			return err
		}
		if kind != eventDelete {
			if err := writeChildren(ctx, tx, key, event); err != nil {
				return err
			}
		}
		// Capacity is an invariant of committed images, including child fanout.
		if kind != eventDelete {
			if _, err := imageCharge(ctx, tx, key.calendar); err != nil {
				return err
			}
		}
		calendar, err = bumpCalendar(ctx, tx, calendar)
		if err != nil {
			return err
		}
		stamp, err := validUpdated(updated)
		if err != nil {
			return err
		}
		presence := Present
		definition := cloneEvent(event)
		if kind == eventDelete {
			presence = Removed
			definition = Event{}
		}
		result = EventSnapshot{key: key, definition: definition, revision: Revision{calendar: key.calendar, uid: key.uid, presence: presence, number: number}, calendarRevision: calendar.revision, updatedAt: stamp}
		return nil
	})
	if err != nil {
		return EventSnapshot{}, err
	}
	return result, nil
}
func writeChildren(ctx context.Context, tx pgx.Tx, key EventKey, event Event) error {
	entity, _ := key.calendar.entity.Value()
	for i, points := range [][]Point{event.additions, event.exclusions} {
		for _, point := range points {
			args := []any{entity, key.uid.value, int16(i + 1)}
			args = append(args, encodePoint(point).arguments()...)
			if _, err := tx.Exec(ctx, `INSERT INTO public.calendar_dates(calendar_id,uid,date_kind,temporal_kind,point_key,date_value,local_value) VALUES($1,$2,$3,$4,$5,$6,$7)`, args...); err != nil {
				return err
			}
		}
	}
	for _, exception := range event.exceptions {
		action := int16(1)
		replacement := storedSpan{}
		if exception.kind == MoveOccurrence {
			action = 2
			replacement = encodeSpan(exception.replacement)
		}
		args := []any{entity, key.uid.value}
		args = append(args, encodePoint(exception.original).arguments()...)
		args = append(args, action)
		args = append(args, replacement.arguments()[1:]...)
		if _, err := tx.Exec(ctx, `INSERT INTO public.calendar_exceptions(calendar_id,uid,temporal_kind,point_key,date_value,local_value,action,start_date,end_date,anchor_local,start_at,end_at,timezone,timezone_data) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, args...); err != nil {
			return err
		}
	}
	return nil
}
func (r *Repository) CreateEvent(ctx context.Context, key EventKey, expected Revision, event Event) (EventSnapshot, error) {
	return r.mutateEvent(ctx, key, expected, event, eventCreate)
}
func (r *Repository) UpdateEvent(ctx context.Context, key EventKey, expected Revision, event Event) (EventSnapshot, error) {
	return r.mutateEvent(ctx, key, expected, event, eventUpdate)
}
func (r *Repository) DeleteEvent(ctx context.Context, key EventKey, expected Revision) (EventSnapshot, error) {
	return r.mutateEvent(ctx, key, expected, Event{}, eventDelete)
}
