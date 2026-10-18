package hilo

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

var ErrProviderShape = errors.New("hilo: unsupported provider observation")

type hubDevice struct {
	ID        int      `json:"id"`
	HiloID    string   `json:"hiloId"`
	Type      string   `json:"type"`
	Model     string   `json:"modelNumber"`
	Supported []string `json:"supportedAttributesList"`
}

func decodeHubInventory(raw json.RawMessage) ([]hubDevice, error) {
	var rows []hubDevice
	if json.Unmarshal(raw, &rows) != nil || rows == nil || len(rows) > 512 {
		return nil, ErrProviderShape
	}
	ids := make(map[int]bool, len(rows))
	hiloIDs := make(map[string]bool, len(rows))
	for i := range rows {
		row := &rows[i]
		if row.ID <= 0 || row.HiloID == "" || len(row.HiloID) > 256 || row.Type == "" || len(row.Type) > 80 || ids[row.ID] || hiloIDs[row.HiloID] {
			return nil, ErrProviderShape
		}
		row.Model = strings.ReplaceAll(row.Model, "Model_", "")
		if row.Model == "" && row.Type == "Thermostat" {
			row.Model = "EQ000016"
		}
		ids[row.ID], hiloIDs[row.HiloID] = true, true
	}
	return rows, nil
}

type hubFrame struct {
	Type      int               `json:"type"`
	Target    string            `json:"target"`
	Arguments []json.RawMessage `json:"arguments"`
	Error     string            `json:"error"`
}

func decodeHubFrames(data []byte) ([]hubFrame, error) {
	if len(data) > 1<<20 || len(data) == 0 || data[len(data)-1] != 0x1e {
		return nil, ErrProviderShape
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0x1e})
	if len(parts) > 64 {
		return nil, ErrProviderShape
	}
	frames := make([]hubFrame, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		var frame hubFrame
		if json.Unmarshal(part, &frame) != nil || frame.Type < 0 || frame.Type > 7 {
			return nil, ErrProviderShape
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

func hubInvocation(method string, locationID int) ([]byte, error) {
	if method != "SubscribeToLocation" && method != "UnsubscribeFromLocation" || locationID <= 0 {
		return nil, ErrProviderShape
	}
	frame := struct {
		Type         int    `json:"type"`
		InvocationID string `json:"invocationId"`
		Target       string `json:"target"`
		Arguments    []int  `json:"arguments"`
	}{Type: 1, InvocationID: "1", Target: method, Arguments: []int{locationID}}
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, ErrProviderShape
	}
	return append(raw, 0x1e), nil
}

type fieldNumber struct {
	present bool
	value   *float64
}
type fieldBool struct {
	present bool
	value   *bool
}
type deviceReading struct {
	power        fieldNumber
	ambient      fieldNumber
	target       fieldNumber
	probe        fieldNumber
	on           fieldBool
	disconnected fieldBool
	heating      fieldBool
}

func (r *deviceReading) merge(next deviceReading) {
	if next.power.present {
		r.power = next.power
	}
	if next.ambient.present {
		r.ambient = next.ambient
	}
	if next.target.present {
		r.target = next.target
	}
	if next.probe.present {
		r.probe = next.probe
	}
	if next.on.present {
		r.on = next.on
	}
	if next.disconnected.present {
		r.disconnected = next.disconnected
	}
	if next.heating.present {
		r.heating = next.heating
	}
}

func (r deviceReading) hasPayload() bool {
	return r.power.present || r.ambient.present || r.target.present || r.probe.present || r.on.present || r.disconnected.present || r.heating.present
}

func decodeGraphQLDevice(raw json.RawMessage) (string, deviceReading, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 || len(object) > 80 {
		return "", deviceReading{}, ErrProviderShape
	}
	var id string
	if json.Unmarshal(object["hiloId"], &id) != nil || id == "" || len(id) > 256 {
		return "", deviceReading{}, ErrProviderShape
	}
	var out deviceReading
	if value, ok := object["power"]; ok {
		out.power = decodeGraphQLNumber(value, true)
	}
	if value, ok := object["ambientTemperature"]; ok {
		out.ambient = decodeGraphQLNumber(value, false)
	}
	if value, ok := object["ambientTempSetpoint"]; ok {
		out.target = decodeGraphQLNumber(value, false)
	}
	if value, ok := object["probeTemp"]; ok {
		out.probe = decodeGraphQLNumber(value, false)
	}
	if value, ok := object["state"]; ok {
		out.on = decodeStateBool(value)
	}
	if value, ok := object["connectionStatus"]; ok {
		out.disconnected = decodeDisconnected(value)
	}
	if value, ok := object["currentState"]; ok {
		out.heating = decodeHeating(value)
	}
	if value, ok := object["heatDemand"]; ok {
		out.heating = decodeHeatDemand(value)
	}
	return id, out, nil
}

func graphQLDeviceType(raw json.RawMessage) string {
	var entry struct {
		DeviceType string `json:"deviceType"`
	}
	if json.Unmarshal(raw, &entry) != nil {
		return ""
	}
	return entry.DeviceType
}

func decodeHeatDemand(raw json.RawMessage) fieldBool {
	out := fieldBool{present: true}
	var demand *float64
	if json.Unmarshal(raw, &demand) != nil || demand == nil || !finite(*demand) || *demand < 0 {
		return out
	}
	heating := *demand > 0
	out.value = &heating
	return out
}

func decodeGraphQLNumber(raw json.RawMessage, power bool) fieldNumber {
	out := fieldNumber{present: true}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return out
	}
	var value struct {
		Value *float64 `json:"value"`
		Kind  string   `json:"kind"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Value == nil || !finite(*value.Value) {
		return out
	}
	number := *value.Value
	if power {
		switch strings.ToLower(value.Kind) {
		case "kilowatt":
			number *= 1000
		case "watt":
		default:
			return out
		}
		if number < 0 || !finite(number) {
			return out
		}
	} else if value.Kind != "" && !strings.EqualFold(value.Kind, "Celsius") {
		// The pinned mapper takes this field's value as Celsius. It does not
		// establish a GraphQL enum literal; a contradictory kind is refused.
		return out
	}
	out.value = &number
	return out
}

func decodeStateBool(raw json.RawMessage) fieldBool {
	out := fieldBool{present: true}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return out
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch strings.ToLower(text) {
		case "on":
			value := true
			out.value = &value
		case "off":
			value := false
			out.value = &value
		}
		return out
	}
	var value *bool
	if json.Unmarshal(raw, &value) == nil {
		out.value = value
		return out
	}
	var binary *int
	if json.Unmarshal(raw, &binary) == nil && binary != nil && (*binary == 0 || *binary == 1) {
		v := *binary == 1
		out.value = &v
	}
	return out
}

func decodeDisconnected(raw json.RawMessage) fieldBool {
	out := fieldBool{present: true}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch strings.ToLower(text) {
		case "connected":
			value := false
			out.value = &value
		case "disconnected":
			value := true
			out.value = &value
		}
		return out
	}
	var number *int
	if json.Unmarshal(raw, &number) == nil && number != nil && (*number == 1 || *number == 2) {
		value := *number == 2
		out.value = &value
	}
	return out
}

func decodeHeating(raw json.RawMessage) fieldBool {
	out := fieldBool{present: true}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch strings.ToLower(text) {
		case "heating":
			value := true
			out.value = &value
		case "idle":
			value := false
			out.value = &value
		}
		return out
	}
	return out
}

type hubAttribute uint8

const (
	hubUnknown hubAttribute = iota
	hubPower
	hubCurrentTemperature
	hubTargetTemperature
	hubOnOff
	hubDisconnected
	hubHeating
)

type hubValue struct {
	deviceID  int
	hiloID    string
	location  int
	attribute hubAttribute
	reading   deviceReading
	at        time.Time
}

func decodeHubValues(raw json.RawMessage) ([]hubValue, error) {
	var rows []struct {
		ID        int             `json:"deviceId"`
		HiloID    string          `json:"hiloId"`
		Location  int             `json:"locationId"`
		Attribute string          `json:"attribute"`
		Value     json.RawMessage `json:"value"`
		ValueType string          `json:"valueType"`
		Timestamp string          `json:"timeStampUTC"`
	}
	if json.Unmarshal(raw, &rows) != nil || rows == nil || len(rows) > 512 {
		return nil, ErrProviderShape
	}
	out := make([]hubValue, 0, len(rows))
	for _, row := range rows {
		if row.ID < 0 || row.ID == 0 && row.HiloID == "" || len(row.HiloID) > 256 || row.Attribute == "" || len(row.Attribute) > 80 || len(row.ValueType) > 80 {
			return nil, ErrProviderShape
		}
		item := hubValue{deviceID: row.ID, hiloID: row.HiloID, location: row.Location}
		switch row.Attribute {
		case "Power":
			item.attribute, item.reading.power = hubPower, fieldNumber{present: true, value: parseHubNumber(row.Value)}
		case "CurrentTemperature":
			item.attribute, item.reading.ambient = hubCurrentTemperature, fieldNumber{present: true, value: parseHubNumber(row.Value)}
		case "TargetTemperature":
			item.attribute, item.reading.target = hubTargetTemperature, fieldNumber{present: true, value: parseHubNumber(row.Value)}
		case "OnOff":
			item.attribute, item.reading.on = hubOnOff, decodeStateBool(row.Value)
		case "Disconnected":
			item.attribute, item.reading.disconnected = hubDisconnected, fieldBool{present: true, value: parseHubBool(row.Value)}
		case "Heating":
			item.attribute, item.reading.heating = hubHeating, parseHubHeating(row.Value)
		default:
			continue
		}
		at := parseSourceTime(row.Timestamp, time.Now().UTC())
		if at == nil {
			return nil, ErrProviderShape
		}
		item.at = *at
		out = append(out, item)
	}
	return out, nil
}

func parseHubHeating(raw json.RawMessage) fieldBool {
	out := fieldBool{present: true}
	if value := parseHubBool(raw); value != nil {
		out.value = value
		return out
	}
	if value := parseHubNumber(raw); value != nil && *value >= 0 && *value <= 100 {
		heating := *value > 0
		out.value = &heating
	}
	return out
}

func parseHubNumber(raw json.RawMessage) *float64 {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var scalar *float64
	if json.Unmarshal(raw, &scalar) != nil {
		var wrapped struct {
			Value *float64 `json:"value"`
		}
		if json.Unmarshal(raw, &wrapped) != nil || wrapped.Value == nil {
			return nil
		}
		scalar = wrapped.Value
	}
	if scalar == nil || !finite(*scalar) {
		return nil
	}
	return scalar
}
func parseHubBool(raw json.RawMessage) *bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var value *bool
	if json.Unmarshal(raw, &value) != nil {
		var wrapped struct {
			Value *bool `json:"value"`
		}
		if json.Unmarshal(raw, &wrapped) != nil || wrapped.Value == nil {
			return nil
		}
		value = wrapped.Value
	}
	return value
}
func finite(value float64) bool { return !math.IsInf(value, 0) && !math.IsNaN(value) }

func decodeNotificationCount(raw []byte) (float64, error) {
	var rows []struct {
		Viewed *bool `json:"viewed"`
	}
	if json.Unmarshal(raw, &rows) != nil || rows == nil || len(rows) > 4096 {
		return 0, ErrProviderShape
	}
	var count float64
	for _, row := range rows {
		if row.Viewed == nil {
			return 0, ErrProviderShape
		}
		if !*row.Viewed {
			count++
		}
	}
	return count, nil
}

func decodeWeather(raw []byte) (float64, error) {
	var rows []struct {
		Temperature *float64 `json:"temperature"`
	}
	// python-hilo documents a list, normally containing one current record.
	if json.Unmarshal(raw, &rows) != nil || len(rows) == 0 || len(rows) > 24 || rows[0].Temperature == nil || !finite(*rows[0].Temperature) || *rows[0].Temperature < -273.15 {
		return 0, ErrProviderShape
	}
	return *rows[0].Temperature, nil
}

func decodeLocations(raw []byte, id int, hiloID string) error {
	var rows []struct {
		ID     int    `json:"id"`
		HiloID string `json:"locationHiloId"`
	}
	if json.Unmarshal(raw, &rows) != nil || len(rows) == 0 || len(rows) > 100 {
		return ErrProviderShape
	}
	for _, row := range rows {
		if row.ID == id && row.HiloID == hiloID {
			return nil
		}
	}
	return ErrProviderShape
}

func parseSourceTime(raw string, received time.Time) *time.Time {
	if raw == "" {
		return nil
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || value.IsZero() || value.After(received.Add(2*time.Minute)) || value.Before(received.Add(-24*time.Hour)) {
		return nil
	}
	value = value.UTC()
	return &value
}
