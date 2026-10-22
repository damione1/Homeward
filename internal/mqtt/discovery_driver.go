package mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/integrations"
)

type discoveryPilot struct {
	*Pilot
	mapper *DiscoveryMapper
}

// CommandDriver returns the one session's transport driver only in active
// mode. Supervisor attaches it before Start; discovery itself never publishes.
func (p *discoveryPilot) CommandDriver() integrations.CommandDriver {
	if p == nil || p.mapper == nil || p.mapper.mode != ModeActive {
		return nil
	}
	return p
}

func (p *discoveryPilot) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if p == nil || p.Pilot == nil || p.mapper == nil || ctx == nil {
		return ErrInvalidSettings
	}
	_, entity, err := command.RouteIdentity()
	if err != nil {
		return err
	}
	request, err := command.Request()
	if err != nil {
		return err
	}
	m := p.mapper
	m.mu.Lock()
	defer m.mu.Unlock()
	runtime := m.entities[entity]
	if runtime == nil || !runtime.profile.hasCommand {
		return integrations.ErrCommandRouteUnavailable
	}
	profile := runtime.profile
	var payload []byte
	switch profile.kind {
	case componentSwitch:
		if request.GetCapabilitySchemaId() != contracts.SwitchSchema || request.GetArguments().GetSetSwitch() == nil {
			return integrations.ErrCommandRouteUnavailable
		}
		if request.GetArguments().GetSetSwitch().On {
			payload = scalarBytes(profile.payloadOn)
		} else {
			payload = scalarBytes(profile.payloadOff)
		}
	case componentLight:
		if request.GetCapabilitySchemaId() != contracts.LightSchema || request.GetArguments().GetSetLight() == nil {
			return integrations.ErrCommandRouteUnavailable
		}
		encoding, err := lightEncodingFor(profile)
		if err != nil {
			return integrations.ErrCommandRouteUnavailable
		}
		brightness, hasBrightness, err := command.MQTTLightEmission(encoding)
		if err != nil {
			return err
		}
		arg := request.GetArguments().GetSetLight()
		fields := make(map[string]any, 4)
		if arg.TransitionSeconds != nil {
			seconds := *arg.TransitionSeconds
			if profile.admittedTransition != transitionIkeaDriver30W ||
				arg.On == nil && arg.BrightnessPercent == nil && arg.ColorTempMireds == nil ||
				math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > 6553 ||
				math.Abs(seconds*10-math.Round(seconds*10)) > 1e-9 ||
				arg.ColorTempMireds != nil || arg.On != nil && !*arg.On && arg.BrightnessPercent != nil {
				return integrations.ErrCommandRouteUnavailable
			}
			fields["transition"] = seconds
		}
		if arg.On != nil {
			if *arg.On {
				fields["state"] = "ON"
			} else {
				fields["state"] = "OFF"
			}
		}
		if arg.BrightnessPercent != nil {
			if !profile.brightness || !hasBrightness {
				return integrations.ErrCommandRouteUnavailable
			}
			fields["brightness"] = brightness
		} else if hasBrightness {
			return integrations.ErrCommandRouteUnavailable
		}
		if arg.ColorTempMireds != nil {
			if !profile.colorTemp || *arg.ColorTempMireds < profile.minMireds || *arg.ColorTempMireds > profile.maxMireds {
				return integrations.ErrCommandRouteUnavailable
			}
			fields["color_temp"] = *arg.ColorTempMireds
		}
		if len(fields) == 0 {
			return integrations.ErrCommandRouteUnavailable
		}
		payload, err = json.Marshal(fields)
		if err != nil {
			return err
		}
	default:
		return integrations.ErrCommandRouteUnavailable
	}
	publication, err := NewCommandPublication(profile.commandTopic, payload)
	if err != nil {
		return err
	}
	// Reuse only the admitted component's existing subscribed state route.
	// Peers without an explicit MQTT5 echo continue to have no confirmation.
	if p.Pilot.allowedObservation(profile.stateTopic) {
		causality, err := command.Causality()
		if err != nil {
			return err
		}
		publication, err = publication.withResponseCausality(profile.stateTopic, causality, runtime.correlationScope)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !command.ReadyForTransport() {
		return integrations.ErrCommandRouteUnavailable
	}
	writeErr := p.Pilot.WriteCommand(ctx, publication)
	if errors.Is(writeErr, ErrCommandWriteUncertain) {
		return writeErr
	}
	return writeErr
}

func scalarBytes(value scalar) []byte {
	switch value.kind {
	case scalarText:
		return []byte(value.text)
	case scalarBool:
		if value.boolean {
			return []byte("true")
		}
		return []byte("false")
	default:
		return nil
	}
}

// NewDiscoveryPilotFactory allocates a fresh mapper for each supervised
// session. A reconnect cannot reuse bindings or stale availability state.
func NewDiscoveryPilotFactory(settings Settings, repository discoveryRegistry) (integrations.PilotFactory, error) {
	if _, err := newDiscoveryMapper(settings, repository, nil); err != nil {
		return nil, err
	}
	return func() integrations.Pilot {
		mapper, err := newDiscoveryMapper(settings, repository, nil)
		if err != nil {
			return nil
		}
		pilot, err := NewPilot(settings, mapper)
		if err != nil {
			return nil
		}
		return &discoveryPilot{Pilot: pilot, mapper: mapper}
	}, nil
}
