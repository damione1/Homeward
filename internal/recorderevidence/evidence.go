package recorderevidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

// Evidence is an immutable native admission fact, not a permission.
// A reader must authorize every recorded slot with the current authority.
// Its zero value denotes missing provenance and never authorizes a read.
type Evidence struct {
	device   domain.DeviceID
	entity   domain.EntityID
	instance domain.IntegrationInstanceID
	source   Source
	slots    []authorization.CapabilitySlot
	encoded  string
}

const MaxBytes = 40 << 10

var ErrInvalid = errors.New("invalid recorder admission evidence")

var ErrBudget = errors.New("recorder admission evidence exceeds its byte budget")

func New(device domain.DeviceID, entity domain.EntityID, instance domain.IntegrationInstanceID, source Source, slots []authorization.CapabilitySlot) (Evidence, error) {
	d, err := device.Value()
	if err != nil {
		return Evidence{}, ErrInvalid
	}
	e, err := entity.Value()
	if err != nil {
		return Evidence{}, ErrInvalid
	}
	i, err := instance.Value()
	if err != nil {
		return Evidence{}, ErrInvalid
	}
	sd, se, err := source.Components()
	if err != nil || len(slots) == 0 || len(slots) > authorization.MaxSlots {
		return Evidence{}, ErrInvalid
	}
	slots = append([]authorization.CapabilitySlot(nil), slots...)
	sort.Slice(slots, func(i, j int) bool {
		a, b, _ := slots[i].Values()
		c, d, _ := slots[j].Values()
		return a < c || a == c && b < d
	})
	charge := 2
	for _, value := range []string{d, e, i, sd, se} {
		if len(value) > 65535 {
			return Evidence{}, ErrBudget
		}
		charge += 2 + len(value)
	}
	for n, slot := range slots {
		a, b, err := slot.Values()
		if err != nil || n > 0 && slots[n-1] == slot {
			return Evidence{}, ErrInvalid
		}
		charge += 4 + len(a) + len(b)
	}
	if charge > MaxBytes {
		return Evidence{}, ErrBudget
	}
	var out bytes.Buffer
	out.Grow(charge)
	out.WriteByte(1)
	put := func(v string) { _ = binary.Write(&out, binary.BigEndian, uint16(len(v))); out.WriteString(v) }
	for _, v := range []string{d, e, i, sd, se} {
		if len(v) > 65535 {
			return Evidence{}, ErrBudget
		}
		put(v)
	}
	out.WriteByte(byte(len(slots)))
	for n, slot := range slots {
		a, b, err := slot.Values()
		if err != nil || n > 0 && slots[n-1] == slot {
			return Evidence{}, ErrInvalid
		}
		put(a)
		put(b)
	}
	if out.Len() > MaxBytes {
		return Evidence{}, ErrBudget
	}
	return Evidence{device, entity, instance, source, slots, out.String()}, nil
}

func (e Evidence) Present() bool { return e.encoded != "" }
func (e Evidence) Bytes() []byte { return []byte(e.encoded) }
func (e Evidence) RetainedBytes() int {
	if !e.Present() {
		return 0
	}
	return 2*len(e.encoded) + len(e.slots)*64 + 2048
}
func (e Evidence) Digest() [32]byte                       { return sha256.Sum256([]byte(e.encoded)) }
func (e Evidence) Entity() domain.EntityID                { return e.entity }
func (e Evidence) Instance() domain.IntegrationInstanceID { return e.instance }
func (e Evidence) Slots() []authorization.CapabilitySlot {
	return append([]authorization.CapabilitySlot(nil), e.slots...)
}

// Parse admits only the exact canonical representation written
// by the native admission path. It never fills missing evidence from live state.
func Parse(encoded []byte) (Evidence, error) {
	if len(encoded) < 2 || len(encoded) > MaxBytes || encoded[0] != 1 {
		return Evidence{}, ErrInvalid
	}
	rest := encoded[1:]
	take := func() (string, bool) {
		if len(rest) < 2 {
			return "", false
		}
		n := int(binary.BigEndian.Uint16(rest))
		rest = rest[2:]
		if n > len(rest) {
			return "", false
		}
		v := string(rest[:n])
		rest = rest[n:]
		return v, true
	}
	var fields [5]string
	for i := range fields {
		v, ok := take()
		if !ok {
			return Evidence{}, ErrInvalid
		}
		fields[i] = v
	}
	d, err := domain.ParseDeviceID(fields[0])
	if err != nil {
		return Evidence{}, ErrInvalid
	}
	e, err := domain.ParseEntityID(fields[1])
	if err != nil {
		return Evidence{}, ErrInvalid
	}
	i, err := domain.ParseIntegrationInstanceID(fields[2])
	if err != nil {
		return Evidence{}, ErrInvalid
	}
	source, err := NewSource(fields[3], fields[4])
	if err != nil || len(rest) == 0 {
		return Evidence{}, ErrInvalid
	}
	count := int(rest[0])
	rest = rest[1:]
	if count == 0 || count > authorization.MaxSlots {
		return Evidence{}, ErrInvalid
	}
	slots := make([]authorization.CapabilitySlot, count)
	for n := range slots {
		a, ok := take()
		if !ok {
			return Evidence{}, ErrInvalid
		}
		b, ok := take()
		if !ok {
			return Evidence{}, ErrInvalid
		}
		slots[n], err = authorization.NewCapabilitySlot(a, b)
		if err != nil {
			return Evidence{}, ErrInvalid
		}
	}
	if len(rest) != 0 {
		return Evidence{}, ErrInvalid
	}
	result, err := New(d, e, i, source, slots)
	if err != nil || result.encoded != string(encoded) {
		return Evidence{}, ErrInvalid
	}
	return result, nil
}

// Source preserves the exact native instance-local source binding. It is never
// a public resource identifier and never grants permission.
type Source struct{ device, entity string }

func NewSource(device, entity string) (Source, error) {
	if device == "" || entity == "" || len(device) > 256 || len(entity) > 256 || !utf8.ValidString(device) || !utf8.ValidString(entity) {
		return Source{}, ErrInvalid
	}
	return Source{device, entity}, nil
}
func (s Source) Components() (string, string, error) {
	if _, err := NewSource(s.device, s.entity); err != nil {
		return "", "", err
	}
	return s.device, s.entity, nil
}
