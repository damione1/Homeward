package consumer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/sdk/conformance"
)

type syntheticAdapter struct {
	mu             sync.Mutex
	current        *syntheticPilot
	commands       map[conformance.CommandID]syntheticCommand
	ready          chan struct{}
	delivered      chan conformance.CommandID
	deliveries     int
	faulty         bool
	rejectDispatch bool
	deliveryExited chan struct{}
	profile        conformance.FixtureProfile
	partialLight   bool
	matchedLight   bool
}

type syntheticCommand struct {
	pilot   *syntheticPilot
	command conformance.AdmittedCommand
}

type syntheticPilot struct {
	adapter  *syntheticAdapter
	host     conformance.SessionHost
	bindings [2]conformance.Binding
	crash    chan struct{}
}

func newAdapter(faulty bool) *syntheticAdapter {
	return newAdapterWithProfile(faulty, conformance.DefaultFixtureProfile())
}

func newAdapterWithProfile(faulty bool, profile conformance.FixtureProfile) *syntheticAdapter {
	return &syntheticAdapter{commands: make(map[conformance.CommandID]syntheticCommand), ready: make(chan struct{}, 2),
		delivered: make(chan conformance.CommandID, 16), faulty: faulty, profile: profile}
}

func (a *syntheticAdapter) Spec(instance conformance.IntegrationInstanceID) (conformance.PilotSpec, error) {
	return conformance.NewPilotSpec(instance, func() conformance.Pilot {
		p := &syntheticPilot{adapter: a, crash: make(chan struct{})}
		a.mu.Lock()
		a.current = p
		a.mu.Unlock()
		return p
	})
}

func (a *syntheticAdapter) Ready(ctx context.Context) error {
	select {
	case <-a.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *syntheticAdapter) Delivered(ctx context.Context, want conformance.CommandID) error {
	if a.rejectDispatch {
		defer close(a.deliveryExited)
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case got := <-a.delivered:
		if got != want {
			return fmt.Errorf("delivered %s, want %s", got, want)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *syntheticAdapter) Deliveries() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deliveries
}

func (a *syntheticAdapter) Completion() conformance.CompletionSemantics {
	return conformance.CompletionAcknowledged
}

func (a *syntheticAdapter) Close() error { return nil }

func (p *syntheticPilot) CommandDriver() conformance.CommandDriver { return p }

func (p *syntheticPilot) Start(ctx context.Context, host conformance.SessionHost, tasks conformance.Tasks) error {
	p.host = host
	for i, source := range []conformance.FixtureSource{p.adapter.profile.Switch(), p.adapter.profile.Light()} {
		key, err := conformance.NewSourceKey(source.Device, source.Entity)
		if err != nil {
			return err
		}
		var description *registry.CapabilityDescription
		if i == 0 {
			description = &registry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1,
				Endpoint: source.Endpoint, DisplayName: "Synthetic relay",
				Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: true}}}
		} else {
			lightConstraints := &registry.LightConstraints{SetOn: true,
				BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100}}
			if p.adapter.profile.LightShape() == conformance.LightBrightnessAndColorTemp {
				lightConstraints.ColorTempMireds = &registry.NumberBounds{Minimum: 250, Maximum: 454}
			}
			description = &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1,
				Endpoint: source.Endpoint, DisplayName: "Synthetic light",
				Constraints: &registry.CapabilityDescription_Light{Light: lightConstraints}}
		}
		candidate, err := conformance.NewCandidate(key, []*registry.CapabilityDescription{description})
		if err != nil {
			return err
		}
		bindings, err := host.Discover(ctx, []conformance.Candidate{candidate})
		if err != nil || len(bindings) != 1 {
			return fmt.Errorf("discover %d: %v", i, err)
		}
		p.bindings[i] = bindings[0]
		quality, err := conformance.ObservedQuality()
		if err != nil {
			return err
		}
		value := conformance.NewSwitchValue(false)
		if i == 1 {
			brightness := 50.0
			var colorTemp *uint32
			if p.adapter.profile.LightShape() == conformance.LightBrightnessAndColorTemp {
				value := uint32(300)
				colorTemp = &value
			}
			value, err = conformance.NewLightValue(true, &brightness, colorTemp)
			if err != nil {
				return err
			}
		}
		at := time.Now()
		observation, err := conformance.NewStateObservation(source.Endpoint, quality, &value, &at, at, nil)
		if err != nil {
			return err
		}
		if err := host.Publish(ctx, p.bindings[i], observation); err != nil {
			return err
		}
	}
	health, err := conformance.NewHealthReport(conformance.HealthReady, "")
	if err != nil {
		return err
	}
	if err := host.ReportHealth(ctx, health); err != nil {
		return err
	}
	if err := tasks.Go(func(taskCtx context.Context) error {
		select {
		case <-p.crash:
			return errors.New("synthetic pilot crash")
		case <-taskCtx.Done():
			return nil
		}
	}); err != nil {
		return err
	}
	select {
	case p.adapter.ready <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (p *syntheticPilot) Stop(ctx context.Context) error { return ctx.Err() }

func (p *syntheticPilot) Dispatch(ctx context.Context, command conformance.AdmittedCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.adapter.rejectDispatch {
		return errors.New("synthetic dispatch refusal")
	}
	id, _, _, err := command.Identity()
	if err != nil {
		return err
	}
	p.adapter.mu.Lock()
	p.adapter.commands[id] = syntheticCommand{pilot: p, command: command}
	p.adapter.deliveries++
	p.adapter.mu.Unlock()
	select {
	case p.adapter.delivered <- id:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type falseProof struct{}

func (falseProof) QualifyProtocolConfirmation(conformance.AdmittedCommand) error { return nil }

func (a *syntheticAdapter) Crash(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	p := a.current
	a.mu.Unlock()
	if p == nil {
		return errors.New("pilot not started")
	}
	close(p.crash)
	return nil
}

func (a *syntheticAdapter) Apply(ctx context.Context, id conformance.CommandID, stimulus conformance.Stimulus) error {
	a.mu.Lock()
	saved, ok := a.commands[id]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown synthetic command %v", id)
	}
	if stimulus == conformance.StimulusAcknowledge {
		if err := saved.command.Acknowledge(); err != nil {
			return err
		}
		if a.faulty {
			return saved.command.ConfirmProtocol(falseProof{})
		}
		return nil
	}
	request, err := saved.command.Request()
	if err != nil {
		return err
	}
	cause, err := saved.command.Causality()
	if err != nil {
		return err
	}
	if stimulus == conformance.StimulusWrongCorrelation {
		cause, err = conformance.NewCommandCausality("nha095-foreign-correlation")
		if err != nil {
			return err
		}
	}
	quality, err := conformance.ObservedQuality()
	if err != nil {
		return err
	}
	if stimulus == conformance.StimulusStale {
		quality, err = conformance.StaleQuality()
		if err != nil {
			return err
		}
	}
	index := 0
	source := a.profile.Switch()
	var value conformance.StateValue
	if request.GetCapabilitySchemaId() == contracts.LightSchema {
		index = 1
		source = a.profile.Light()
		light := request.GetArguments().GetSetLight()
		if light == nil || light.On == nil || light.BrightnessPercent == nil {
			return errors.New("invalid light test request")
		}
		if a.profile.LightShape() == conformance.LightBrightnessOnly && light.ColorTempMireds != nil {
			return errors.New("brightness-only light was given unadvertised color temperature")
		}
		if a.profile.LightShape() == conformance.LightBrightnessAndColorTemp && light.ColorTempMireds == nil {
			return errors.New("color temperature was omitted from advertised light")
		}
		brightness := light.BrightnessPercent
		if stimulus == conformance.StimulusPartialLight {
			brightness = nil
			a.mu.Lock()
			a.partialLight = true
			a.mu.Unlock()
		} else if stimulus == conformance.StimulusMatch {
			a.mu.Lock()
			a.matchedLight = true
			a.mu.Unlock()
		}
		value, err = conformance.NewLightValue(*light.On, brightness, light.ColorTempMireds)
		if err != nil {
			return err
		}
	} else if request.GetCapabilitySchemaId() == contracts.SwitchSchema {
		switchArgs := request.GetArguments().GetSetSwitch()
		if switchArgs == nil {
			return errors.New("invalid switch test request")
		}
		value = conformance.NewSwitchValue(switchArgs.On)
	} else {
		return errors.New("unsupported test schema")
	}
	at := time.Now()
	observation, err := conformance.NewStateObservation(source.Endpoint, quality, &value, &at, at, nil)
	if err != nil {
		return err
	}
	if stimulus != conformance.StimulusUncorrelated {
		observation, err = observation.WithCausality(cause)
		if err != nil {
			return err
		}
	}
	return saved.pilot.host.Publish(ctx, saved.pilot.bindings[index], observation)
}

func TestNHA095ThirdPartyPilotConformance(t *testing.T) {
	conformance.Run(t, func(conformance.Case, string) (conformance.Adapter, error) {
		return newAdapter(false), nil
	})
}

func TestNHA095ThirdPartyCustomBrightnessOnlyProfile(t *testing.T) {
	profile, err := conformance.NewFixtureProfile(
		conformance.FixtureSource{Device: "contributor-bridge", Entity: "contact-output", Endpoint: "contact", Schema: contracts.SwitchSchema},
		conformance.FixtureSource{Device: "contributor-bridge", Entity: "dimmer-output", Endpoint: "brightness", Schema: contracts.LightSchema},
		conformance.LightBrightnessOnly)
	if err != nil {
		t.Fatal(err)
	}
	var adapters []*syntheticAdapter
	failures := conformance.Check(t.Context(), func(conformance.Case, string) (conformance.Adapter, error) {
		adapter := newAdapterWithProfile(false, profile)
		adapters = append(adapters, adapter)
		return adapter, nil
	}, conformance.Options{WorkDir: t.TempDir(), Profile: &profile})
	if len(failures) != 0 {
		t.Fatalf("custom profile failed: %v", failures)
	}
	if len(adapters) != 3 || !adapters[0].partialLight || !adapters[0].matchedLight {
		t.Fatalf("brightness-only light did not exercise partial then complete proof: adapters=%d", len(adapters))
	}
}

func TestNHA095RealWorkerSameConformance(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	pythonPath, err := os.ReadFile(filepath.Join(root, ".tools/worker-python/python-path"))
	if err != nil {
		t.Fatalf("reuse scripts/ensure-worker-python.sh environment: %v", err)
	}
	executable := strings.TrimSpace(string(pythonPath))
	script := filepath.Join(root, "testworker/fake_worker.py")
	socketRoot := os.Getenv("GOTMPDIR")
	if socketRoot == "" {
		socketRoot = os.TempDir()
	}
	conformance.Run(t, func(conformance.Case, string) (conformance.Adapter, error) {
		return conformance.NewWorkerAdapter(executable, script, socketRoot)
	})
}

func TestNHA095ThirdPartyFalseConfirmationFailsPrecisely(t *testing.T) {
	failures := conformance.Check(t.Context(), func(conformance.Case, string) (conformance.Adapter, error) {
		return newAdapter(true), nil
	}, conformance.Options{WorkDir: t.TempDir()})
	if len(failures) != 3 {
		t.Fatalf("expected each common case to reject false confirmation, got %v", failures)
	}
	for _, failure := range failures {
		if failure.Code() != conformance.FailureUnprovenConfirmation {
			t.Fatalf("wrong failure cause: %v", failure)
		}
	}
}

type teardownAdapter struct{ *syntheticAdapter }

func (*teardownAdapter) Close() error { return errors.New("synthetic close failed") }

func TestNHA095ConsumerInvalidAdmissionsAndTeardown(t *testing.T) {
	instance, err := conformance.ParseIntegrationInstanceID("nha095-admission")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conformance.NewPilotSpec(conformance.IntegrationInstanceID{}, func() conformance.Pilot { return nil }); err == nil {
		t.Fatal("zero instance admitted")
	}
	if _, err := conformance.NewPilotSpec(instance, nil); err == nil {
		t.Fatal("nil factory admitted")
	}
	if _, err := conformance.NewWorkerSpec(conformance.IntegrationInstanceID{}, "/missing", nil); err == nil {
		t.Fatal("zero worker instance admitted")
	}
	if _, err := conformance.NewFixtureProfile(conformance.SwitchSource(), conformance.LightSource(), conformance.LightShapeUnspecified); err == nil {
		t.Fatal("unknown fixture shape admitted")
	}
	zeroProfile := conformance.FixtureProfile{}
	profileFailures := conformance.Check(t.Context(), func(conformance.Case, string) (conformance.Adapter, error) {
		t.Fatal("invalid profile must reject before constructing an adapter")
		return nil, nil
	}, conformance.Options{WorkDir: t.TempDir(), Profile: &zeroProfile})
	if len(profileFailures) != 1 || profileFailures[0].Code() != conformance.FailureSetup {
		t.Fatalf("zero profile was not rejected at setup: %v", profileFailures)
	}
	for name, factory := range map[string]conformance.AdapterFactory{
		"nil adapter": func(conformance.Case, string) (conformance.Adapter, error) { return nil, nil },
		"typed nil adapter": func(conformance.Case, string) (conformance.Adapter, error) {
			var adapter *syntheticAdapter
			return adapter, nil
		},
		"zero spec": func(conformance.Case, string) (conformance.Adapter, error) {
			return &zeroSpecAdapter{syntheticAdapter: newAdapter(false)}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			failures := conformance.Check(t.Context(), factory, conformance.Options{WorkDir: t.TempDir()})
			if len(failures) != 3 {
				t.Fatalf("want three setup failures, got %v", failures)
			}
			for _, failure := range failures {
				if failure.Code() != conformance.FailureSetup {
					t.Fatalf("want setup failure, got %v", failure)
				}
			}
		})
	}
	typedPilotFailures := conformance.Check(t.Context(), func(conformance.Case, string) (conformance.Adapter, error) {
		return &typedNilPilotAdapter{syntheticAdapter: newAdapter(false)}, nil
	}, conformance.Options{WorkDir: t.TempDir(), Timeout: 100 * time.Millisecond})
	if len(typedPilotFailures) == 0 {
		t.Fatal("typed-nil Pilot factory falsely passed")
	}
	count := 0
	failures := conformance.Check(t.Context(), func(conformance.Case, string) (conformance.Adapter, error) {
		count++
		return &teardownAdapter{newAdapter(false)}, nil
	}, conformance.Options{WorkDir: t.TempDir()})
	if count != 1 || len(failures) != 1 || failures[0].Code() != conformance.FailureTeardown {
		t.Fatalf("teardown must fail and stop next case: factory calls=%d failures=%v", count, failures)
	}
}

func TestNHA095EarlyDispatchRefusalJoinsDelivery(t *testing.T) {
	var adapters []*syntheticAdapter
	failures := conformance.Check(t.Context(), func(conformance.Case, string) (conformance.Adapter, error) {
		adapter := newAdapter(false)
		adapter.rejectDispatch = true
		adapter.deliveryExited = make(chan struct{})
		adapters = append(adapters, adapter)
		return adapter, nil
	}, conformance.Options{WorkDir: t.TempDir(), Timeout: 2 * time.Second})
	if len(failures) != 3 || len(adapters) != 3 {
		t.Fatalf("early refusal must fail all cases without hanging: %v, adapters=%d", failures, len(adapters))
	}
	for i, failure := range failures {
		if failure.Code() != conformance.FailureDispatch {
			t.Fatalf("case %d failed for wrong reason: %v", i, failure)
		}
		select {
		case <-adapters[i].deliveryExited:
		default:
			t.Fatalf("case %d left Delivered running", i)
		}
	}
}

type zeroSpecAdapter struct{ *syntheticAdapter }

func (*zeroSpecAdapter) Spec(conformance.IntegrationInstanceID) (conformance.PilotSpec, error) {
	return conformance.PilotSpec{}, nil
}

type typedNilPilotAdapter struct{ *syntheticAdapter }

func (*typedNilPilotAdapter) Spec(instance conformance.IntegrationInstanceID) (conformance.PilotSpec, error) {
	return conformance.NewPilotSpec(instance, func() conformance.Pilot {
		var pilot *syntheticPilot
		return pilot
	})
}
