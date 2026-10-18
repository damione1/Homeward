package integrations_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/scope"
)

func TestNHA046PreparedLightCTAndNativeUnits(t *testing.T) {
	rig := nha179Setup(t, false)
	level := 35.0
	rig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	description := nha179Description(false)
	description.GetLight().ColorTempMireds = &registry.NumberBounds{Minimum: 250, Maximum: 454}
	candidate, err := integrations.NewCandidate(rig.key, []*registry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = candidate.WithLightEncoding(rig.encoding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.session.Discover(context.Background(), []integrations.Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("light.nha179")
	capture, err := rig.service.CaptureLight(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	brightness, ct, transition := uint8(89), uint32(357), 2.0
	id, _ := domain.ParseCommandID("nha046-light-ct")
	prepared, err := capture.PrepareHAWithCT(nha022Actor(t), id, time.Now().Add(time.Minute), true, &brightness, &ct, &transition, false)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := prepared.Request()
	if err != nil || wire.GetArguments().GetSetLight().GetColorTempMireds() != 357 ||
		wire.GetArguments().GetSetLight().BrightnessPercent == nil || wire.GetArguments().GetSetLight().TransitionSeconds == nil {
		t.Fatalf("CT must share the brightness/transition intention: %v %v", wire, err)
	}
	if q, ok := prepared.MQTTTarget(); !ok || q != 89 {
		t.Fatalf("prepared b89 target must be q89/254: %d %t", q, ok)
	}
	response, err := rig.service.DispatchPrepared(context.Background(), prepared)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || rig.driver.Count() != 1 {
		t.Fatalf("CT dispatch: %v %v", response, err)
	}
	if _, err := rig.service.DispatchPrepared(context.Background(), prepared); err != nil || rig.driver.Count() != 1 {
		t.Fatal("CT retry emitted a second command")
	}

	capture, err = rig.service.CaptureLight(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	secondID, _ := domain.ParseCommandID("nha046-light-ct-profile-fence")
	stale, err := capture.PrepareHAWithCT(nha022Actor(t), secondID, time.Now().Add(time.Minute), true, &brightness, &ct, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	newEncoding, _ := integrations.NewMQTTLightEncoding(255, sha256.Sum256([]byte("nha046-scale255")), false)
	changed, _ := integrations.NewCandidate(rig.key, []*registry.CapabilityDescription{description})
	changed, _ = changed.WithLightEncoding(newEncoding)
	if _, err := rig.session.Discover(context.Background(), []integrations.Candidate{changed}); err != nil {
		t.Fatal(err)
	}
	before := rig.driver.Count()
	response, err = rig.service.DispatchPrepared(context.Background(), stale)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || rig.driver.Count() != before {
		t.Fatalf("profile change before first dispatch sent: %v %v", response, err)
	}

	nativeRig := nha179Setup(t, true)
	nativeRig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	capture, err = nativeRig.service.CaptureLight(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	fraction := 37.5
	nativeID, _ := domain.ParseCommandID("nha046-native-fraction")
	native, err := capture.PrepareNative(nha022Actor(t), nativeID, time.Now().Add(time.Minute), true, &fraction, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	nativeWire, _ := native.Request()
	if nativeWire.GetArguments().GetSetLight().GetBrightnessPercent() != fraction {
		t.Fatal("native percent was rounded through HA 0..255")
	}
	if _, hasQ := native.MQTTTarget(); hasQ {
		t.Fatal("Caseta native value acquired MQTT target")
	}
	if _, err := nativeRig.service.DispatchPrepared(context.Background(), native); err != nil || nativeRig.driver.Count() != 1 {
		t.Fatal("native exact fraction failed")
	}
}

func TestNHA046TemporarySceneCaptureCASAndLedger(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	clock := &nha179Clock{now: now}
	rig := nha179SetupWithClock(t, false, clock)
	level := 37.5
	rig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	service, err := scenes.NewService(catalogue, rig.fixture.Sequencer(), rig.gen, rig.service, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	alias, _ := domain.ParseHAAlias("light.nha179")
	id, _ := domain.ParseHAAlias("scene.nha046_temporary")
	ref, omitted, err := service.CreateTemporary(ctx, id, nil, []domain.HAAlias{alias}, time.Hour, nil)
	if err != nil || len(omitted) != 0 || ref.Version() == 0 {
		t.Fatalf("temporary capture: %v %v", err, omitted)
	}
	invocation, _ := domain.ParseCommandID("nha046-scene-occurrence")
	slot, err := scenes.NewPreparationSlot(service, ref, nha022Actor(t), invocation, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(ctx)
	if err != nil || len(prepared.Members()) != 1 || prepared.Members()[0].Outcome() != scenes.OutcomePrepared {
		t.Fatalf("scene preparation: %v %+v", err, prepared.Members())
	}
	preparedAgain, err := slot.PrepareOnce(ctx)
	if err != nil || len(preparedAgain.Members()) != 1 {
		t.Fatal("invocation slot was rebuilt")
	}
	result, err := service.ApplyPrepared(ctx, prepared)
	if err != nil || result.Aggregate() != scenes.AggregateComplete || rig.driver.Count() != 1 {
		t.Fatalf("scene apply: %v %+v response=%v sends=%d", err, result, result.Members()[0].Response(), rig.driver.Count())
	}
	if _, err := service.ApplyPrepared(ctx, prepared); err != nil || rig.driver.Count() != 1 {
		t.Fatal("scene retry sent again")
	}
	copy := prepared.Members()
	copy[0] = scenes.MemberResult{}
	if prepared.Members()[0].Outcome() != scenes.OutcomePrepared {
		t.Fatal("prepared member view is mutable")
	}

	changed := 60.0
	rig.publish(t, true, &changed, domain.FreshnessFresh, domain.ProvenanceObserved)
	staleInvocation, _ := domain.ParseCommandID("nha046-scene-stale-occurrence")
	staleSlot, err := scenes.NewPreparationSlot(service, ref, nha022Actor(t), staleInvocation, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	stale, err := staleSlot.PrepareOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	staleResult, err := service.ApplyPrepared(ctx, stale)
	if err != nil || staleResult.Aggregate() != scenes.AggregateNone || rig.driver.Count() != 1 {
		t.Fatalf("revision mutation sent: %v %+v", err, staleResult)
	}
	version := ref.Version()
	replaced, _, err := service.CreateTemporary(ctx, id, nil, []domain.HAAlias{alias}, time.Hour, &version)
	if err != nil || replaced.Version() <= ref.Version() {
		t.Fatalf("CAS replacement: %v %d", err, replaced.Version())
	}
	if _, _, err := service.CreateTemporary(ctx, id, nil, []domain.HAAlias{alias}, time.Hour, &version); err == nil {
		t.Fatal("stale CAS replaced scene")
	}
	if old, err := service.ApplyPrepared(ctx, prepared); err != nil || old.Aggregate() != scenes.AggregateComplete || rig.driver.Count() != 1 {
		t.Fatalf("recorded result was lost after replacement: %v %+v", err, old)
	}
	unsentID, _ := domain.ParseCommandID("nha046-scene-never-admitted")
	unsentSlot, err := scenes.NewPreparationSlot(service, replaced, nha022Actor(t), unsentID, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	unsent, err := unsentSlot.PrepareOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteTemporary(ctx, replaced); err != nil {
		t.Fatal(err)
	}
	if missing, err := service.ApplyPrepared(ctx, unsent); err != nil || missing.Aggregate() != scenes.AggregateNone || rig.driver.Count() != 1 {
		t.Fatalf("deleted scene acquired first send: %v %+v", err, missing)
	}
	if old, err := service.ApplyPrepared(ctx, prepared); err != nil || old.Aggregate() != scenes.AggregateComplete || rig.driver.Count() != 1 {
		t.Fatalf("recorded result was lost after delete: %v %+v", err, old)
	}
	recreated, _, err := service.CreateTemporary(ctx, id, nil, []domain.HAAlias{alias}, time.Hour, nil)
	if err != nil || recreated.Version() <= replaced.Version() {
		t.Fatal("delete/recreate reused version")
	}
	preAdmissionID, _ := domain.ParseCommandID("nha046-scene-pre-admission-canceled")
	preAdmissionSlot, err := scenes.NewPreparationSlot(service, recreated, nha022Actor(t), preAdmissionID, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	preAdmission, err := preAdmissionSlot.PrepareOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := service.ApplyPrepared(canceled, preAdmission); err != nil || rig.driver.Count() != 1 {
		t.Fatalf("canceled attempt reached driver: %v", err)
	}
	clock.Set(now.Add(2 * time.Hour))
	if _, err := service.Temporary(ctx, id); err == nil {
		t.Fatal("expired scene stayed visible")
	}
	if missing, err := service.ApplyPrepared(ctx, preAdmission); err != nil || missing.Aggregate() != scenes.AggregateNone || rig.driver.Count() != 1 {
		t.Fatalf("pre-reservation failure replayed after expiry: %v %+v", err, missing)
	}
	if old, err := service.ApplyPrepared(ctx, prepared); err != nil || old.Aggregate() != scenes.AggregateComplete || rig.driver.Count() != 1 {
		t.Fatalf("recorded result was lost after expiry: %v %+v", err, old)
	}
}

func TestNHA046CasetaFanProfilePreparedAndNativeDirect(t *testing.T) {
	rig := nha179Setup(t, false)
	ctx := context.Background()
	instance := nha016Instance(t, "nha179-synthetic")
	key, _ := integrations.NewSourceKey("nha046-fan-device", "speed")
	ids, err := rig.fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("fan.living_room_ceiling_fan")
	record := nha022Record(t, ids[0], instance, key, "fan.living_room_ceiling_fan", "fan.living_room_ceiling_fan", false)
	if err := rig.fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	description := &registry.CapabilityDescription{SchemaId: contracts.FanSchema, Major: 1, Endpoint: "speed",
		DisplayName: "Synthetic four-speed fan", Constraints: &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{
			SetOn: false, SpeedOptions: []string{"Off", "Low", "Medium", "MediumHigh", "High"}}}}
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
		t.Fatalf("fan discovery: %v", err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value, _ := domain.NewFanStateValue(domain.FanSpeedLow)
	now := time.Now()
	observation, err := integrations.NewStateObservation("speed", quality, &value, &now, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.session.Publish(ctx, bindings[0], observation); err != nil {
		t.Fatal(err)
	}
	sceneService, _ := nha046SceneService(t, rig, &nha179Clock{now: time.Now()}, nil)
	for _, sceneName := range []string{"scene.ceilling_fan_day", "scene.ceilling_fan_night"} {
		sceneAlias, _ := domain.ParseHAAlias(sceneName)
		ref, err := sceneService.Declarative(sceneAlias)
		if err != nil {
			t.Fatal(err)
		}
		invocation, _ := domain.ParseCommandID("nha046-" + sceneName)
		slot, err := scenes.NewPreparationSlot(sceneService, ref, nha022Actor(t), invocation, nil, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		plan, err := slot.PrepareOnce(ctx)
		if err != nil || len(plan.Members()) != 1 || plan.Members()[0].Outcome() != scenes.OutcomePrepared {
			t.Fatalf("%s fan75 scene not qualified: %v %+v", sceneName, err, plan.Members())
		}
	}
	capture, err := rig.service.CaptureFan(ctx, alias)
	if err != nil {
		t.Fatal(err)
	}
	pct := uint8(75)
	id, _ := domain.ParseCommandID("nha046-fan75")
	prepared, err := capture.PrepareHA(nha022Actor(t), id, now.Add(time.Minute), true, &pct)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := prepared.Request()
	setFan := wire.GetArguments().GetSetFan()
	if setFan == nil || setFan.On != nil || setFan.GetSpeed() != "MediumHigh" {
		t.Fatalf("fan75 not projected to speed alone: %v", wire)
	}
	response, err := rig.service.DispatchPreparedFan(ctx, prepared)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || rig.driver.Count() != 1 {
		t.Fatalf("fan dispatch: %v %v", response, err)
	}
	if _, err := rig.service.DispatchPreparedFan(ctx, prepared); err != nil || rig.driver.Count() != 1 {
		t.Fatal("fan retry sent twice")
	}

	capture, err = rig.service.CaptureFan(ctx, alias)
	if err != nil {
		t.Fatal(err)
	}
	secondID, _ := domain.ParseCommandID("nha046-fan-profile-fence")
	stale, err := capture.PrepareHA(nha022Actor(t), secondID, now.Add(time.Minute), true, &pct)
	if err != nil {
		t.Fatal(err)
	}
	unprofiled, _ := integrations.NewCandidate(key, []*registry.CapabilityDescription{description})
	if _, err := rig.session.Discover(ctx, []integrations.Candidate{unprofiled}); err != nil {
		t.Fatal(err)
	}
	response, err = rig.service.DispatchPreparedFan(ctx, stale)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || rig.driver.Count() != 1 {
		t.Fatalf("fan profile loss sent: %v %v", response, err)
	}
	if _, err := rig.service.CaptureFan(ctx, alias); err == nil {
		t.Fatal("unprofiled fan admitted HA projection")
	}
	direct, err := rig.service.CaptureDirect(ctx, alias, integrations.DirectFan)
	if err != nil {
		t.Fatal(err)
	}
	directID, _ := domain.ParseCommandID("nha046-native-fan")
	native, err := direct.PrepareFan(nha022Actor(t), directID, now.Add(time.Minute), domain.FanSpeedMediumHigh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.service.DispatchPreparedDirect(ctx, native); err != nil || rig.driver.Count() != 2 {
		t.Fatal("native fan changed after HA profile removal")
	}
}
