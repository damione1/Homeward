package integrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

func nha016Instance(t *testing.T, value string) domain.IntegrationInstanceID {
	t.Helper()
	id, err := domain.ParseIntegrationInstanceID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nha016Candidate(t *testing.T, source, display string) integrations.Candidate {
	t.Helper()
	key, err := integrations.NewSourceKey("controller", source)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{{
		SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: display, Endpoint: "relay",
		Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func nha016Observation(t *testing.T, at time.Time) integrations.StateObservation {
	t.Helper()
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	value := domain.NewSwitchValue(false)
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func TestNHA016FakeDriverSessionOwnershipAndReconnect(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "fake-a")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	driver, err := fake.NewDriver(at)
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
	if value, _ := device.Value(); value == "controller" {
		t.Fatal("driver source key became Host device ID")
	}
	if value, _ := entity.Value(); value == "relay" {
		t.Fatal("driver source key became Host entity ID")
	}
	declarations, err := host.Capabilities(entity)
	if err != nil || len(declarations) != 1 || declarations[0].SchemaId != contracts.SwitchSchema || declarations[0].GetSwitch() == nil || declarations[0].GetSwitch().SetOn {
		t.Fatalf("capability not registered in shared catalogue: %+v %v", declarations, err)
	}
	declarations[0].DisplayName = "mutated by caller"
	again, _ := host.Capabilities(entity)
	if again[0].DisplayName != "Fake relay" {
		t.Fatal("caller mutated admitted capability")
	}
	state, ok := fixture.State(entity)
	if !ok {
		t.Fatal("fake did not publish state")
	}
	_, _, _, firstSource, revision, err := state.Components()
	if err != nil || revision != 1 {
		t.Fatalf("first state revision: %d %v", revision, err)
	}
	if value, ok := state.CurrentValue(); !ok {
		t.Fatal("fake state absent")
	} else if on, ok := value.Switch(); !ok || on {
		t.Fatal("fake state changed")
	}
	firstSessionID, _, _ := firstSource.Components()
	events := fixture.Events()
	if len(events) != 1 || events[0].Entity != entity {
		t.Fatalf("fake event count/identity: %+v", events)
	}
	eventSessionID, _, _ := events[0].Event.Source.Components()
	if firstSessionID != eventSessionID {
		t.Fatal("state and event source sessions differ")
	}
	if health, ok := fixture.Health(instance); !ok {
		t.Fatal("fake health absent")
	} else if status, _, _ := health.Components(); status != integrations.HealthReady {
		t.Fatal("fake health was not ready")
	}

	foreign, err := host.OpenSession(nha016Instance(t, "fake-b"))
	if err != nil {
		t.Fatal(err)
	}
	observation := nha016Observation(t, at.Add(time.Second))
	event, _ := integrations.NewDeviceEvent("fixture.edge", at, nil)
	if err := foreign.Publish(ctx, binding, observation); !errors.Is(err, integrations.ErrForeignBinding) {
		t.Fatalf("foreign state admitted: %v", err)
	}
	if err := foreign.Emit(ctx, binding, event); !errors.Is(err, integrations.ErrForeignBinding) {
		t.Fatalf("foreign event admitted: %v", err)
	}
	if got, _ := fixture.State(entity); got != state {
		t.Fatal("foreign publish mutated state")
	}
	if len(fixture.Events()) != 1 {
		t.Fatal("foreign emit mutated event log")
	}

	if err := host.Revoke(session); err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(ctx, binding, observation); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("revoked state admitted: %v", err)
	}
	if err := session.Emit(ctx, binding, event); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("revoked event admitted: %v", err)
	}
	if err := session.ReportHealth(ctx, mustNHA016Health(t)); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("revoked health admitted: %v", err)
	}
	if got, _ := fixture.State(entity); got != state {
		t.Fatal("revoked publish mutated state")
	}
	if len(fixture.Events()) != 1 {
		t.Fatal("revoked emit mutated event log")
	}

	reconnected, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconnected.Publish(ctx, binding, observation); !errors.Is(err, integrations.ErrForeignBinding) {
		t.Fatalf("old binding admitted in new session: %v", err)
	}
	newDriver, _ := fake.NewDriver(at.Add(-time.Second))
	if err := newDriver.Start(ctx, reconnected); err != nil {
		t.Fatal(err)
	}
	newBinding, _ := newDriver.Binding()
	newDevice, newEntity, _ := newBinding.IDs()
	if newDevice != device || newEntity != entity {
		t.Fatal("reconnection changed Host-assigned IDs")
	}
	newState, _ := fixture.State(entity)
	_, _, times, newSource, revision, err := newState.Components()
	if err != nil || revision != 2 || !times.LastReported.Equal(at.Add(-time.Second)) {
		t.Fatalf("reconnection revision/wall clock: %+v %d %v", times, revision, err)
	}
	newSessionID, _, _ := newSource.Components()
	if newSessionID == firstSessionID {
		t.Fatal("reconnection reused source session")
	}
	if err := reconnected.Publish(ctx, newBinding, observation); err != nil {
		t.Fatalf("current owner rejected: %v", err)
	}
	if err := session.Publish(ctx, binding, observation); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("old session republished after reconnect: %v", err)
	}
	final, _ := fixture.State(entity)
	_, _, _, _, revision, _ = final.Components()
	if revision != 3 || len(fixture.Events()) != 2 {
		t.Fatal("current owner/state/event admission was not isolated")
	}
	replacement, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconnected.Publish(ctx, newBinding, observation); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("superseded session still published: %v", err)
	}
	replacementDriver, _ := fake.NewDriver(at.Add(2 * time.Second))
	if err := replacementDriver.Start(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	replacementBinding, _ := replacementDriver.Binding()
	if _, replacementEntity, _ := replacementBinding.IDs(); replacementEntity != entity {
		t.Fatal("replacement session lost stable Host ID")
	}
}

func mustNHA016Health(t *testing.T) integrations.HealthReport {
	t.Helper()
	h, err := integrations.NewHealthReport(integrations.HealthReady, "")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestNHA016InvalidInputsAndAtomicCapabilities(t *testing.T) {
	ctx := context.Background()
	host, _, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.OpenSession(domain.IntegrationInstanceID{}); err == nil {
		t.Fatal("zero integration instance admitted")
	}
	var nilAuthority *fake.Fixture
	catalog, _ := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if _, err := integrations.NewHost(nilAuthority, nilAuthority, catalog); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("typed nil dependencies admitted: %v", err)
	}
	if _, err := integrations.NewSourceKey("", "relay"); err == nil {
		t.Fatal("zero source key admitted")
	}
	if _, _, err := (integrations.SourceKey{}).Components(); err == nil {
		t.Fatal("zero source key components admitted")
	}
	if _, err := integrations.NewCandidate(integrations.SourceKey{}, []*registry.CapabilityDescription{{}}); err == nil {
		t.Fatal("zero candidate key admitted")
	}
	if _, err := integrations.NewDeviceEvent("", time.Now(), nil); err == nil {
		t.Fatal("zero event admitted")
	}
	if _, err := integrations.NewHealthReport(integrations.HealthUnspecified, ""); err == nil {
		t.Fatal("zero health admitted")
	}
	if _, err := integrations.NewStateObservation("relay", domain.Quality{}, nil, nil, time.Now(), nil); err == nil {
		t.Fatal("zero quality admitted")
	}
	if _, err := integrations.NewStateObservation("", domain.Quality{}, nil, nil, time.Now(), nil); err == nil {
		t.Fatal("zero endpoint admitted")
	}
	if _, _, err := (integrations.Binding{}).IDs(); err == nil {
		t.Fatal("zero binding admitted")
	}
	var nilSession *integrations.Session
	if err := nilSession.Publish(ctx, integrations.Binding{}, integrations.StateObservation{}); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("typed nil session admitted: %v", err)
	}
	driver, _ := fake.NewDriver(time.Now())
	if err := driver.Start(ctx, nilSession); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("typed nil Host admitted by fake: %v", err)
	}

	session, _ := host.OpenSession(nha016Instance(t, "atomic"))
	good := nha016Candidate(t, "first", "original")
	bindings, err := session.Discover(ctx, []integrations.Candidate{good})
	if err != nil {
		t.Fatal(err)
	}
	_, entity, _ := bindings[0].IDs()
	badKey, _ := integrations.NewSourceKey("controller", "second")
	bad, _ := integrations.NewCandidate(badKey, []*registry.CapabilityDescription{{
		SchemaId: contracts.SwitchSchema, Major: 0, DisplayName: "bad", Endpoint: "relay",
		Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{}},
	}})
	if _, err := session.Discover(ctx, []integrations.Candidate{nha016Candidate(t, "first", "replacement"), bad}); err == nil {
		t.Fatal("invalid batch admitted")
	}
	descriptions, _ := host.Capabilities(entity)
	if len(descriptions) != 1 || descriptions[0].DisplayName != "original" {
		t.Fatal("invalid batch partially replaced catalogue")
	}
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if err := session.Publish(ctx, bindings[0], nha016Observation(t, at)); err != nil {
		t.Fatalf("existing binding lost after invalid discovery: %v", err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(false)
	wrongEndpoint, _ := integrations.NewStateObservation("other", quality, &value, nil, at, nil)
	if err := session.Publish(ctx, bindings[0], wrongEndpoint); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("unadvertised endpoint admitted: %v", err)
	}
	if _, err := session.Discover(ctx, []integrations.Candidate{nha016Candidate(t, "first", "replacement")}); err != nil {
		t.Fatal(err)
	}
	descriptions, _ = host.Capabilities(entity)
	if len(descriptions) != 1 || descriptions[0].DisplayName != "replacement" {
		t.Fatal("valid rediscovery did not replace catalogue")
	}
}

func TestNHA016FixtureResolutionIsAtomicForDirectSDKCall(t *testing.T) {
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "direct-fixture")
	valid, _ := integrations.NewSourceKey("controller", "first")
	if _, err := fixture.Resolve(context.Background(), instance, []integrations.SourceKey{valid, {}}); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("invalid later key admitted: %v", err)
	}
	resolved, err := fixture.Resolve(context.Background(), instance, []integrations.SourceKey{valid})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 {
		t.Fatal("fixture did not resolve valid key")
	}
	device, _ := resolved[0].Device.Value()
	entity, _ := resolved[0].Entity.Value()
	if device != "fixture-device-1" || entity != "Host:fixture-1" {
		t.Fatal("rejected batch allocated fixture IDs")
	}
	if _, err := fixture.Resolve(nil, instance, []integrations.SourceKey{valid}); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatal("nil fixture context admitted")
	}
	var zeroFixture fake.Fixture
	if _, err := zeroFixture.Resolve(context.Background(), instance, []integrations.SourceKey{valid}); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatal("zero fixture admitted")
	}
}

func TestNHA016MeasurementUsesAdvertisedMetadata(t *testing.T) {
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, _ := host.OpenSession(nha016Instance(t, "meter"))
	key, _ := integrations.NewSourceKey("meter", "power")
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{{
		SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Power", Endpoint: "power",
		Constraints: &registry.CapabilityDescription_Measurement{Measurement: &registry.MeasurementConstraints{
			Unit: "W", Precision: 1, Semantic: registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT,
			Aggregation: registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	_, entity, _ := bindings[0].IDs()
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, example := range []struct {
		unit      string
		precision int
		allowed   bool
	}{{"W", 1, true}, {"W", 2, false}, {"kW", 1, false}} {
		unit, _ := domain.NewUnitCatalog().Parse(example.unit)
		measurement, err := domain.NewMeasurement(0, unit, example.precision, domain.SemanticMeasurement, domain.AggregationCurrent)
		if err != nil {
			t.Fatal(err)
		}
		value, _ := domain.NewMeasurementValue(measurement)
		observation, err := integrations.NewStateObservation("power", quality, &value, nil, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = session.Publish(context.Background(), bindings[0], observation)
		if example.allowed && err != nil {
			t.Fatalf("valid zero measurement rejected: %v", err)
		}
		if !example.allowed && err == nil {
			t.Fatalf("invalid measurement accepted: %+v", example)
		}
	}
	state, ok := fixture.State(entity)
	if !ok {
		t.Fatal("valid measurement missing")
	}
	_, _, _, _, revision, _ := state.Components()
	if revision != 1 {
		t.Fatal("rejected measurements mutated state")
	}
}

func TestNHA016BooleanStateRequiresCompatibleDeclaration(t *testing.T) {
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, _ := host.OpenSession(nha016Instance(t, "typed-values"))
	for _, example := range []struct {
		source      string
		description *registry.CapabilityDescription
		allowed     bool
	}{
		{"binary", &registry.CapabilityDescription{SchemaId: contracts.BinarySchema, Major: 1, DisplayName: "Contact", Endpoint: "relay", Constraints: &registry.CapabilityDescription_Binary{Binary: &registry.BinaryConstraints{}}}, true},
		{"measurement", &registry.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Power", Endpoint: "relay", Constraints: &registry.CapabilityDescription_Measurement{Measurement: &registry.MeasurementConstraints{Unit: "W", Precision: 1, Semantic: registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}, false},
	} {
		key, _ := integrations.NewSourceKey("controller", example.source)
		candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{example.description})
		if err != nil {
			t.Fatal(err)
		}
		bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
		if err != nil {
			t.Fatal(err)
		}
		_, entity, _ := bindings[0].IDs()
		err = session.Publish(context.Background(), bindings[0], nha016Observation(t, time.Now()))
		_, published := fixture.State(entity)
		if example.allowed && (err != nil || !published) {
			t.Fatalf("compatible boolean report rejected: %v", err)
		}
		if !example.allowed && (err == nil || published) {
			t.Fatalf("unrelated boolean report admitted: %v", err)
		}
	}
}

type nha016CollisionAuthority struct {
	base     *fake.Fixture
	force    domain.IntegrationInstanceID
	identity integrations.ResolvedIdentity
}

func (a *nha016CollisionAuthority) Resolve(ctx context.Context, instance domain.IntegrationInstanceID, keys []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	if instance == a.force {
		return []integrations.ResolvedIdentity{a.identity}, nil
	}
	return a.base.Resolve(ctx, instance, keys)
}

func TestNHA016AuthorityCannotAssignAnotherLiveOwnersEntity(t *testing.T) {
	ctx := context.Background()
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	foreign := nha016Instance(t, "foreign")
	authority := &nha016CollisionAuthority{base: fixture, force: foreign}
	host, err := integrations.NewHost(authority, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := host.OpenSession(nha016Instance(t, "owner"))
	bindings, err := owner.Discover(ctx, []integrations.Candidate{nha016Candidate(t, "original", "Original")})
	if err != nil {
		t.Fatal(err)
	}
	device, entity, _ := bindings[0].IDs()
	authority.identity = integrations.ResolvedIdentity{Device: device, Entity: entity}
	other, _ := host.OpenSession(foreign)
	if _, err := other.Discover(ctx, []integrations.Candidate{nha016Candidate(t, "colliding", "Collision")}); !errors.Is(err, integrations.ErrIdentityConflict) {
		t.Fatalf("colliding authority stole entity: %v", err)
	}
	descriptions, err := catalog.Descriptions(entity)
	if err != nil || len(descriptions) != 1 || descriptions[0].DisplayName != "Original" {
		t.Fatal("colliding batch mutated shared catalogue")
	}
	if err := owner.Publish(ctx, bindings[0], nha016Observation(t, time.Now())); err != nil {
		t.Fatalf("current owner lost admission: %v", err)
	}
}

type nha016BlockingAuthority struct {
	base    *fake.Fixture
	blocked domain.IntegrationInstanceID
	entered chan struct{}
	release chan struct{}
}

func (a *nha016BlockingAuthority) Resolve(ctx context.Context, instance domain.IntegrationInstanceID, keys []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	if instance == a.blocked {
		close(a.entered)
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return a.base.Resolve(ctx, instance, keys)
}

func TestNHA016BlockedDiscoveryDoesNotStopPublicationAndRevocationWins(t *testing.T) {
	ctx := context.Background()
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	blocked := nha016Instance(t, "slow-registry")
	authority := &nha016BlockingAuthority{base: fixture, blocked: blocked, entered: make(chan struct{}), release: make(chan struct{})}
	catalog, _ := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	host, err := integrations.NewHost(authority, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	active, _ := host.OpenSession(nha016Instance(t, "active"))
	bindings, err := active.Discover(ctx, []integrations.Candidate{nha016Candidate(t, "first", "First")})
	if err != nil {
		t.Fatal(err)
	}
	slow, _ := host.OpenSession(blocked)
	slowCandidate := nha016Candidate(t, "second", "Second")
	discoveryDone := make(chan error, 1)
	go func() {
		_, err := slow.Discover(ctx, []integrations.Candidate{slowCandidate})
		discoveryDone <- err
	}()
	select {
	case <-authority.entered:
	case <-time.After(time.Second):
		t.Fatal("discovery never reached registry")
	}
	publishDone := make(chan error, 1)
	observation := nha016Observation(t, time.Now())
	go func() { publishDone <- active.Publish(ctx, bindings[0], observation) }()
	select {
	case err := <-publishDone:
		if err != nil {
			t.Fatalf("live session publish failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled identity registry blocked unrelated state publication")
	}
	if err := host.Revoke(slow); err != nil {
		t.Fatal(err)
	}
	close(authority.release)
	select {
	case err := <-discoveryDone:
		if !errors.Is(err, integrations.ErrInactiveSession) {
			t.Fatalf("revoked discovery committed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("revoked discovery did not return")
	}
	if _, err := slow.Discover(ctx, []integrations.Candidate{nha016Candidate(t, "third", "Third")}); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("revoked session rediscovered: %v", err)
	}
}
