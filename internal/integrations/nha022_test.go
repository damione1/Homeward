package integrations_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha022Generation struct {
	mu       sync.Mutex
	revision uint64
}

type nha022PausingGeneration struct {
	entered chan struct{}
	release chan struct{}
}

func (g *nha022PausingGeneration) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	close(g.entered)
	select {
	case <-g.release:
		return fn(7)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type nha022Clock struct {
	mu    sync.Mutex
	now   time.Time
	calls int
}

func (c *nha022Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= 4 {
		return c.now.Add(2 * time.Minute)
	}
	return c.now
}

type nha022BlockingCodec struct {
	host    *integrations.Manager
	entity  domain.EntityID
	entered chan struct{}
	release chan struct{}
}

func (c *nha022BlockingCodec) Validate(proto.Message) error {
	if _, err := c.host.Capabilities(c.entity); err != nil {
		return err
	}
	close(c.entered)
	<-c.release
	return nil
}

func (g *nha022Generation) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn(g.revision)
}

type nha022Driver struct {
	mu         sync.Mutex
	calls      []integrations.AdmittedCommand
	entered    chan struct{}
	release    chan struct{}
	onDispatch func() error
	blocked    bool
}

func (d *nha022Driver) Dispatch(ctx context.Context, c integrations.AdmittedCommand) error {
	if d.onDispatch != nil {
		if err := d.onDispatch(); err != nil {
			return err
		}
	}
	d.mu.Lock()
	block := d.entered != nil && !d.blocked
	if block {
		d.blocked = true
	}
	d.mu.Unlock()
	if block {
		close(d.entered)
		select {
		case <-d.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	d.mu.Lock()
	d.calls = append(d.calls, c)
	d.mu.Unlock()
	return nil
}
func (d *nha022Driver) count() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.calls) }

func nha022Alias(t *testing.T, value string) domain.HAAlias {
	t.Helper()
	a, err := domain.ParseHAAlias(value)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func nha022Actor(t *testing.T) domain.ActorID {
	t.Helper()
	a, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func nha022Record(t *testing.T, identity integrations.ResolvedIdentity, instance domain.IntegrationInstanceID, key integrations.SourceKey, original, current string, retired bool) integrations.RegistryRecord {
	t.Helper()
	a := nha022Alias(t, original)
	b := nha022Alias(t, current)
	aliases := []domain.HAAlias{a}
	if b != a {
		aliases = append(aliases, b)
	}
	record, err := integrations.NewRegistryRecord(identity.Device, identity.Entity, instance, key, a, b, aliases, retired)
	if err != nil {
		t.Fatal(err)
	}
	return record
}
func nha022Request(t *testing.T, entity domain.EntityID, epoch string, schema, endpoint string, args *actions.ActionArgs) *actions.CommandRequest {
	t.Helper()
	id, _ := entity.Value()
	return &actions.CommandRequest{CommandId: "command-1", AdmissionEpoch: epoch, ConfigRevision: 7, Target: &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: id}}, CapabilitySchemaId: schema, Endpoint: endpoint, Arguments: args, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
}
func nha022Switch() *actions.ActionArgs {
	return &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}}
}
func nha022Light(value float64) *actions.ActionArgs {
	return &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{BrightnessPercent: &value}}}
}
func nha022Check(t *testing.T, router *integrations.CommandRouter, actor domain.ActorID, request *actions.CommandRequest, admitted bool) {
	t.Helper()
	result, err := router.Dispatch(context.Background(), actor, request)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || (result.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED) != admitted {
		t.Fatalf("dispatch=%v, want %v", result, admitted)
	}
	if !admitted && result.Error == nil {
		t.Fatalf("rejection lacks contract error: %v", result)
	}
}

func nha022Busy(t *testing.T, router *integrations.CommandRouter, actor domain.ActorID, request *actions.CommandRequest) {
	t.Helper()
	result, err := router.Dispatch(context.Background(), actor, request)
	if result != nil || !errors.Is(err, integrations.ErrCommandRouteBusy) {
		t.Fatalf("busy route = %v %v", result, err)
	}
}

func TestNHA022FakeDriverAdmissionAndFences(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "fake-relay")
	key, _ := integrations.NewSourceKey("fixture-controller", "relay")
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	record := nha022Record(t, ids[0], instance, key, "switch.relay_old", "switch.relay", false)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, _ := fake.NewActionDriver(time.Now())
	if err := driver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	router, err := integrations.NewCommandRouter(host, &nha022Generation{revision: 7})
	if err != nil {
		t.Fatal(err)
	}
	start, _ := fixture.Sequencer().Start()
	epoch, _, _ := start.Components()
	request := nha022Request(t, ids[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	request.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_HaAlias{HaAlias: "switch.relay_old"}}
	expiring, err := integrations.NewCommandRouterWithClock(host, &nha022Generation{revision: 7}, &nha022Clock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	nha022Check(t, expiring, nha022Actor(t), request, false)
	if len(driver.Commands()) != 0 {
		t.Fatal("deadline crossed after lease reached fake pilot")
	}
	nha022Check(t, router, nha022Actor(t), request, true)
	if got := len(driver.Commands()); got != 1 {
		t.Fatalf("fake pilot calls=%d", got)
	}
	bad := proto.Clone(request).(*actions.CommandRequest)
	bad.AdmissionEpoch = "previous-epoch"
	stale, err := router.Dispatch(ctx, nha022Actor(t), bad)
	if err != nil || stale == nil || stale.Error == nil || stale.Error.Code != actions.ErrorCode_ERROR_CODE_STALE_EPOCH {
		t.Fatalf("old epoch = %v %v", stale, err)
	}
	bad = proto.Clone(request).(*actions.CommandRequest)
	bad.ConfigRevision = 6
	stale, err = router.Dispatch(ctx, nha022Actor(t), bad)
	if err != nil || stale == nil || stale.Error == nil || stale.Error.Code != actions.ErrorCode_ERROR_CODE_STALE_CONFIGURATION {
		t.Fatalf("old config = %v %v", stale, err)
	}
	bad = proto.Clone(request).(*actions.CommandRequest)
	bad.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_HaAlias{HaAlias: "switch.missing"}}
	nha022Check(t, router, nha022Actor(t), bad, false)
	bad = proto.Clone(request).(*actions.CommandRequest)
	bad.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 100, protowire.VarintType), 1))
	nha022Check(t, router, nha022Actor(t), bad, false)
	bad = proto.Clone(request).(*actions.CommandRequest)
	bad.Arguments.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 100, protowire.VarintType), 1))
	nha022Check(t, router, nha022Actor(t), bad, false)
	bad = proto.Clone(request).(*actions.CommandRequest)
	bad.ExpiresAt = timestamppb.New(time.Now().Add(-time.Second))
	nha022Check(t, router, nha022Actor(t), bad, false)
	bad = proto.Clone(request).(*actions.CommandRequest)
	wrong := uint64(999)
	bad.ExpectedRevision = &wrong
	nha022Check(t, router, nha022Actor(t), bad, false)
	if got := len(driver.Commands()); got != 1 {
		t.Fatalf("invalid command reached fake pilot: %d", got)
	}
	replacement, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	nha022Check(t, router, nha022Actor(t), request, false)
	newDriver, _ := fake.NewActionDriver(time.Now())
	if err := newDriver.Start(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := host.AttachCommandDriver(replacement, newDriver); err != nil {
		t.Fatal(err)
	}
	nha022Check(t, router, nha022Actor(t), request, true)
	if len(driver.Commands()) != 1 || len(newDriver.Commands()) != 1 {
		t.Fatal("reconnection dispatched to old fake driver")
	}
	fixture.InvalidateRegistry()
	nha022Check(t, router, nha022Actor(t), request, false)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{nha022Record(t, ids[0], instance, key, "switch.relay_old", "switch.relay", true)}); err != nil {
		t.Fatal(err)
	}
	nha022Check(t, router, nha022Actor(t), request, false)
	if err := host.Revoke(replacement); err != nil {
		t.Fatal(err)
	}
}

func TestNHA022BrightnessAndMissingOwner(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "light-source")
	key, _ := integrations.NewSourceKey("lamp", "dimmer")
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{nha022Record(t, ids[0], instance, key, "light.lamp", "light.lamp", false)}); err != nil {
		t.Fatal(err)
	}
	start, _ := fixture.Sequencer().Start()
	epoch, _, _ := start.Components()
	router, err := integrations.NewCommandRouter(host, &nha022Generation{revision: 7})
	if err != nil {
		t.Fatal(err)
	}
	request := nha022Request(t, ids[0].Entity, epoch, contracts.LightSchema, "dimmer", nha022Light(50))
	nha022Check(t, router, nha022Actor(t), request, false) // SQL binding exists, no live owner
	session, _ := host.OpenSession(instance)
	candidate, _ := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "dimmer", DisplayName: "Fake dimmer", Constraints: &wireregistry.CapabilityDescription_Light{Light: &wireregistry.LightConstraints{BrightnessPercent: &wireregistry.NumberBounds{Minimum: 0, Maximum: 100}}}}})
	if _, err := session.Discover(ctx, []integrations.Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	driver := &nha022Driver{}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	bad := proto.Clone(request).(*actions.CommandRequest)
	bad.Arguments = nha022Light(101)
	nha022Check(t, router, nha022Actor(t), bad, false)
	nha022Check(t, router, nha022Actor(t), request, true)
	if driver.count() != 1 {
		t.Fatalf("only in-bound action must reach designated fake pilot: %d", driver.count())
	}
	driver.mu.Lock()
	admitted := driver.calls[0]
	driver.mu.Unlock()
	commandID, actorID, target, err := admitted.Identity()
	if err != nil || target != ids[0].Entity {
		t.Fatalf("admitted identity = %v %v", target, err)
	}
	commandText, _ := commandID.Value()
	actorText, _ := actorID.Value()
	if commandText != "command-1" || actorText != "trusted-actor" {
		t.Fatalf("command/actor = %s %s", commandText, actorText)
	}
	copy, err := admitted.Request()
	if err != nil || copy.AdmissionEpoch != epoch || copy.ConfigRevision != 7 || copy.ExpiresAt == nil || copy.GetTarget().GetEntityId() == "" {
		t.Fatalf("admitted envelope = %v %v", copy, err)
	}
}

func TestNHA022TwoBindingsOneExplicitDevice(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	device, _ := domain.ParseDeviceID("audited-shared-device")
	instances := []domain.IntegrationInstanceID{nha016Instance(t, "provider-a"), nha016Instance(t, "provider-b")}
	keys := make([]integrations.SourceKey, 2)
	ids := make([]integrations.ResolvedIdentity, 2)
	for i := range instances {
		keys[i], _ = integrations.NewSourceKey("source-"+string(rune('a'+i)), "relay")
		if err := fixture.DeclareSharedDevice(instances[i], "source-"+string(rune('a'+i)), device); err != nil {
			t.Fatal(err)
		}
		resolved, err := fixture.Resolve(ctx, instances[i], []integrations.SourceKey{keys[i]})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = resolved[0]
	}
	records := []integrations.RegistryRecord{nha022Record(t, ids[0], instances[0], keys[0], "switch.provider_a", "switch.provider_a", false), nha022Record(t, ids[1], instances[1], keys[1], "switch.provider_b", "switch.provider_b", false)}
	if err := fixture.PublishRegistry(ctx, records); err != nil {
		t.Fatal(err)
	}
	drivers := []*nha022Driver{{}, {}}
	sessions := make([]*integrations.Session, 2)
	for i := range instances {
		session, _ := host.OpenSession(instances[i])
		sessions[i] = session
		candidate, _ := integrations.NewCandidate(keys[i], []*wireregistry.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "Fake relay", Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: true}}}})
		if _, err := session.Discover(ctx, []integrations.Candidate{candidate}); err != nil {
			t.Fatal(err)
		}
		if err := host.AttachCommandDriver(session, drivers[i]); err != nil {
			t.Fatal(err)
		}
	}
	start, _ := fixture.Sequencer().Start()
	epoch, _, _ := start.Components()
	router, _ := integrations.NewCommandRouter(host, &nha022Generation{revision: 7})
	a := nha022Request(t, ids[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	b := nha022Request(t, ids[1].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	nha022Check(t, router, nha022Actor(t), a, false)
	nha022Check(t, router, nha022Actor(t), b, false)
	if err := host.DesignateCommandOwner(device, contracts.SwitchSchema, "relay", ids[0].Entity); err != nil {
		t.Fatal(err)
	}
	nha022Check(t, router, nha022Actor(t), a, true)
	nha022Check(t, router, nha022Actor(t), b, false)
	if drivers[0].count() != 1 || drivers[1].count() != 0 {
		t.Fatalf("wrong fake pilots called: %d %d", drivers[0].count(), drivers[1].count())
	}
	drivers[0].entered = make(chan struct{})
	drivers[0].release = make(chan struct{})
	drivers[0].onDispatch = func() error {
		if _, err := host.Capabilities(ids[0].Entity); err != nil {
			return err
		}
		_, err := fixture.Sequencer().Capture()
		return err
	}
	done := make(chan error, 1)
	actor := nha022Actor(t)
	go func() {
		response, err := router.Dispatch(ctx, actor, a)
		if err == nil && response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			err = integrations.ErrCommandRouteUnavailable
		}
		done <- err
	}()
	select {
	case <-drivers[0].entered:
	case <-time.After(3 * time.Second):
		t.Fatal("driver did not enter")
	}
	if err := host.DesignateCommandOwner(device, contracts.SwitchSchema, "relay", ids[1].Entity); !errors.Is(err, integrations.ErrCommandRouteBusy) {
		t.Fatalf("owner changed while slot in flight: %v", err)
	}
	nha022Busy(t, router, actor, b)
	nha022Busy(t, router, actor, a)
	if err := host.Revoke(sessions[0]); err != nil {
		t.Fatal(err)
	}
	close(drivers[0].release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leased dispatch stuck")
	}
	if err := host.DesignateCommandOwner(device, contracts.SwitchSchema, "relay", ids[1].Entity); err != nil {
		t.Fatal(err)
	}
	nha022Check(t, router, actor, b, true)
	nha022Check(t, router, actor, a, false)
	if drivers[0].count() != 2 || drivers[1].count() != 1 {
		t.Fatalf("lease crossed owner change: %d %d", drivers[0].count(), drivers[1].count())
	}
}

func TestNHA022AliasCollisionFailsClosed(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "collision")
	a, _ := integrations.NewSourceKey("one", "relay")
	b, _ := integrations.NewSourceKey("two", "relay")
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{a, b})
	if err != nil {
		t.Fatal(err)
	}
	records := []integrations.RegistryRecord{nha022Record(t, ids[0], instance, a, "switch.collision", "switch.collision", false), nha022Record(t, ids[1], instance, b, "switch.collision", "switch.collision", false)}
	if err := fixture.PublishRegistry(ctx, records); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.ResolveCommandAlias(nha022Alias(t, "switch.collision")); err == nil {
		t.Fatal("ambiguous alias resolved by map order")
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]integrations.Candidate, 2)
	for i, key := range []integrations.SourceKey{a, b} {
		candidates[i], err = integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "Fake relay", Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: true}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := session.Discover(ctx, candidates); err != nil {
		t.Fatal(err)
	}
	driver := &nha022Driver{}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	start, _ := fixture.Sequencer().Start()
	epoch, _, _ := start.Components()
	router, _ := integrations.NewCommandRouter(host, &nha022Generation{revision: 7})
	request := nha022Request(t, ids[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	request.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_HaAlias{HaAlias: "switch.collision"}}
	nha022Check(t, router, nha022Actor(t), request, false)
	if driver.count() != 0 {
		t.Fatal("ambiguous alias reached fake pilot")
	}
}

func TestNHA022EndpointSerializationAcrossCapabilities(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "multi-capability")
	key, _ := integrations.NewSourceKey("controller", "function")
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{nha022Record(t, ids[0], instance, key, "switch.multicap", "switch.multicap", false)}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{
		{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "shared", DisplayName: "Fake switch", Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: true}}},
		{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "shared", DisplayName: "Fake light", Constraints: &wireregistry.CapabilityDescription_Light{Light: &wireregistry.LightConstraints{BrightnessPercent: &wireregistry.NumberBounds{Minimum: 0, Maximum: 100}}}},
		{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "other", DisplayName: "Fake other", Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: true}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Discover(ctx, []integrations.Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	driver := &nha022Driver{entered: make(chan struct{}), release: make(chan struct{})}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	router, err := integrations.NewCommandRouter(host, &nha022Generation{revision: 7})
	if err != nil {
		t.Fatal(err)
	}
	start, _ := fixture.Sequencer().Start()
	epoch, _, _ := start.Components()
	first := nha022Request(t, ids[0].Entity, epoch, contracts.SwitchSchema, "shared", nha022Switch())
	actor := nha022Actor(t)
	done := make(chan error, 1)
	go func() {
		response, err := router.Dispatch(ctx, actor, first)
		if err == nil && response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			err = integrations.ErrCommandRouteUnavailable
		}
		done <- err
	}()
	select {
	case <-driver.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first dispatch did not enter")
	}
	sameEndpoint := nha022Request(t, ids[0].Entity, epoch, contracts.LightSchema, "shared", nha022Light(50))
	nha022Busy(t, router, actor, sameEndpoint)
	otherEndpoint := nha022Request(t, ids[0].Entity, epoch, contracts.SwitchSchema, "other", nha022Switch())
	nha022Check(t, router, actor, otherEndpoint, true)
	close(driver.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first dispatch stuck")
	}
	if driver.count() != 2 {
		t.Fatalf("endpoint routing count=%d", driver.count())
	}
}

func TestNHA022AliasCollisionAfterResolution(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "alias-race")
	a, _ := integrations.NewSourceKey("source-a", "relay")
	b, _ := integrations.NewSourceKey("source-b", "relay")
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{a, b})
	if err != nil {
		t.Fatal(err)
	}
	first := nha022Record(t, ids[0], instance, a, "switch.race", "switch.race", false)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{first}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(a, []*wireregistry.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "Fake relay", Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Discover(ctx, []integrations.Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	driver := &nha022Driver{}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	generation := &nha022PausingGeneration{entered: make(chan struct{}), release: make(chan struct{})}
	router, err := integrations.NewCommandRouter(host, generation)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := fixture.Sequencer().Start()
	epoch, _, _ := start.Components()
	request := nha022Request(t, ids[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	request.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_HaAlias{HaAlias: "switch.race"}}
	actor := nha022Actor(t)
	type outcome struct {
		response *actions.CommandResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() { response, err := router.Dispatch(ctx, actor, request); done <- outcome{response, err} }()
	select {
	case <-generation.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("target was not resolved")
	}
	second := nha022Record(t, ids[1], instance, b, "switch.race", "switch.race", false)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{first, second}); err != nil {
		t.Fatal(err)
	}
	close(generation.release)
	select {
	case result := <-done:
		if result.err != nil || result.response == nil || result.response.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("alias collision dispatched: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("admission stuck")
	}
	if driver.count() != 0 {
		t.Fatal("stale alias reached fake pilot")
	}
}

func TestNHA022PluginValidationOutsideHostAndRevocationFence(t *testing.T) {
	ctx := context.Background()
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../contracts/golden/extension-description.bin")
	if err != nil {
		t.Fatal(err)
	}
	description := new(wireregistry.CapabilityDescription)
	if err := proto.Unmarshal(data, description); err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "plugin-source")
	key, _ := integrations.NewSourceKey("plugin-device", "function")
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{nha022Record(t, ids[0], instance, key, "switch.plugin", "switch.plugin", false)}); err != nil {
		t.Fatal(err)
	}
	codec := &nha022BlockingCodec{host: host, entity: ids[0].Entity, entered: make(chan struct{}), release: make(chan struct{})}
	if err := catalog.RegisterExtensionCodec(description, codec); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Discover(ctx, []integrations.Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	driver := &nha022Driver{}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	router, err := integrations.NewCommandRouter(host, &nha022Generation{revision: 7})
	if err != nil {
		t.Fatal(err)
	}
	start, _ := fixture.Sequencer().Start()
	epoch, _, _ := start.Components()
	args := &actions.ActionArgs{Arguments: &actions.ActionArgs_Extension{Extension: &actions.ExtensionPayload{SchemaId: description.SchemaId, Major: description.Major, DescriptorSha256: description.DescriptorSha256, ProtobufPayload: []byte{0x08, 0x05}}}}
	request := nha022Request(t, ids[0].Entity, epoch, description.SchemaId, description.Endpoint, args)
	type outcome struct {
		response *actions.CommandResponse
		err      error
	}
	done := make(chan outcome, 1)
	actor := nha022Actor(t)
	go func() { response, err := router.Dispatch(ctx, actor, request); done <- outcome{response, err} }()
	select {
	case <-codec.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("plugin did not enter")
	}
	revoked := make(chan error, 1)
	go func() { revoked <- host.Revoke(session) }()
	select {
	case err := <-revoked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		close(codec.release)
		t.Fatal("plugin held Host lock")
	}
	close(codec.release)
	select {
	case result := <-done:
		if result.err != nil || result.response == nil || result.response.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("revoked session dispatched: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("plugin admission stuck")
	}
	if driver.count() != 0 {
		t.Fatal("revoked plugin action reached fake pilot")
	}
}
