package server

// Test-only injectable synthetic devices. Neither this source nor its controls
// are linked into homed, and no provider address or credential is accepted.
import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/damione1/Homeward/contracts"
	control "github.com/damione1/Homeward/gen/home/controls/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

type nhf009DeviceKind string

const (
	nhf009Cover  nhf009DeviceKind = "cover"
	nhf009Garage nhf009DeviceKind = "garage"
	nhf009Lock   nhf009DeviceKind = "lock"
	nhf009Alarm  nhf009DeviceKind = "alarm"
)

func (k *nhf009DeviceKind) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	switch value := nhf009DeviceKind(text); value {
	case nhf009Cover, nhf009Garage, nhf009Lock, nhf009Alarm:
		*k = value
		return nil
	default:
		return errors.New("unknown NHF009 synthetic device kind")
	}
}

type nhf009Device struct {
	kind            nhf009DeviceKind
	alias           domain.HAAlias
	key             integrations.SourceKey
	endpoint, title string
}

func nhf009ReadDevices(file string) ([]nhf009Device, error) {
	input, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	raw, err := io.ReadAll(io.LimitReader(input, 16385))
	if err != nil {
		return nil, err
	}
	if len(raw) > 16384 {
		return nil, errors.New("synthetic devices exceed bound")
	}
	var rows []struct {
		Kind     nhf009DeviceKind `json:"kind"`
		Alias    string           `json:"alias"`
		Title    string           `json:"title"`
		Endpoint string           `json:"endpoint"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rows); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing device source")
	}
	if len(rows) != 4 {
		return nil, errors.New("four synthetic device configurations required")
	}
	seen := make(map[nhf009DeviceKind]bool)
	aliases := make(map[string]bool)
	var result []nhf009Device
	for _, row := range rows {
		if seen[row.Kind] || aliases[row.Alias] || row.Kind == "" || row.Title == "" || len(row.Title) > 128 {
			return nil, errors.New("invalid synthetic device source")
		}
		seen[row.Kind] = true
		aliases[row.Alias] = true
		alias, err := domain.ParseHAAlias(row.Alias)
		if err != nil {
			return nil, err
		}
		key, err := integrations.NewSourceKey(string(row.Kind), row.Endpoint)
		if err != nil {
			return nil, err
		}
		result = append(result, nhf009Device{row.Kind, alias, key, row.Endpoint, row.Title})
	}
	return result, nil
}
func (d nhf009Device) capability() *registrywire.CapabilityDescription {
	result := &registrywire.CapabilityDescription{Major: 1, Endpoint: d.endpoint, DisplayName: d.title}
	switch d.kind {
	case nhf009Cover, nhf009Garage:
		result.SchemaId = contracts.CoverSchema
		limits := &control.CoverConstraints{Operations: []control.CoverOperation{control.CoverOperation_COVER_OPERATION_OPEN, control.CoverOperation_COVER_OPERATION_CLOSE, control.CoverOperation_COVER_OPERATION_STOP}, ObservedStates: []control.CoverState{control.CoverState_COVER_STATE_OPEN, control.CoverState_COVER_STATE_CLOSED, control.CoverState_COVER_STATE_OPENING, control.CoverState_COVER_STATE_CLOSING}, ObservesPositionPercent: true, ObservesObstruction: true}
		if d.kind == nhf009Cover {
			limits.SetPositionPercent = &control.PercentBounds{Minimum: 10, Maximum: 90, Step: nhf008Pointer(5.)}
		}
		result.Constraints = &registrywire.CapabilityDescription_Cover{Cover: limits}
	case nhf009Lock:
		result.SchemaId = contracts.LockSchema
		result.Constraints = &registrywire.CapabilityDescription_Lock{Lock: &control.LockConstraints{Operations: []control.LockOperation{control.LockOperation_LOCK_OPERATION_LOCK, control.LockOperation_LOCK_OPERATION_UNLOCK}, ObservedStates: []control.LockState{control.LockState_LOCK_STATE_LOCKED, control.LockState_LOCK_STATE_UNLOCKED, control.LockState_LOCK_STATE_LOCKING, control.LockState_LOCK_STATE_UNLOCKING, control.LockState_LOCK_STATE_JAMMED, control.LockState_LOCK_STATE_OPENING, control.LockState_LOCK_STATE_OPEN}}}
	case nhf009Alarm:
		result.SchemaId = contracts.AlarmSchema
		result.Constraints = &registrywire.CapabilityDescription_Alarm{Alarm: &control.AlarmConstraints{Operations: []control.AlarmOperation{control.AlarmOperation_ALARM_OPERATION_ARM_HOME, control.AlarmOperation_ALARM_OPERATION_ARM_AWAY, control.AlarmOperation_ALARM_OPERATION_DISARM}, ObservedStates: []control.AlarmState{control.AlarmState_ALARM_STATE_DISARMED, control.AlarmState_ALARM_STATE_ARMED_HOME, control.AlarmState_ALARM_STATE_ARMED_AWAY, control.AlarmState_ALARM_STATE_ARMED_NIGHT, control.AlarmState_ALARM_STATE_ARMED_VACATION, control.AlarmState_ALARM_STATE_ARMED_CUSTOM_BYPASS, control.AlarmState_ALARM_STATE_PENDING, control.AlarmState_ALARM_STATE_ARMING, control.AlarmState_ALARM_STATE_DISARMING, control.AlarmState_ALARM_STATE_TRIGGERED}}}
	default:
		panic("unparsed synthetic device kind")
	}
	return result
}
func (d nhf009Device) observation(missing bool) (integrations.StateObservation, error) {
	var value domain.StateValue
	var err error
	switch d.kind {
	case nhf009Cover, nhf009Garage:
		var position *float64
		if d.kind == nhf009Cover && !missing {
			position = nhf008Pointer(35.)
		}
		var state *domain.CoverState
		if !missing {
			state = nhf008Pointer(domain.CoverStateOpen)
		}
		cover, e := domain.NewCoverValue(state, position, nhf008Pointer(d.kind == nhf009Garage))
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewCoverStateValue(cover)
	case nhf009Lock:
		value, err = domain.NewLockStateValue(domain.LockStateJammed)
	case nhf009Alarm:
		value, err = domain.NewAlarmStateValue(domain.AlarmStateTriggered)
	default:
		return integrations.StateObservation{}, errors.New("unparsed synthetic device kind")
	}
	if err != nil {
		return integrations.StateObservation{}, err
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	at := time.Now().UTC()
	return integrations.NewStateObservation(d.endpoint, quality, &value, &at, at, nil)
}
