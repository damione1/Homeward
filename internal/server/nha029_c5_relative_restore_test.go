package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/light"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/proto"
)

const nha029C5RelativeAlias = "light.kitchen_lights"

// The simulated effect journal belongs to the test, not either Run. The base
// driver independently checks the real SQL dispatching row before its effect.
type nha029C5RelativeDriver struct {
	base  *nha042ServerDriver
	mu    sync.Mutex
	wires []*actions.CommandRequest
}

func (d *nha029C5RelativeDriver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	wire, err := command.Request()
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.wires = append(d.wires, wire)
	d.mu.Unlock()
	return d.base.Dispatch(ctx, command)
}

func (d *nha029C5RelativeDriver) snapshot() []*actions.CommandRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]*actions.CommandRequest, len(d.wires))
	for index, wire := range d.wires {
		result[index] = proto.Clone(wire).(*actions.CommandRequest)
	}
	return result
}

type nha029C5RelativePilot struct {
	host    *integrations.Manager
	key     integrations.SourceKey
	driver  *nha029C5RelativeDriver
	session *integrations.Session
	binding integrations.Binding
	ready   chan struct{}
}

func (p *nha029C5RelativePilot) Start(ctx context.Context, source integrations.SessionHost, _ integrations.Tasks) error {
	session, ok := source.(*integrations.Session)
	if !ok {
		return integrations.ErrInvalidInput
	}
	candidate, err := integrations.NewCandidate(p.key, []*registrywire.CapabilityDescription{{
		SchemaId: contracts.LightSchema, Major: 1, Endpoint: "state", DisplayName: "C5 simulated relative light",
		Constraints: &registrywire.CapabilityDescription_Light{Light: &registrywire.LightConstraints{
			SetOn: true, BrightnessPercent: &registrywire.NumberBounds{Minimum: 0, Maximum: 100},
		}},
	}})
	if err != nil {
		return err
	}
	encoding, err := integrations.NewMQTTLightEncoding(254, sha256.Sum256([]byte("nha029-c5-relative-scale254")), false)
	if err != nil {
		return err
	}
	candidate, err = candidate.WithLightEncoding(encoding)
	if err != nil {
		return err
	}
	bindings, err := source.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		return errors.Join(integrations.ErrInvalidInput, err)
	}
	p.session, p.binding = session, bindings[0]
	if err := p.publish(ctx, 35); err != nil {
		return err
	}
	if err := p.host.AttachCommandDriver(session, p.driver); err != nil {
		return err
	}
	close(p.ready)
	return nil
}

func (*nha029C5RelativePilot) Stop(context.Context) error { return nil }

func (p *nha029C5RelativePilot) publish(ctx context.Context, percent float64) error {
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return err
	}
	light, err := domain.NewLightValue(true, &percent, nil)
	if err != nil {
		return err
	}
	value, err := domain.NewLightStateValue(light)
	if err != nil {
		return err
	}
	at := time.Now()
	observation, err := integrations.NewStateObservation("state", quality, &value, &at, at, nil)
	if err != nil {
		return err
	}
	return p.session.Publish(ctx, p.binding, observation)
}

func nha029C5RelativeCall(t *testing.T) ir.Call {
	t.Helper()
	entity, err := ir.NewEntity(nha029C5RelativeAlias, ir.ReferencePresent, ir.SupportLightTurnOn, ir.SupportLightBrightnessRelative)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scriptID, err := ir.ParseScriptID("script.nha029_c5_relative")
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err = catalogue.WithScripts([]ir.ScriptID{scriptID})
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile("c5-relative.yaml", []byte("sequence:\n- action: light.turn_on\n  target: {entity_id: "+nha029C5RelativeAlias+"}\n  data: {brightness_step_pct: 15}\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileScriptSources("script.nha029_c5_relative", file.Path(), []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 || len(compiled.Rejected()) != 0 {
		t.Fatalf("relative call fixture rejected: %v", err)
	}
	return compiled.Accepted()[0].Actions()[0].Call()
}

func TestNHA029C5RestoreDoesNotRecaptureOrReplayCompletedRelativeSlot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	environment := nha042ServerEnvironmentFor(t)
	driver := &nha029C5RelativeDriver{base: &nha042ServerDriver{observer: environment.driverObserver, entered: make(chan struct{}), release: make(chan struct{})}}
	driver.base.unblock()
	instance, err := domain.ParseIntegrationInstanceID("nha029-c5-relative-pilot")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("nha029-c5-relative-device", "state")
	if err != nil {
		t.Fatal(err)
	}
	pilot := &nha029C5RelativePilot{key: key, driver: driver, ready: make(chan struct{})}
	attach := func(ctx context.Context, plan *RuntimePlan, repository *registry.Repository) error {
		if _, err := repository.ConfigureBinding(ctx, instance, key, nha029C5RelativeAlias); err != nil {
			return err
		}
		spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
		if err != nil {
			return err
		}
		plan.Pilots = append(plan.Pilots, spec)
		previous := plan.Configure
		plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
			pilot.host = host
			if previous != nil {
				return previous(ctx, host, store)
			}
			return nil
		}
		return nil
	}
	r := nha029StartOwnerEnvironment(t, environment, nha029OwnerParent, nha029OwnerChild, func(plan *RuntimePlan) {
		if err := attach(ctx, plan, plan.Identity.(*registry.Repository)); err != nil {
			t.Fatal(err)
		}
	}, true)
	nha034WaitSignal(t, pilot.ready, "relative fake light admission")
	a := nha029C5HistoryActive(t, ctx, r)
	commands, present := r.jobServices.Commands()
	if !present || commands == nil || r.commands != r.jobComposition.CommandComposition() {
		t.Fatal("relative slot did not borrow the actual Run-owned script/job DCS")
	}
	internal, err := commands.InternalAutomationPort()
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseCommandID("nha029-c5-relative-completed")
	if err != nil {
		t.Fatal(err)
	}
	var slot *light.PreparationSlot
	var prepared light.PreparedAction
	var response *actions.CommandResponse
	call := nha029C5RelativeCall(t)
	err = internal.WithOccurrence(ctx, "nha029-c5-relative-original-occurrence", func(inside context.Context) error {
		var err error
		slot, err = light.NewInternalPreparationSlot(inside, internal, commands, call, 0, id, time.Now().Add(time.Minute))
		if err != nil {
			return err
		}
		prepared, err = slot.PrepareOnce(inside)
		if err != nil {
			return err
		}
		response, err = slot.Dispatch(inside)
		return err
	})
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("relative occurrence did not terminate before B: %v", err)
	}
	wire, err := prepared.Request()
	if err != nil || prepared.BrightnessKind() != ir.BrightnessRelativePercent || wire.ExpectedRevision == nil || wire.ConfigRevision != uint64(a.revision.Number()) {
		t.Fatalf("relative preparation lost its observed revision or A stamp: %v", err)
	}
	if brightness, present := prepared.HABrightness(); !present || brightness != 128 {
		t.Fatal("relative action did not capture 35 percent plus 15 exactly once")
	}
	_, entity, err := pilot.binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	entityText, err := entity.Value()
	if err != nil {
		t.Fatal(err)
	}
	expectedTransport := proto.Clone(wire).(*actions.CommandRequest)
	expectedTransport.Target = &registrywire.EntityRef{Reference: &registrywire.EntityRef_EntityId{EntityId: entityText}}
	if transported := driver.snapshot(); len(transported) != 1 || !proto.Equal(transported[0], expectedTransport) || len(driver.base.snapshot()) != 1 {
		t.Fatal("relative effect did not use its unique originally prepared wire")
	}
	state, terminal := nha042ServerIntent(t, environment.observer, id)
	if state != "result" || !proto.Equal(terminal, response) {
		t.Fatal("relative effect did not reach a durable terminal result")
	}
	intent := nha029C5RestoreDigest(t, ctx, environment.observer, nha029C5RestoreIntentDigest, wire.CommandId)
	if err := pilot.publish(ctx, 70); err != nil {
		t.Fatal(err)
	}
	b := nha029C5HistoryActivate(t, ctx, r, "relative B after terminal action and changed state")
	clear(r.raw)
	r.raw = nil
	replay := nha029C5HistoryReplay(t, ctx, r)
	historical, err := replay.ReadHistoricalProtectedSource(ctx, a.revision, a.manifest, a.ref)
	if err != nil || historical.SHA256() != a.hash {
		t.Fatalf("relative restore did not authenticate historical SQL A: %v", err)
	}
	raw := historical.Bytes()
	err = os.WriteFile(r.path, raw, 0600)
	clear(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.owner.Activate(ctx, r.request(t, nil, nil, nil))
	if err != nil || result.Outcome() != configactivation.Published {
		t.Fatalf("relative restore C was not published: %v", err)
	}
	c := nha029C5HistoryActive(t, ctx, r)
	if b.revision.Number() <= a.revision.Number() || c.revision.Number() <= b.revision.Number() || c.hash != a.hash || c.ref.Revision().Number() <= b.ref.Revision().Number() || c.manifest == a.manifest {
		t.Fatal("relative restore did not bind SQL A to a fresh authenticated generation C")
	}
	cached, err := slot.PrepareOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cachedWire, err := cached.Request()
	if err != nil || !proto.Equal(cachedWire, wire) {
		t.Fatalf("relative slot recaptured state or retargeted to B/C: %v", err)
	}
	if err := internal.WithOccurrence(ctx, "nha029-c5-relative-new-occurrence", func(inside context.Context) error {
		_, err := slot.Dispatch(inside)
		return err
	}); !errors.Is(err, light.ErrInvalidSlot) {
		t.Fatalf("old relative slot escaped its original occurrence: %v", err)
	}
	counts := nha029C5HistoryCounts(t, ctx, r)
	if counts[3] != 1 || nha029C5RestoreDigest(t, ctx, environment.observer, nha029C5RestoreIntentDigest, wire.CommandId) != intent || len(driver.snapshot()) != 1 || len(driver.base.snapshot()) != 1 {
		t.Fatal("B/C rewrote, repeated or inverted the historical relative action")
	}
	r.close(t)
	if err := os.WriteFile(r.path, []byte("invalid: [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(r.path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	// Restart the real command runtime with the same fake transport journal.
	// No transient IR is presented as a stored/restored Logic1 definition.
	pilot = &nha029C5RelativePilot{key: key, driver: driver, ready: make(chan struct{})}
	factory := func(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
		active, present, err := store.ReadActiveConfig(ctx)
		if err != nil || !present || active.Revision() != c.revision || active.ManifestSHA256() != c.manifest {
			return RuntimePlan{}, errors.New("relative restart composition preceded SQL C admission")
		}
		manifest, err := registry.ParseManifest(scope.ManifestV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		commands, err := NewDurableCommandsComposition(DurableCommandsInput{Generation: nha094Generation{}, Clock: automationtriggers.RealClock{}})
		if err != nil {
			return RuntimePlan{}, err
		}
		plan := RuntimePlan{Identity: repository, DurableCommands: commands}
		if err := attach(ctx, &plan, repository); err != nil {
			return RuntimePlan{}, err
		}
		return plan, nil
	}
	restarted := nha034LaunchRun(t, startup, (&net.ListenConfig{}).Listen, factory)
	restarted.waitReady(t)
	nha034WaitSignal(t, pilot.ready, "relative fake transport after SQL C restart")
	restarted.cancel()
	if err := restarted.waitDone(t); err != nil {
		t.Fatal(err)
	}
	if nha029C5HistoryCounts(t, ctx, r) != counts || nha029C5RestoreDigest(t, ctx, environment.observer, nha029C5RestoreIntentDigest, wire.CommandId) != intent || len(driver.snapshot()) != 1 || len(driver.base.snapshot()) != 1 {
		t.Fatal("real C restart rewrote or replayed the old relative intention")
	}
}
