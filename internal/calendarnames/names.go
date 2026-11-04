// Package calendarnames binds public calendar names to existing registry and
// imported event identities without changing any persistent native identity.
package calendarnames

import (
	"errors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/resourceid"
	"strings"
)

var ErrInvalid = errors.New("invalid calendar resource name")

type CalendarName struct {
	home   apitypes.HomeName
	entity domain.EntityID
}
type EventName struct {
	calendar CalendarName
	uid      calendars.EventUID
}

func Calendar(home apitypes.HomeName, entity domain.EntityID) (CalendarName, error) {
	raw, err := entity.Value()
	if err != nil || !home.Valid() {
		return CalendarName{}, ErrInvalid
	}
	if _, err := resourceid.Encode(raw); err != nil {
		return CalendarName{}, ErrInvalid
	}
	return CalendarName{home, entity}, nil
}
func Event(parent CalendarName, uid calendars.EventUID) (EventName, error) {
	if _, ok := uid.Value(); !ok || parent.String() == "" {
		return EventName{}, ErrInvalid
	}
	return EventName{parent, uid}, nil
}

func (n CalendarName) Home() apitypes.HomeName { return n.home }
func (n CalendarName) Entity() domain.EntityID { return n.entity }
func (n CalendarName) String() string {
	if !n.home.Valid() {
		return ""
	}
	raw, _ := n.entity.Value()
	id, _ := resourceid.Encode(raw)
	return n.home.String() + "/calendars/" + id
}
func (n EventName) Parent() CalendarName    { return n.calendar }
func (n EventName) UID() calendars.EventUID { return n.uid }
func (n EventName) String() string {
	if n.calendar.String() == "" {
		return ""
	}
	raw, _ := n.uid.Value()
	id, _ := resourceid.Encode(raw)
	return n.calendar.String() + "/events/" + id
}

func ParseCalendar(raw string) (CalendarName, error) {
	if len(raw) > 1200 {
		return CalendarName{}, ErrInvalid
	}
	p := strings.Split(raw, "/")
	if len(p) != 4 || p[2] != "calendars" {
		return CalendarName{}, ErrInvalid
	}
	home, err := apitypes.ParseHomeName(strings.Join(p[:2], "/"))
	if err != nil {
		return CalendarName{}, ErrInvalid
	}
	value, err := resourceid.Decode(p[3])
	if err != nil {
		return CalendarName{}, ErrInvalid
	}
	entity, err := domain.ParseEntityID(value)
	if err != nil {
		return CalendarName{}, ErrInvalid
	}
	return Calendar(home, entity)
}
func ParseEvent(raw string) (EventName, error) {
	if len(raw) > 1800 {
		return EventName{}, ErrInvalid
	}
	p := strings.Split(raw, "/")
	if len(p) != 6 || p[4] != "events" {
		return EventName{}, ErrInvalid
	}
	parent, err := ParseCalendar(strings.Join(p[:4], "/"))
	if err != nil {
		return EventName{}, err
	}
	value, err := resourceid.Decode(p[5])
	if err != nil {
		return EventName{}, ErrInvalid
	}
	uid, err := calendars.ParseUID(value)
	if err != nil {
		return EventName{}, ErrInvalid
	}
	return Event(parent, uid)
}
