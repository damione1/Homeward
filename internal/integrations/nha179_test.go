package integrations_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/light"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/proto"
)

type nha179Rig struct {
	host     *integrations.Manager
	fixture  *fake.Fixture
	session  *integrations.Session
	binding  integrations.Binding
	key      integrations.SourceKey
	service  *integrations.VolatileCommandService
	gen      *nha022Generation
	driver   *nha025Driver
	encoding integrations.LightEncoding
}

func nha179Setup(t *testing.T, caseta bool) nha179Rig {
	return nha179SetupWithClock(t, caseta, nil)
}

type nha179Clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *nha179Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *nha179Clock) Set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

func nha179SetupWithClock(t *testing.T, caseta bool, clock integrations.CommandClock) nha179Rig {
	t.Helper()
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "nha179-synthetic")
	key, err := integrations.NewSourceKey("nha179-controller", "level")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	record := nha022Record(t, ids[0], instance, key, "light.nha179", "light.nha179", false)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver := &nha025Driver{}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	gen := &nha022Generation{revision: 7}
	var service *integrations.VolatileCommandService
	if clock == nil {
		service, err = integrations.NewVolatileCommandService(host, gen, integrations.DefaultVolatileCommandLimits())
	} else {
		service, err = integrations.NewVolatileCommandServiceWithClock(host, gen, clock, integrations.DefaultVolatileCommandLimits())
	}
	if err != nil {
		t.Fatal(err)
	}
	rig := nha179Rig{host: host, fixture: fixture, session: session, key: key, service: service, gen: gen, driver: driver}
	if caseta {
		rig.discoverCaseta(t)
	} else {
		rig.discoverMQTT(t, 254)
	}
	return rig
}

func TestNHA179ExpiredPreparedEnvelopeNeverDispatches(t *testing.T) {
	now := time.Date(2026, 10, 18, 18, 0, 0, 0, time.UTC)
	clock := &nha179Clock{now: now}
	rig := nha179SetupWithClock(t, false, clock)
	observed := 30.0
	rig.publish(t, true, &observed, domain.FreshnessFresh, domain.ProvenanceObserved)
	id, _ := domain.ParseCommandID("nha179-expired")
	slot, err := light.NewPreparationSlot(rig.service, nha179Call(t, "light.turn_on", "{brightness_pct: 50}"), 0,
		nha022Actor(t), id, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slot.PrepareOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Set(now.Add(2 * time.Minute))
	response, err := slot.Dispatch(context.Background())
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || rig.driver.Count() != 0 {
		t.Fatalf("expired token emitted: %v %v calls=%d", response, err, rig.driver.Count())
	}
}

func TestNHA179CanceledFirstPreparationIsMemoizedAndOldTokenCannotCrossRouter(t *testing.T) {
	first := nha179Setup(t, false)
	level := 30.0
	first.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	slot := nha179Slot(t, first.service, nha179Call(t, "light.turn_on", "{brightness_step_pct: 15}"), "nha179-canceled-once")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := slot.PrepareOnce(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled first call was not retained: %v", err)
	}
	if _, err := slot.Dispatch(context.Background()); !errors.Is(err, context.Canceled) || first.driver.Count() != 0 {
		t.Fatalf("canceled slot was rebuilt: %v sends=%d", err, first.driver.Count())
	}
	alias, _ := domain.ParseHAAlias("light.nha179")
	capture, err := first.service.CaptureLight(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	brightness := uint8(115)
	id, _ := domain.ParseCommandID("nha179-old-epoch-token")
	prepared, err := capture.PrepareHA(nha022Actor(t), id, time.Now().Add(time.Minute), true, &brightness, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	second := nha179Setup(t, false)
	second.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	firstEpoch, err := first.fixture.Sequencer().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	secondEpoch, err := second.fixture.Sequencer().CurrentAdmissionEpoch()
	if err != nil || firstEpoch == secondEpoch {
		t.Fatalf("restart epoch not distinct: %q %q %v", firstEpoch, secondEpoch, err)
	}
	if response, err := second.service.DispatchPrepared(context.Background(), prepared); response != nil || !errors.Is(err, integrations.ErrInvalidInput) || second.driver.Count() != 0 {
		t.Fatalf("opaque token crossed restarted router: %v %v sends=%d", response, err, second.driver.Count())
	}
}

func TestNHA179PreparedIkeaMinimumIsSingleProjection(t *testing.T) {
	rig := nha179Setup(t, false)
	encoding, err := integrations.NewMQTTLightEncoding(254, sha256.Sum256([]byte("nha179-ikea-30w")), true)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(rig.key, []*registry.CapabilityDescription{nha179Description(false)})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = candidate.WithLightEncoding(encoding)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := rig.session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("IKEA discovery: %v", err)
	}
	rig.binding, rig.encoding = bindings[0], encoding
	level := 30.0
	rig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	alias, _ := domain.ParseHAAlias("light.nha179")
	capture, err := rig.service.CaptureLight(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	one := uint8(1)
	id, _ := domain.ParseCommandID("nha179-ikea-minimum")
	prepared, err := capture.PrepareHA(nha022Actor(t), id, time.Now().Add(time.Minute), true, &one, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if q, present := prepared.MQTTTarget(); !present || q != 2 {
		t.Fatalf("HA1 must use qualified IKEA minimum q2: %d %t", q, present)
	}
	rig.driver.call = func(_ context.Context, admitted integrations.AdmittedCommand) error {
		q, present, err := admitted.MQTTLightEmission(encoding)
		if err != nil || !present || q != 2 {
			return fmt.Errorf("prepared IKEA target changed: q=%d present=%t err=%v", q, present, err)
		}
		return nil
	}
	response, err := rig.service.DispatchPrepared(context.Background(), prepared)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || rig.driver.Count() != 1 {
		t.Fatalf("IKEA prepared target rejected: %v %v sends=%d", response, err, rig.driver.Count())
	}
}

func nha179Description(caseta bool) *registry.CapabilityDescription {
	step := 1.0
	endpoint := "mqtt"
	if caseta {
		endpoint = "level"
	}
	return &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, Endpoint: endpoint,
		DisplayName: "Synthetic light", Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{
			SetOn: true, BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100},
			TransitionSeconds: &registry.NumberBounds{Minimum: 0, Maximum: 6553, Step: &step},
		}}}
}

func (r *nha179Rig) discoverMQTT(t *testing.T, scale uint32) {
	t.Helper()
	encoding, err := integrations.NewMQTTLightEncoding(scale, sha256.Sum256([]byte(fmt.Sprintf("nha179-scale-%d", scale))), false)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(r.key, []*registry.CapabilityDescription{nha179Description(false)})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = candidate.WithLightEncoding(encoding)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := r.session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("MQTT discovery: %v", err)
	}
	r.binding, r.encoding = bindings[0], encoding
}

func (r *nha179Rig) discoverCaseta(t *testing.T) {
	t.Helper()
	alias, err := domain.ParseHAAlias("light.nha179")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(r.key, []*registry.CapabilityDescription{nha179Description(true)})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = candidate.WithCasetaHA100V1(alias)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := r.session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("Caseta discovery: %v", err)
	}
	r.binding = bindings[0]
}

func (r *nha179Rig) publish(t *testing.T, on bool, percent *float64, freshness domain.Freshness, provenance domain.Provenance) {
	t.Helper()
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, freshness, provenance)
	if err != nil {
		t.Fatal(err)
	}
	r.publishQuality(t, on, percent, quality)
}

func (r *nha179Rig) publishQuality(t *testing.T, on bool, percent *float64, quality domain.Quality) {
	t.Helper()
	value, err := domain.NewLightValue(on, percent, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := domain.NewLightStateValue(value)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	endpoint := "mqtt"
	if r.encoding == (integrations.LightEncoding{}) {
		endpoint = "level"
	}
	var current *domain.StateValue
	var observedAt *time.Time
	availability, _, _, _ := quality.Components()
	if availability == domain.AvailabilityAvailable {
		current, observedAt = &state, &at
	}
	observation, err := integrations.NewStateObservation(endpoint, quality, current, observedAt, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.session.Publish(context.Background(), r.binding, observation); err != nil {
		t.Fatal(err)
	}
}

func TestNHA179AbsoluteAndOffRequireObservedStateAndRevisionFence(t *testing.T) {
	for _, quality := range []struct {
		name         string
		availability domain.Availability
		freshness    domain.Freshness
		provenance   domain.Provenance
	}{
		{"stale", domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceObserved},
		{"restored", domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceRestored},
		{"estimated", domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceEstimated},
		{"unavailable", domain.AvailabilityUnavailable, domain.FreshnessFresh, domain.ProvenanceObserved},
	} {
		t.Run(quality.name, func(t *testing.T) {
			rig := nha179Setup(t, false)
			q, err := domain.NewQuality(quality.availability, quality.freshness, quality.provenance)
			if err != nil {
				t.Fatal(err)
			}
			percent := 30.0
			rig.publishQuality(t, true, &percent, q)
			for _, action := range []struct{ name, data string }{{"light.turn_on", "{brightness_pct: 50}"}, {"light.turn_off", "{transition: 2}"}} {
				slot := nha179Slot(t, rig.service, nha179Call(t, action.name, action.data), "nha179-"+quality.name+"-"+action.name)
				if _, err := slot.PrepareOnce(context.Background()); !errors.Is(err, integrations.ErrLightObservationUnavailable) {
					t.Fatalf("%s accepted %s: %v", quality.name, action.name, err)
				}
			}
			if rig.driver.Count() != 0 {
				t.Fatal("untrusted state reached driver")
			}
		})
	}
	rig := nha179Setup(t, false)
	percent := 30.0
	rig.publish(t, true, &percent, domain.FreshnessFresh, domain.ProvenanceObserved)
	slot := nha179Slot(t, rig.service, nha179Call(t, "light.turn_on", "{brightness_pct: 50}"), "nha179-old-revision")
	prepared, err := slot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := prepared.Request()
	if wire.ExpectedRevision == nil {
		t.Fatal("absolute lost observed revision fence")
	}
	percent = 31
	rig.publish(t, true, &percent, domain.FreshnessFresh, domain.ProvenanceObserved)
	response, err := slot.Dispatch(context.Background())
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || rig.driver.Count() != 0 {
		t.Fatalf("old revision emitted: %v %v calls=%d", response, err, rig.driver.Count())
	}
	newSlot := nha179Slot(t, rig.service, nha179Call(t, "light.turn_on", "{brightness_pct: 50}"), "nha179-new-revision")
	if response, err := newSlot.Dispatch(context.Background()); err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || rig.driver.Count() != 1 {
		t.Fatalf("new occurrence rejected: %v %v calls=%d", response, err, rig.driver.Count())
	}
}

func TestNHA179ConfigurationRevisionInvalidatesPreparedLight(t *testing.T) {
	rig := nha179Setup(t, false)
	percent := 30.0
	rig.publish(t, true, &percent, domain.FreshnessFresh, domain.ProvenanceObserved)
	slot := nha179Slot(t, rig.service, nha179Call(t, "light.turn_on", "{brightness_pct: 50}"), "nha179-old-config")
	if _, err := slot.PrepareOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rig.gen.mu.Lock()
	rig.gen.revision++
	rig.gen.mu.Unlock()
	response, err := slot.Dispatch(context.Background())
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || rig.driver.Count() != 0 {
		t.Fatalf("changed generation emitted: %v %v calls=%d", response, err, rig.driver.Count())
	}
}

func TestNHA179AliasPrelookupMutationRefusesCapture(t *testing.T) {
	rig := nha179Setup(t, false)
	observed := 30.0
	rig.publish(t, true, &observed, domain.FreshnessFresh, domain.ProvenanceObserved)
	pause := &nha022PausingGeneration{entered: make(chan struct{}), release: make(chan struct{})}
	router, err := integrations.NewCommandRouter(rig.host, pause)
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("light.nha179")
	finished := make(chan error, 1)
	go func() { _, err := router.CaptureLight(context.Background(), alias); finished <- err }()
	select {
	case <-pause.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("capture did not reach generation fence")
	}
	device, entity, err := rig.binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	instance, key, err := rig.binding.Source()
	if err != nil {
		t.Fatal(err)
	}
	retired := nha022Record(t, integrations.ResolvedIdentity{Device: device, Entity: entity}, instance, key,
		"light.nha179", "light.nha179", true)
	if err := rig.fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{retired}); err != nil {
		t.Fatal(err)
	}
	close(pause.release)
	select {
	case err := <-finished:
		if !errors.Is(err, integrations.ErrCommandRouteUnavailable) {
			t.Fatalf("stale prelookup admitted: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capture blocked after registry mutation")
	}
	if rig.driver.Count() != 0 {
		t.Fatal("prelookup mutation reached driver")
	}
}

func nha179Call(t *testing.T, action, data string) ir.Call {
	t.Helper()
	entity, err := ir.NewEntity("light.nha179", ir.ReferencePresent, ir.SupportLightTurnOn, ir.SupportLightTurnOff,
		ir.SupportLightBrightnessAbsolute, ir.SupportLightBrightnessRelative, ir.SupportLightTransition)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("id: nha179_fixture\ntriggers:\n- trigger: sun\n  event: sunset\n  offset: -01:00:00\nactions:\n- action: %s\n  target: {entity_id: light.nha179}\n  data: %s\n", action, data)
	source, err := parser.NewSourceFile("automation.nha179_fixture.yaml", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ir.CompileSources("automation.nha179_fixture", source.Path(), []parser.SourceFile{source}, catalogue)
	if err != nil || len(result.Accepted()) != 1 || len(result.Rejected()) != 0 {
		t.Fatalf("IR call: %v, rejected=%+v", err, result.Rejected())
	}
	return result.Accepted()[0].Actions()[0].Call()
}

func nha179Slot(t *testing.T, service light.Service, call ir.Call, id string) *light.PreparationSlot {
	t.Helper()
	commandID, err := domain.ParseCommandID(id)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := light.NewPreparationSlot(service, call, 0, nha022Actor(t), commandID, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return slot
}

type nha179PausedCapture struct {
	service *integrations.VolatileCommandService
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
	once    sync.Once
}

func (s *nha179PausedCapture) CaptureLight(ctx context.Context, alias domain.HAAlias) (integrations.LightCapture, error) {
	capture, err := s.service.CaptureLight(ctx, alias)
	s.calls.Add(1)
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return capture, err
}

func (s *nha179PausedCapture) DispatchPrepared(ctx context.Context, prepared integrations.PreparedLight) (*actions.CommandResponse, error) {
	return s.service.DispatchPrepared(ctx, prepared)
}

func TestNHA179PreparedProfileFenceBeforeFirstDispatchAndReplay(t *testing.T) {
	rig := nha179Setup(t, false)
	observed := 30.0
	rig.publish(t, true, &observed, domain.FreshnessFresh, domain.ProvenanceObserved)
	call := nha179Call(t, "light.turn_on", "{brightness_pct: 50, transition: 2}")
	slot := nha179Slot(t, rig.service, call, "nha179-profile-old")
	prepared, err := slot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := prepared.HABrightness(); !ok || b != 128 {
		t.Fatalf("HA brightness=%d present=%v", b, ok)
	}
	if source := prepared.Source(); source.Object() != "automation.nha179_fixture" || source.Path() == "" {
		t.Fatalf("HA brightness lost its IR source trace: %+v", source)
	}
	if q, ok := prepared.MQTTTarget(); !ok || q != 127 {
		t.Fatalf("prepared q=%d present=%v", q, ok)
	}
	original, err := prepared.Request()
	if err != nil {
		t.Fatal(err)
	}
	copyRequest, _ := prepared.Request()
	copyRequest.GetArguments().GetSetLight().BrightnessPercent = nil
	again, _ := prepared.Request()
	if !proto.Equal(original, again) {
		t.Fatal("caller changed cached envelope")
	}
	rig.discoverMQTT(t, 255) // No public description, state, epoch or config change.
	response, err := slot.Dispatch(context.Background())
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || rig.driver.Count() != 0 {
		t.Fatalf("stale private profile emitted: %v %v count=%d", response, err, rig.driver.Count())
	}
	newSlot := nha179Slot(t, rig.service, call, "nha179-profile-new")
	next, err := newSlot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q, ok := next.MQTTTarget(); !ok || q != 128 {
		t.Fatalf("new q=%d present=%v", q, ok)
	}
	rig.driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		q, present, err := command.MQTTLightEmission(rig.encoding)
		if err != nil || !present || q != 128 || !command.ReadyForTransport() {
			return fmt.Errorf("emission q=%d present=%v ready=%v err=%v", q, present, command.ReadyForTransport(), err)
		}
		return nil
	}
	first, err := newSlot.Dispatch(context.Background())
	if err != nil || first == nil || first.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("first=%v %v", first, err)
	}
	second, err := newSlot.Dispatch(context.Background())
	if err != nil || !proto.Equal(first, second) || rig.driver.Count() != 1 {
		t.Fatalf("replay=%v %v count=%d", second, err, rig.driver.Count())
	}
	changed, _ := next.Request()
	changed.GetArguments().GetSetLight().On = nil
	if _, err := rig.service.Dispatch(context.Background(), nha022Actor(t), changed); !errors.Is(err, integrations.ErrCommandIDConflict) {
		t.Fatalf("same ID accepted different presence: %v", err)
	}
}

func TestNHA179RelativeZeroAndCriticalObservation(t *testing.T) {
	rig := nha179Setup(t, false)
	percent := 15.0
	rig.publish(t, true, &percent, domain.FreshnessFresh, domain.ProvenanceObserved)
	call := nha179Call(t, "light.turn_on", "{brightness_step_pct: -15, transition: 2}")
	slot := nha179Slot(t, rig.service, call, "nha179-step-zero")
	prepared, err := slot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := prepared.Request()
	arg := wire.GetArguments().GetSetLight()
	if arg == nil || arg.On == nil || *arg.On || arg.BrightnessPercent != nil || arg.TransitionSeconds == nil || *arg.TransitionSeconds != 2 || wire.ExpectedRevision == nil {
		t.Fatalf("zero step must be OFF with revision and transition: %+v", wire)
	}
	if _, ok := prepared.MQTTTarget(); ok {
		t.Fatal("OFF got minimum MQTT brightness")
	}
	if response, err := slot.Dispatch(context.Background()); err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || rig.driver.Count() != 1 {
		t.Fatalf("OFF dispatch=%v %v count=%d", response, err, rig.driver.Count())
	}
	stale := nha179Setup(t, false)
	stale.publish(t, true, &percent, domain.FreshnessStale, domain.ProvenanceObserved)
	refused := nha179Slot(t, stale.service, call, "nha179-step-stale")
	if _, err := refused.PrepareOnce(context.Background()); !errors.Is(err, integrations.ErrLightObservationUnavailable) {
		t.Fatalf("stale observation: %v", err)
	}
	stale.publish(t, true, &percent, domain.FreshnessFresh, domain.ProvenanceObserved)
	if _, err := refused.Dispatch(context.Background()); !errors.Is(err, integrations.ErrLightObservationUnavailable) || stale.driver.Count() != 0 {
		t.Fatalf("refusal was reconstructed after fresh observation: %v sends=%d", err, stale.driver.Count())
	}
}

func TestNHA179ConcurrentSlotHasOneEnvelope(t *testing.T) {
	rig := nha179Setup(t, false)
	percent := 30.0
	rig.publish(t, true, &percent, domain.FreshnessFresh, domain.ProvenanceObserved)
	slot := nha179Slot(t, rig.service, nha179Call(t, "light.turn_on", "{brightness_step_pct: 15}"), "nha179-concurrent")
	const workers = 16
	results := make([]*actions.CommandRequest, workers)
	errorsSeen := make([]error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			prepared, err := slot.PrepareOnce(context.Background())
			errorsSeen[index] = err
			if err == nil {
				results[index], err = prepared.Request()
				errorsSeen[index] = err
			}
		}(i)
	}
	wait.Wait()
	for i := 0; i < workers; i++ {
		if errorsSeen[i] != nil || !proto.Equal(results[0], results[i]) {
			t.Fatalf("slot %d differs: %v", i, errorsSeen[i])
		}
	}
	if results[0].ExpectedRevision == nil {
		t.Fatal("relative slot lost revision")
	}
}

func TestNHA179ConcurrentSlotKeepsFirstCaptureAcrossStateChange(t *testing.T) {
	rig := nha179Setup(t, false)
	first := 30.0
	rig.publish(t, true, &first, domain.FreshnessFresh, domain.ProvenanceObserved)
	paused := &nha179PausedCapture{service: rig.service, entered: make(chan struct{}), release: make(chan struct{})}
	slot := nha179Slot(t, paused, nha179Call(t, "light.turn_on", "{brightness_step_pct: 15}"), "nha179-concurrent-mutation")
	const callers = 16
	results := make([]light.PreparedAction, callers)
	errorsSeen := make([]error, callers)
	var wait sync.WaitGroup
	for i := range results {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errorsSeen[index] = slot.PrepareOnce(context.Background())
		}(i)
	}
	select {
	case <-paused.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first capture did not reach the barrier")
	}
	second := 60.0
	rig.publish(t, true, &second, domain.FreshnessFresh, domain.ProvenanceObserved)
	close(paused.release)
	wait.Wait()
	if paused.calls.Load() != 1 {
		t.Fatalf("one occurrence captured state %d times", paused.calls.Load())
	}
	firstWire, err := results[0].Request()
	if err != nil {
		t.Fatal(err)
	}
	for i, prepared := range results {
		wire, err := prepared.Request()
		if errorsSeen[i] != nil || err != nil || !proto.Equal(firstWire, wire) {
			t.Fatalf("caller %d reconstructed a different envelope: prepare=%v request=%v", i, errorsSeen[i], err)
		}
		if brightness, present := prepared.HABrightness(); !present || brightness != 115 {
			t.Fatalf("caller %d used second observation: b=%d present=%v", i, brightness, present)
		}
	}
	response, err := slot.Dispatch(context.Background())
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || rig.driver.Count() != 0 {
		t.Fatalf("changed state crossed revision fence: response=%v err=%v sends=%d", response, err, rig.driver.Count())
	}
}

func TestNHA179DirectNativeSetLightKeepsFractionalPercentWithoutOptIn(t *testing.T) {
	rig := nha179Setup(t, false)
	candidate, err := integrations.NewCandidate(rig.key, []*registry.CapabilityDescription{nha179Description(true)})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := rig.session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("unqualified native discovery: %v", err)
	}
	rig.binding = bindings[0]
	alias, _ := domain.ParseHAAlias("light.nha179")
	if _, err := rig.service.CaptureLight(context.Background(), alias); !errors.Is(err, integrations.ErrLightPreparationUnsupported) {
		t.Fatalf("unqualified native route acquired HA profile: %v", err)
	}
	rig.driver.call = func(_ context.Context, admitted integrations.AdmittedCommand) error {
		wire, err := admitted.Request()
		if err != nil || wire.GetArguments().GetSetLight().GetBrightnessPercent() != 42.375 {
			return fmt.Errorf("native fractional target changed: %v %v", wire, err)
		}
		if !admitted.ReadyForTransport() {
			return fmt.Errorf("native command lost transport lease")
		}
		return nil
	}
	epoch, err := rig.fixture.Sequencer().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	_, entity, err := rig.binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	request := nha022Request(t, entity, epoch, contracts.LightSchema, "level", nha022Light(42.375))
	request.CommandId = "nha179-native-fraction"
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), request)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || rig.driver.Count() != 1 {
		t.Fatalf("native SetLight failed: %v %v calls=%d", response, err, rig.driver.Count())
	}
}

func TestNHA179CasetaQualifiedNativeLevelAndWithdrawal(t *testing.T) {
	rig := nha179Setup(t, true)
	observed := 25.0
	rig.publish(t, true, &observed, domain.FreshnessFresh, domain.ProvenanceObserved)
	alias, _ := domain.ParseHAAlias("light.nha179")
	capture, err := rig.service.CaptureLight(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		ha     uint8
		native float64
	}{{25, 10}, {76, 30}, {89, 35}} {
		id, _ := domain.ParseCommandID(fmt.Sprintf("nha179-caseta-%d", sample.ha))
		item, err := capture.PrepareHA(nha022Actor(t), id, time.Now().Add(time.Minute), true, &sample.ha, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := item.NativePercent(); !ok || p != sample.native {
			t.Fatalf("HA%d -> Caseta %v present=%v", sample.ha, p, ok)
		}
	}
	one := uint8(1)
	lowID, _ := domain.ParseCommandID("nha179-caseta-positive-zero")
	if _, err := capture.PrepareHA(nha022Actor(t), lowID, time.Now().Add(time.Minute), true, &one, nil, false); !errors.Is(err, integrations.ErrLightPreparationUnsupported) {
		t.Fatalf("positive HA 1 became an impossible Caseta ON/0: %v", err)
	}
	slot := nha179Slot(t, rig.service, nha179Call(t, "light.turn_on", "{brightness_pct: 50, transition: 2}"), "nha179-caseta")
	prepared, err := slot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := prepared.HABrightness(); !ok || b != 128 {
		t.Fatalf("source=%d %v", b, ok)
	}
	if p, ok := prepared.NativePercent(); !ok || p != 50 {
		t.Fatalf("native=%v %v", p, ok)
	}
	if _, ok := prepared.MQTTTarget(); ok {
		t.Fatal("Caseta exposed MQTT q")
	}
	if err := rig.session.Withdraw(context.Background(), rig.binding); err != nil {
		t.Fatal(err)
	}
	response, err := slot.Dispatch(context.Background())
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || rig.driver.Count() != 0 {
		t.Fatalf("withdrawn Caseta route emitted: %v %v calls=%d", response, err, rig.driver.Count())
	}
}

// These inbound fixtures follow HA Core 2026.10.1's MQTT JSON
// _state_received -> color.value_to_brightness((1,S), raw) path. They are
// separate from the exhaustive outbound async_turn_on oracle in P0.
func TestNHA179MQTTObservationInverseFixtures(t *testing.T) {
	for _, fixture := range []struct {
		scale uint32
		raw   float64
		want  uint8
	}{
		{254, 1, 1}, {254, 2, 2}, {254, 127, 128}, {254, 128, 129}, {254, 254, 255},
		{255, 1, 1}, {255, 127, 127}, {255, 128, 128}, {255, 255, 255},
	} {
		t.Run(fmt.Sprintf("s%d-q%.0f", fixture.scale, fixture.raw), func(t *testing.T) {
			rig := nha179Setup(t, false)
			if fixture.scale != 254 {
				rig.discoverMQTT(t, fixture.scale)
			}
			percent := fixture.raw * 100 / float64(fixture.scale)
			rig.publish(t, true, &percent, domain.FreshnessFresh, domain.ProvenanceObserved)
			alias, _ := domain.ParseHAAlias("light.nha179")
			capture, err := rig.service.CaptureLight(context.Background(), alias)
			if err != nil {
				t.Fatal(err)
			}
			got, _, err := capture.ObservedHABrightness()
			if err != nil || got != fixture.want {
				t.Fatalf("HA inbound b=%d want=%d err=%v", got, fixture.want, err)
			}
		})
	}
	rig := nha179Setup(t, false)
	zero := 0.0
	rig.publish(t, true, &zero, domain.FreshnessFresh, domain.ProvenanceObserved)
	alias, _ := domain.ParseHAAlias("light.nha179")
	capture, err := rig.service.CaptureLight(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := capture.ObservedHABrightness(); !errors.Is(err, integrations.ErrLightObservationUnavailable) {
		t.Fatalf("MQTT ON/raw zero must not guess HA's retained brightness: %v", err)
	}
}

func TestNHA179ExhaustivePinnedHAOutboundOracle(t *testing.T) {
	data, err := os.ReadFile("../../compat/ha/light/testdata/ha_outbound_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if digest := fmt.Sprintf("%x", sha256.Sum256(data)); digest != "887e2560d18a148c547c3de243261b4ce78321b3822d517163fa64ca9846ef55" {
		t.Fatalf("P0 source oracle changed: %s", digest)
	}
	var oracle struct {
		Table       [][]json.RawMessage `json:"table"`
		TableSHA256 string              `json:"table_sha256"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil || len(oracle.Table) != 512 || oracle.TableSHA256 != "7ff36e8953e153afc3d6fa2ebed3d6ea377cd26619b5848ee38fcc9567beb81e" {
		t.Fatalf("pinned P0 oracle missing or changed: %v rows=%d digest=%s", err, len(oracle.Table), oracle.TableSHA256)
	}
	for _, scale := range []uint32{254, 255} {
		rig := nha179Setup(t, false)
		if scale != 254 {
			rig.discoverMQTT(t, scale)
		}
		observed := 30.0
		rig.publish(t, true, &observed, domain.FreshnessFresh, domain.ProvenanceObserved)
		alias, _ := domain.ParseHAAlias("light.nha179")
		capture, err := rig.service.CaptureLight(context.Background(), alias)
		if err != nil {
			t.Fatal(err)
		}
		for b := 0; b <= 255; b++ {
			row := oracle.Table[int(scale-254)*256+b]
			var rowScale uint32
			var rowB uint8
			var operation string
			var expected *uint32
			if len(row) != 4 || json.Unmarshal(row[0], &rowScale) != nil || json.Unmarshal(row[1], &rowB) != nil || json.Unmarshal(row[2], &operation) != nil || json.Unmarshal(row[3], &expected) != nil || rowScale != scale || int(rowB) != b {
				t.Fatalf("invalid oracle row S%d b%d", scale, b)
			}
			id, _ := domain.ParseCommandID(fmt.Sprintf("nha179-oracle-%d-%d", scale, b))
			brightness := uint8(b)
			prepared, err := capture.PrepareHA(nha022Actor(t), id, time.Now().Add(time.Minute), true, &brightness, nil, false)
			if err != nil {
				t.Fatalf("S%d b%d: %v", scale, b, err)
			}
			wire, _ := prepared.Request()
			arg := wire.GetArguments().GetSetLight()
			q, present := prepared.MQTTTarget()
			if b == 0 {
				if operation != "off" || expected != nil || arg == nil || arg.On == nil || *arg.On || arg.BrightnessPercent != nil || present {
					t.Fatalf("S%d b0 was not HA service OFF: %v q=%d present=%v", scale, arg, q, present)
				}
			} else if operation != "on" || expected == nil || !present || q != *expected || arg == nil || arg.On == nil || !*arg.On {
				t.Fatalf("S%d b%d q=%d present=%v want=%v arg=%v", scale, b, q, present, expected, arg)
			}
		}
	}
}

func TestNHA179ExhaustivePinnedHAInboundOracle(t *testing.T) {
	data, err := os.ReadFile("../../compat/ha/light/testdata/ha_inbound_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if digest := fmt.Sprintf("%x", sha256.Sum256(data)); digest != "23cd3625f9274b0be8d600350ad3277f3ed8285ed721ff7bc3bdf920a32c408b" {
		t.Fatalf("pinned HA inbound oracle changed: %s", digest)
	}
	var oracle struct {
		HomeAssistant string `json:"homeassistant"`
		Table         []struct {
			Scale        uint32 `json:"scale"`
			Raw          uint32 `json:"raw"`
			HABrightness *uint8 `json:"ha_brightness"`
		} `json:"table"`
		TableSHA256 string `json:"table_sha256"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil || oracle.HomeAssistant != "2026.10.1" || len(oracle.Table) != 511 || oracle.TableSHA256 != "96d8c265d5a3101ebf9a5adf6995587ebf02ef1399f92e91c24d711e37fd0768" {
		t.Fatalf("invalid inbound oracle: %v rows=%d", err, len(oracle.Table))
	}
	for _, scale := range []uint32{254, 255} {
		rig := nha179Setup(t, false)
		if scale == 255 {
			rig.discoverMQTT(t, 255)
		}
		alias, _ := domain.ParseHAAlias("light.nha179")
		for raw := uint32(0); raw <= scale; raw++ {
			index := int(raw)
			if scale == 255 {
				index += 255
			}
			row := oracle.Table[index]
			if row.Scale != scale || row.Raw != raw {
				t.Fatalf("inbound oracle order S%d raw%d: %+v", scale, raw, row)
			}
			percent := float64(raw) * 100 / float64(scale)
			rig.publish(t, true, &percent, domain.FreshnessFresh, domain.ProvenanceObserved)
			capture, err := rig.service.CaptureLight(context.Background(), alias)
			if err != nil {
				t.Fatalf("capture S%d raw%d: %v", scale, raw, err)
			}
			got, _, err := capture.ObservedHABrightness()
			if row.HABrightness == nil {
				if !errors.Is(err, integrations.ErrLightObservationUnavailable) {
					t.Fatalf("HA ignored S%d raw%d but Homeward produced %d: %v", scale, raw, got, err)
				}
			} else if err != nil || got != *row.HABrightness {
				t.Fatalf("HA inbound S%d raw%d got%d want%d err=%v", scale, raw, got, *row.HABrightness, err)
			}
		}
	}
}
