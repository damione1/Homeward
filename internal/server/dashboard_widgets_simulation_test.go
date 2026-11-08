package server

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

// This simulator is compiled only into the explicitly isolated review server.
// It reports a command's synthetic effect through the same causal observation
// path as a real driver. An ACK alone remains insufficient for confirmation.
type dashboardWidgetsSimulatedDevice struct {
	definition dashboardWidgetsDevice
	binding    integrations.Binding
	value      *domain.StateValue
	quality    domain.Quality
	// The synthetic motor has its own operation result. CoverStateOpen alone
	// never establishes that a STOP command was actually handled.
	coverMotorStopped bool
	coverStopReceipt  *dashboardWidgetsCoverStopReceipt
}

type dashboardWidgetsCoverStopReceipt struct {
	command domain.CommandID
	entity  domain.EntityID
	request *actions.CommandRequest
	stopped bool
	at      time.Time
}

func (d *dashboardWidgetsSimulatedDevice) retain(request *actions.CommandRequest, id domain.CommandID, entity domain.EntityID, value domain.StateValue, at time.Time) {
	d.value = &value
	d.coverStopReceipt = nil
	if cover := request.Arguments.GetCover(); cover != nil {
		if operation, ok := cover.Command.(*controls.CoverAction_Operation); ok && operation.Operation == controls.CoverOperation_COVER_OPERATION_STOP {
			// This is an explicit operation on the synthetic motor, under the
			// driver's mutex, independent of the reported cover position.
			d.coverMotorStopped = true
			d.coverStopReceipt = &dashboardWidgetsCoverStopReceipt{id, entity, proto.Clone(request).(*actions.CommandRequest), d.coverMotorStopped, at}
		}
	}
}

// This private protocol proof belongs only to the isolated synthetic driver.
// Production drivers retain their own independent protocol semantics.
type dashboardWidgetsProtocolReadback struct {
	command             domain.CommandID
	entity              domain.EntityID
	request             *actions.CommandRequest
	value               domain.StateValue
	quality             domain.Quality
	publishedAt, readAt time.Time
	stopReceipt         *dashboardWidgetsCoverStopReceipt
}

func (p dashboardWidgetsProtocolReadback) QualifyProtocolConfirmation(command integrations.AdmittedCommand) error {
	id, entity, err := command.RouteIdentity()
	if err != nil {
		return err
	}
	request, err := command.Request()
	if err != nil {
		return err
	}
	return p.qualify(id, entity, request)
}

func (p dashboardWidgetsProtocolReadback) qualify(id domain.CommandID, entity domain.EntityID, request *actions.CommandRequest) error {
	reject := integrations.ErrCommandOutcomeIndeterminate
	if _, err := id.Value(); err != nil {
		return reject
	}
	if _, err := entity.Value(); err != nil {
		return reject
	}
	if id != p.command || entity != p.entity || request == nil || p.request == nil ||
		!proto.Equal(request, p.request) ||
		p.publishedAt.IsZero() || p.readAt.Before(p.publishedAt) {
		return reject
	}
	availability, freshness, provenance, err := p.quality.Components()
	if err != nil || availability != domain.AvailabilityAvailable || freshness != domain.FreshnessFresh || provenance != domain.ProvenanceObserved {
		return reject
	}
	switch request.CapabilitySchemaId {
	case contracts.ClimateSchema:
		return p.qualifyClimate(request)
	case contracts.CoverSchema:
		return p.qualifyCover(request)
	case contracts.LockSchema:
		observed, ok := p.value.Lock()
		wanted := request.Arguments.GetLock()
		if !ok || wanted == nil {
			return reject
		}
		switch wanted.Operation {
		case controls.LockOperation_LOCK_OPERATION_LOCK:
			if observed == domain.LockStateLocked {
				return nil
			}
		case controls.LockOperation_LOCK_OPERATION_UNLOCK:
			if observed == domain.LockStateUnlocked {
				return nil
			}
		}
	case contracts.AlarmSchema:
		observed, ok := p.value.Alarm()
		wanted := request.Arguments.GetAlarm()
		if !ok || wanted == nil {
			return reject
		}
		switch wanted.Operation {
		case controls.AlarmOperation_ALARM_OPERATION_ARM_HOME:
			if observed == domain.AlarmStateArmedHome {
				return nil
			}
		case controls.AlarmOperation_ALARM_OPERATION_ARM_AWAY:
			if observed == domain.AlarmStateArmedAway {
				return nil
			}
		case controls.AlarmOperation_ALARM_OPERATION_DISARM:
			if observed == domain.AlarmStateDisarmed {
				return nil
			}
		}
	}
	return reject
}

func (p dashboardWidgetsProtocolReadback) qualifyCover(request *actions.CommandRequest) error {
	reject := integrations.ErrCommandOutcomeIndeterminate
	cover, ok := p.value.Cover()
	wanted := request.Arguments.GetCover()
	if !ok || wanted == nil {
		return reject
	}
	state, position, _, err := cover.Components()
	if err != nil {
		return reject
	}
	switch action := wanted.Command.(type) {
	case *controls.CoverAction_Operation:
		switch action.Operation {
		case controls.CoverOperation_COVER_OPERATION_OPEN:
			if state != nil && *state == domain.CoverStateOpen && position != nil && *position == 100 {
				return nil
			}
		case controls.CoverOperation_COVER_OPERATION_CLOSE:
			if state != nil && *state == domain.CoverStateClosed && position != nil && *position == 0 {
				return nil
			}
		case controls.CoverOperation_COVER_OPERATION_STOP:
			if state != nil && (*state == domain.CoverStateOpening || *state == domain.CoverStateClosing) {
				return reject
			}
			receipt := p.stopReceipt
			if receipt != nil && receipt.command == p.command && receipt.entity == p.entity &&
				receipt.stopped && proto.Equal(receipt.request, request) && !receipt.at.IsZero() &&
				!receipt.at.Before(p.publishedAt) && !p.readAt.Before(receipt.at) {
				return nil
			}
		}
	case *controls.CoverAction_PositionPercent:
		if position != nil && math.Abs(*position-action.PositionPercent) <= 1e-6 {
			return nil
		}
	}
	return reject
}

func (p dashboardWidgetsProtocolReadback) qualifyClimate(request *actions.CommandRequest) error {
	reject := integrations.ErrCommandOutcomeIndeterminate
	climate, ok := p.value.Climate()
	if !ok {
		return reject
	}
	observed, err := climate.Components()
	if err != nil {
		return reject
	}
	wanted := request.Arguments.GetSetClimate()
	if wanted == nil {
		return reject
	}
	fields := 0
	if wanted.SetpointCelsius != nil {
		if observed.Setpoint == nil || wanted.HeatThresholdCelsius != nil || wanted.CoolThresholdCelsius != nil {
			return reject
		}
		value, ok := observed.Setpoint.Single()
		if !ok || !dashboardWidgetsTemperatureMatches(value, *wanted.SetpointCelsius) {
			return reject
		}
		fields++
	}
	if wanted.HeatThresholdCelsius != nil || wanted.CoolThresholdCelsius != nil {
		if observed.Setpoint == nil || wanted.HeatThresholdCelsius == nil || wanted.CoolThresholdCelsius == nil {
			return reject
		}
		low, high, ok := observed.Setpoint.Range()
		if !ok || !dashboardWidgetsTemperatureMatches(low, *wanted.HeatThresholdCelsius) || !dashboardWidgetsTemperatureMatches(high, *wanted.CoolThresholdCelsius) {
			return reject
		}
		fields++
	}
	if wanted.Mode != nil {
		if observed.Mode == nil {
			return reject
		}
		mode, err := contracts.WireClimateMode(*observed.Mode)
		if err != nil || mode != *wanted.Mode {
			return reject
		}
		fields++
	}
	if wanted.FanTarget != nil {
		if observed.FanTarget == nil {
			return reject
		}
		target, err := contracts.WireClimateFanTarget(*observed.FanTarget)
		if err != nil || target != *wanted.FanTarget {
			return reject
		}
		fields++
	}
	if fields == 0 {
		return reject
	}
	return nil
}

func dashboardWidgetsTemperatureMatches(value domain.CelsiusTemperature, expected float64) bool {
	number, err := value.Value()
	return err == nil && math.Abs(number-expected) <= 1e-6
}

func TestDashboardWidgetsClimateReadbackRequiresExactIdentityAndObservedResult(t *testing.T) {
	id, _ := domain.ParseCommandID("synthetic-climate-command")
	entity, _ := domain.ParseEntityID("synthetic-climate-entity")
	newProof := func() dashboardWidgetsProtocolReadback {
		t.Helper()
		temperature, _ := domain.NewCelsiusTemperature(20)
		target, _ := domain.NewCelsiusTemperature(20.5)
		setpoint, _ := domain.NewSingleClimateSetpoint(target)
		climate, err := domain.NewClimateValue(domain.ClimateValueInput{CurrentTemperature: &temperature, Setpoint: &setpoint})
		if err != nil {
			t.Fatal(err)
		}
		value, err := domain.NewClimateStateValue(climate)
		if err != nil {
			t.Fatal(err)
		}
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		now := time.Now().UTC()
		return dashboardWidgetsProtocolReadback{command: id, entity: entity, quality: quality, value: value, publishedAt: now, readAt: now.Add(time.Millisecond),
			request: &actions.CommandRequest{CommandId: "synthetic-climate-command", Target: &registrywire.EntityRef{Reference: &registrywire.EntityRef_EntityId{EntityId: "synthetic-climate-entity"}}, Endpoint: "temperature", CapabilitySchemaId: contracts.ClimateSchema,
				Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: nhf008Pointer(20.5)}}}}}
	}
	proof := newProof()
	if err := proof.qualify(id, entity, proto.Clone(proof.request).(*actions.CommandRequest)); err != nil {
		t.Fatal("canonical 20.5 setpoint did not qualify", err)
	}
	cases := []struct {
		name   string
		change func(*dashboardWidgetsProtocolReadback, *domain.CommandID, *domain.EntityID, *actions.CommandRequest)
	}{
		{"wrong-command", func(_ *dashboardWidgetsProtocolReadback, id *domain.CommandID, _ *domain.EntityID, _ *actions.CommandRequest) {
			*id, _ = domain.ParseCommandID("other-command")
		}},
		{"wrong-entity", func(_ *dashboardWidgetsProtocolReadback, _ *domain.CommandID, entity *domain.EntityID, _ *actions.CommandRequest) {
			*entity, _ = domain.ParseEntityID("other-entity")
		}},
		{"wrong-request", func(_ *dashboardWidgetsProtocolReadback, _ *domain.CommandID, _ *domain.EntityID, request *actions.CommandRequest) {
			request.Endpoint = "other-endpoint"
		}},
		{"wrong-observed-value", func(p *dashboardWidgetsProtocolReadback, _ *domain.CommandID, _ *domain.EntityID, _ *actions.CommandRequest) {
			target, _ := domain.NewCelsiusTemperature(20)
			setpoint, _ := domain.NewSingleClimateSetpoint(target)
			climate, _ := domain.NewClimateValue(domain.ClimateValueInput{Setpoint: &setpoint})
			p.value, _ = domain.NewClimateStateValue(climate)
		}},
		{"ack-only", func(p *dashboardWidgetsProtocolReadback, _ *domain.CommandID, _ *domain.EntityID, _ *actions.CommandRequest) {
			p.publishedAt = time.Time{}
		}},
		{"old-readback", func(p *dashboardWidgetsProtocolReadback, _ *domain.CommandID, _ *domain.EntityID, _ *actions.CommandRequest) {
			p.readAt = p.publishedAt.Add(-time.Second)
		}},
		{"stale", func(p *dashboardWidgetsProtocolReadback, _ *domain.CommandID, _ *domain.EntityID, _ *actions.CommandRequest) {
			p.quality, _ = domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceObserved)
		}},
		{"restored", func(p *dashboardWidgetsProtocolReadback, _ *domain.CommandID, _ *domain.EntityID, _ *actions.CommandRequest) {
			p.quality, _ = domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceRestored)
		}},
		{"unavailable", func(p *dashboardWidgetsProtocolReadback, _ *domain.CommandID, _ *domain.EntityID, _ *actions.CommandRequest) {
			p.quality, _ = domain.NewQuality(domain.AvailabilityUnavailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		}},
		{"absent-value", func(p *dashboardWidgetsProtocolReadback, _ *domain.CommandID, _ *domain.EntityID, _ *actions.CommandRequest) {
			p.value = domain.StateValue{}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proof := newProof()
			actualID, actualEntity := id, entity
			request := proto.Clone(proof.request).(*actions.CommandRequest)
			tc.change(&proof, &actualID, &actualEntity, request)
			if err := proof.qualify(actualID, actualEntity, request); err == nil {
				t.Fatal("unqualified synthetic readback confirmed")
			}
		})
	}
}

func TestDashboardWidgetsClimateReadbackMatchesEveryRequestedField(t *testing.T) {
	id, _ := domain.ParseCommandID("synthetic-range-command")
	entity, _ := domain.ParseEntityID("synthetic-range-entity")
	low, _ := domain.NewCelsiusTemperature(19)
	high, _ := domain.NewCelsiusTemperature(24)
	setpoint, _ := domain.NewRangeClimateSetpoint(low, high)
	mode := domain.ClimateModeAuto
	fan := domain.NewClimateFanTarget(false)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	request := &actions.CommandRequest{CapabilitySchemaId: contracts.ClimateSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{
		HeatThresholdCelsius: nhf008Pointer(19.), CoolThresholdCelsius: nhf008Pointer(24.), Mode: nhf008Pointer(registrywire.ClimateMode_CLIMATE_MODE_AUTO), FanTarget: nhf008Pointer(registrywire.ClimateFanTarget_CLIMATE_FAN_TARGET_ON),
	}}}}
	now := time.Now().UTC()
	input := domain.ClimateValueInput{Setpoint: &setpoint, Mode: &mode, FanTarget: &fan}
	for _, missing := range []string{"none", "range", "mode", "fan", "wrong-mode", "wrong-fan"} {
		t.Run(missing, func(t *testing.T) {
			observed := input
			switch missing {
			case "range":
				observed.Setpoint = nil
			case "mode":
				observed.Mode = nil
			case "fan":
				observed.FanTarget = nil
			case "wrong-mode":
				observed.Mode = nhf008Pointer(domain.ClimateModeHeat)
			case "wrong-fan":
				observed.FanTarget = nhf008Pointer(domain.NewClimateFanTarget(true))
			}
			climate, err := domain.NewClimateValue(observed)
			if err != nil {
				t.Fatal(err)
			}
			value, err := domain.NewClimateStateValue(climate)
			if err != nil {
				t.Fatal(err)
			}
			proof := dashboardWidgetsProtocolReadback{command: id, entity: entity, request: proto.Clone(request).(*actions.CommandRequest), value: value, quality: quality, publishedAt: now, readAt: now}
			err = proof.qualify(id, entity, request)
			if (err == nil) != (missing == "none") {
				t.Fatal("partial/mismatched range+mode+fan proof was not distinguished", err)
			}
		})
	}
}

func TestDashboardWidgetsProtocolReadbackRequiresExactControlTarget(t *testing.T) {
	id, _ := domain.ParseCommandID("synthetic-control-command")
	entity, _ := domain.ParseEntityID("synthetic-control-entity")
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	cover := func(state domain.CoverState, position float64) domain.StateValue {
		t.Helper()
		value, err := domain.NewCoverValue(&state, &position, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := domain.NewCoverStateValue(value)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	locked, _ := domain.NewLockStateValue(domain.LockStateLocked)
	unlocked, _ := domain.NewLockStateValue(domain.LockStateUnlocked)
	home, _ := domain.NewAlarmStateValue(domain.AlarmStateArmedHome)
	away, _ := domain.NewAlarmStateValue(domain.AlarmStateArmedAway)
	disarmed, _ := domain.NewAlarmStateValue(domain.AlarmStateDisarmed)
	cases := []struct {
		name, schema string
		args         *actions.ActionArgs
		correct      domain.StateValue
		wrong        domain.StateValue
	}{
		{"lock", contracts.LockSchema, &actions.ActionArgs{Arguments: &actions.ActionArgs_Lock{Lock: &controls.LockAction{Operation: controls.LockOperation_LOCK_OPERATION_LOCK}}}, locked, unlocked},
		{"unlock", contracts.LockSchema, &actions.ActionArgs{Arguments: &actions.ActionArgs_Lock{Lock: &controls.LockAction{Operation: controls.LockOperation_LOCK_OPERATION_UNLOCK}}}, unlocked, locked},
		{"arm-home", contracts.AlarmSchema, &actions.ActionArgs{Arguments: &actions.ActionArgs_Alarm{Alarm: &controls.AlarmAction{Operation: controls.AlarmOperation_ALARM_OPERATION_ARM_HOME}}}, home, away},
		{"arm-away", contracts.AlarmSchema, &actions.ActionArgs{Arguments: &actions.ActionArgs_Alarm{Alarm: &controls.AlarmAction{Operation: controls.AlarmOperation_ALARM_OPERATION_ARM_AWAY}}}, away, home},
		{"disarm", contracts.AlarmSchema, &actions.ActionArgs{Arguments: &actions.ActionArgs_Alarm{Alarm: &controls.AlarmAction{Operation: controls.AlarmOperation_ALARM_OPERATION_DISARM}}}, disarmed, home},
		{"open", contracts.CoverSchema, &actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &controls.CoverAction{Command: &controls.CoverAction_Operation{Operation: controls.CoverOperation_COVER_OPERATION_OPEN}}}}, cover(domain.CoverStateOpen, 100), cover(domain.CoverStateOpen, 50)},
		{"close", contracts.CoverSchema, &actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &controls.CoverAction{Command: &controls.CoverAction_Operation{Operation: controls.CoverOperation_COVER_OPERATION_CLOSE}}}}, cover(domain.CoverStateClosed, 0), cover(domain.CoverStateClosed, 50)},
		{"position", contracts.CoverSchema, &actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &controls.CoverAction{Command: &controls.CoverAction_PositionPercent{PositionPercent: 37}}}}, cover(domain.CoverStateOpen, 37), cover(domain.CoverStateOpen, 38)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			request := &actions.CommandRequest{CapabilitySchemaId: tc.schema, Arguments: tc.args}
			proof := dashboardWidgetsProtocolReadback{command: id, entity: entity, request: proto.Clone(request).(*actions.CommandRequest), value: tc.correct, quality: quality, publishedAt: now, readAt: now}
			if err := proof.qualify(id, entity, request); err != nil {
				t.Fatal("exact typed target did not qualify", err)
			}
			proof.value = tc.wrong
			if err := proof.qualify(id, entity, request); err == nil {
				t.Fatal("different typed target qualified")
			}
			// The fake device operation must produce the target independently
			// checked above; a decoder success alone is insufficient.
			actual, err := dashboardWidgetsApply(tc.wrong, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			proof.value = actual
			if err := proof.qualify(id, entity, request); err != nil {
				t.Fatal("applied operation was not observed", err)
			}
		})
	}
}

func TestDashboardWidgetsCoverStopRequiresExactMotorReceipt(t *testing.T) {
	id, _ := domain.ParseCommandID("synthetic-stop-command")
	entity, _ := domain.ParseEntityID("synthetic-stop-entity")
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	opening, position := domain.CoverStateOpening, 37.
	cover, _ := domain.NewCoverValue(&opening, &position, nil)
	current, _ := domain.NewCoverStateValue(cover)
	request := &actions.CommandRequest{CapabilitySchemaId: contracts.CoverSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &controls.CoverAction{Command: &controls.CoverAction_Operation{Operation: controls.CoverOperation_COVER_OPERATION_STOP}}}}}
	next, err := dashboardWidgetsApply(current, request.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	observed, _ := next.Cover()
	state, actualPosition, _, err := observed.Components()
	if err != nil || state != nil || actualPosition == nil || *actualPosition != position {
		t.Fatal("stop fabricated position or retained a moving state")
	}
	now := time.Now().UTC()
	device := dashboardWidgetsSimulatedDevice{value: &current, quality: quality}
	proof := dashboardWidgetsProtocolReadback{command: id, entity: entity, request: proto.Clone(request).(*actions.CommandRequest), value: next, quality: quality, publishedAt: now, readAt: now}
	if err := proof.qualify(id, entity, request); err == nil {
		t.Fatal("position observation alone proved stop")
	}
	device.retain(request, id, entity, next, now)
	proof.value, proof.stopReceipt = *device.value, device.coverStopReceipt
	if !device.coverMotorStopped || proof.qualify(id, entity, request) != nil {
		t.Fatal("explicit synthetic motor stop was not confirmed")
	}
	for _, invalid := range []string{"command", "entity", "request", "not-stopped", "old-receipt", "future-receipt"} {
		t.Run(invalid, func(t *testing.T) {
			mutated := proof
			receipt := *proof.stopReceipt
			mutated.stopReceipt = &receipt
			switch invalid {
			case "command":
				receipt.command, _ = domain.ParseCommandID("other-command")
			case "entity":
				receipt.entity, _ = domain.ParseEntityID("other-entity")
			case "request":
				receipt.request = proto.Clone(request).(*actions.CommandRequest)
				receipt.request.Endpoint = "other-endpoint"
			case "not-stopped":
				receipt.stopped = false
			case "old-receipt":
				receipt.at = now.Add(-time.Second)
			case "future-receipt":
				receipt.at = now.Add(time.Second)
			}
			if err := mutated.qualify(id, entity, request); err == nil {
				t.Fatal("unrelated/unexecuted stop receipt qualified")
			}
		})
	}
	proof.value = current
	if err := proof.qualify(id, entity, request); err == nil {
		t.Fatal("motor stop receipt contradicted by moving readback")
	}
}

func TestDashboardWidgetsCoverPositionUpdatesStateFromClosed(t *testing.T) {
	closed, position := domain.CoverStateClosed, 0.
	cover, _ := domain.NewCoverValue(&closed, &position, nil)
	current, _ := domain.NewCoverStateValue(cover)
	for _, target := range []float64{37, 0} {
		args := &actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &controls.CoverAction{Command: &controls.CoverAction_PositionPercent{PositionPercent: target}}}}
		next, err := dashboardWidgetsApply(current, args)
		if err != nil {
			t.Fatal(err)
		}
		value, _ := next.Cover()
		state, position, _, err := value.Components()
		want := domain.CoverStateOpen
		if target == 0 {
			want = domain.CoverStateClosed
		}
		if err != nil || state == nil || *state != want || position == nil || *position != target {
			t.Fatal("synthetic position retained contradictory cover state")
		}
		current = next
	}
}

func (d *dashboardWidgetsDriver) configure(source *integrations.Session, devices []dashboardWidgetsDevice, bindings []integrations.Binding) error {
	if len(devices) != len(bindings) {
		return integrations.ErrInvalidInput
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.source = source
	d.devices = make(map[string]*dashboardWidgetsSimulatedDevice, len(devices))
	for i, device := range devices {
		_, entity, err := bindings[i].IDs()
		if err != nil {
			return err
		}
		key, err := entity.Value()
		if err != nil {
			return err
		}
		observation, err := device.observation(nil, false)
		if err != nil {
			return err
		}
		_, quality, value, _, _, _, _, err := observation.Components()
		if err != nil {
			return err
		}
		d.devices[key] = &dashboardWidgetsSimulatedDevice{definition: device, binding: bindings[i], value: value, quality: quality}
	}
	return nil
}

func (d *dashboardWidgetsDriver) publish(ctx context.Context, condition *bool, stale bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, device := range d.devices {
		if device.definition.Kind == "binary" {
			observation, err := device.definition.observation(condition, stale)
			if err != nil {
				return err
			}
			_, quality, value, _, _, _, _, err := observation.Components()
			if err != nil {
				return err
			}
			device.quality, device.value = quality, value
		}
	}
	return d.publishLocked(ctx)
}

func (d *dashboardWidgetsDriver) heartbeat(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.publishLocked(ctx)
}

func (d *dashboardWidgetsDriver) publishLocked(ctx context.Context) error {
	for _, device := range d.devices {
		now := time.Now().UTC()
		var observed *time.Time
		if device.value != nil {
			observed = &now
		}
		observation, err := integrations.NewStateObservation(device.definition.Endpoint, device.quality, device.value, observed, now, nil)
		if err != nil {
			return err
		}
		if err = d.source.Publish(ctx, device.binding, observation); err != nil {
			return err
		}
	}
	return nil
}

func dashboardWidgetsApply(current domain.StateValue, arguments *actions.ActionArgs) (domain.StateValue, error) {
	if arguments == nil {
		return domain.StateValue{}, integrations.ErrInvalidInput
	}
	switch action := arguments.Arguments.(type) {
	case *actions.ActionArgs_SetSwitch:
		if _, ok := current.Switch(); !ok || action.SetSwitch == nil {
			break
		}
		return domain.NewSwitchValue(action.SetSwitch.On), nil
	case *actions.ActionArgs_SetLight:
		light, ok := current.Light()
		if !ok || action.SetLight == nil {
			break
		}
		lightParts, lightPartsErr := light.Components()
		on, brightness, color, err := lightParts.On, lightParts.Brightness, lightParts.ColorTemperatureMireds, lightPartsErr
		if err != nil {
			return domain.StateValue{}, err
		}
		if action.SetLight.On != nil {
			on = *action.SetLight.On
		}
		if action.SetLight.BrightnessPercent != nil {
			brightness = action.SetLight.BrightnessPercent
		}
		if action.SetLight.ColorTempMireds != nil {
			color = action.SetLight.ColorTempMireds
		}
		next, err := domain.NewLightValue(domain.LightValueInput{On: on, Brightness: brightness, ColorTemperatureMireds: color})
		if err != nil {
			return domain.StateValue{}, err
		}
		return domain.NewLightStateValue(next)
	case *actions.ActionArgs_SetFan:
		speed, ok := current.Fan()
		if !ok || action.SetFan == nil {
			break
		}
		parts, err := speed.Components()
		if err != nil {
			return domain.StateValue{}, err
		}
		if action.SetFan.Speed != nil {
			parts.Speed, parts.Level = action.SetFan.Speed, nil
		}
		if action.SetFan.Level != nil {
			parts.Level, parts.Speed = action.SetFan.Level, nil
		}
		if action.SetFan.On != nil {
			parts.On = action.SetFan.On
		}
		next, err := domain.NewFanValue(parts)
		if err != nil {
			return domain.StateValue{}, err
		}
		return domain.NewFanStateValue(next)
	case *actions.ActionArgs_SetClimate:
		climate, ok := current.Climate()
		if !ok || action.SetClimate == nil {
			break
		}
		next, err := climate.Components()
		if err != nil {
			return domain.StateValue{}, err
		}
		if value := action.SetClimate.SetpointCelsius; value != nil {
			target, err := domain.NewCelsiusTemperature(*value)
			if err != nil {
				return domain.StateValue{}, err
			}
			setpoint, err := domain.NewSingleClimateSetpoint(target)
			if err != nil {
				return domain.StateValue{}, err
			}
			next.Setpoint = &setpoint
		}
		if action.SetClimate.HeatThresholdCelsius != nil || action.SetClimate.CoolThresholdCelsius != nil {
			if action.SetClimate.HeatThresholdCelsius == nil || action.SetClimate.CoolThresholdCelsius == nil {
				break
			}
			low, err := domain.NewCelsiusTemperature(*action.SetClimate.HeatThresholdCelsius)
			if err != nil {
				return domain.StateValue{}, err
			}
			high, err := domain.NewCelsiusTemperature(*action.SetClimate.CoolThresholdCelsius)
			if err != nil {
				return domain.StateValue{}, err
			}
			setpoint, err := domain.NewRangeClimateSetpoint(low, high)
			if err != nil {
				return domain.StateValue{}, err
			}
			next.Setpoint = &setpoint
		}
		if action.SetClimate.Mode != nil {
			mode := domain.ClimateModeUnspecified
			switch *action.SetClimate.Mode {
			case registrywire.ClimateMode_CLIMATE_MODE_OFF:
				mode = domain.ClimateModeOff
			case registrywire.ClimateMode_CLIMATE_MODE_HEAT:
				mode = domain.ClimateModeHeat
			case registrywire.ClimateMode_CLIMATE_MODE_COOL:
				mode = domain.ClimateModeCool
			case registrywire.ClimateMode_CLIMATE_MODE_AUTO:
				mode = domain.ClimateModeAuto
			case registrywire.ClimateMode_CLIMATE_MODE_DRY:
				mode = domain.ClimateModeDry
			case registrywire.ClimateMode_CLIMATE_MODE_FAN_ONLY:
				mode = domain.ClimateModeFanOnly
			default:
				return domain.StateValue{}, integrations.ErrInvalidInput
			}
			next.Mode = &mode
		}
		if action.SetClimate.FanTarget != nil {
			var target domain.ClimateFanTarget
			switch *action.SetClimate.FanTarget {
			case registrywire.ClimateFanTarget_CLIMATE_FAN_TARGET_ON:
				target = domain.NewClimateFanTarget(false)
			case registrywire.ClimateFanTarget_CLIMATE_FAN_TARGET_AUTO:
				target = domain.NewClimateFanTarget(true)
			default:
				return domain.StateValue{}, integrations.ErrInvalidInput
			}
			next.FanTarget = &target
		}
		updated, err := domain.NewClimateValue(next)
		if err != nil {
			return domain.StateValue{}, err
		}
		return domain.NewClimateStateValue(updated)
	case *actions.ActionArgs_Cover:
		cover, ok := current.Cover()
		if !ok || action.Cover == nil {
			break
		}
		state, position, obstruction, err := cover.Components()
		if err != nil {
			return domain.StateValue{}, err
		}
		switch value := action.Cover.Command.(type) {
		case *controls.CoverAction_Operation:
			switch value.Operation {
			case controls.CoverOperation_COVER_OPERATION_OPEN:
				state, position = nhf008Pointer(domain.CoverStateOpen), nhf008Pointer(100.)
			case controls.CoverOperation_COVER_OPERATION_CLOSE:
				state, position = nhf008Pointer(domain.CoverStateClosed), nhf008Pointer(0.)
			case controls.CoverOperation_COVER_OPERATION_STOP:
				// The stopped motor receipt is retained separately. Clear a
				// moving state instead of claiming it is still opening/closing.
				if state != nil && (*state == domain.CoverStateOpening || *state == domain.CoverStateClosing) {
					state = nil
				}
			default:
				return domain.StateValue{}, integrations.ErrInvalidInput
			}
		case *controls.CoverAction_PositionPercent:
			position = &value.PositionPercent
			if value.PositionPercent == 0 {
				state = nhf008Pointer(domain.CoverStateClosed)
			} else {
				state = nhf008Pointer(domain.CoverStateOpen)
			}
		default:
			return domain.StateValue{}, integrations.ErrInvalidInput
		}
		next, err := domain.NewCoverValue(state, position, obstruction)
		if err != nil {
			return domain.StateValue{}, err
		}
		return domain.NewCoverStateValue(next)
	case *actions.ActionArgs_Lock:
		if _, ok := current.Lock(); !ok || action.Lock == nil {
			break
		}
		switch action.Lock.Operation {
		case controls.LockOperation_LOCK_OPERATION_LOCK:
			return domain.NewLockStateValue(domain.LockStateLocked)
		case controls.LockOperation_LOCK_OPERATION_UNLOCK:
			return domain.NewLockStateValue(domain.LockStateUnlocked)
		}
	case *actions.ActionArgs_Alarm:
		if _, ok := current.Alarm(); !ok || action.Alarm == nil {
			break
		}
		switch action.Alarm.Operation {
		case controls.AlarmOperation_ALARM_OPERATION_ARM_HOME:
			return domain.NewAlarmStateValue(domain.AlarmStateArmedHome)
		case controls.AlarmOperation_ALARM_OPERATION_ARM_AWAY:
			return domain.NewAlarmStateValue(domain.AlarmStateArmedAway)
		case controls.AlarmOperation_ALARM_OPERATION_DISARM:
			return domain.NewAlarmStateValue(domain.AlarmStateDisarmed)
		}
	}
	return domain.StateValue{}, errors.New("unsupported synthetic command or incompatible observed value")
}

func TestDashboardWidgetsFanSimulationUsesCanonicalSpeedNames(t *testing.T) {
	current, err := domain.NewFanStateValue(nha212FixtureFan(t, "High"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := dashboardWidgetsApply(current, &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Speed: nhf008Pointer("Low")}}})
	if err != nil {
		t.Fatal(err)
	}
	if speed, ok := next.Fan(); !ok || speed != nha212FixtureFan(t, "Low") {
		t.Fatal("canonical speed was not observed")
	}
	if _, err = dashboardWidgetsApply(current, &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Speed: nhf008Pointer("low")}}}); err == nil {
		t.Fatal("noncanonical speed admitted")
	}
}

func TestDashboardWidgetsSyntheticReadbackPreservesUnchangedFields(t *testing.T) {
	brightness, color := 35., uint32(300)
	light, err := domain.NewLightValue(domain.LightValueInput{On: true, Brightness: &brightness, ColorTemperatureMireds: &color})
	if err != nil {
		t.Fatal(err)
	}
	current, err := domain.NewLightStateValue(light)
	if err != nil {
		t.Fatal(err)
	}
	nextBrightness := 65.
	next, err := dashboardWidgetsApply(current, &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{BrightnessPercent: &nextBrightness}}})
	if err != nil {
		t.Fatal(err)
	}
	value, ok := next.Light()
	if !ok {
		t.Fatal("state kind changed")
	}
	on, b, c, err := value.Components()
	if err != nil || !on || b == nil || *b != 65 || c == nil || *c != 300 {
		t.Fatal("partial command reset unchanged fields")
	}
	before, _ := current.Light()
	_, old, _, _ := before.Components()
	if old == nil || *old != 35 {
		t.Fatal("readback mutated the prior observation")
	}
	if _, err = dashboardWidgetsApply(current, &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Speed: nhf008Pointer("high")}}}); err == nil {
		t.Fatal("incompatible command admitted")
	}
}
