package neviweb

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

// readback spends one targeted observation after a successful PUT. It uses the
// active session and ordinary model parser/publication; it cannot reconnect,
// repeat the write, accelerate the global poll or invent the requested value.
func (p *Pilot) readback(ctx context.Context, command integrations.AdmittedCommand, selected Binding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !command.ReadyForTransport() {
		return integrations.ErrInactiveSession
	}
	p.mu.RLock()
	host := p.host
	signature := append(json.RawMessage(nil), p.signatures[selected.deviceID]...)
	p.mu.RUnlock()
	if host == nil {
		return ErrSession
	}
	model, err := parseModel(signature, selected.kind)
	if err != nil {
		return err
	}
	attributes := requestedAttributes(selected.kind, model)
	if attributes == "" {
		return ErrProtocol
	}
	writtenAt := time.Now().UTC()
	response, readErr := p.fetch(ctx, "/api/device/"+selected.deviceID+"/attribute", url.Values{"attributes": []string{attributes}})
	snapshot := cycleSnapshot{frames: make(map[string]attributeFrame, 1), received: map[string]time.Time{selected.alias: time.Now().UTC()}}
	if readErr == nil {
		var frame attributeFrame
		frame, readErr = frameFor(selected.kind, response.Bytes(), signature)
		if readErr == nil {
			snapshot.frames[selected.alias] = frame
		}
	}
	// Even an unavailable target only invalidates its own current reading. A
	// targeted success cannot clear health faults belonging to other devices.
	if err := p.publishBindings(ctx, host, snapshot, []Binding{selected}); err != nil {
		return err
	}
	if readErr != nil {
		p.report(ctx, host, integrations.HealthDegraded, "neviweb command readback unavailable")
		return readErr
	}
	frame := snapshot.frames[selected.alias]
	if frame.diagnostic {
		p.report(ctx, host, integrations.HealthDegraded, "neviweb command readback partial")
		return ErrPartialObservation
	}
	id, entity, err := command.RouteIdentity()
	if err != nil {
		return err
	}
	request, err := command.Request()
	if err != nil {
		return err
	}
	proof := readbackProof{id, entity, request, frame, writtenAt, snapshot.received[selected.alias]}
	if err := proof.QualifyProtocolConfirmation(command); err != nil {
		// The device may apply the command later. Retain ACKNOWLEDGED, and
		// leave later observations to the unchanged normal cadence.
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !command.ReadyForTransport() {
		return integrations.ErrInactiveSession
	}
	return command.ConfirmProtocol(proof)
}

// A private proof is constructed only after a distinct GET on the serialized
// client was parsed and published by Host for this command's exact binding.
// The generic action tracker intentionally has no climate value predicate.
type readbackProof struct {
	command           domain.CommandID
	entity            domain.EntityID
	request           *actions.CommandRequest
	frame             attributeFrame
	writtenAt, readAt time.Time
}

func (p readbackProof) QualifyProtocolConfirmation(command integrations.AdmittedCommand) error {
	id, entity, err := command.RouteIdentity()
	if err != nil || id != p.command || entity != p.entity || p.writtenAt.IsZero() || p.readAt.Before(p.writtenAt) || p.frame.diagnostic {
		return ErrPartialObservation
	}
	request, err := command.Request()
	if err != nil || !proto.Equal(request, p.request) {
		return ErrPartialObservation
	}
	if change := request.GetArguments().GetSetSwitch(); change != nil {
		if p.frame.on == nil || *p.frame.on != change.On {
			return ErrPartialObservation
		}
		return nil
	}
	change := request.GetArguments().GetSetClimate()
	if change == nil || change.SetpointCelsius == nil && change.Mode == nil {
		return ErrPartialObservation
	}
	if change.SetpointCelsius != nil && (p.frame.target == nil || math.Abs(*p.frame.target-*change.SetpointCelsius) > 1e-6) {
		return ErrPartialObservation
	}
	if change.Mode != nil {
		expected, err := modeAttribute(p.frame.model.family, *change.Mode)
		if err != nil {
			return err
		}
		for key, want := range expected {
			var got string
			if json.Unmarshal(p.frame.raw[key], &got) != nil || got != want {
				return ErrPartialObservation
			}
		}
	}
	return nil
}
