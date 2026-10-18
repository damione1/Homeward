package light

import (
	"context"
	"math"

	"github.com/damione1/Homeward/compat/ha/ir"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/integrations"
)

// PrepareOnce never reads a new state or profile after the first attempt,
// including when that attempt was canceled or refused.
func (s *PreparationSlot) PrepareOnce(ctx context.Context) (PreparedAction, error) {
	if s == nil {
		return PreparedAction{}, ErrInvalidSlot
	}
	s.once.Do(func() { s.result, s.err = s.prepare(ctx) })
	return s.result, s.err
}

func (s *PreparationSlot) prepare(ctx context.Context) (PreparedAction, error) {
	if ctx == nil {
		return PreparedAction{}, ErrInvalidSlot
	}
	capture, err := s.service.CaptureLight(ctx, s.alias)
	if err != nil {
		return PreparedAction{}, err
	}
	var brightness *uint8
	var source ir.Source
	var relative bool
	on := s.call.Kind() == ir.CallLightTurnOn
	if on {
		switch s.call.Brightness().Kind() {
		case ir.BrightnessUnchanged:
		case ir.BrightnessAbsolutePercent:
			number := s.call.BrightnessPercent()
			if !number.Present() {
				return PreparedAction{}, ErrUnsupportedAction
			}
			value := number.Value()
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
				return PreparedAction{}, ErrUnsupportedAction
			}
			converted := uint8(math.RoundToEven(255 * value / 100))
			brightness, source = &converted, number.Source()
		case ir.BrightnessRelativePercent:
			number := s.call.BrightnessStepPercent()
			if !number.Present() {
				return PreparedAction{}, ErrUnsupportedAction
			}
			step := number.Value()
			if math.IsNaN(step) || math.IsInf(step, 0) || step < -100 || step > 100 {
				return PreparedAction{}, ErrUnsupportedAction
			}
			observed, _, err := capture.ObservedHABrightness()
			if err != nil {
				return PreparedAction{}, err
			}
			// HA rounds the observed 0..255 brightness to a percent first,
			// applies the step, then rounds back to 0..255 independently.
			percent := math.RoundToEven(float64(observed) * 100 / 255)
			converted := uint8(max(0, min(255, math.RoundToEven((percent+step)*255/100))))
			brightness, source, relative = &converted, number.Source(), true
		default:
			return PreparedAction{}, ErrUnsupportedAction
		}
	}
	var transition *float64
	if number := s.call.TransitionSeconds(); number.Present() {
		seconds := number.Value()
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > 6553 {
			return PreparedAction{}, ErrUnsupportedAction
		}
		transition = &seconds
	}
	command, err := capture.PrepareHA(s.actor, s.id, s.expires, on, brightness, transition, relative)
	if err != nil {
		return PreparedAction{}, err
	}
	return PreparedAction{command: command, source: source, kind: s.call.Brightness().Kind()}, nil
}

// Dispatch reuses the exact cached command. A failed first preparation stays
// failed; callers cannot supply only an ID and reconstruct a relative step.
func (s *PreparationSlot) Dispatch(ctx context.Context) (*actions.CommandResponse, error) {
	prepared, err := s.PrepareOnce(ctx)
	if err != nil {
		return nil, err
	}
	return s.service.DispatchPrepared(ctx, prepared.command)
}

var _ Service = (*integrations.VolatileCommandService)(nil)
var _ Service = (*integrations.DurableCommandService)(nil)
