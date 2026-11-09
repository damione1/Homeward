package configcompilation

import (
	"bytes"
	"encoding/json"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"strings"
	"unicode/utf8"
)

// MigrateFanContextSQL33 repairs only the historical compiler-input encoding at
// the SQL migration boundary. Runtime/API readers retain strict current codecs.
// Already-admitted contexts stay byte-identical; no live capability is invented.
func MigrateFanContextSQL33(raw []byte) (Context, bool, error) {
	if current, err := ParseContext(raw); err == nil {
		if !bytes.Equal(current.Bytes(), raw) {
			return Context{}, false, ErrInvalidContext
		}
		return current, false, nil
	}
	if len(raw) == 0 || len(raw) > MaxContextBytes {
		return Context{}, false, ErrInvalidContext
	}
	var wire wireContext
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Context{}, false, ErrInvalidContext
	}
	// NewContext has always produced this exact JSON form. Refuse duplicate or
	// unknown keys and noncanonical input instead of silently normalizing it.
	canonical, err := json.Marshal(wire)
	if err != nil || !bytes.Equal(canonical, raw) || wire.Version != FormatVersion {
		return Context{}, false, ErrInvalidContext
	}
	changed := false
	for i := range wire.Capabilities {
		for j, encoded := range wire.Capabilities[i].Descriptions {
			next, didChange, err := migrateHistoricalFanDescription(encoded)
			if err != nil {
				return Context{}, false, err
			}
			if didChange {
				changed = true
				wire.Capabilities[i].Descriptions[j] = next
			}
		}
	}
	if !changed {
		return Context{}, false, ErrInvalidContext
	}
	next, err := json.Marshal(wire)
	if err != nil {
		return Context{}, false, ErrInvalidContext
	}
	admitted, err := ParseContext(next)
	if err != nil || !bytes.Equal(admitted.Bytes(), next) {
		return Context{}, false, ErrInvalidContext
	}
	return admitted, true, nil
}

func migrateHistoricalFanDescription(raw []byte) ([]byte, bool, error) {
	if len(raw) == 0 || len(raw) > MaxCapabilityBytes {
		return nil, false, ErrInvalidContext
	}
	current := new(registry.CapabilityDescription)
	if err := proto.Unmarshal(raw, current); err == nil && validDescriptionRepresentation(current) {
		canonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(current)
		if err == nil && bytes.Equal(canonical, raw) {
			return raw, false, nil
		}
	}
	// Retain every unrelated field byte. The final strict current decoder checks
	// those fields, their oneof, canonical order and absence of unknown data.
	var rebuilt []byte
	found := false
	for rest := raw; len(rest) > 0; {
		number, kind, tagLen := protowire.ConsumeTag(rest)
		if tagLen < 0 {
			return nil, false, ErrInvalidContext
		}
		valueLen := protowire.ConsumeFieldValue(number, kind, rest[tagLen:])
		if valueLen < 0 {
			return nil, false, ErrInvalidContext
		}
		field := rest[:tagLen+valueLen]
		if number == 9 {
			if found || kind != protowire.BytesType {
				return nil, false, ErrInvalidContext
			}
			found = true
			old, n := protowire.ConsumeBytes(rest[tagLen:])
			if n < 0 {
				return nil, false, ErrInvalidContext
			}
			canonicalField := protowire.AppendTag(nil, 9, protowire.BytesType)
			canonicalField = protowire.AppendBytes(canonicalField, old)
			if !bytes.Equal(canonicalField, field) {
				return nil, false, ErrInvalidContext
			}
			fan, err := migrateHistoricalFanConstraints(old)
			if err != nil {
				return nil, false, err
			}
			encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(fan)
			if err != nil {
				return nil, false, ErrInvalidContext
			}
			rebuilt = protowire.AppendTag(rebuilt, 9, protowire.BytesType)
			rebuilt = protowire.AppendBytes(rebuilt, encoded)
		} else {
			rebuilt = append(rebuilt, field...)
		}
		rest = rest[len(field):]
	}
	if !found {
		return nil, false, ErrInvalidContext
	}
	result := new(registry.CapabilityDescription)
	if err := proto.Unmarshal(rebuilt, result); err != nil || !validDescriptionRepresentation(result) || result.SchemaId != "core.fan.v1" || result.GetFan() == nil {
		return nil, false, ErrInvalidContext
	}
	canonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(result)
	if err != nil || !bytes.Equal(canonical, rebuilt) {
		return nil, false, ErrInvalidContext
	}
	return rebuilt, true, nil
}

func migrateHistoricalFanConstraints(raw []byte) (*registry.FanConstraints, error) {
	fan := &registry.FanConstraints{ObservesPower: true}
	var legacy []byte
	seenPower := false
	seenKeys := map[string]bool{}
	for rest := raw; len(rest) > 0; {
		n, kind, k := protowire.ConsumeTag(rest)
		if k < 0 {
			return nil, ErrInvalidContext
		}
		rest = rest[k:]
		switch {
		case n == 1 && kind == protowire.VarintType:
			v, k := protowire.ConsumeVarint(rest)
			if k < 0 || v != 1 || seenPower || len(seenKeys) != 0 {
				return nil, ErrInvalidContext
			}
			seenPower = true
			fan.SetOn = true
			legacy = protowire.AppendTag(legacy, 1, protowire.VarintType)
			legacy = protowire.AppendVarint(legacy, 1)
			rest = rest[k:]
		case n == 2 && kind == protowire.BytesType:
			key, k := protowire.ConsumeString(rest)
			if k < 0 || !utf8.ValidString(key) || key == "" || strings.TrimSpace(key) != key || seenKeys[key] {
				return nil, ErrInvalidContext
			}
			seenKeys[key] = true
			if fan.Speed == nil {
				fan.Speed = &registry.ControlChoices{}
			}
			fan.Speed.Choices = append(fan.Speed.Choices, &registry.ControlChoice{Key: key})
			legacy = protowire.AppendTag(legacy, 2, protowire.BytesType)
			legacy = protowire.AppendString(legacy, key)
			rest = rest[k:]
		default:
			return nil, ErrInvalidContext
		}
	}
	// The old proto3 codec omitted false booleans and serialized fields in order.
	if !bytes.Equal(legacy, raw) {
		return nil, ErrInvalidContext
	}
	return fan, nil
}
