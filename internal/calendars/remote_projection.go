package calendars

import (
	"context"
	"github.com/damione1/Homeward/internal/storage"
	"slices"
	"strings"
	"time"
)

const MaxAutomationOccurrences = 512
const MaxAutomationHorizon = 48 * time.Hour
const automationPageSize = 128
const automationOccurrenceCharge = 1536

type AutomationWindowRequest struct {
	from, to Instant
	zone     Zone
}

func NewAutomationWindowRequest(from, to Instant, zone Zone) (AutomationWindowRequest, error) {
	q := AutomationWindowRequest{from: from, to: to, zone: zone}
	if !q.valid() {
		return AutomationWindowRequest{}, failure(InvalidInput)
	}
	return q, nil
}
func (q AutomationWindowRequest) valid() bool {
	return q.from.Valid() && q.to.Valid() && q.zone.Valid() && q.to.value.After(q.from.value) && q.to.value.Sub(q.from.value) <= MaxAutomationHorizon
}
func (q AutomationWindowRequest) window(ctx context.Context) (Window, error) {
	if !q.valid() {
		return Window{}, failure(InvalidInput)
	}
	a, e := q.zone.localAt(ctx, q.from.value)
	if e != nil {
		return Window{}, e
	}
	b, e := q.zone.localAt(ctx, q.to.value)
	if e != nil {
		return Window{}, e
	}
	w := Window{kind: Timed, fromDate: a.date, toDate: b.date, from: q.from, to: q.to, zone: q.zone}
	if !w.Valid() {
		return Window{}, failure(InvalidDate)
	}
	return w, nil
}

type AutomationSpanKind uint8

const (
	AutomationPoint AutomationSpanKind = iota + 1
	AutomationInterval
	AutomationAllDay
)

type AutomationZoneKind uint8

const (
	AutomationPinnedIANA AutomationZoneKind = iota + 1
	AutomationEmbedded
)

type AutomationOccurrence struct {
	id           RecurrenceID
	start, end   time.Time
	kind         AutomationSpanKind
	zoneKind     AutomationZoneKind
	zone, dataID string
	freshness    RemoteFreshness
}

func (o AutomationOccurrence) Valid() bool {
	if !o.id.Valid() || o.start.IsZero() || o.end.IsZero() || o.zone == "" || o.dataID == "" || o.zoneKind != AutomationPinnedIANA && o.zoneKind != AutomationEmbedded || o.freshness != RemoteFresh && o.freshness != RemoteStale {
		return false
	}
	switch o.kind {
	case AutomationPoint:
		return o.start.Equal(o.end)
	case AutomationInterval, AutomationAllDay:
		return o.end.After(o.start)
	default:
		return false
	}
}
func (o AutomationOccurrence) ID() RecurrenceID { return o.id }
func (o AutomationOccurrence) Instants() (time.Time, time.Time, AutomationSpanKind, bool) {
	return o.start, o.end, o.kind, o.Valid()
}
func (o AutomationOccurrence) Zone() (AutomationZoneKind, string, string, bool) {
	return o.zoneKind, o.zone, o.dataID, o.Valid()
}
func (o AutomationOccurrence) Freshness() (RemoteFreshness, bool) { return o.freshness, o.Valid() }
func (o AutomationOccurrence) String() string                     { return "calendars: automation occurrence [redacted]" }
func (o AutomationOccurrence) GoString() string                   { return o.String() }
func (o AutomationOccurrence) MarshalJSON() ([]byte, error)       { return []byte(`"[redacted]"`), nil }

type AutomationWindow struct {
	calendar CalendarID
	revision Revision
	remote   RemoteState
	digest   [32]byte
	from, to time.Time
	items    []AutomationOccurrence
}

func (w AutomationWindow) Valid() bool {
	if !w.calendar.Valid() || w.calendar.origin != RemoteICS || !w.revision.validFor(w.calendar, EventUID{}) || w.revision.presence != Present || !w.remote.Valid() || w.from.IsZero() || !w.to.After(w.from) || w.to.Sub(w.from) > MaxAutomationHorizon || len(w.items) > MaxAutomationOccurrences {
		return false
	}
	if w.remote.kind == RemoteNeverValidated {
		return len(w.items) == 0 && w.digest == ([32]byte{})
	}
	if w.digest == ([32]byte{}) {
		return false
	}
	for _, o := range w.items {
		if !o.Valid() || o.id.event.calendar != w.calendar || o.freshness != w.remote.freshness {
			return false
		}
	}
	return true
}
func (w AutomationWindow) Calendar() CalendarID     { return w.calendar }
func (w AutomationWindow) Revision() Revision       { return w.revision }
func (w AutomationWindow) RemoteState() RemoteState { return w.remote }
func (w AutomationWindow) Digest() ([32]byte, bool) {
	return w.digest, w.Valid() && w.remote.kind == RemoteValidated
}
func (w AutomationWindow) Bounds() (time.Time, time.Time, bool) { return w.from, w.to, w.Valid() }
func (w AutomationWindow) Items() []AutomationOccurrence        { return slices.Clone(w.items) }
func (w AutomationWindow) MatchesCommit(c storage.RemoteCalendarCommit, o storage.RemoteCalendarObservation) bool {
	if !w.Valid() || !o.Valid() {
		return false
	}
	source, ok := w.remote.SourceKey()
	id, iok := o.ID()
	key, kok := o.Source().Key()
	revision, rok := c.Revision()
	digest, dok := c.Digest()
	return ok && iok && kok && rok && dok && source == key && c.Matches(w.calendar.entity, source, id) && revision == w.revision.number && digest == w.digest
}
func (w AutomationWindow) String() string               { return "calendars: automation window [redacted]" }
func (w AutomationWindow) GoString() string             { return w.String() }
func (w AutomationWindow) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func projectAutomationOccurrence(ctx context.Context, o Occurrence, view Zone, freshness RemoteFreshness) (AutomationOccurrence, error) {
	if !o.Valid() {
		return AutomationOccurrence{}, failure(InvalidOccurrence)
	}
	s := o.span
	zone := s.zone
	start, end := s.start.value, s.end.value
	kind := AutomationInterval
	if s.IsPoint() {
		kind = AutomationPoint
	}
	if s.kind == AllDay {
		kind = AutomationAllDay
		zone = view
		var present bool
		var e error
		start, present, e = resolveWallTime(ctx, LocalDateTime{date: s.startDate}, zone, true)
		if e != nil {
			return AutomationOccurrence{}, e
		}
		if !present {
			return AutomationOccurrence{}, failure(InvalidDate)
		}
		end, present, e = resolveWallTime(ctx, LocalDateTime{date: s.endDate}, zone, true)
		if e != nil {
			return AutomationOccurrence{}, e
		}
		if !present {
			return AutomationOccurrence{}, failure(InvalidDate)
		}
	}
	zkind := AutomationPinnedIANA
	if zone.embedded != nil {
		zkind = AutomationEmbedded
	}
	result := AutomationOccurrence{id: o.id, start: start.UTC(), end: end.UTC(), kind: kind, zoneKind: zkind, zone: strings.Clone(zone.Name()), dataID: strings.Clone(zone.DataID()), freshness: freshness}
	if !result.Valid() {
		return AutomationOccurrence{}, failure(InvalidOccurrence)
	}
	return result, nil
}

// Len and At permit a bounded consumer to reserve its aggregate before copying.
func (w AutomationWindow) Len() int { return len(w.items) }
func (w AutomationWindow) At(index int) (AutomationOccurrence, bool) {
	if !w.Valid() || index < 0 || index >= len(w.items) {
		return AutomationOccurrence{}, false
	}
	return w.items[index], true
}
