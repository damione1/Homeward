package ir

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
)

// A catalogue is an explicit snapshot supplied by the caller. It does not
// prove a live device, provider support, authorization, or dispatch rights.
var ErrInvalidCatalogue = errors.New("invalid semantic catalogue")

type ReferenceStatus uint8

const (
	ReferenceStatusInvalid ReferenceStatus = iota
	ReferencePresent
	ReferenceExcluded
)

// DeclaredSupport is a caller assertion for compilation only. NHA-046/115
// must resolve each reference against the authoritative capability registry
// before turning any admitted action into a command. No conversion is implied.
type DeclaredSupport uint8

const (
	SupportInvalid DeclaredSupport = iota
	SupportObserveState
	SupportClimateSetpoint
	SupportSwitchTurnOn
	SupportLightTurnOn
	SupportLightTurnOff
	SupportLightBrightnessAbsolute
	SupportLightBrightnessRelative
	SupportLightTransition
	SupportSceneTurnOn
	SupportSceneTransition
)

type Entity struct {
	alias    domain.HAAlias
	status   ReferenceStatus
	supports []DeclaredSupport
}

func NewEntity(id string, status ReferenceStatus, supports ...DeclaredSupport) (Entity, error) {
	alias, err := domain.ParseHAAlias(id)
	if err != nil || (status != ReferencePresent && status != ReferenceExcluded) {
		return Entity{}, ErrInvalidCatalogue
	}
	seen := make(map[DeclaredSupport]bool, len(supports))
	for _, support := range supports {
		if support <= SupportInvalid || support > SupportSceneTransition || seen[support] {
			return Entity{}, ErrInvalidCatalogue
		}
		seen[support] = true
	}
	if status == ReferenceExcluded && len(supports) != 0 {
		return Entity{}, ErrInvalidCatalogue
	}
	return Entity{alias: alias, status: status, supports: append([]DeclaredSupport(nil), supports...)}, nil
}
func (e Entity) Alias() domain.HAAlias       { return e.alias }
func (e Entity) Status() ReferenceStatus     { return e.status }
func (e Entity) Supports() []DeclaredSupport { return append([]DeclaredSupport(nil), e.supports...) }
func (e Entity) has(support DeclaredSupport) bool {
	for _, candidate := range e.supports {
		if candidate == support {
			return true
		}
	}
	return false
}

// HADeviceRef is not domain.DeviceID: the latter is a Homeward-owned identity.
type HADeviceRef struct{ value string }

func ParseHADeviceRef(value string) (HADeviceRef, error) {
	if !validName(value) {
		return HADeviceRef{}, ErrInvalidCatalogue
	}
	return HADeviceRef{value: value}, nil
}
func (r HADeviceRef) Value() (string, error) {
	if !validName(r.value) {
		return "", ErrInvalidCatalogue
	}
	return r.value, nil
}

type EventType struct{ value string }

func ParseEventType(value string) (EventType, error) {
	if !validName(value) || strings.ContainsAny(value, " \t") {
		return EventType{}, ErrInvalidCatalogue
	}
	return EventType{value: value}, nil
}
func (t EventType) Value() (string, error) {
	if _, err := ParseEventType(t.value); err != nil {
		return "", err
	}
	return t.value, nil
}

type Device struct {
	ref    HADeviceRef
	status ReferenceStatus
}

func NewDevice(id string, status ReferenceStatus) (Device, error) {
	ref, err := ParseHADeviceRef(id)
	if err != nil || (status != ReferencePresent && status != ReferenceExcluded) {
		return Device{}, ErrInvalidCatalogue
	}
	return Device{ref: ref, status: status}, nil
}
func (d Device) Ref() HADeviceRef        { return d.ref }
func (d Device) Status() ReferenceStatus { return d.status }

type Event struct {
	eventType EventType
	status    ReferenceStatus
}

func NewEvent(name string, status ReferenceStatus) (Event, error) {
	eventType, err := ParseEventType(name)
	if err != nil || (status != ReferencePresent && status != ReferenceExcluded) {
		return Event{}, ErrInvalidCatalogue
	}
	return Event{eventType: eventType, status: status}, nil
}
func (e Event) Type() EventType         { return e.eventType }
func (e Event) Status() ReferenceStatus { return e.status }

type Catalogue struct {
	valid    bool
	entities []Entity
	devices  []Device
	events   []Event
	scripts  []ScriptID
}

func NewCatalogue(entities []Entity, devices []Device, events []Event) (Catalogue, error) {
	c := Catalogue{valid: true, entities: make([]Entity, 0, len(entities)), devices: make([]Device, 0, len(devices)), events: make([]Event, 0, len(events))}
	ids := make(map[string]bool, len(entities)+len(devices)+len(events))
	for _, entity := range entities {
		id, err := entity.alias.Value()
		if err != nil || ids["entity:"+id] || (entity.status != ReferencePresent && entity.status != ReferenceExcluded) {
			return Catalogue{}, ErrInvalidCatalogue
		}
		if entity.status == ReferenceExcluded && len(entity.supports) != 0 {
			return Catalogue{}, ErrInvalidCatalogue
		}
		seenSupports := make(map[DeclaredSupport]bool, len(entity.supports))
		for _, support := range entity.supports {
			if support <= SupportInvalid || support > SupportSceneTransition || seenSupports[support] {
				return Catalogue{}, ErrInvalidCatalogue
			}
			seenSupports[support] = true
		}
		ids["entity:"+id] = true
		entity.supports = entity.Supports()
		c.entities = append(c.entities, entity)
	}
	for _, device := range devices {
		id, err := device.ref.Value()
		if err != nil || ids["device:"+id] || (device.status != ReferencePresent && device.status != ReferenceExcluded) {
			return Catalogue{}, ErrInvalidCatalogue
		}
		ids["device:"+id] = true
		c.devices = append(c.devices, device)
	}
	for _, event := range events {
		name, err := event.eventType.Value()
		if err != nil || ids["event:"+name] || (event.status != ReferencePresent && event.status != ReferenceExcluded) {
			return Catalogue{}, ErrInvalidCatalogue
		}
		ids["event:"+name] = true
		c.events = append(c.events, event)
	}
	return c, nil
}
func (c Catalogue) Valid() bool { return c.valid }
func (c Catalogue) Entities() []Entity {
	out := make([]Entity, len(c.entities))
	for i, v := range c.entities {
		v.supports = v.Supports()
		out[i] = v
	}
	return out
}
func (c Catalogue) Devices() []Device { return append([]Device(nil), c.devices...) }
func (c Catalogue) Events() []Event   { return append([]Event(nil), c.events...) }

// WithScripts adds the explicit script namespace supplied by the composing
// generation. It confers no execution or command authority.
func (c Catalogue) WithScripts(ids []ScriptID) (Catalogue, error) {
	if !c.Valid() || len(ids) > 64 {
		return Catalogue{}, ErrInvalidCatalogue
	}
	seen := make(map[ScriptID]struct{}, len(ids))
	for _, id := range ids {
		if _, err := id.Value(); err != nil {
			return Catalogue{}, ErrInvalidCatalogue
		}
		if _, duplicate := seen[id]; duplicate {
			return Catalogue{}, ErrInvalidCatalogue
		}
		seen[id] = struct{}{}
	}
	c.scripts = append([]ScriptID(nil), ids...)
	return c, nil
}
func (c Catalogue) Scripts() []ScriptID { return append([]ScriptID(nil), c.scripts...) }
func (c Catalogue) hasScript(id ScriptID) bool {
	for _, candidate := range c.scripts {
		if candidate == id {
			return true
		}
	}
	return false
}
func (c Catalogue) entity(alias domain.HAAlias) (Entity, bool) {
	for _, v := range c.entities {
		if v.alias == alias {
			return v, true
		}
	}
	return Entity{}, false
}
func (c Catalogue) device(ref HADeviceRef) (Device, bool) {
	for _, v := range c.devices {
		if v.ref == ref {
			return v, true
		}
	}
	return Device{}, false
}
func (c Catalogue) event(eventType EventType) (Event, bool) {
	for _, v := range c.events {
		if v.eventType == eventType {
			return v, true
		}
	}
	return Event{}, false
}

func validName(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}
