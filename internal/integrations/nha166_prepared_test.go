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
	"github.com/damione1/Homeward/internal/scenes"
)

func nha166ReadOnlyLight(t *testing.T) nha179Rig {
	t.Helper()
	host, fixture, _, generation := nha166Host(t)
	instance := nha016Instance(t, "nha179-synthetic")
	key, err := integrations.NewSourceKey("nha179-controller", "level")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := fixture.Resolve(context.Background(), instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{nha022Record(t, ids[0], instance, key, "light.nha179", "light.nha179", false)}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	service, err := integrations.NewVolatileCommandService(host, generation, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	rig := nha179Rig{host: host, fixture: fixture, session: session, key: key, service: service, gen: generation, driver: &nha025Driver{}}
	rig.discoverMQTT(t, 254)
	level := 30.0
	rig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	return rig
}
func TestNHA166PreparedLightFanAndSceneRefuseAtCaptureBeforeLedger(t *testing.T) {
	rig := nha166ReadOnlyLight(t)
	lightSlot := nha179Slot(t, rig.service, nha179Call(t, "light.turn_on", "{brightness_pct: 50}"), "nha166-light")
	if _, err := lightSlot.PrepareOnce(context.Background()); !errors.Is(err, integrations.ErrCommandRouteUnavailable) {
		t.Fatal("observation minted opaque prepared light", err)
	}
	if _, err := lightSlot.Dispatch(context.Background()); !errors.Is(err, integrations.ErrCommandRouteUnavailable) {
		t.Fatal("observation light slot reached ledger", err)
	}
	ctx := context.Background()
	instance := nha016Instance(t, "nha179-synthetic")
	key, _ := integrations.NewSourceKey("nha046-fan-device", "speed")
	ids, err := rig.fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	alias := nha022Alias(t, "fan.living_room_ceiling_fan")
	if err := rig.fixture.PublishRegistry(ctx, []integrations.RegistryRecord{nha022Record(t, ids[0], instance, key, "fan.living_room_ceiling_fan", "fan.living_room_ceiling_fan", false)}); err != nil {
		t.Fatal(err)
	}
	description := &registry.CapabilityDescription{SchemaId: contracts.FanSchema, Major: 1, Endpoint: "speed", DisplayName: "Synthetic fan", Constraints: &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{SpeedOptions: []string{"Off", "Low", "Medium", "MediumHigh", "High"}}}}
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = candidate.WithCasetaHAFan4V1(alias)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := rig.session.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value, _ := domain.NewFanStateValue(domain.FanSpeedLow)
	now := time.Now()
	observed, err := integrations.NewStateObservation("speed", quality, &value, &now, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.session.Publish(ctx, bindings[0], observed); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.service.CaptureFan(ctx, alias); !errors.Is(err, integrations.ErrCommandRouteUnavailable) {
		t.Fatal("observation minted opaque prepared fan", err)
	}
	sceneService, _ := nha046SceneService(t, rig, &nha179Clock{now: time.Now()}, nil)
	sceneAlias := nha022Alias(t, "scene.ceilling_fan_day")
	ref, err := sceneService.Declarative(sceneAlias)
	if err != nil {
		t.Fatal(err)
	}
	invocation, _ := domain.ParseCommandID("nha166-scene")
	slot, err := scenes.NewPreparationSlot(sceneService, ref, nha022Actor(t), invocation, nil, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	preparedScene, err := slot.PrepareOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := sceneService.ApplyPrepared(ctx, preparedScene)
	if err != nil || result.Aggregate() != scenes.AggregateNone {
		t.Fatal("observation scene did not refuse its members", err)
	}
	status, err := rig.service.Status()
	if err != nil || status.Entries != 0 || status.RetainedBytes != 0 || rig.host.CommandDriverEntries() != 0 || rig.driver.Count() != 0 {
		t.Fatal("prepared light/fan/scene mutated admission or entered driver", err, status)
	}
}
