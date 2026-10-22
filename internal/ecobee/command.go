package ecobee

import (
	"context"
	"math"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/mctofu/homekit/client"
	"google.golang.org/protobuf/proto"
)

type commandJob struct {
	ctx     context.Context
	command integrations.AdmittedCommand
	reply   chan error
}

func (p *Pilot) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if ctx == nil || p == nil {
		return ErrSettings
	}
	p.mu.Lock()
	running := p.running
	p.mu.Unlock()
	if !running {
		return ErrUnavailable
	}
	job := commandJob{ctx, command, make(chan error, 1)}
	select {
	case p.commands <- job:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return ErrUnavailable
	default:
		return ErrUnavailable
	}
	return awaitCommandReply(ctx, job.reply, p.done)
}

// The run task publishes its result before closing done. Preserve that result
// even when both channels are ready; cancellation behavior is unchanged.
func awaitCommandReply(ctx context.Context, reply <-chan error, done <-chan struct{}) error {
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		select {
		case err := <-reply:
			return err
		default:
			return ErrUnavailable
		}
	}
}

func (p climateProfile) commandValues(request *actions.CommandRequest) (map[climateField]float64, error) {
	if request == nil || request.CapabilitySchemaId != contracts.ClimateSchema || request.Endpoint != Endpoint || request.Arguments == nil || len(request.Arguments.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrSettings
	}
	arg := request.Arguments.GetSetClimate()
	if arg == nil || len(arg.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrSettings
	}
	values := make(map[climateField]float64, 4)
	if arg.SetpointCelsius != nil {
		if arg.HeatThresholdCelsius != nil || arg.CoolThresholdCelsius != nil {
			return nil, ErrSettings
		}
		values[targetTemperature] = *arg.SetpointCelsius
	}
	if (arg.HeatThresholdCelsius == nil) != (arg.CoolThresholdCelsius == nil) {
		return nil, ErrSettings
	}
	if arg.HeatThresholdCelsius != nil {
		if *arg.HeatThresholdCelsius > *arg.CoolThresholdCelsius {
			return nil, ErrSettings
		}
		values[heatThreshold] = *arg.HeatThresholdCelsius
		values[coolThreshold] = *arg.CoolThresholdCelsius
	}
	if arg.Mode != nil {
		switch *arg.Mode {
		case registry.ClimateMode_CLIMATE_MODE_OFF:
			values[requestedMode] = 0
		case registry.ClimateMode_CLIMATE_MODE_HEAT:
			values[requestedMode] = 1
		case registry.ClimateMode_CLIMATE_MODE_COOL:
			values[requestedMode] = 2
		case registry.ClimateMode_CLIMATE_MODE_AUTO:
			values[requestedMode] = 3
		case registry.ClimateMode_CLIMATE_MODE_UNSPECIFIED, registry.ClimateMode_CLIMATE_MODE_DRY, registry.ClimateMode_CLIMATE_MODE_FAN_ONLY:
			return nil, ErrSettings
		default:
			return nil, ErrSettings
		}
	}
	if arg.FanTarget != nil {
		switch *arg.FanTarget {
		case registry.ClimateFanTarget_CLIMATE_FAN_TARGET_ON:
			values[targetFan] = 0
		case registry.ClimateFanTarget_CLIMATE_FAN_TARGET_AUTO:
			values[targetFan] = 1
		case registry.ClimateFanTarget_CLIMATE_FAN_TARGET_UNSPECIFIED:
			return nil, ErrSettings
		default:
			return nil, ErrSettings
		}
	}
	if len(values) == 0 {
		return nil, ErrSettings
	}
	for field, number := range values {
		if _, err := p.write(field, number); err != nil {
			return nil, err
		}
	}
	return values, nil
}
func (p *Pilot) execute(ctx context.Context, command integrations.AdmittedCommand) error {
	request, err := command.Request()
	if err != nil {
		return err
	}
	commandID, entity, err := command.RouteIdentity()
	if err != nil {
		return err
	}
	_, expected, err := p.binding.IDs()
	if err != nil || entity != expected {
		return ErrSettings
	}
	values, err := p.profile.commandValues(request)
	if err != nil {
		return err
	}
	writes := &client.CharacteristicsWriteRequest{}
	// A stable field order gives deterministic frames and tests without changing HAP semantics.
	for field := climateField(0); field < climateFieldCount; field++ {
		if value, ok := values[field]; ok {
			row, e := p.profile.write(field, value)
			if e != nil {
				return e
			}
			writes.Characteristics = append(writes.Characteristics, row)
		}
	}
	if !command.ReadyForTransport() {
		return integrations.ErrInactiveSession
	}
	operation, cancel := context.WithTimeout(ctx, pollTimeout)
	rows, err := p.api.SetCharacteristics(operation, writes)
	cancel()
	if err != nil {
		return ErrCommandUncertain
	}
	seen := make(map[client.CharacteristicReadRequest]bool, len(rows))
	for _, row := range rows {
		if row == nil || row.Status != nil && *row.Status != 0 {
			return ErrCommandUncertain
		}
		id := client.CharacteristicReadRequest{AccessoryID: row.AccessoryID, CharacteristicID: row.CharacteristicID}
		if seen[id] {
			return ErrCommandUncertain
		}
		seen[id] = true
	}
	if len(seen) != len(writes.Characteristics) {
		return ErrCommandUncertain
	}
	for _, write := range writes.Characteristics {
		if !seen[client.CharacteristicReadRequest{AccessoryID: write.AccessoryID, CharacteristicID: write.CharacteristicID}] {
			return ErrCommandUncertain
		}
	}
	writtenAt := p.clock.Now()
	if err := command.Acknowledge(); err != nil {
		return err
	}
	if !command.ReadyForTransport() {
		return integrations.ErrInactiveSession
	}
	observation, err := p.read(ctx)
	if err != nil {
		return ErrCommandUncertain
	}
	at := p.clock.Now()
	proof := readbackProof{commandID, entity, request, values, observation, writtenAt, at}
	if err := proof.QualifyProtocolConfirmation(command); err != nil {
		return ErrCommandUncertain
	}
	causality, err := command.Causality()
	if err != nil {
		return err
	}
	if err := p.publish(ctx, observation, causality); err != nil {
		return err
	}
	if !command.ReadyForTransport() {
		return integrations.ErrInactiveSession
	}
	return command.ConfirmProtocol(proof)
}

// readbackProof is only constructed following a real separate GET on the same
// serialized authenticated client. Neither ACK nor a public field creates it.
type readbackProof struct {
	command           domain.CommandID
	entity            domain.EntityID
	request           *actions.CommandRequest
	values            map[climateField]float64
	observed          climateObservation
	writtenAt, readAt time.Time
}

func (p readbackProof) QualifyProtocolConfirmation(command integrations.AdmittedCommand) error {
	id, entity, err := command.RouteIdentity()
	if err != nil || id != p.command || entity != p.entity || p.writtenAt.IsZero() || p.readAt.Before(p.writtenAt) {
		return ErrCommandUncertain
	}
	request, err := command.Request()
	if err != nil || !proto.Equal(request, p.request) || len(p.values) == 0 {
		return ErrCommandUncertain
	}
	for field, expected := range p.values {
		value := p.observed.value(field, p.readAt)
		if value.kind != valuePresent || value.at.Before(p.writtenAt) || math.Abs(value.number-expected) > 1e-6 {
			return ErrCommandUncertain
		}
	}
	return nil
}
