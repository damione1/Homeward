package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

// Private filesystem controls for the explicitly selected synthetic test. They
// are absent from homed, have no network route, and accept no device/provider.
type nhf006ControlAction string

const (
	nhf006Cut            nhf006ControlAction = "outage"
	nhf006Restore        nhf006ControlAction = "restore"
	nhf006DenyWrite      nhf006ControlAction = "deny-write"
	nhf006AllowWrite     nhf006ControlAction = "allow-write"
	nhf006WithdrawLight  nhf006ControlAction = "withdraw-light"
	nhf006WithdrawSwitch nhf006ControlAction = "withdraw-switch"
	nhf006Rediscover     nhf006ControlAction = "rediscover"
	nhf006Confirm        nhf006ControlAction = "confirm-last"
	nhf006Restart        nhf006ControlAction = "restart"
)

func (a *nhf006ControlAction) UnmarshalJSON(raw []byte) error {
	var input string
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	value := nhf006ControlAction(input)
	switch value {
	case nhf006Cut, nhf006Restore, nhf006DenyWrite, nhf006AllowWrite, nhf006WithdrawLight, nhf006WithdrawSwitch, nhf006Rediscover, nhf006Confirm, nhf006Restart:
		*a = value
		return nil
	default:
		return errors.New("unknown synthetic action control")
	}
}

type nhf006Control struct {
	Sequence uint64              `json:"sequence"`
	Action   nhf006ControlAction `json:"action"`
}

func nhf006ReadControl(directory string, previous uint64) (*nhf006Control, error) {
	file, err := os.Open(filepath.Join(directory, "command.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return nil, err
	}
	if len(raw) > 4096 {
		return nil, errors.New("synthetic control exceeds bound")
	}
	var command nhf006Control
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&command); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing synthetic control data")
	}
	if command.Sequence == 0 || command.Sequence > 32 || command.Action == "" {
		return nil, errors.New("invalid synthetic action control")
	}
	if command.Sequence == previous {
		return nil, nil
	}
	if command.Sequence != previous+1 {
		return nil, errors.New("out of order synthetic action control")
	}
	return &command, nil
}

func nhf006DeviceIndex(devices []nhf006Device, kind nhf006DeviceKind) (int, error) {
	for index, device := range devices {
		if device.kind == kind {
			return index, nil
		}
	}
	return 0, errors.New("synthetic family absent")
}

// Only an explicit test control publishes an effect, with the real command's
// causality token. Acknowledge alone never publishes an observation.
func (d *nhf006Driver) confirm(ctx context.Context, source *integrations.Session, bindings []integrations.Binding, devices []nhf006Device) error {
	d.mu.Lock()
	last := d.last
	d.mu.Unlock()
	if last == nil {
		return errors.New("no fake command to confirm")
	}
	_, entity, err := last.RouteIdentity()
	if err != nil {
		return err
	}
	request, err := last.Request()
	if err != nil {
		return err
	}
	for index, binding := range bindings {
		_, candidate, err := binding.IDs()
		if err != nil {
			return err
		}
		if candidate != entity {
			continue
		}
		var value domain.StateValue
		switch devices[index].kind {
		case nhf006Switch:
			args := request.Arguments.GetSetSwitch()
			if args == nil {
				return errors.New("fake switch arguments absent")
			}
			value = domain.NewSwitchValue(args.On)
		case nhf006Light:
			args := request.Arguments.GetSetLight()
			if args == nil {
				return errors.New("fake light arguments absent")
			}
			brightness := 30.
			if args.BrightnessPercent != nil {
				brightness = *args.BrightnessPercent
			}
			on := false
			if args.On != nil {
				on = *args.On
			}
			light, err := domain.NewLightValue(on, &brightness, nil)
			if err != nil {
				return err
			}
			value, err = domain.NewLightStateValue(light)
			if err != nil {
				return err
			}
		case nhf006Binary, nhf006Measurement:
			return errors.New("read-only fake family received a command")
		default:
			return errors.New("unparsed synthetic device kind")
		}
		quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		if err != nil {
			return err
		}
		at := time.Now().UTC()
		observation, err := integrations.NewStateObservation(string(devices[index].endpoint), quality, &value, &at, at, nil)
		if err != nil {
			return err
		}
		causality, err := last.Causality()
		if err != nil {
			return err
		}
		observation, err = observation.WithCausality(causality)
		if err != nil {
			return err
		}
		return source.Publish(ctx, binding, observation)
	}
	return errors.New("last fake command no longer has an active binding")
}
