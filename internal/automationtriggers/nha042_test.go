package automationtriggers

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/mqtt"
)

func nha042ScriptWait(t *testing.T, triggerYAML string) ir.Script {
	t.Helper()
	entity, _ := ir.NewEntity("switch.fixture", ir.ReferencePresent, ir.SupportObserveState)
	calendar, _ := ir.NewEntity("calendar.fixture", ir.ReferencePresent)
	event, err := ir.NewEvent("nha042_release", ir.ReferencePresent)
	if err != nil {
		t.Fatal(err)
	}
	device, err := ir.NewDevice("nha042-device", ir.ReferencePresent)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity, calendar}, []ir.Device{device}, []ir.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := ir.ParseScriptID("script.nha042_wait")
	catalogue, err = catalogue.WithScripts([]ir.ScriptID{id})
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile("wait.yaml", []byte("sequence:\n- wait_for_trigger:\n"+triggerYAML+"\n- stop: done\n"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ir.CompileScriptSources("script.nha042_wait", "wait.yaml", []parser.SourceFile{file}, catalogue)
	if err != nil || len(result.Accepted()) != 1 {
		t.Fatalf("script compilation: %v %+v", err, result.Rejected())
	}
	return result.Accepted()[0]
}

func TestNHA042ConcurrentStateWaitsUseOneServiceAndNoResultReader(t *testing.T) {
	ctx := context.Background()
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	host, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("fixture")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	binding, _ := driver.Binding()
	_, entity, _ := binding.IDs()
	program := nha042ScriptWait(t, "  - trigger: state\n    entity_id: switch.fixture\n    to: 'on'")
	plan, err := NewPlan(PlanInput{Scripts: []ir.Script{program}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.AutomationIDs()) != 0 || plan.Count() != 0 {
		t.Fatal("real script became fake automation")
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	source := program.Waits()[0].Source()
	first, err := service.ObserveWait(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ObserveWait(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	subscription := service.sub
	service.mu.Unlock()
	<-first.Close()
	<-first.Close()
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value, at := domain.NewSwitchValue(true), clock.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(ctx, binding, observation); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second.Ready():
	case <-time.After(time.Second):
		t.Fatal("second wait lost its shared subscription")
	}
	result, err := second.Outcome()
	if err != nil || result.Kind() != ResultFired || result.Automation() != "" {
		t.Fatalf("wait result: %+v %v", result, err)
	}
	<-second.Close()
	service.mu.Lock()
	sameSubscription := service.sub == subscription
	leaked := len(service.waits) + len(service.waitOwners) + len(service.pending) + service.resultBytes
	service.mu.Unlock()
	if !sameSubscription || leaked != 0 {
		t.Fatalf("shared service replaced or leaked wait resources: %v %d", sameSubscription, leaked)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if result, err := service.NextResult(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait stole/emitted permanent result: %+v %v", result, err)
	}
}

func TestNHA042WaitOnlySunSourceIsCancelledThenJoinedByServiceClose(t *testing.T) {
	ctx := context.Background()
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	program := nha042ScriptWait(t, "  - trigger: sun\n    event: sunset")
	source := &nha040ExitGateSun{entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	position, _ := NewSolarPosition(45, -73)
	plan, err := NewPlan(PlanInput{Scripts: []ir.Script{program}, Zone: "America/Toronto", Resolver: nha040Resolver{}, SunSource: source, SolarPosition: position})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	armed := make(chan error, 1)
	go func() { _, err := service.ObserveWait(ctx, program.Waits()[0].Source()); armed <- err }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("existing SunSource was not called")
	}
	done := service.BeginClose()
	select {
	case <-source.cancelled:
	case <-time.After(time.Second):
		t.Fatal("source was not cancelled")
	}
	select {
	case <-done:
		t.Fatal("service Done preceded actual source join")
	default:
	}
	close(source.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("service failed to join source")
	}
	select {
	case <-source.exited:
	default:
		t.Fatal("source did not actually exit")
	}
	if err := <-armed; !errors.Is(err, ErrServiceClosed) && !errors.Is(err, context.Canceled) {
		t.Fatalf("closed wait admitted: %v", err)
	}
}

func TestNHA042CalendarWaitRefusesMissingOperationalWindow(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	program := nha042ScriptWait(t, "  - trigger: calendar\n    entity_id: calendar.fixture\n    event: start")
	plan, err := NewPlan(PlanInput{Scripts: []ir.Script{program}, Zone: "America/Toronto", Resolver: nha040Resolver{}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if handle, err := service.ObserveWait(context.Background(), program.Waits()[0].Source()); handle != nil || !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("unbacked calendar admitted: %v %v", handle, err)
	}
}

func nha042EventWaitService(t *testing.T) (*Service, ir.Script, integrations.SessionHost, integrations.Binding, *nha040Clock) {
	t.Helper()
	ctx := context.Background()
	clock := &nha040Clock{wall: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}
	host, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha042-events")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	program := nha042ScriptWait(t, "  - trigger: event\n    event_type: nha042_release")
	plan, err := NewPlan(PlanInput{Scripts: []ir.Script{program}, Zone: "UTC", Resolver: nha040Resolver{}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return service, program, session, binding, clock
}
func TestNHA042WaitRegistrationBarrierRejectsPastEventsAndRearm(t *testing.T) {
	service, program, session, binding, clock := nha042EventWaitService(t)
	emit := func() {
		t.Helper()
		event, err := integrations.NewDeviceEvent("nha042_release", clock.Now(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Emit(context.Background(), binding, event); err != nil {
			t.Fatal(err)
		}
	}
	emit() // admitted before registration and flushed without an armed rule
	first, err := service.ObserveWait(context.Background(), program.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.Ready():
		t.Fatal("past event completed newly registered wait")
	default:
	}
	emit()
	barrier, err := service.ObserveWait(context.Background(), program.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	<-barrier.Close()
	select {
	case <-first.Ready():
	case <-time.After(time.Second):
		t.Fatal("armed event was lost")
	}
	metadata, present := first.Trigger()
	if !present || metadata.ID() != "0" || metadata.Index() != 0 || metadata.Kind() != ir.TriggerEvent {
		t.Fatalf("admitted trigger metadata lost: %+v %v", metadata, present)
	}
	if _, present := first.CompletedAt(); !present {
		t.Fatal("completion lacks shared monotonic stamp")
	}
	<-first.Close()
	next, err := service.ObserveWait(context.Background(), program.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-next.Ready():
		t.Fatal("new generation received prior event")
	default:
	}
	emit()
	select {
	case <-next.Ready():
	case <-time.After(time.Second):
		t.Fatal("rearmed wait missed new event")
	}
	if _, err := next.Outcome(); err != nil {
		t.Fatal(err)
	}
	<-next.Close()
	service.mu.Lock()
	live, owners, bytes := len(service.waits), len(service.waitOwners), service.resultBytes
	service.mu.Unlock()
	if live != 0 || owners != 0 || bytes != 0 {
		t.Fatalf("disarmed generations leaked: %d/%d/%d", live, owners, bytes)
	}
}
func TestNHA042WaitRuleAndHandleCapacityIsShared(t *testing.T) {
	service, program, _, _, _ := nha042EventWaitService(t)
	handles := make([]*WaitHandle, 0, MaxWaits)
	for index := 0; index < MaxWaits; index++ {
		handle, err := service.ObserveWait(context.Background(), program.Waits()[0].Source())
		if err != nil {
			t.Fatalf("wait admission %d: %v", index, err)
		}
		handles = append(handles, handle)
	}
	if handle, err := service.ObserveWait(context.Background(), program.Waits()[0].Source()); handle != nil || !errors.Is(err, ErrCapacity) {
		t.Fatalf("wait bound exceeded: %v %v", handle, err)
	}
	// Every handle removes its own slots; sibling waits remain registered.
	<-handles[0].Close()
	replacement, err := service.ObserveWait(context.Background(), program.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	for _, handle := range handles[1:] {
		<-handle.Close()
	}
	<-replacement.Close()
	service.mu.Lock()
	live, slots := len(service.waits), len(service.waitOwners)
	service.mu.Unlock()
	if live != 0 || slots != 0 {
		t.Fatalf("capacity not reclaimed: %d/%d", live, slots)
	}
}

// The source observes cancellation but deliberately joins later with a valid
// result. Registration must retain ownership and discard that late success.
type nha042RegistrationSun struct {
	entered, cancelled, release, exited chan struct{}
}

func (s *nha042RegistrationSun) NextSunset(ctx context.Context, after time.Time, _ string, _ SolarPosition) (SunResult, error) {
	close(s.entered)
	<-ctx.Done()
	close(s.cancelled)
	<-s.release
	defer close(s.exited)
	return SunEventAt(after.Add(time.Hour))
}

func TestNHA042CancelWhileRegisteringWaitJoinsBeforeDone(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller-cancel", true: "caller-cancel-and-service-close"}[shutdown], func(t *testing.T) {
			clock := &nha040Clock{wall: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
			_, fixture, err := fake.NewFixtureWithClock(clock)
			if err != nil {
				t.Fatal(err)
			}
			program := nha042ScriptWait(t, "  - trigger: sun\n    event: sunset")
			source := &nha042RegistrationSun{make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})}
			position, err := NewSolarPosition(45, -73)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := NewPlan(PlanInput{Scripts: []ir.Script{program}, Zone: "UTC", Resolver: nha040Resolver{}, SunSource: source, SolarPosition: position})
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewService(plan, fixture.Sequencer(), clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			var release sync.Once
			unblock := func() { release.Do(func() { close(source.release) }) }
			t.Cleanup(func() {
				cancel()
				unblock()
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			type registration struct {
				handle *WaitHandle
				err    error
			}
			result := make(chan registration, 1)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				handle, err := service.ObserveWait(ctx, program.Waits()[0].Source())
				result <- registration{handle, err}
			}()
			t.Cleanup(func() {
				cancel()
				unblock()
				select {
				case <-joined:
				case <-time.After(3 * time.Second):
					t.Error("registration goroutine did not join")
				}
			})
			select {
			case <-source.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("source registration not entered")
			}
			service.mu.Lock()
			var owned *WaitHandle
			for _, handle := range service.waits {
				owned = handle
			}
			subscription := service.sub
			service.mu.Unlock()
			if owned == nil {
				t.Fatal("source call escaped wait ownership")
			}
			cancel()
			select {
			case <-source.cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("parent cancellation did not reach source")
			}
			select {
			case <-owned.Done():
				t.Fatal("wait Done preceded late source exit")
			default:
			}
			select {
			case <-result:
				t.Fatal("registration returned before its source joined")
			default:
			}
			var serviceDone <-chan struct{}
			if shutdown {
				serviceDone = service.BeginClose()
				select {
				case <-serviceDone:
					t.Fatal("service Done preceded source join")
				default:
				}
			}
			unblock()
			select {
			case <-source.exited:
			case <-time.After(3 * time.Second):
				t.Fatal("late source did not actually exit")
			}
			select {
			case <-owned.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("wait did not join cancelled registration")
			}
			var outcome registration
			select {
			case outcome = <-result:
			case <-time.After(3 * time.Second):
				t.Fatal("registration did not return after joining")
			}
			if outcome.handle != nil || !errors.Is(outcome.err, context.Canceled) {
				t.Fatalf("late valid source admitted after cancellation: %v %v", outcome.handle, outcome.err)
			}
			if shutdown {
				select {
				case <-serviceDone:
				case <-time.After(3 * time.Second):
					t.Fatal("service shutdown did not join")
				}
			}
			service.mu.Lock()
			leaked := len(service.waits) + len(service.waitOwners) + len(service.scheduled) + len(service.scheduledMono) + service.resultBytes
			sameSubscription := service.sub == subscription
			service.mu.Unlock()
			if leaked != 0 || !shutdown && !sameSubscription {
				t.Fatalf("late result changed shared ownership: leaked=%d subscription=%v", leaked, sameSubscription)
			}
			if _, err := owned.Outcome(); !errors.Is(err, context.Canceled) {
				t.Fatalf("late success replaced cancellation: %v", err)
			}
		})
	}
}

// No discovery or physical destination is configured. The actual factory is
// nevertheless admitted for the same synthetic subscription/instance as040.
type nha042WaitRegistry struct{}

func (nha042WaitRegistry) MatchMQTTSource(context.Context, domain.IntegrationInstanceID, []string, string) (integrations.SourceKey, error) {
	return integrations.SourceKey{}, errors.New("no synthetic discovery mapping")
}
func (nha042WaitRegistry) MQTTTopicSources(context.Context, domain.IntegrationInstanceID, string) ([]integrations.SourceKey, error) {
	return nil, nil
}
func (nha042WaitRegistry) RememberMQTTTopics(context.Context, domain.IntegrationInstanceID, string, []integrations.SourceKey) error {
	return nil
}
func (nha042WaitRegistry) Tombstone(context.Context, domain.EntityID) error { return nil }

type nha042KindResolver struct {
	entity domain.EntityID
	device domain.DeviceID
}

func (r nha042KindResolver) ResolveEntity(domain.HAAlias) (domain.EntityID, error) {
	return r.entity, nil
}
func (r nha042KindResolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return r.device, nil
}
func (nha042KindResolver) ResolveCalendar(domain.HAAlias) (CalendarSourceID, error) {
	return ParseCalendarSourceID("fixture-calendar")
}

type nha042KindRig struct {
	service *Service
	program ir.Script
	session *integrations.Session
	binding integrations.Binding
	clock   *nha040Clock
	entity  domain.EntityID
}

func nha042TriggerRig(t *testing.T, yaml string) *nha042KindRig {
	t.Helper()
	return nha042TriggerRigAt(t, yaml, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), "UTC")
}
func nha042TriggerRigAt(t *testing.T, yaml string, wall time.Time, zone string) *nha042KindRig {
	t.Helper()
	ctx := context.Background()
	clock := &nha040Clock{wall: wall}
	_, fixture, err := fake.NewCommandFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, fixture.Sequencer(), catalogue)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha042-trigger-types")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.EnableNativeMQTTEdges(instance); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	device, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	alias, err := domain.ParseHAAlias("switch.fixture")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("fixture-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	settings, err := mqtt.ParseSettings(mqtt.Input{Profile: "5", Broker: "mqtt://127.0.0.1:1883", Instance: "nha042-trigger-types", Mode: "passive", UsernameSecret: "canary-user", PasswordSecret: "canary-password", Subscriptions: []string{"homeassistant/#", "nha042/wait"}})
	if err != nil {
		t.Fatal(err)
	}
	filter, err := mqtt.ParseTopicFilter("nha042/wait")
	if err != nil {
		t.Fatal(err)
	}
	ingress, err := mqtt.NewTriggerEdgePilot(settings, nha042WaitRegistry{}, []mqtt.TopicFilter{filter})
	if err != nil {
		t.Fatal(err)
	}
	program := nha042ScriptWait(t, yaml)
	numeric := program.Waits()[0].Triggers()[0].Kind() == ir.TriggerNumericState
	if numeric {
		candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{{SchemaId: contracts.MeasurementSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic wait measurement", Constraints: &registrywire.CapabilityDescription_Measurement{Measurement: &registrywire.MeasurementConstraints{Unit: "W", Semantic: registrywire.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registrywire.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}})
		if err != nil {
			t.Fatal(err)
		}
		bindings, err := session.Discover(ctx, []integrations.Candidate{candidate})
		if err != nil || len(bindings) != 1 {
			t.Fatalf("native measurement admission: %v %d", err, len(bindings))
		}
		binding = bindings[0]
		_, actualEntity, err := binding.IDs()
		if err != nil || actualEntity != entity {
			t.Fatalf("measurement source identity changed: %v %v", actualEntity, err)
		}
	}
	position, err := NewSolarPosition(45, -73)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(PlanInput{Scripts: []ir.Script{program}, Zone: zone, Resolver: nha042KindResolver{entity, device}, MQTTSettings: settings, MQTTIngress: ingress, SunSource: nha040Sun{clock.Now().Add(time.Second)}, SolarPosition: position})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	rig := &nha042KindRig{service, program, session, binding, clock, entity}
	if numeric {
		rig.publishMeasurement(t, 0)
	} else {
		rig.publishSwitch(t, false)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return rig
}
func (r *nha042KindRig) publishSwitch(t *testing.T, on bool) {
	t.Helper()
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	value, at := domain.NewSwitchValue(on), r.clock.Now()
	report, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.session.Publish(context.Background(), r.binding, report); err != nil {
		t.Fatal(err)
	}
}
func (r *nha042KindRig) native(t *testing.T, sequence uint64, payload []byte) {
	t.Helper()
	edge, err := integrations.NewNativeMQTTEdge("nha042/wait", payload, false, sequence)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.session.PublishNativeMQTTEdge(context.Background(), edge); err != nil {
		t.Fatal(err)
	}
}
func (r *nha042KindRig) publishMeasurement(t *testing.T, number float64) {
	t.Helper()
	unit, err := domain.NewUnitCatalog().Parse("W")
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := domain.NewMeasurement(number, unit, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	at := r.clock.Now()
	report, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.session.Publish(context.Background(), r.binding, report); err != nil {
		t.Fatal(err)
	}
}
func nha042WaitReady(t *testing.T, handle *WaitHandle) {
	t.Helper()
	select {
	case <-handle.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("transient trigger did not complete")
	}
}

func TestNHA042AllTriggerKindsShareExistingScheduler(t *testing.T) {
	for _, test := range []struct {
		name, yaml string
		kind       ir.TriggerKind
	}{
		{"state", "  - trigger: state\n    entity_id: switch.fixture\n    from: 'off'\n    to: 'on'", ir.TriggerState},
		{"numeric", "  - trigger: numeric_state\n    entity_id: switch.fixture\n    above: 0\n    below: 10", ir.TriggerNumericState},
		{"event", "  - trigger: event\n    event_type: nha042_release", ir.TriggerEvent},
		{"mqtt", "  - trigger: mqtt\n    topic: nha042/wait\n    payload: live", ir.TriggerMQTT},
		{"device", "  - trigger: device\n    domain: mqtt\n    device_id: nha042-device\n    type: action\n    subtype: single", ir.TriggerDeviceMQTT},
		{"time", "  - trigger: time\n    at: '00:00:01'", ir.TriggerTime},
		{"pattern", "  - trigger: time_pattern\n    seconds: '*'", ir.TriggerTimePattern},
		{"sun", "  - trigger: sun\n    event: sunset", ir.TriggerSunset},
		{"calendar", "  - trigger: calendar\n    entity_id: calendar.fixture\n    event: start", ir.TriggerCalendar},
	} {
		t.Run(test.name, func(t *testing.T) {
			rig := nha042TriggerRig(t, test.yaml+"\n    id: kind-"+test.name)
			if test.kind == ir.TriggerCalendar {
				source, err := ParseCalendarSourceID("fixture-calendar")
				if err != nil {
					t.Fatal(err)
				}
				start := rig.clock.Now().Add(time.Second)
				item, err := NewCalendarOccurrence(source, "nha042", "instance", start, start.Add(time.Minute), "UTC", false, domain.FreshnessFresh)
				if err != nil {
					t.Fatal(err)
				}
				window, err := NewCalendarWindow(source, 1, rig.clock.Now().Add(-time.Minute), rig.clock.Now().Add(time.Hour), nil, []CalendarOccurrence{item})
				if err != nil {
					t.Fatal(err)
				}
				if err := rig.service.ReplaceCalendarWindow(context.Background(), window); err != nil {
					t.Fatal(err)
				}
			}
			handle, err := rig.service.ObserveWait(context.Background(), rig.program.Waits()[0].Source())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { <-handle.Close() })
			select {
			case <-handle.Ready():
				t.Fatal("past/initial state fired a new transient trigger")
			default:
			}
			switch test.kind {
			case ir.TriggerState:
				rig.publishSwitch(t, true)
			case ir.TriggerNumericState:
				// Cross the actual Host admission, shared sequencer and reader;
				// bypassing that reader would not qualify its dynamic wait slots.
				rig.publishMeasurement(t, 0)
				rig.publishMeasurement(t, 1)
			case ir.TriggerEvent, ir.TriggerDeviceMQTT:
				code := "nha042_release"
				if test.kind == ir.TriggerDeviceMQTT {
					code = "z2m.single"
				}
				event, err := integrations.NewDeviceEvent(code, rig.clock.Now(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := rig.session.Emit(context.Background(), rig.binding, event); err != nil {
					t.Fatal(err)
				}
			case ir.TriggerMQTT:
				rig.native(t, 1, []byte("live"))
			case ir.TriggerTime, ir.TriggerTimePattern, ir.TriggerSunset, ir.TriggerCalendar:
				rig.clock.Advance(time.Second, time.Second)
			case ir.TriggerInvalid:
				t.Fatal("invalid trigger kind")
			default:
				t.Fatal("unhandled admitted trigger kind")
			}
			nha042WaitReady(t, handle)
			result, err := handle.Outcome()
			if err != nil || result.Kind() != ResultFired || result.Automation() != "" {
				t.Fatalf("transient family did not use040 result: %+v %v", result, err)
			}
			selected, present := handle.Trigger()
			if !present || selected.Kind() != test.kind || selected.Index() != 0 || selected.ID() != "kind-"+test.name {
				t.Fatalf("selected trigger metadata drift: %+v %v", selected, present)
			}
			if at, present := handle.CompletedAt(); !present || at != rig.clock.MonoNow() {
				t.Fatalf("completion did not use shared monotone clock: %v %v", at, present)
			}
			<-handle.Close()
			rig.service.mu.Lock()
			leaked := len(rig.service.waits) + len(rig.service.waitOwners) + rig.service.resultBytes
			rig.service.mu.Unlock()
			if leaked != 0 {
				t.Fatalf("transient family leaked ownership: %d", leaked)
			}
		})
	}
}

func TestNHA042WaitResultBytesSaturateAndRecoverAfterClose(t *testing.T) {
	rig := nha042TriggerRig(t, "  - trigger: mqtt\n    topic: nha042/wait")
	payload := bytes.Repeat([]byte{0x42}, 64<<10)
	handles := make([]*WaitHandle, 0, 16)
	t.Cleanup(func() {
		for _, handle := range handles {
			<-handle.Close()
		}
	})
	for index := 0; index < 16; index++ {
		handle, err := rig.service.ObserveWait(context.Background(), rig.program.Waits()[0].Source())
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, handle)
		rig.native(t, uint64(index+1), payload)
		nha042WaitReady(t, handle)
		result, err := handle.Outcome()
		if index < 15 {
			if err != nil || result.Kind() != ResultFired || len(result.Payload()) != len(payload) {
				t.Fatalf("completed result %d: %v %v", index, result.Kind(), err)
			}
			copied := result.Payload()
			copied[0] = 0
			if result.Payload()[0] != 0x42 {
				t.Fatal("result exposed retained payload ownership")
			}
		} else if !errors.Is(err, ErrCapacity) || result.Kind() != ResultInvalid {
			t.Fatalf("sixteenth retained64KiB result exceeded shared bytes: %v %v", result.Kind(), err)
		}
		rig.service.mu.Lock()
		retained := rig.service.resultBytes
		rig.service.mu.Unlock()
		if retained > MaxOutputBytes {
			t.Fatalf("wait results escaped output budget: %d", retained)
		}
	}
	for _, handle := range handles {
		<-handle.Close()
		<-handle.Close()
	}
	rig.service.mu.Lock()
	retained := rig.service.resultBytes
	rig.service.mu.Unlock()
	if retained != 0 {
		t.Fatalf("actual result charges not released exactly once: %d", retained)
	}
	rearmed, err := rig.service.ObserveWait(context.Background(), rig.program.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { <-rearmed.Close() })
	rig.native(t, 17, []byte("recovered"))
	nha042WaitReady(t, rearmed)
	result, err := rearmed.Outcome()
	if err != nil || string(result.Payload()) != "recovered" {
		t.Fatalf("capacity refusal poisoned subsequent registration: %+v %v", result, err)
	}
}

func TestNHA042TransientStateForReboundUsesJournalAdmissionTick(t *testing.T) {
	rig := nha042TriggerRig(t, "  - trigger: state\n    entity_id: switch.fixture\n    from: 'off'\n    to: 'on'\n    for: {seconds: 3}")
	handle, err := rig.service.ObserveWait(context.Background(), rig.program.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { <-handle.Close() })
	pending := func(want int, deadline time.Duration) {
		t.Helper()
		nha040Wait(t, func() bool {
			rig.service.mu.Lock()
			defer rig.service.mu.Unlock()
			if len(rig.service.pending) != want {
				return false
			}
			if want == 0 {
				return true
			}
			value, present := rig.service.pending[handle.slots[0]]
			return present && value.deadline == deadline
		})
	}
	rig.publishSwitch(t, true)
	pending(1, 3*time.Second)
	rig.clock.Advance(time.Second, time.Second)
	rig.publishSwitch(t, false)
	pending(0, 0)
	rig.clock.Advance(time.Second, time.Second)
	rig.publishSwitch(t, true)
	pending(1, 5*time.Second)
	rig.clock.Advance(2*time.Second, 2*time.Second)
	rig.service.mu.Lock()
	rig.service.runDueLocked()
	rig.service.mu.Unlock()
	select {
	case <-handle.Ready():
		t.Fatal("rebound reused the first admission tick or fired before its full interval")
	default:
	}
	rig.clock.Advance(time.Second, time.Second)
	nha042WaitReady(t, handle)
	result, err := handle.Outcome()
	if err != nil || result.Kind() != ResultFired {
		t.Fatalf("rebound wait: %v %v", result.Kind(), err)
	}
	if at, present := handle.CompletedAt(); !present || at != 5*time.Second {
		t.Fatalf("for did not use the monotone journal interval: %v %v", at, present)
	}
	<-handle.Close()
	rig.service.mu.Lock()
	remaining := len(rig.service.pending) + len(rig.service.scheduledMono) + len(rig.service.waitOwners)
	rig.service.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("rebound retained timer/consumer ownership: %d", remaining)
	}
}

func TestNHA042TransientNumericForDoesNotResetAndRearmsOutsideRange(t *testing.T) {
	rig := nha042TriggerRig(t, "  - trigger: numeric_state\n    entity_id: switch.fixture\n    above: 0\n    below: 10\n    for: {seconds: 3}")
	observe := func() *WaitHandle {
		t.Helper()
		handle, err := rig.service.ObserveWait(context.Background(), rig.program.Waits()[0].Source())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { <-handle.Close() })
		return handle
	}
	first := observe()
	rig.publishMeasurement(t, 1)
	nha040Wait(t, func() bool {
		rig.service.mu.Lock()
		defer rig.service.mu.Unlock()
		value, present := rig.service.pending[first.slots[0]]
		return present && value.deadline == 3*time.Second
	})
	rig.clock.Advance(time.Second, time.Second)
	rig.publishMeasurement(t, 2)
	// Another admission flushes the same reader and witnesses consumption of
	// the in-range update, without privately invoking the numeric matcher.
	probe := observe()
	<-probe.Close()
	rig.service.mu.Lock()
	value, present := rig.service.pending[first.slots[0]]
	rig.service.mu.Unlock()
	if !present || value.deadline != 3*time.Second {
		t.Fatalf("matching update reset numeric for: %+v %v", value, present)
	}
	rig.clock.Advance(2*time.Second, 2*time.Second)
	nha042WaitReady(t, first)
	if result, err := first.Outcome(); err != nil || result.Kind() != ResultFired {
		t.Fatalf("numeric for: %v %v", result.Kind(), err)
	}
	<-first.Close()
	second := observe()
	rig.publishMeasurement(t, 3)
	probe = observe()
	<-probe.Close()
	rig.service.mu.Lock()
	pending := len(rig.service.pending)
	armed := rig.service.armed[second.slots[0]]
	rig.service.mu.Unlock()
	if pending != 0 || armed {
		t.Fatalf("initial or continuing in-range state manufactured a crossing: pending%d armed%v", pending, armed)
	}
	rig.publishMeasurement(t, 0)
	rig.publishMeasurement(t, 1)
	nha040Wait(t, func() bool {
		rig.service.mu.Lock()
		defer rig.service.mu.Unlock()
		value, present := rig.service.pending[second.slots[0]]
		return present && value.deadline == 6*time.Second
	})
	rig.clock.Advance(3*time.Second, 3*time.Second)
	nha042WaitReady(t, second)
	if result, err := second.Outcome(); err != nil || result.Kind() != ResultFired {
		t.Fatalf("rearmed numeric wait: %v %v", result.Kind(), err)
	}
	if at, present := second.CompletedAt(); !present || at != 6*time.Second {
		t.Fatalf("rearmed numeric completion tick: %v %v", at, present)
	}
}

func TestNHA042TransientCivilTimersRespectGapFoldAndOneShotOwnership(t *testing.T) {
	location, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, at   string
		start, due time.Time
	}{
		{"gap", "02:30:00", time.Date(2026, 3, 8, 0, 0, 0, 0, location), time.Date(2026, 3, 9, 2, 30, 0, 0, location)},
		{"fold", "01:30:00", time.Date(2026, 11, 1, 0, 0, 0, 0, location), time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)},
	} {
		t.Run(test.name, func(t *testing.T) {
			rig := nha042TriggerRigAt(t, "  - trigger: time\n    at: '"+test.at+"'", test.start, "America/Toronto")
			handle, err := rig.service.ObserveWait(context.Background(), rig.program.Waits()[0].Source())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { <-handle.Close() })
			rig.service.mu.Lock()
			due, scheduled := rig.service.scheduled[handle.slots[0]]
			rig.service.mu.Unlock()
			if !scheduled || !due.Equal(test.due) {
				t.Fatalf("transient civil %s scheduled %v, want explicit instant %v", test.name, due, test.due)
			}
			elapsed := test.due.Sub(test.start)
			rig.clock.Advance(elapsed-time.Second, elapsed-time.Second)
			rig.service.mu.Lock()
			rig.service.runDueLocked()
			rig.service.mu.Unlock()
			select {
			case <-handle.Ready():
				t.Fatal("civil wait fired before its admitted instant")
			default:
			}
			rig.clock.Advance(time.Second, time.Second)
			nha042WaitReady(t, handle)
			if result, err := handle.Outcome(); err != nil || result.Kind() != ResultFired || !result.At().Equal(test.due) {
				t.Fatalf("civil transient result: %+v %v", result, err)
			}
			if at, present := handle.CompletedAt(); !present || at != elapsed {
				t.Fatalf("civil timer lost its monotone completion: %v %v", at, present)
			}
			rig.clock.Advance(-time.Hour, time.Second)
			rig.clock.Advance(2*time.Hour, 2*time.Hour)
			rig.service.mu.Lock()
			rig.service.runDueLocked()
			remaining := len(rig.service.scheduled) + len(rig.service.civil) + len(rig.service.waitOwners)
			permanentResults := len(rig.service.results)
			rig.service.mu.Unlock()
			if remaining != 0 || permanentResults != 0 {
				t.Fatalf("closed one-shot civil wait rearmed after wall rollback: owned%d permanent%d", remaining, permanentResults)
			}
		})
	}
}

func TestNHA042TransientCalendarRevisionMovesOwnedAlarmAndClosePurgesKeys(t *testing.T) {
	rig := nha042TriggerRig(t, "  - trigger: calendar\n    entity_id: calendar.fixture\n    event: start")
	ctx := context.Background()
	source, err := ParseCalendarSourceID("fixture-calendar")
	if err != nil {
		t.Fatal(err)
	}
	base := rig.clock.Now()
	replace := func(revision uint64, seconds time.Duration) {
		t.Helper()
		start := base.Add(seconds)
		item, err := NewCalendarOccurrence(source, "same-stable-occurrence", "recurrence-1", start, start.Add(time.Minute), "UTC", false, domain.FreshnessFresh)
		if err != nil {
			t.Fatal(err)
		}
		window, err := NewCalendarWindow(source, revision, base.Add(-time.Minute), base.Add(time.Hour), nil, []CalendarOccurrence{item})
		if err != nil {
			t.Fatal(err)
		}
		if err := rig.service.ReplaceCalendarWindow(ctx, window); err != nil {
			t.Fatal(err)
		}
	}
	replace(1, 2*time.Second)
	handle, err := rig.service.ObserveWait(ctx, rig.program.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { <-handle.Close() })
	replace(2, 3*time.Second)
	rig.service.mu.Lock()
	due, present := rig.service.scheduled[handle.slots[0]]
	rig.service.mu.Unlock()
	if !present || !due.Equal(base.Add(3*time.Second)) {
		t.Fatalf("revision did not replace the admitted transient alarm: %v %v", due, present)
	}
	rig.clock.Advance(2*time.Second, 2*time.Second)
	rig.service.mu.Lock()
	rig.service.runDueLocked()
	rig.service.mu.Unlock()
	select {
	case <-handle.Ready():
		t.Fatal("superseded calendar revision fired")
	default:
	}
	rig.clock.Advance(time.Second, time.Second)
	nha042WaitReady(t, handle)
	if result, err := handle.Outcome(); err != nil || result.Kind() != ResultFired {
		t.Fatalf("revised calendar result: %+v %v", result, err)
	}
	<-handle.Close()
	replace(3, 4*time.Second)
	rig.clock.Advance(time.Second, time.Second)
	rig.service.mu.Lock()
	rig.service.runDueLocked()
	remaining := len(rig.service.seen) + rig.service.seenBytes + len(rig.service.scheduled) + len(rig.service.waitOwners)
	permanentResults := len(rig.service.results)
	rig.service.mu.Unlock()
	if remaining != 0 || permanentResults != 0 {
		t.Fatalf("closed calendar occurrence retained/replayed ownership: owned%d permanent%d", remaining, permanentResults)
	}
}
