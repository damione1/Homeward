package contracts

import (
	"errors"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"math"
	"unicode/utf8"
)

var ErrInvalidHistoryContract = errors.New("history: invalid contract")

// historyTree bounds nesting/cardinality, rejects typed nils, unknown fields and
// enum variants. Operational presence/relationships are checked separately.
func historyTree(m protoreflect.Message, depth int) bool {
	if !m.IsValid() || depth > 16 || len(m.GetUnknown()) != 0 || observationNilOneof(m.Interface()) {
		return false
	}
	valid := true
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if f.IsMap() {
			valid = false
			return false
		}
		check := func(v protoreflect.Value) bool {
			switch f.Kind() {
			case protoreflect.MessageKind:
				return historyTree(v.Message(), depth+1)
			case protoreflect.EnumKind:
				return v.Enum() > 0 && f.Enum().Values().ByNumber(v.Enum()) != nil
			case protoreflect.StringKind:
				return len(v.String()) <= historyquery.MaxPointBytes && utf8.ValidString(v.String())
			case protoreflect.BytesKind:
				return len(v.Bytes()) <= historyquery.MaxPointBytes
			case protoreflect.DoubleKind:
				return !math.IsNaN(v.Float()) && !math.IsInf(v.Float(), 0)
			case protoreflect.FloatKind:
				return !math.IsNaN(v.Float()) && !math.IsInf(v.Float(), 0)
			default:
				return true
			}
		}
		if f.IsList() {
			list := v.List()
			maximum := 100
			switch f.Name() {
			case "epochs":
				maximum = 64
			case "gaps", "retention":
				maximum = 512
			case "reasons":
				maximum = 12
			case "losses":
				maximum = 4
			}
			if list.Len() > maximum {
				valid = false
				return false
			}
			for i := 0; i < list.Len(); i++ {
				if !check(list.Get(i)) {
					valid = false
					return false
				}
			}
			return true
		}
		valid = check(v)
		return valid
	})
	if !valid {
		return false
	}
	if t, ok := m.Interface().(*timestamppb.Timestamp); ok {
		return t.CheckValid() == nil && !t.AsTime().IsZero()
	}
	return true
}

// AdmitHistoricalState checks the persisted value without applying today's
// catalogue constraints. Historical measurement interpretation is separate.
func AdmitHistoricalState(v *state.ObservedState) error {
	if v == nil || !historyTree(v.ProtoReflect(), 0) || v.Entity == nil || v.Entity.GetEntityId() == "" || v.Revision == 0 {
		return ErrInvalidHistoryContract
	}
	if _, err := domain.ParseEntityID(v.Entity.GetEntityId()); err != nil {
		return ErrInvalidHistoryContract
	}
	if _, err := admitQuality(v.Quality); err != nil {
		return ErrInvalidHistoryContract
	}
	if (v.Quality.Availability == state.Availability_AVAILABILITY_AVAILABLE) != (v.Value != nil) {
		return ErrInvalidHistoryContract
	}
	for _, t := range []*timestamppb.Timestamp{v.ReceivedAt, v.LastChanged, v.LastUpdated, v.LastReported} {
		if t == nil || t.CheckValid() != nil || t.AsTime().IsZero() {
			return ErrInvalidHistoryContract
		}
	}
	if _, err := domain.NewSourceStamp(v.SourceSession, v.SourceSeq); err != nil {
		return ErrInvalidHistoryContract
	}
	switch x := v.Value.(type) {
	case nil:
	case *state.ObservedState_SwitchOn:
		if x == nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Measurement:
		if x == nil || x.Measurement == nil || x.Measurement.Unit == "" {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Light:
		if x == nil || x.Light == nil || x.Light.On == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitLightValue(x.Light); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Fan:
		if x == nil || x.Fan == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireFanValue(x.Fan); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Climate:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireClimateValue(x.Climate); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Helper:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, _, err := admitHelperWire(x.Helper, true); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Solar:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireSolarValue(x.Solar); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_SolarEvent:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireSolarEventValue(x.SolarEvent); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Cover:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireCoverValue(x.Cover); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Lock:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireLockValue(x.Lock); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Alarm:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireAlarmValue(x.Alarm); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Weather:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireWeatherValue(x.Weather); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Presence:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWirePresenceValue(x.Presence); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_PresenceDetails:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWirePresenceDetailsValue(x.PresenceDetails); err != nil {
			return ErrInvalidHistoryContract
		}
	default:
		return ErrInvalidHistoryContract
	}
	return nil
}
