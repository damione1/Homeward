package nha047

import (
	"context"
	"errors"
	"fmt"
	"github.com/damione1/Homeward/compat/ha/parser"
	hascene "github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/scenes"
	"google.golang.org/protobuf/proto"
	"math"
	"sync"
	"time"
)

type qualificationGeneration struct{}

func (qualificationGeneration) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrInvalidInput
	}
	return fn(1)
}

// Only native requests actually admitted to the fake driver become evidence.
type sceneCommandObserver struct {
	target string
	mu     sync.Mutex
	facts  []Demand
}

func (o *sceneCommandObserver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	request, err := command.Request()
	if err != nil {
		return err
	}
	var kind DemandKind
	value, secondary := 0.0, 0.0
	switch args := request.Arguments.Arguments.(type) {
	case *actions.ActionArgs_SetLight:
		kind = LightOff
		if args.SetLight.GetOn() {
			kind = LightOn
			if args.SetLight.BrightnessPercent == nil {
				return ErrInvalidCapture
			}
			value = math.Round(args.SetLight.GetBrightnessPercent() * 255 / 100)
			secondary = float64(args.SetLight.GetColorTempMireds())
		}
	case *actions.ActionArgs_SetSwitch:
		kind = SwitchOff
		if args.SetSwitch.On {
			kind = SwitchOn
		}
	case *actions.ActionArgs_SetFan:
		kind = FanOff
		switch args.SetFan.GetSpeed() {
		case "Off":
		case "MediumHigh":
			kind, value = FanOn, 75
		default:
			return ErrInvalidCapture
		}
	default:
		return ErrInvalidCapture
	}
	demand, err := NewDemand(kind, o.target, value, secondary, "")
	if err != nil {
		return err
	}
	if len(o.facts) >= MaxFacts {
		return ErrInvalidCapture
	}
	o.facts = append(o.facts, demand)
	return nil
}

func runConsumedScene(ctx context.Context, c Case, consumed parser.SourceFile) (facts []Demand, err error) {
	alias, err := domain.ParseHAAlias(c.Object())
	if err != nil {
		return nil, err
	}
	ref, err := hascene.NewSourceRef(alias, consumed.Path(), consumed.SHA256())
	if err != nil {
		return nil, err
	}
	compiled, err := hascene.Load(ref, consumed.Bytes())
	if err != nil {
		return nil, err
	}
	if len(compiled.Accepted()) != 1 || len(compiled.Rejected()) != 0 {
		return nil, ErrInvalidInput
	}
	definition := compiled.Accepted()[0]
	if len(definition.Members()) != 1 {
		return nil, ErrInvalidInput
	}
	member := definition.Members()[0]
	if len(member.Diagnostics()) != 0 {
		return nil, ErrInvalidInput
	}
	target, err := member.Alias().Value()
	if err != nil {
		return nil, err
	}

	// Scene reproduction has no time trigger. The delivered router also uses
	// context.WithDeadline, so command expiry must be in the real future.
	clock := &qualificationClock{origin: time.Now()}
	host, fixture, err := fake.NewCommandFixtureWithClock(clock)
	if err != nil {
		return nil, err
	}
	instance, err := domain.ParseIntegrationInstanceID("nha047-scene-fake")
	if err != nil {
		return nil, err
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, host.Revoke(session)) }()
	catalogue, e := scenes.NewCatalogue([]hascene.Definition{definition})
	if e != nil {
		return nil, e
	}
	commands, e := integrations.NewVolatileCommandServiceWithClock(host, qualificationGeneration{}, clock, integrations.DefaultVolatileCommandLimits())
	if e != nil {
		return nil, e
	}
	service, e := scenes.NewService(catalogue, fixture.Sequencer(), qualificationGeneration{}, commands, clock)
	if e != nil {
		return nil, e
	}
	defer service.Close()
	actor, e := domain.ParseActorID("nha047-synthetic-command-port")
	if e != nil {
		return nil, e
	}
	commandID, e := domain.ParseCommandID("nha047-scene-occurrence")
	if e != nil {
		return nil, e
	}
	prepare := func() (scenes.PreparedScene, error) {
		ref, e := service.Declarative(alias)
		if e != nil {
			return scenes.PreparedScene{}, e
		}
		slot, e := scenes.NewPreparationSlot(service, ref, actor, commandID, nil, clock.Now().Add(time.Minute))
		if e != nil {
			return scenes.PreparedScene{}, e
		}
		return slot.PrepareOnce(ctx)
	}
	if member.Kind() == hascene.MemberClimate {
		prepared, e := prepare()
		if e != nil {
			return nil, e
		}
		members := prepared.Members()
		if len(members) != 1 || members[0].Outcome() != scenes.OutcomeRefused || members[0].Code() != scenes.RefusalCapability || members[0].Field() != "humidity,fan_mode,preset_mode" {
			return nil, ErrInvalidCapture
		}
		status, e := commands.Status()
		if e != nil || status.Entries != 0 {
			return nil, ErrInvalidCapture
		}
		demand, e := NewDemand(Refusal, target, 0, 0, "unsupported_climate_attributes")
		return []Demand{demand}, e
	}
	key, err := integrations.NewSourceKey("nha047", target)
	if err != nil {
		return nil, err
	}
	description := &registryv1.CapabilityDescription{Major: 1, Endpoint: "value", DisplayName: "Synthetic scene target"}
	var state domain.StateValue
	switch member.Kind() {
	case hascene.MemberLight:
		description.SchemaId = contracts.LightSchema
		description.Constraints = &registryv1.CapabilityDescription_Light{Light: &registryv1.LightConstraints{SetOn: true, BrightnessPercent: &registryv1.NumberBounds{Minimum: 0, Maximum: 100}, ColorTempMireds: &registryv1.NumberBounds{Minimum: 250, Maximum: 454}}}
		intent, _ := member.Light()
		desiredOn, _, _ := intent.Components()
		light, e := domain.NewLightValue(!desiredOn, nil, nil)
		if e != nil {
			return nil, e
		}
		state, err = domain.NewLightStateValue(light)
	case hascene.MemberSwitch:
		description.SchemaId = contracts.SwitchSchema
		description.Constraints = &registryv1.CapabilityDescription_Switch{Switch: &registryv1.SwitchConstraints{SetOn: true}}
		on, _ := member.Switch()
		state = domain.NewSwitchValue(!on)
	case hascene.MemberFan:
		description.SchemaId = contracts.FanSchema
		description.Endpoint = "speed"
		description.Constraints = &registryv1.CapabilityDescription_Fan{Fan: &registryv1.FanConstraints{SpeedOptions: []string{"Off", "Low", "Medium", "MediumHigh", "High"}}}
		intent, _ := member.Fan()
		initialSpeed := domain.FanSpeedLow
		if intent.On() {
			initialSpeed = domain.FanSpeedOff
		}
		state, err = domain.NewFanStateValue(initialSpeed)
	default:
		return nil, ErrUnavailable
	}
	if err != nil {
		return nil, err
	}
	candidate, err := integrations.NewCandidate(key, []*registryv1.CapabilityDescription{description})
	if err != nil {
		return nil, err
	}
	switch member.Kind() {
	case hascene.MemberLight:
		encoded, e := proto.Marshal(description)
		if e != nil {
			return nil, e
		}
		encoding, e := integrations.NewMQTTLightEncoding(255, [32]byte(Hash(encoded)), false)
		if e != nil {
			return nil, e
		}
		candidate, err = candidate.WithLightEncoding(encoding)
	case hascene.MemberFan:
		candidate, err = candidate.WithCasetaHAFan4V1(member.Alias())
	case hascene.MemberSwitch:
	}
	if err != nil {
		return nil, err
	}
	// Qualified profiles join their configured alias with the registry at
	// discovery; publish the fake authority's existing IDs first (NHA046).
	identities, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil || len(identities) != 1 {
		return nil, errors.Join(err, ErrInvalidCapture)
	}
	record, err := integrations.NewRegistryRecord(identities[0].Device, identities[0].Entity, instance, key, member.Alias(), member.Alias(), []domain.HAAlias{member.Alias()}, false)
	if err != nil {
		return nil, err
	}
	if err = fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		return nil, err
	}
	bindings, err := session.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		return nil, errors.Join(err, ErrInvalidCapture)
	}
	device, id, err := bindings[0].IDs()
	if err != nil || device != identities[0].Device || id != identities[0].Entity {
		return nil, errors.Join(err, ErrInvalidCapture)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return nil, err
	}
	now := clock.Now()
	observation, err := integrations.NewStateObservation(description.Endpoint, quality, &state, &now, now, nil)
	if err != nil {
		return nil, err
	}
	if err = session.Publish(ctx, bindings[0], observation); err != nil {
		return nil, err
	}
	observer := &sceneCommandObserver{target: target}
	if err = host.AttachCommandDriver(session, observer); err != nil {
		return nil, err
	}
	prepared, e := prepare()
	if e != nil {
		return nil, e
	}
	if members := prepared.Members(); len(members) != 1 || members[0].Outcome() != scenes.OutcomePrepared {
		if len(members) == 1 {
			return nil, fmt.Errorf("%w: scene preparation outcome=%d code=%d field=%s", ErrInvalidCapture, members[0].Outcome(), members[0].Code(), members[0].Field())
		}
		return nil, ErrInvalidCapture
	}
	result, e := service.ApplyPrepared(ctx, prepared)
	if e != nil {
		return nil, e
	}
	members := result.Members()
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(members) != 1 || members[0].Outcome() != scenes.OutcomeDispatched || len(observer.facts) != 1 {
		if len(members) == 1 && members[0].Response() != nil {
			response := members[0].Response()
			return nil, fmt.Errorf("%w: scene outcome=%d status=%d code=%d field=%s demands=%d", ErrInvalidCapture, members[0].Outcome(), response.Status, response.GetError().GetCode(), response.GetError().GetFieldPath(), len(observer.facts))
		}
		return nil, fmt.Errorf("%w: scene result members=%d demands=%d", ErrInvalidCapture, len(members), len(observer.facts))
	}
	return append([]Demand(nil), observer.facts...), nil
}
