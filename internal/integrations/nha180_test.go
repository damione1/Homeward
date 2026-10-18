package integrations_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

func TestNHA180CapturedLightTargetAndDedupFence(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	_, key, err := rig.binding.Source()
	if err != nil {
		t.Fatal(err)
	}
	description := &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "relay", DisplayName: "Fake quantized light", Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{SetOn: true, BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100}}}}
	encoding, err := integrations.NewMQTTLightEncoding(254, sha256.Sum256([]byte("nha180-fixture-s254")), false)
	if err != nil {
		t.Fatal(err)
	}
	discover := func(profile integrations.LightEncoding) {
		t.Helper()
		candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{description})
		if err != nil {
			t.Fatal(err)
		}
		candidate, err = candidate.WithLightEncoding(profile)
		if err != nil {
			t.Fatal(err)
		}
		bindings, err := rig.session.Discover(context.Background(), []integrations.Candidate{candidate})
		if err != nil || len(bindings) != 1 {
			t.Fatalf("light discovery: %v", err)
		}
		rig.binding = bindings[0]
	}
	discover(encoding)
	on, percent := true, 30.0
	rig.request.CapabilitySchemaId = contracts.LightSchema
	rig.request.Arguments = &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{On: &on, BrightnessPercent: &percent}}}
	var cause domain.Causality
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		wire, err := command.Request()
		if err != nil {
			return err
		}
		arg := wire.GetArguments().GetSetLight()
		if arg == nil || arg.On == nil || !*arg.On || arg.BrightnessPercent == nil || *arg.BrightnessPercent != 30 {
			t.Fatalf("original intent changed: %+v", arg)
		}
		q, present, err := command.MQTTLightEmission(encoding)
		if err != nil || !present || q != 76 || !command.ReadyForTransport() {
			t.Fatalf("captured emission q=%d present=%v ready=%v err=%v", q, present, command.ReadyForTransport(), err)
		}
		cause, err = command.Causality()
		if err != nil {
			return err
		}
		return command.Acknowledge()
	}
	publish := func(level float64, correlated bool) {
		t.Helper()
		light, err := domain.NewLightValue(true, &level, nil)
		if err != nil {
			t.Fatal(err)
		}
		value, err := domain.NewLightStateValue(light)
		if err != nil {
			t.Fatal(err)
		}
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		at := time.Now()
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if correlated {
			observation, err = observation.WithCausality(cause)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := rig.session.Publish(context.Background(), rig.binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	actor := nha022Actor(t)
	first, err := rig.service.Dispatch(context.Background(), actor, rig.request)
	if err != nil || first == nil || first.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("first dispatch: %v %v", first, err)
	}
	id := nha025ID(t, rig.request)
	effective := float64(76) * 100 / 254
	publish(effective, false)
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	for _, neighbor := range []float64{75 * 100.0 / 254, 77 * 100.0 / 254} {
		publish(neighbor, true)
		nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	}
	publish(effective, true)
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	secondRequest := proto.Clone(rig.request).(*actions.CommandRequest)
	secondRequest.CommandId = "nha180-second"
	second, err := rig.service.Dispatch(context.Background(), actor, secondRequest)
	if err != nil || second == nil || second.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || driver.Count() != 2 {
		t.Fatalf("second dispatch: %v %v count=%d", second, err, driver.Count())
	}
	secondID := nha025ID(t, secondRequest)
	newEncoding, err := integrations.NewMQTTLightEncoding(255, sha256.Sum256([]byte("nha180-fixture-s255")), false)
	if err != nil {
		t.Fatal(err)
	}
	discover(newEncoding) // Same public description; private scale/digest changed.
	publish(effective, true)
	nha025Status(t, rig.host, secondID, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	replay, err := rig.service.Dispatch(context.Background(), actor, proto.Clone(secondRequest).(*actions.CommandRequest))
	if err != nil || !proto.Equal(second, replay) || driver.Count() != 2 {
		t.Fatalf("same envelope replay: %v %v count=%d", replay, err, driver.Count())
	}
	changed := proto.Clone(secondRequest).(*actions.CommandRequest)
	changed.GetArguments().GetSetLight().On = nil
	_, err = rig.service.Dispatch(context.Background(), actor, changed)
	if !errors.Is(err, integrations.ErrCommandIDConflict) || driver.Count() != 2 {
		t.Fatalf("same ID, different presence: %v count=%d", err, driver.Count())
	}
}
