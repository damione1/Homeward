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
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
	"google.golang.org/protobuf/proto"
)

const maxEntities = 1024

var ErrFixtureFull = errors.New("fake Host fixture is full")

type deviceKey struct {
	instance domain.IntegrationInstanceID
	source   string
}
type entityKey struct {
	instance domain.IntegrationInstanceID
	source   integrations.SourceKey
}

// Fixture supplies in-memory identities and delegates every accepted mutation
// to one sequencer. It has no parallel state or revision authority.
type Fixture struct {
	mu       sync.Mutex
	devices  map[deviceKey]domain.DeviceID
	entities map[entityKey]integrations.ResolvedIdentity
	sequence *sequencer.Sequencer
}

type RecordedEvent struct {
	Entity    domain.EntityID
	Event     integrations.DeviceEvent
	Source    domain.SourceStamp
	Instance  domain.IntegrationInstanceID
	Causality domain.Causality
}

func NewFixture() (*integrations.Manager, *Fixture, error) {
	return NewFixtureWithClock(sequencer.RealClock{})
}

func NewFixtureWithClock(clock sequencer.Clock) (*integrations.Manager, *Fixture, error) {
	return newFixture(clock, false)
}

// NewCommandFixture enables the in-memory registry/capability projection used
// by command tests. Legacy state fixtures keep their original three reports.
func NewCommandFixture() (*integrations.Manager, *Fixture, error) {
	return newFixture(sequencer.RealClock{}, true)
}

func newFixture(clock sequencer.Clock, commandProjection bool) (*integrations.Manager, *Fixture, error) {
	sequence, err := sequencer.New(clock, 16384)
	if err != nil {
		return nil, nil, err
	}
	fixture := &Fixture{
		devices: make(map[deviceKey]domain.DeviceID), entities: make(map[entityKey]integrations.ResolvedIdentity),
		sequence: sequence,
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		return nil, nil, err
	}
	var sink integrations.Sink = fixtureStateSink{fixture}
	if commandProjection {
		sink = fixture
	}
	host, err := integrations.NewHost(fixture, sink, catalog)
	return host, fixture, err
}

type fixtureStateSink struct{ fixture *Fixture }

func (s fixtureStateSink) AcceptState(ctx context.Context, entity domain.EntityID, state integrations.AcceptedState) error {
	return s.fixture.AcceptState(ctx, entity, state)
}
func (s fixtureStateSink) AcceptEvent(ctx context.Context, entity domain.EntityID, event integrations.AcceptedEvent) error {
	return s.fixture.AcceptEvent(ctx, entity, event)
}
func (s fixtureStateSink) AcceptHealth(ctx context.Context, instance domain.IntegrationInstanceID, health integrations.HealthReport) error {
	return s.fixture.AcceptHealth(ctx, instance, health)
}

func (f *Fixture) Sequencer() *sequencer.Sequencer {
	if f == nil {
		return nil
	}
	return f.sequence
}

func (f *Fixture) PublishRegistry(ctx context.Context, records []integrations.RegistryRecord) error {
	if f == nil || f.sequence == nil {
		return integrations.ErrInvalidInput
	}
	return f.sequence.PublishRegistry(ctx, records)
}

func (f *Fixture) AcceptDiscovery(ctx context.Context, declarations []integrations.DiscoveryDeclaration) error {
	if f == nil || f.sequence == nil {
		return integrations.ErrInvalidInput
	}
	return f.sequence.AcceptDiscovery(ctx, declarations)
}

func (f *Fixture) InvalidateRegistry() {
	if f != nil && f.sequence != nil {
		f.sequence.InvalidateRegistry()
	}
}

func (f *Fixture) ResolveCommandAlias(alias domain.HAAlias) (domain.EntityID, error) {
	if f == nil || f.sequence == nil {
		return domain.EntityID{}, integrations.ErrInvalidInput
	}
	return f.sequence.ResolveCommandAlias(alias)
}

func (f *Fixture) AdmitCommandRoute(route integrations.CommandRoute) error {
	if f == nil || f.sequence == nil {
		return integrations.ErrInvalidInput
	}
	return f.sequence.AdmitCommandRoute(route)
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

// DeclareSharedDevice supplies an explicit physical identity in a fixture.
// Tests use it for two distinct provider bindings; names imply no equivalence.
func (f *Fixture) DeclareSharedDevice(instance domain.IntegrationInstanceID, sourceDevice string, device domain.DeviceID) error {
	if f == nil || f.devices == nil || sourceDevice == "" {
		return integrations.ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return err
	}
	if _, err := device.Value(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := deviceKey{instance, sourceDevice}
	if _, exists := f.devices[key]; exists {
		return integrations.ErrIdentityConflict
	}
	f.devices[key] = device
	return nil
}

func (f *Fixture) AcceptState(ctx context.Context, entity domain.EntityID, state integrations.AcceptedState) error {
	if f == nil || f.sequence == nil {
		return integrations.ErrInvalidInput
	}
	return f.sequence.AcceptState(ctx, entity, state)
}

func (f *Fixture) AcceptEvent(ctx context.Context, entity domain.EntityID, event integrations.AcceptedEvent) error {
	if f == nil || f.sequence == nil {
		return integrations.ErrInvalidInput
	}
	return f.sequence.AcceptEvent(ctx, entity, event)
}

func (f *Fixture) AcceptHealth(ctx context.Context, instance domain.IntegrationInstanceID, report integrations.HealthReport) error {
	if f == nil || f.sequence == nil {
		return integrations.ErrInvalidInput
	}
	return f.sequence.AcceptHealth(ctx, instance, report)
}

func (f *Fixture) State(entity domain.EntityID) (domain.ObservedState, bool) {
	if f == nil || f.sequence == nil {
		return domain.ObservedState{}, false
	}
	state, ok, _ := f.sequence.State(entity)
	return state, ok
}

func (f *Fixture) Events() ([]RecordedEvent, error) {
	if f == nil || f.sequence == nil {
		return nil, integrations.ErrInvalidInput
	}
	start, err := f.sequence.Start()
	if err != nil {
		return nil, err
	}
	entries, _, err := f.sequence.ReadAfter(start, 16384)
	if err != nil {
		return nil, err
	}
	events := make([]RecordedEvent, 0)
	for _, entry := range entries {
		entity, event, ok := entry.Event()
		if !ok {
			continue
		}
		instance, source, causality, present := entry.Origin()
		if !present {
			return nil, integrations.ErrInvalidInput
		}
		events = append(events, RecordedEvent{Entity: entity, Event: event, Source: source, Instance: instance, Causality: causality})
	}
	return events, nil
}

func (f *Fixture) Health(instance domain.IntegrationInstanceID) (integrations.HealthReport, bool) {
	if f == nil || f.sequence == nil {
		return integrations.HealthReport{}, false
	}
	snapshot, err := f.sequence.Capture()
	if err != nil {
		return integrations.HealthReport{}, false
	}
	report, ok := snapshot.Health[instance]
	return report, ok
}

type Driver struct {
	mu         sync.Mutex
	at         time.Time
	binding    integrations.Binding
	commands   []*actions.CommandRequest
	actionable bool
}

func NewDriver(at time.Time) (*Driver, error) {
	if at.IsZero() {
		return nil, integrations.ErrInvalidInput
	}
	return &Driver{at: at.UTC()}, nil
}

func NewActionDriver(at time.Time) (*Driver, error) {
	driver, err := NewDriver(at)
	if err != nil {
		return nil, err
	}
	driver.actionable = true
	return driver, nil
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
		Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: d.actionable}},
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

// Dispatch records a command in memory. It has no device, network, automation,
// notification, or retained-message output.
func (d *Driver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if d == nil || ctx == nil {
		return integrations.ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, _, entity, err := command.Identity()
	if err != nil {
		return err
	}
	_, bound, err := d.binding.IDs()
	if err != nil || bound != entity {
		return integrations.ErrForeignBinding
	}
	request, err := command.Request()
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.commands = append(d.commands, request)
	d.mu.Unlock()
	return nil
}

func (d *Driver) Commands() []*actions.CommandRequest {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*actions.CommandRequest, len(d.commands))
	for i, request := range d.commands {
		out[i] = proto.Clone(request).(*actions.CommandRequest)
	}
	return out
}
