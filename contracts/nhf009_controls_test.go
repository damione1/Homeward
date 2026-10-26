package contracts

import (
	"bytes"
	"math"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	control "github.com/damione1/Homeward/gen/home/controls/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestNHF009ControlEnumCoverageAndPresenceBytes(t *testing.T) {
	for _, item := range []struct {
		descriptor protoreflect.EnumDescriptor
		expected   int
	}{
		{control.CoverState(0).Descriptor(), len(coverStates) + 1},
		{control.LockState(0).Descriptor(), len(lockStates) + 1},
		{control.AlarmState(0).Descriptor(), len(alarmStates) + 1},
		{control.CoverOperation(0).Descriptor(), 4},
		{control.LockOperation(0).Descriptor(), 3},
		{control.AlarmOperation(0).Descriptor(), 4},
	} {
		if item.descriptor.Values().Len() != item.expected {
			t.Fatalf("new control enum requires admission review: %s", item.descriptor.FullName())
		}
	}
	encoded, err := proto.Marshal(&control.CoverValue{PositionPercent: nha174Pointer(0.0), Obstructed: nha174Pointer(false)})
	if err != nil || !bytes.Equal(encoded, []byte{0x11, 0, 0, 0, 0, 0, 0, 0, 0, 0x18, 0}) {
		t.Fatal("explicit zero/false protobuf presence differs from Rust fixture", err)
	}
}

func nhf009ControlDescriptions() []*registry.CapabilityDescription {
	return []*registry.CapabilityDescription{
		{SchemaId: CoverSchema, Major: 1, Endpoint: "cover", DisplayName: "Synthetic cover", Constraints: &registry.CapabilityDescription_Cover{Cover: &control.CoverConstraints{
			Operations:              []control.CoverOperation{control.CoverOperation_COVER_OPERATION_OPEN, control.CoverOperation_COVER_OPERATION_STOP},
			SetPositionPercent:      &control.PercentBounds{Minimum: 10, Maximum: 90, Step: nha174Pointer(5.0)},
			ObservedStates:          []control.CoverState{control.CoverState_COVER_STATE_OPEN, control.CoverState_COVER_STATE_CLOSED, control.CoverState_COVER_STATE_OPENING, control.CoverState_COVER_STATE_CLOSING},
			ObservesPositionPercent: true, ObservesObstruction: true,
		}}},
		{SchemaId: LockSchema, Major: 1, Endpoint: "lock", DisplayName: "Synthetic lock", Constraints: &registry.CapabilityDescription_Lock{Lock: &control.LockConstraints{
			Operations:     []control.LockOperation{control.LockOperation_LOCK_OPERATION_LOCK},
			ObservedStates: []control.LockState{control.LockState_LOCK_STATE_LOCKED, control.LockState_LOCK_STATE_UNLOCKED, control.LockState_LOCK_STATE_LOCKING, control.LockState_LOCK_STATE_UNLOCKING, control.LockState_LOCK_STATE_JAMMED, control.LockState_LOCK_STATE_OPENING, control.LockState_LOCK_STATE_OPEN},
		}}},
		{SchemaId: AlarmSchema, Major: 1, Endpoint: "alarm", DisplayName: "Synthetic alarm", Constraints: &registry.CapabilityDescription_Alarm{Alarm: &control.AlarmConstraints{
			Operations:     []control.AlarmOperation{control.AlarmOperation_ALARM_OPERATION_ARM_HOME},
			ObservedStates: []control.AlarmState{control.AlarmState_ALARM_STATE_DISARMED, control.AlarmState_ALARM_STATE_ARMED_HOME, control.AlarmState_ALARM_STATE_ARMED_AWAY, control.AlarmState_ALARM_STATE_ARMED_NIGHT, control.AlarmState_ALARM_STATE_ARMED_VACATION, control.AlarmState_ALARM_STATE_ARMED_CUSTOM_BYPASS, control.AlarmState_ALARM_STATE_PENDING, control.AlarmState_ALARM_STATE_ARMING, control.AlarmState_ALARM_STATE_DISARMING, control.AlarmState_ALARM_STATE_TRIGGERED},
		}}},
	}
}

func TestNHF009ControlCatalogueCommandAdmission(t *testing.T) {
	catalog, entity := testNHA014Catalog(t)
	for _, description := range nhf009ControlDescriptions() {
		if err := catalog.RegisterDriverCapability(entity, description); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		schema, endpoint string
		args             *actions.ActionArgs
		valid            bool
	}{
		{CoverSchema, "cover", &actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &control.CoverAction{Command: &control.CoverAction_Operation{Operation: control.CoverOperation_COVER_OPERATION_OPEN}}}}, true},
		{CoverSchema, "cover", &actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &control.CoverAction{Command: &control.CoverAction_Operation{Operation: control.CoverOperation_COVER_OPERATION_CLOSE}}}}, false},
		{CoverSchema, "other", &actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &control.CoverAction{Command: &control.CoverAction_Operation{Operation: control.CoverOperation_COVER_OPERATION_OPEN}}}}, false},
		{LockSchema, "lock", &actions.ActionArgs{Arguments: &actions.ActionArgs_Lock{Lock: &control.LockAction{Operation: control.LockOperation_LOCK_OPERATION_LOCK}}}, true},
		{LockSchema, "lock", &actions.ActionArgs{Arguments: &actions.ActionArgs_Lock{Lock: &control.LockAction{Operation: control.LockOperation_LOCK_OPERATION_UNLOCK}}}, false},
		{AlarmSchema, "alarm", &actions.ActionArgs{Arguments: &actions.ActionArgs_Alarm{Alarm: &control.AlarmAction{Operation: control.AlarmOperation_ALARM_OPERATION_ARM_HOME}}}, true},
		{AlarmSchema, "alarm", &actions.ActionArgs{Arguments: &actions.ActionArgs_Alarm{Alarm: &control.AlarmAction{Operation: control.AlarmOperation_ALARM_OPERATION_DISARM}}}, false},
		{LockSchema, "lock", &actions.ActionArgs{Arguments: &actions.ActionArgs_Alarm{Alarm: &control.AlarmAction{Operation: control.AlarmOperation_ALARM_OPERATION_ARM_HOME}}}, false},
		{CoverSchema, "cover", &actions.ActionArgs{Arguments: (*actions.ActionArgs_Cover)(nil)}, false},
	} {
		cmd := testNHA014Command(item.schema, item.endpoint, item.args)
		if err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1); (err == nil) != item.valid {
			t.Fatalf("%s/%s valid=%v: %v", item.schema, item.endpoint, item.valid, err)
		}
		if item.valid {
			if err := catalog.AdmitCapabilityCommand(cmd, "epoch-b", 1); err == nil {
				t.Fatal("stale epoch admitted")
			}
			if err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 2); err == nil {
				t.Fatal("stale config admitted")
			}
		}
	}
	for _, number := range []float64{0, 9, 10, 12, 15, 90, 95, math.NaN(), math.Inf(1)} {
		args := &actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &control.CoverAction{Command: &control.CoverAction_PositionPercent{PositionPercent: number}}}}
		valid := number == 10 || number == 15 || number == 90
		if err := catalog.AdmitCapabilityCommand(testNHA014Command(CoverSchema, "cover", args), "epoch-a", 1); (err == nil) != valid {
			t.Fatalf("position=%v valid=%v: %v", number, valid, err)
		}
	}
	command := testNHA014Command(LockSchema, "lock", &actions.ActionArgs{Arguments: &actions.ActionArgs_Lock{Lock: &control.LockAction{Operation: control.LockOperation_LOCK_OPERATION_LOCK}}})
	if err := catalog.WithdrawDriverCapabilities(entity, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := catalog.AdmitCapabilityCommand(command, "epoch-a", 1); err == nil {
		t.Fatal("withdrawn lock capability remained actionable")
	}
}

func TestNHF009ControlDescriptionsAndReadOnlyAdmission(t *testing.T) {
	catalog, _ := NewCapabilityCatalog(domain.NewUnitCatalog())
	for _, d := range nhf009ControlDescriptions() {
		if err := catalog.AdmitReadOnlyDriverCapability(d); err == nil {
			t.Fatal("actionable description admitted as read-only")
		}
	}
	cover, lock, alarm := nhf009ControlDescriptions()[0], nhf009ControlDescriptions()[1], nhf009ControlDescriptions()[2]
	cover.GetCover().Operations, cover.GetCover().SetPositionPercent = nil, nil
	lock.GetLock().Operations, alarm.GetAlarm().Operations = nil, nil
	for _, d := range []*registry.CapabilityDescription{cover, lock, alarm} {
		if err := catalog.AdmitReadOnlyDriverCapability(d); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutate := range []func(*registry.CapabilityDescription){
		func(d *registry.CapabilityDescription) { d.SchemaId = LockSchema },
		func(d *registry.CapabilityDescription) { d.GetCover().Operations = []control.CoverOperation{99} },
		func(d *registry.CapabilityDescription) {
			d.GetCover().Operations = append(d.GetCover().Operations, d.GetCover().Operations[0])
		},
		func(d *registry.CapabilityDescription) { d.GetCover().ObservedStates = []control.CoverState{0} },
		func(d *registry.CapabilityDescription) { d.GetCover().SetPositionPercent.Maximum = 101 },
		func(d *registry.CapabilityDescription) { d.GetCover().SetPositionPercent.Step = nha174Pointer(0.0) },
		func(d *registry.CapabilityDescription) {
			d.GetCover().SetPositionPercent.ProtoReflect().SetUnknown([]byte{0x98, 6, 1})
		},
	} {
		d := nhf009ControlDescriptions()[0]
		mutate(d)
		if err := catalog.AdmitDriverCapability(d); err == nil {
			t.Fatal("malformed control declaration admitted")
		}
	}
	for _, command := range []*control.CoverAction{nil, {}, {Command: (*control.CoverAction_Operation)(nil)}, {Command: (*control.CoverAction_PositionPercent)(nil)}, {Command: &control.CoverAction_Operation{Operation: 99}}} {
		if admitCoverAction(command, nhf009ControlDescriptions()[0].GetCover()) {
			t.Fatal("invalid cover action admitted")
		}
	}
	unknown := &control.LockAction{Operation: control.LockOperation_LOCK_OPERATION_LOCK}
	unknown.ProtoReflect().SetUnknown([]byte{0x98, 6, 1})
	if admitLockAction(unknown, nhf009ControlDescriptions()[1].GetLock()) {
		t.Fatal("unknown lock command field admitted")
	}
}

func TestNHF009ControlObservationsRoundTripAndCapabilityBoundary(t *testing.T) {
	q, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	for _, description := range nhf009ControlDescriptions() {
		catalog, entity := nha174Catalog(t, description)
		var values []domain.StateValue
		switch description.SchemaId {
		case CoverSchema:
			for _, state := range description.GetCover().ObservedStates {
				wire := &control.CoverValue{State: &state, PositionPercent: nha174Pointer(0.0), Obstructed: nha174Pointer(false)}
				cover, err := AdmitWireCoverValue(wire)
				if err != nil {
					t.Fatal(err)
				}
				if err := catalog.AdmitDomainCover(entity, description.Endpoint, cover); err != nil {
					t.Fatal("observation incorrectly constrained by command bounds", err)
				}
				value, _ := domain.NewCoverStateValue(cover)
				values = append(values, value)
			}
		case LockSchema:
			for _, state := range description.GetLock().ObservedStates {
				lock, err := AdmitWireLockValue(&control.LockValue{State: state})
				if err != nil {
					t.Fatal(err)
				}
				value, _ := domain.NewLockStateValue(lock)
				values = append(values, value)
			}
		case AlarmSchema:
			for _, state := range description.GetAlarm().ObservedStates {
				alarm, err := AdmitWireAlarmValue(&control.AlarmValue{State: state})
				if err != nil {
					t.Fatal(err)
				}
				value, _ := domain.NewAlarmStateValue(alarm)
				values = append(values, value)
			}
		}
		for _, value := range values {
			wire, _ := testNHA017Wire(t, nil, q, &value, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), 1)
			admission := &StateCapabilityAdmission{Catalog: catalog, Endpoint: description.Endpoint}
			decoded, err := AdmitWireState(wire, nil, admission)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodeObservedState(decoded)
			if err != nil || !proto.Equal(wire, encoded) {
				t.Fatal("roundtrip changed typed observation", err)
			}
			if _, err := AdmitWireState(wire, nil, nil); err == nil {
				t.Fatal("observation bypassed capability admission")
			}
			admission.Endpoint = "other"
			if _, err := AdmitWireState(wire, nil, admission); err == nil {
				t.Fatal("wrong endpoint admitted")
			}
		}
	}
	for _, value := range []*control.CoverValue{nil, {}, {State: nha174Pointer(control.CoverState(0))}, {State: nha174Pointer(control.CoverState(99))}, {PositionPercent: nha174Pointer(math.NaN())}, {PositionPercent: nha174Pointer(101.0)}} {
		if _, err := AdmitWireCoverValue(value); err == nil {
			t.Fatal("invalid cover wire observation admitted")
		}
	}
	for _, value := range []*control.LockValue{nil, {}, {State: 99}} {
		if _, err := AdmitWireLockValue(value); err == nil {
			t.Fatal("invalid lock wire observation admitted")
		}
	}
	for _, value := range []*control.AlarmValue{nil, {}, {State: 99}} {
		if _, err := AdmitWireAlarmValue(value); err == nil {
			t.Fatal("invalid alarm wire observation admitted")
		}
	}
}
