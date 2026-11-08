package wled

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

func presetKey(n int) string                               { return strconv.Itoa(n) }
func (p *pilot) CommandDriver() integrations.CommandDriver { return p }
func payload(r route, d document, a *actions.SetLight) ([]byte, error) {
	if d.Info == nil || d.Info.Live || a == nil || a.ColorTempMireds != nil || a.TransitionSeconds != nil {
		return nil, ErrUnavailable
	}
	fields := map[string]any{}
	if a.On != nil {
		fields["on"] = *a.On
	}
	if a.BrightnessPercent != nil {
		v := *a.BrightnessPercent
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
			return nil, ErrUnavailable
		}
		q := uint32(math.Round(v * 255 / 100))
		if q == 0 && a.On != nil && *a.On {
			return nil, ErrUnavailable
		}
		fields["bri"] = q
	}
	if a.Rgb != nil {
		if r.input.Segment == nil || !r.description.GetLight().Rgb || a.Rgb.Red == nil || a.Rgb.Green == nil || a.Rgb.Blue == nil || *a.Rgb.Red > 255 || *a.Rgb.Green > 255 || *a.Rgb.Blue > 255 {
			return nil, ErrUnavailable
		}
		fields["col"] = [][]uint32{{*a.Rgb.Red, *a.Rgb.Green, *a.Rgb.Blue, 0}}
	}
	if a.Preset != nil {
		if r.input.Segment != nil || !containsPreset(r.description.GetLight().Presets, *a.Preset) || len(fields) != 0 {
			return nil, ErrUnavailable
		}
		n, _ := strconv.Atoi(*a.Preset)
		fields["ps"] = n
	}
	if len(fields) == 0 {
		return nil, ErrUnavailable
	}
	if r.input.Segment != nil {
		if _, ok := d.find(*r.input.Segment); !ok {
			return nil, ErrUnavailable
		}
		fields["id"] = *r.input.Segment
		fields = map[string]any{"seg": []any{fields}}
	}
	return json.Marshal(fields)
}
func (p *pilot) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	_, id, e := command.RouteIdentity()
	if e != nil {
		return e
	}
	request, e := command.Request()
	if e != nil || request.CapabilitySchemaId != contracts.LightSchema || request.Endpoint != "light" || request.ExpiresAt == nil || request.ExpiresAt.CheckValid() != nil {
		return ErrUnavailable
	}
	p.mu.Lock()
	parent := p.ctx
	controllers := append([]*controller(nil), p.controllers...)
	p.mu.Unlock()
	if parent == nil {
		return ErrUnavailable
	}
	budget, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(parent, cancel)
	defer stop()
	current := func() bool {
		return budget.Err() == nil && time.Now().Before(request.ExpiresAt.AsTime()) && command.ReadyForTransport()
	}
	for _, c := range controllers {
		if !lock(budget, c) {
			return ErrUnavailable
		}
		r, ok := c.routes[id]
		if !ok {
			unlock(c)
			continue
		}
		defer unlock(c)
		if !current() || c.last.IsZero() || time.Since(c.last) > 30*time.Second {
			return ErrUnavailable
		}
		before, e := p.client.read(budget, c.input.Address, c.input.MAC)
		if e != nil {
			p.unavailable(parent, c)
			return ErrUnavailable
		}
		// Catalog/capability changes fence the admitted old route before any write.
		fresh := description(r.input, before, p.client.presets(budget, c.input.Address))
		if !proto.Equal(fresh, r.description) {
			_ = p.admit(budget, c, before)
			return ErrUnavailable
		}
		body, e := payload(r, before, request.GetArguments().GetSetLight())
		if e != nil || !current() {
			return ErrUnavailable
		}
		var ack struct {
			Success bool `json:"success"`
		}
		if e = p.client.request(budget, c.input.Address, "/json/state", body, &ack); e != nil || !ack.Success {
			p.unavailable(parent, c)
			return ErrUnavailable
		}
		if e = command.Acknowledge(); e != nil {
			return e
		}
		// ACK is not physical confirmation. Only subsequent identity-verified GETs
		// may confirm the exact quantized fields; timeout never retries the POST.
		for current() {
			after, e := p.client.read(budget, c.input.Address, c.input.MAC)
			if e != nil {
				p.unavailable(parent, c)
				return nil
			}
			if e = p.publish(budget, r, &after); e != nil {
				return e
			}
			if current() && matches(r, after, request.GetArguments().GetSetLight()) {
				return command.ConfirmProtocol(proof{r, after, proto.Clone(request.GetArguments().GetSetLight()).(*actions.SetLight)})
			}
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-budget.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
		return nil
	}
	return ErrUnavailable
}
func matches(r route, d document, a *actions.SetLight) bool {
	if d.Info == nil || d.Info.Live {
		return false
	}
	on, bri := *d.State.On, *d.State.Brightness
	nativeOn := on
	var col []uint32
	if r.input.Segment != nil {
		s, ok := d.find(*r.input.Segment)
		if !ok {
			return false
		}
		nativeOn = *s.On
		on = on && nativeOn
		bri = *s.Brightness
		if len(s.Colors) > 0 {
			col = s.Colors[0]
		}
	}
	// Effective power cannot prove a segment OFF write while its master is OFF.
	if a.On != nil && (*a.On != nativeOn || *a.On && !on) {
		return false
	}
	if a.BrightnessPercent != nil {
		q := uint32(math.Round(*a.BrightnessPercent * 255 / 100))
		// WLED v0.15.0 json.cpp deserializeState/deserializeSegment and
		// serializeState/serializeSegment: zero switches off, preserving briLast
		// or segment opacity. The readback must prove native OFF, not bri==0.
		if q == 0 {
			if nativeOn {
				return false
			}
		} else if q != bri || a.On == nil && !nativeOn {
			return false
		}
	}
	if a.Rgb != nil && (len(col) < 3 || col[0] != a.Rgb.GetRed() || col[1] != a.Rgb.GetGreen() || col[2] != a.Rgb.GetBlue()) {
		return false
	}
	if a.Preset != nil && (d.State.Preset == nil || presetKey(*d.State.Preset) != *a.Preset) {
		return false
	}
	return true
}

type proof struct {
	route    route
	document document
	action   *actions.SetLight
}

func (p proof) QualifyProtocolConfirmation(command integrations.AdmittedCommand) error {
	request, e := command.Request()
	if e != nil || !proto.Equal(request.GetArguments().GetSetLight(), p.action) || !matches(p.route, p.document, p.action) {
		return ErrUnavailable
	}
	return nil
}
