// Package remotecalendar composes the readonly ICS source with the existing
// calendar owner, conditional CloudClient and supervised Tasks.
package remotecalendar

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"hash"
	"reflect"
	"strings"
	"unicode/utf8"
)

type Failure uint8

const (
	Invalid Failure = iota + 1
	Closed
	Transport
	Content
	CachePolicy
	CommitUnknown
	RevisionChanged
	PublicationFailed
	Expired
)

type Error struct{ kind Failure }

func (e *Error) Kind() Failure {
	if e == nil {
		return 0
	}
	return e.kind
}
func (e *Error) Error() string {
	switch e.Kind() {
	case Closed:
		return "remote calendar: closed"
	case Transport:
		return "remote calendar: transport unavailable"
	case Content:
		return "remote calendar: content refused"
	case CachePolicy:
		return "remote calendar: cache policy refused"
	case CommitUnknown:
		return "remote calendar: commit outcome unknown"
	case RevisionChanged:
		return "remote calendar: version changed"
	case PublicationFailed:
		return "remote calendar: publication refused"
	case Expired:
		return "remote calendar: expired"
	default:
		return "remote calendar: invalid input"
	}
}
func (e *Error) GoString() string { return e.Error() }
func failure(k Failure) error     { return &Error{kind: k} }
func nilPort(v any) bool {
	if v == nil {
		return true
	}
	x := reflect.ValueOf(v)
	switch x.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return x.IsNil()
	}
	return false
}
func validContext(ctx context.Context) bool { return !nilPort(ctx) && ctx.Err() == nil }

type projectionKind uint8

const (
	projectionAbsent projectionKind = iota + 1
	projectionExplicit
)

type FloatingProjection struct {
	kind projectionKind
	zone calendars.Zone
}

func NoFloatingProjection() FloatingProjection { return FloatingProjection{kind: projectionAbsent} }
func WithFloatingProjection(z calendars.Zone) (FloatingProjection, error) {
	if !z.Valid() {
		return FloatingProjection{}, failure(Invalid)
	}
	return FloatingProjection{kind: projectionExplicit, zone: z}, nil
}
func (p FloatingProjection) valid() bool {
	return p.kind == projectionAbsent && !p.zone.Valid() || p.kind == projectionExplicit && p.zone.Valid()
}

type Settings struct {
	instance    domain.IntegrationInstanceID
	source      integrations.SourceKey
	label       string
	conditional integrations.ConditionalSettings
	projection  FloatingProjection
	view        calendars.Zone
	admitted    bool
}

func hashPart(h hash.Hash, s string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(s)))
	h.Write(size[:])
	h.Write([]byte(s))
}
func NewSettings(instance domain.IntegrationInstanceID, source integrations.SourceKey, label string, resource integrations.ConditionalResource, auth integrations.ConditionalAuth, destination integrations.ConditionalDestination, projection FloatingProjection, view calendars.Zone) (Settings, error) {
	if _, e := instance.Value(); e != nil {
		return Settings{}, failure(Invalid)
	}
	if _, _, e := source.Components(); e != nil || len(label) == 0 || len(label) > 512 || !utf8.ValidString(label) || strings.ContainsAny(label, "\x00\r\n") || !projection.valid() || !view.Valid() {
		return Settings{}, failure(Invalid)
	}
	h := sha256.New()
	hashPart(h, "homeward.remote-calendar/1;codec1;parser1")
	if projection.kind == projectionAbsent {
		hashPart(h, "absent")
	} else {
		hashPart(h, projection.zone.Name())
		hashPart(h, projection.zone.DataID())
	}
	hashPart(h, view.Name())
	hashPart(h, view.DataID())
	var profile [32]byte
	copy(profile[:], h.Sum(nil))
	c, e := integrations.NewConditionalSettings(source, resource, auth, destination, profile)
	if e != nil {
		return Settings{}, failure(Invalid)
	}
	return Settings{instance: instance, source: source, label: strings.Clone(label), conditional: c, projection: projection, view: view, admitted: true}, nil
}
func (s Settings) Valid() bool {
	_, e := s.instance.Value()
	_, _, k := s.source.Components()
	return s.admitted && e == nil && k == nil && s.projection.valid() && s.view.Valid()
}
func (s Settings) Identity() (domain.IntegrationInstanceID, integrations.SourceKey, error) {
	if !s.Valid() {
		return domain.IntegrationInstanceID{}, integrations.SourceKey{}, failure(Invalid)
	}
	return s.instance, s.source, nil
}
func (s Settings) String() string               { return "remote calendar settings [redacted]" }
func (s Settings) GoString() string             { return s.String() }
func (s Settings) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
