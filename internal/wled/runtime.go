package wled

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

type route struct {
	input       RouteInput
	binding     integrations.Binding
	description *registry.CapabilityDescription
}
type controller struct {
	input   ControllerInput
	gate    chan struct{}
	routes  map[domain.EntityID]route
	last    time.Time
	healthy atomic.Bool
}
type pilot struct {
	settings    Settings
	client      *client
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	host        integrations.SessionHost
	controllers []*controller
}

func (p *pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	p.mu.Lock()
	if p.ctx != nil {
		p.mu.Unlock()
		return ErrSettings
	}
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.host = host
	for _, input := range p.settings.Controllers() {
		p.controllers = append(p.controllers, &controller{input: input, gate: make(chan struct{}, 1), routes: map[domain.EntityID]route{}})
	}
	p.mu.Unlock()
	for _, c := range p.controllers {
		if e := tasks.Go(func(task context.Context) error {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				p.refresh(c)
				select {
				case <-task.Done():
					return nil
				case <-p.ctx.Done():
					return nil
				case <-ticker.C:
				}
			}
		}); e != nil {
			return e
		}
		if e := tasks.Go(func(task context.Context) error { return p.listen(task, c) }); e != nil {
			return e
		}
	}
	return nil
}
func (p *pilot) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
	p.client.http.CloseIdleConnections()
	return nil
}
func lock(ctx context.Context, c *controller) bool {
	select {
	case c.gate <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}
func unlock(c *controller) { <-c.gate }
func (p *pilot) refresh(c *controller) {
	ctx, cancel := context.WithTimeout(p.ctx, 12*time.Second)
	defer cancel()
	if !lock(ctx, c) {
		return
	}
	defer unlock(c)
	d, e := p.client.read(ctx, c.input.Address, c.input.MAC)
	if e != nil {
		p.unavailable(p.ctx, c)
		return
	}
	if e := p.admit(ctx, c, d); e != nil {
		p.unavailable(p.ctx, c)
	}
}
func (p *pilot) admit(ctx context.Context, c *controller, d document) error {
	presets := p.client.presets(ctx, c.input.Address)
	var candidates []integrations.Candidate
	var inputs []RouteInput
	var descriptions []*registry.CapabilityDescription
	for _, input := range c.input.Routes {
		if input.Segment != nil {
			if _, ok := d.find(*input.Segment); !ok {
				continue
			}
		}
		desc := description(input, d, presets)
		candidate, e := integrations.NewCandidate(input.SourceKey(c.input.MAC), []*registry.CapabilityDescription{desc})
		if e != nil {
			return e
		}
		candidates = append(candidates, candidate)
		inputs = append(inputs, input)
		descriptions = append(descriptions, desc)
	}
	// Publish unavailable before withdrawing a disappeared segment. The durable
	// configured identity remains; reconnect never dispatches an action.
	for _, r := range c.routes {
		if r.input.Segment != nil {
			if _, ok := d.find(*r.input.Segment); !ok {
				_ = p.publish(ctx, r, nil)
				if h, ok := p.host.(integrations.BindingWithdrawal); ok {
					_ = h.Withdraw(ctx, r.binding)
				}
			}
		}
	}
	bindings, e := p.host.Discover(ctx, candidates)
	if e != nil || len(bindings) != len(inputs) {
		return ErrUnavailable
	}
	next := map[domain.EntityID]route{}
	instance, _ := p.settings.Instance()
	for i, b := range bindings {
		owner, key, e := b.Source()
		_, id, e2 := b.IDs()
		if e != nil || e2 != nil || owner != instance || key != inputs[i].SourceKey(c.input.MAC) {
			return ErrUnavailable
		}
		r := route{inputs[i], b, descriptions[i]}
		next[id] = r
		if e = p.publish(ctx, r, &d); e != nil {
			return e
		}
	}
	c.routes = next
	c.last = time.Now()
	c.healthy.Store(len(next) == len(c.input.Routes))
	return p.reportHealth(ctx)
}
func (p *pilot) unavailable(ctx context.Context, c *controller) {
	c.last = time.Time{}
	c.healthy.Store(false)
	for _, r := range c.routes {
		_ = p.publish(ctx, r, nil)
	}
	_ = p.reportHealth(ctx)
}
func (p *pilot) reportHealth(ctx context.Context) error {
	status, detail := integrations.HealthReady, "wled native observation"
	for _, controller := range p.controllers {
		if !controller.healthy.Load() {
			status, detail = integrations.HealthDegraded, "wled native observation unavailable"
			break
		}
	}
	report, _ := integrations.NewHealthReport(status, detail)
	return p.host.ReportHealth(ctx, report)
}
func (p *pilot) publish(ctx context.Context, r route, d *document) error {
	availability, freshness := domain.AvailabilityUnavailable, domain.FreshnessStale
	var value *domain.StateValue
	var at *time.Time
	now := time.Now()
	if d != nil {
		on, bri := *d.State.On, *d.State.Brightness
		input := domain.LightValueInput{}
		if r.input.Segment != nil {
			s, ok := d.find(*r.input.Segment)
			if !ok {
				return ErrUnavailable
			}
			on = on && *s.On
			bri = *s.Brightness
			if r.description.GetLight().Rgb && len(s.Colors) > 0 && len(s.Colors[0]) >= 3 {
				col := s.Colors[0]
				if col[0] > 255 || col[1] > 255 || col[2] > 255 {
					return ErrUnavailable
				}
				input.RGB = &domain.RGB{Red: uint8(col[0]), Green: uint8(col[1]), Blue: uint8(col[2])}
			}
		} else if d.State.Preset != nil {
			key := presetKey(*d.State.Preset)
			if containsPreset(r.description.GetLight().Presets, key) {
				input.Preset = &key
			}
		}
		brightness := float64(bri) * 100 / 255
		input.On = on
		input.Brightness = &brightness
		light, e := domain.NewLightValue(input)
		if e != nil {
			return e
		}
		v, e := domain.NewLightStateValue(light)
		if e != nil {
			return e
		}
		value = &v
		at = &now
		availability, freshness = domain.AvailabilityAvailable, domain.FreshnessFresh
	}
	q, _ := domain.NewQuality(availability, freshness, domain.ProvenanceObserved)
	o, e := integrations.NewStateObservation("light", q, value, at, now, nil)
	if e != nil {
		return e
	}
	return p.host.Publish(ctx, r.binding, o)
}

// WS messages trigger serialized fresh HTTP reconciliation. We never apply a
// buffered pre-command frame after a newer command readback, and never send a
// JSON mutation, subscription toggle or replay on this connection.
func (p *pilot) listen(task context.Context, c *controller) error {
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	stop := context.AfterFunc(task, cancel)
	defer stop()
	for ctx.Err() == nil {
		dial, cancelDial := context.WithTimeout(ctx, 8*time.Second)
		ws, _, e := websocket.Dial(dial, "ws://"+c.input.Address+"/ws", &websocket.DialOptions{HTTPClient: p.client.http})
		cancelDial()
		if e == nil {
			ws.SetReadLimit(maxJSON)
			for ctx.Err() == nil {
				read, cancelRead := context.WithTimeout(ctx, 45*time.Second)
				kind, b, e := ws.Read(read)
				cancelRead()
				if e != nil {
					break
				}
				var d document
				if kind == websocket.MessageText && json.Unmarshal(b, &d) == nil && d.valid(c.input.MAC) {
					p.refresh(c)
				}
			}
			_ = ws.CloseNow()
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

var _ integrations.SupervisedCommandPilot = (*pilot)(nil)
