// Package fake supplies a deterministic, device-free driver and an in-memory
// Host fixture. It has no network, SQL, automation, or action output.
package fake

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

const maxEntities = 1024
const maxEvents = 256

var ErrFixtureFull = errors.New("fake Host fixture is full")

type deviceKey struct {
	instance domain.IntegrationInstanceID
	source   string
}
type entityKey struct {
	instance domain.IntegrationInstanceID
	source   integrations.SourceKey
}

// Fixture implements both injected Host seams for SDK tests only. Its local
// per-entity revision is not a global sequence or production state store.
type Fixture struct {
	mu       sync.Mutex
	devices  map[deviceKey]domain.DeviceID
	entities map[entityKey]integrations.ResolvedIdentity
	states   map[domain.EntityID]domain.ObservedState
	events   []RecordedEvent
	health   map[domain.IntegrationInstanceID]integrations.HealthReport
}

type RecordedEvent struct {
	Entity domain.EntityID
	Event  integrations.AcceptedEvent
}

func NewFixture() (*integrations.Manager, *Fixture, error) {
	fixture := &Fixture{
		devices: make(map[deviceKey]domain.DeviceID), entities: make(map[entityKey]integrations.ResolvedIdentity),
		states: make(map[domain.EntityID]domain.ObservedState), health: make(map[domain.IntegrationInstanceID]integrations.HealthReport),
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		return nil, nil, err
	}
	host, err := integrations.NewHost(fixture, fixture, catalog)
	return host, fixture, err
}

func (f *Fixture) Resolve(ctx context.Context, instance domain.IntegrationInstanceID, keys []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	if f == nil || ctx == nil || len(keys) == 0 || len(keys) > maxEntities {
		return nil, integrations.ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := make(map[integrations.SourceKey]bool, len(keys))
	for _, key := range keys {
		if _, _, err := key.Components(); err != nil {
			return nil, err
		}
		if seen[key] {
			return nil, integrations.ErrInvalidInput
		}
		seen[key] = true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.devices == nil || f.entities == nil {
		return nil, integrations.ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	newEntities := 0
	for _, key := range keys {
		if _, exists := f.entities[entityKey{instance, key}]; !exists {
			newEntities++
		}
	}
	if len(f.entities)+newEntities > maxEntities {
		return nil, ErrFixtureFull
	}
	out := make([]integrations.ResolvedIdentity, len(keys))
	for i, key := range keys {
		ek := entityKey{instance, key}
		if existing, ok := f.entities[ek]; ok {
			out[i] = existing
			continue
		}
		deviceSource, _, err := key.Components()
		if err != nil {
			return nil, err
		}
		dk := deviceKey{instance, deviceSource}
		device, ok := f.devices[dk]
		if !ok {
			device, err = domain.ParseDeviceID(fmt.Sprintf("fixture-device-%d", len(f.devices)+1))
			if err != nil {
				return nil, err
			}
			f.devices[dk] = device
		}
		entity, err := domain.ParseEntityID(fmt.Sprintf("Host:fixture-%d", len(f.entities)+1))
		if err != nil {
			return nil, err
		}
		out[i] = integrations.ResolvedIdentity{Device: device, Entity: entity}
		f.entities[ek] = out[i]
	}
	return out, nil
}

func (f *Fixture) AcceptState(ctx context.Context, entity domain.EntityID, state integrations.AcceptedState) error {
	if f == nil || ctx == nil {
		return integrations.ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		return integrations.ErrInvalidInput
	}
	previous, exists := f.states[entity]
	var prev *domain.ObservedState
	var revision uint64 = 1
	if exists {
		prev = &previous
		_, _, _, _, current, err := previous.Components()
		if err != nil {
			return err
		}
		revision = current + 1
	}
	report, err := state.Report(revision)
	if err != nil {
		return err
	}
	next, _, err := domain.ApplyStateReport(entity, prev, report)
	if err != nil {
		return err
	}
	f.states[entity] = next
	return nil
}

func (f *Fixture) AcceptEvent(ctx context.Context, entity domain.EntityID, event integrations.AcceptedEvent) error {
	if f == nil || ctx == nil {
		return integrations.ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return err
	}
	if _, _, _, err := event.Event.Components(); err != nil {
		return err
	}
	if _, _, err := event.Source.Components(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entities == nil {
		return integrations.ErrInvalidInput
	}
	if len(f.events) >= maxEvents {
		return ErrFixtureFull
	}
	f.events = append(f.events, RecordedEvent{Entity: entity, Event: event})
	return nil
}

func (f *Fixture) AcceptHealth(ctx context.Context, instance domain.IntegrationInstanceID, report integrations.HealthReport) error {
	if f == nil || ctx == nil {
		return integrations.ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return err
	}
	if _, _, err := report.Components(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.health == nil {
		return integrations.ErrInvalidInput
	}
	f.health[instance] = report
	return nil
}

func (f *Fixture) State(entity domain.EntityID) (domain.ObservedState, bool) {
	if f == nil {
		return domain.ObservedState{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.states[entity]
	return state, ok
}

func (f *Fixture) Events() []RecordedEvent {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RecordedEvent(nil), f.events...)
}

func (f *Fixture) Health(instance domain.IntegrationInstanceID) (integrations.HealthReport, bool) {
	if f == nil {
		return integrations.HealthReport{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	report, ok := f.health[instance]
	return report, ok
}

type Driver struct {
	at      time.Time
	binding integrations.Binding
}

func NewDriver(at time.Time) (*Driver, error) {
	if at.IsZero() {
		return nil, integrations.ErrInvalidInput
	}
	return &Driver{at: at.UTC()}, nil
}

// Start performs one synchronous fixture lifecycle. It never opens a device or
// dispatches an action, including on repeated starts.
func (d *Driver) Start(ctx context.Context, host integrations.SessionHost) error {
	if d == nil || host == nil || (reflect.ValueOf(host).Kind() == reflect.Pointer && reflect.ValueOf(host).IsNil()) {
		return integrations.ErrInvalidInput
	}
	key, err := integrations.NewSourceKey("fixture-controller", "relay")
	if err != nil {
		return err
	}
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{{
		SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Fake relay", Endpoint: "relay",
		Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: false}},
	}})
	if err != nil {
		return err
	}
	bindings, err := host.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil {
		return err
	}
	if len(bindings) != 1 {
		return integrations.ErrIdentityConflict
	}
	d.binding = bindings[0]
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return err
	}
	value := domain.NewSwitchValue(false)
	observation, err := integrations.NewStateObservation("relay", quality, &value, &d.at, d.at, nil)
	if err != nil {
		return err
	}
	if err := host.Publish(ctx, d.binding, observation); err != nil {
		return err
	}
	event, err := integrations.NewDeviceEvent("fixture.relay.observed", d.at, nil)
	if err != nil {
		return err
	}
	if err := host.Emit(ctx, d.binding, event); err != nil {
		return err
	}
	health, err := integrations.NewHealthReport(integrations.HealthReady, "")
	if err != nil {
		return err
	}
	return host.ReportHealth(ctx, health)
}

func (d *Driver) Stop(ctx context.Context) error {
	if d == nil || ctx == nil {
		return integrations.ErrInvalidInput
	}
	return ctx.Err()
}

func (d *Driver) Binding() (integrations.Binding, error) {
	if d == nil {
		return integrations.Binding{}, integrations.ErrInvalidInput
	}
	if _, _, err := d.binding.IDs(); err != nil {
		return integrations.Binding{}, err
	}
	return d.binding, nil
}
