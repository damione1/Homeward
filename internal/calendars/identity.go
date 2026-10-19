package calendars

import (
	"context"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
)

type Origin uint8

const (
	OriginInvalid Origin = iota
	Local
	RemoteICS
	Calculated
)

func (o Origin) valid() bool {
	switch o {
	case Local, RemoteICS, Calculated:
		return true
	default:
		return false
	}
}

// CalendarID is admitted only by Bind against the authoritative registry. The
// original alias and origin cannot be turned into a writable source by callers.
type CalendarID struct {
	entity domain.EntityID
	alias  domain.HAAlias
	origin Origin
}

func (id CalendarID) Valid() bool {
	_, e := id.entity.Value()
	_, a := id.alias.Value()
	return e == nil && a == nil && id.origin.valid()
}
func (id CalendarID) Entity() (domain.EntityID, bool) { return id.entity, id.Valid() }
func (id CalendarID) Alias() (domain.HAAlias, bool)   { return id.alias, id.Valid() }
func (id CalendarID) Origin() Origin {
	if !id.Valid() {
		return OriginInvalid
	}
	return id.origin
}

type EventUID struct{ value string }

func ParseUID(value string) (EventUID, error) {
	if len(value) == 0 || len(value) > 255 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return EventUID{}, failure(InvalidInput)
	}
	return EventUID{strings.Clone(value)}, nil
}
func (u EventUID) Value() (string, bool) {
	parsed, err := ParseUID(u.value)
	return parsed.value, err == nil
}

type EventKey struct {
	calendar CalendarID
	uid      EventUID
}

func EventIdentity(calendar CalendarID, uid EventUID) (EventKey, error) {
	_, ok := uid.Value()
	if !calendar.Valid() || !ok {
		return EventKey{}, failure(InvalidInput)
	}
	return EventKey{calendar, uid}, nil
}
func (k EventKey) Valid() bool          { _, ok := k.uid.Value(); return k.calendar.Valid() && ok }
func (k EventKey) Calendar() CalendarID { return k.calendar }
func (k EventKey) UID() EventUID        { return k.uid }

type Presence uint8

const (
	PresenceInvalid Presence = iota
	Absent
	Present
	Removed
)

func (p Presence) valid() bool {
	switch p {
	case Absent, Present, Removed:
		return true
	default:
		return false
	}
}

// Revision is bound to an exact resource. A calendar token cannot mutate an
// event and an event UID in another calendar never shares its CAS token.
type Revision struct {
	calendar CalendarID
	uid      EventUID
	presence Presence
	number   int64
}

func (r Revision) validFor(calendar CalendarID, uid EventUID) bool {
	if !calendar.Valid() || r.calendar != calendar || r.uid != uid || !r.presence.valid() {
		return false
	}
	if r.presence == Absent {
		return r.number == 0
	}
	return r.number > 0
}
func (r Revision) Number() (uint64, bool) { return uint64(r.number), r.validFor(r.calendar, r.uid) }
func (r Revision) Presence() Presence {
	if !r.validFor(r.calendar, r.uid) {
		return PresenceInvalid
	}
	return r.presence
}

func contextValid(ctx context.Context) error {
	if ctx == nil {
		return failure(InvalidInput)
	}
	v := reflect.ValueOf(ctx)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return failure(InvalidInput)
		}
	}
	return canceled(ctx)
}
func textValid(value string, maximum int, required bool) bool {
	return (!required || len(value) > 0) && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}
