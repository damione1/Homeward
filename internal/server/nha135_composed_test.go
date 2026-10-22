package server

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/ecobee"
	"github.com/damione1/Homeward/internal/energy"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

type nha135Clock struct {
	mu   sync.Mutex
	at   time.Time
	tick time.Duration
}

func (c *nha135Clock) Now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *nha135Clock) MonoNow() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.tick }
func (c *nha135Clock) advance(by time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(by)
	c.tick += by
	c.mu.Unlock()
}

type nha135Report struct {
	activity     *domain.ClimateActivity
	availability domain.Availability
	freshness    domain.Freshness
	provenance   domain.Provenance
	done         chan error
}

// This source has no client, network, credential or command implementation.
// It publishes the same domain ClimateValue contract as delivered driver136.
type nha135Source struct {
	key     integrations.SourceKey
	clock   *nha135Clock
	reports chan nha135Report
}

func (p *nha135Source) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	description := &wireregistry.CapabilityDescription{
		SchemaId: contracts.ClimateSchema, Major: 1, DisplayName: "Synthetic qualified activity", Endpoint: ecobee.Endpoint,
		Constraints: &wireregistry.CapabilityDescription_Climate{Climate: &wireregistry.ClimateConstraints{
			ModeOptions:         []wireregistry.ClimateMode{wireregistry.ClimateMode_CLIMATE_MODE_HEAT},
			ObservedModeOptions: []wireregistry.ClimateMode{wireregistry.ClimateMode_CLIMATE_MODE_HEAT},
			ActivityOptions: []wireregistry.ClimateActivity{
				wireregistry.ClimateActivity_CLIMATE_ACTIVITY_HEATING, wireregistry.ClimateActivity_CLIMATE_ACTIVITY_COOLING,
				wireregistry.ClimateActivity_CLIMATE_ACTIVITY_FAN_ONLY, wireregistry.ClimateActivity_CLIMATE_ACTIVITY_IDLE,
			},
		}},
	}
	candidate, err := integrations.NewCandidate(p.key, []*wireregistry.CapabilityDescription{description})
	if err != nil {
		return err
	}
	bindings, err := host.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil {
		return err
	}
	if len(bindings) != 1 {
		return fmt.Errorf("synthetic climate bindings: %d", len(bindings))
	}
	return tasks.Go(func(taskCtx context.Context) error {
		for {
			select {
			case <-taskCtx.Done():
				return nil
			case report := <-p.reports:
				var value *domain.StateValue
				var err error
				if report.availability == domain.AvailabilityAvailable {
					mode := domain.ClimateModeHeat
					climate, e := domain.NewClimateValue(domain.ClimateValueInput{Mode: &mode, Activity: report.activity})
					if e != nil {
						report.done <- e
						return e
					}
					state, e := domain.NewClimateStateValue(climate)
					if e != nil {
						report.done <- e
						return e
					}
					value = &state
				}
				quality, err := domain.NewQuality(report.availability, report.freshness, report.provenance)
				if err == nil {
					at := p.clock.Now()
					observation, e := integrations.NewStateObservation(ecobee.Endpoint, quality, value, &at, at, nil)
					err = e
					if err == nil {
						err = host.Publish(taskCtx, bindings[0], observation)
					}
				}
				report.done <- err
				if err != nil {
					return err
				}
			}
		}
	})
}
func (*nha135Source) Stop(context.Context) error { return nil }

type nha135Run struct {
	ctx          context.Context
	stop         context.CancelFunc
	done         chan error
	runtime      *observationRuntime
	source       *nha135Source
	profile      energy.Profile
	counters     *energy.Repository
	power        domain.EntityID
	output       domain.EntityID
	joined       bool
	afterPower   uint64
	afterEnergy  uint64
	owner        *configactivation.Owner
	path         string
	powerEnabled bool
}

func nha135Start(t *testing.T, launch []byte, settings ecobee.Settings, clock *nha135Clock, derived, selected, source bool) *nha135Run {
	t.Helper()
	workspace := filepath.Join(t.TempDir(), "configuration.yaml")
	bootstrap := workspace + ".bootstrap.yaml"
	if err := os.WriteFile(workspace, nha135Operational(derived, selected, "nha135-thermostat"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootstrap, launch, 0600); err != nil {
		t.Fatal(err)
	}
	choice, err := config.ExistingBootstrap(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(workspace, choice, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	owners := make(chan *configactivation.Owner, 1)
	manifest, err := registry.LoadManifest(scope.EcobeeMVPV1())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := settings.Instance()
	if err != nil {
		t.Fatal(err)
	}
	pilot := &nha135Source{settings.SourceKey(), clock, make(chan nha135Report)}
	factory := func(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
		repository, e := registry.NewRepository(store, manifest)
		if e != nil {
			return RuntimePlan{}, e
		}
		plan := RuntimePlan{Identity: repository, ConfigureActivation: func(_ context.Context, owner *configactivation.Owner) error { owners <- owner; return nil }}
		// The default runtime attaches its owning repository in Configure. Ask
		// Run for the same native authorization composition in this synthetic
		// factory, before energy's auxiliary repository publishes its snapshot.
		// RegistryReady alone does not attach the activation owner's repository.
		plan.ConfigureAuthorization = func(context.Context, AuthorizationComposition) error { return nil }
		plan.Configure = func(ctx context.Context, _ *integrations.Manager, _ *storage.Store) error {
			if !source {
				return nil
			}
			_, e := repository.ConfigureBinding(ctx, instance, settings.SourceKey(), ecobee.Alias)
			return e
		}
		if source {
			spec, e := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
			if e != nil {
				return RuntimePlan{}, e
			}
			plan.Pilots = []integrations.PilotSpec{spec}
		}
		return plan, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	run := &nha135Run{ctx: ctx, stop: cancel, done: make(chan error, 1), source: pilot, path: workspace, powerEnabled: derived}
	t.Cleanup(func() { run.close(t) })
	ready, observed := make(chan struct{}), make(chan *observationRuntime, 1)
	go func() {
		run.done <- runWithDependencies(ctx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, factory,
			runDependencies{sequenceClock: clock, observeRuntime: func(r *observationRuntime) { observed <- r }})
	}()
	select {
	case run.runtime = <-observed:
	case err := <-run.done:
		run.joined = true
		t.Fatalf("Run composition: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-ready:
	case err := <-run.done:
		run.joined = true
		t.Fatalf("Run readiness: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case run.owner = <-owners:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	repository, err := registry.NewRepository(run.runtime.store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	power, err := repository.LookupAlias(ctx, "sensor.hvac_power_consumption")
	if err != nil {
		t.Fatal(err)
	}
	output, err := repository.LookupAlias(ctx, "sensor.hvac_energy_consumption")
	if err != nil {
		t.Fatal(err)
	}
	run.power, run.output = power.Entity, output.Entity
	unit, err := domain.NewUnitCatalog().Parse("kWh")
	if err != nil {
		t.Fatal(err)
	}
	run.profile, err = energy.NewProfile("mvp-trapezoidal-v1", 1, power.Entity, output.Entity, energy.MethodTrapezoidal, energy.TimeBasisReceipt, unit, 3, domain.SemanticTotal, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	run.counters, err = energy.NewRepository(run.runtime.store)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (r *nha135Run) close(t *testing.T) {
	t.Helper()
	if r.joined {
		return
	}
	r.stop()
	select {
	case err := <-r.done:
		r.joined = true
		if err != nil {
			t.Error(err)
		}
	case <-time.After(6 * time.Second):
		t.Error("NHA135 Run did not join its owners")
	}
}

func nha135Measurement(state domain.ObservedState, want *float64) bool {
	_, quality, _, _, _, err := state.Components()
	if err != nil {
		return false
	}
	availability, freshness, provenance, err := quality.Components()
	if err != nil || provenance != domain.ProvenanceEstimated {
		return false
	}
	value, present := state.CurrentValue()
	if want == nil {
		return !present && availability == domain.AvailabilityUnavailable && freshness == domain.FreshnessStale
	}
	measurement, valid := value.Measurement()
	if !present || !valid {
		return false
	}
	parts, err := measurement.Components()
	return err == nil && availability == domain.AvailabilityAvailable && freshness == domain.FreshnessFresh && parts.Value == *want
}

// Retiring an entire publisher emits the Host's observed session-loss marker.
// That marker is not an estimated output; it still cannot carry current W.
func nha135RetiredPower(state domain.ObservedState) bool {
	_, quality, _, _, _, err := state.Components()
	if err != nil {
		return false
	}
	availability, freshness, provenance, err := quality.Components()
	_, present := state.CurrentValue()
	return err == nil && !present && availability == domain.AvailabilityUnavailable && freshness == domain.FreshnessStale && provenance == domain.ProvenanceObserved
}

func (r *nha135Run) wait(t *testing.T, power, total *float64, fraction string, after uint64) energy.Checkpoint {
	t.Helper()
	sub, _, err := r.runtime.sequence.SubscribeUI()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for {
		p, hasPower, err := r.runtime.sequence.State(r.power)
		if err != nil {
			t.Fatal(err)
		}
		e, hasEnergy, err := r.runtime.sequence.State(r.output)
		if err != nil {
			t.Fatal(err)
		}
		checkpoint, loadErr := r.counters.Load(r.ctx, r.profile)
		num, den, fractionErr := checkpoint.TotalFraction()
		got, valid := new(big.Rat).SetString(num + "/" + den)
		want, _ := new(big.Rat).SetString(fraction)
		_, _, _, _, powerRevision, _ := p.Components()
		_, _, _, _, energyRevision, _ := e.Components()
		powerMatches := hasPower && powerRevision > r.afterPower && (nha135Measurement(p, power) || !r.powerEnabled && power == nil && nha135RetiredPower(p)) || !hasPower && power == nil && r.afterPower == 0
		if powerMatches && hasEnergy && energyRevision > r.afterEnergy && nha135Measurement(e, total) &&
			loadErr == nil && fractionErr == nil && valid && got.Cmp(want) == 0 && checkpoint.Revision() > after {
			return checkpoint
		}
		if _, err := sub.Next(r.ctx); err != nil {
			t.Fatalf("composed output power=%v energy=%v fraction=%s/%s revision=%d after=%d: %v (load=%v)", hasPower, hasEnergy, num, den, checkpoint.Revision(), after, err, loadErr)
		}
	}
}

func nha135Operational(derived, selected bool, accessory string) []byte {
	raw := "energy:\n  - source: sensor.hvac_power_consumption\n    output: sensor.hvac_energy_consumption\n    precision: 3\n    max_gap: 1h\n    time_basis: receipt\n"
	if derived {
		raw += "derived_power: mvp-v1\n"
	}
	if selected {
		raw += fmt.Sprintf("ecobee:\n  instance: nha135-synthetic\n  address: 192.0.2.1:12345\n  accessory_id: %s\n  credential_secret: unused-synthetic\n", accessory)
	}
	return []byte(raw)
}

func (r *nha135Run) mark(t *testing.T) uint64 {
	t.Helper()
	before, err := r.counters.Load(r.ctx, r.profile)
	if err != nil {
		t.Fatal(err)
	}
	if state, present, e := r.runtime.sequence.State(r.power); e != nil {
		t.Fatal(e)
	} else if present {
		_, _, _, _, r.afterPower, e = state.Components()
		if e != nil {
			t.Fatal(e)
		}
	}
	if state, present, e := r.runtime.sequence.State(r.output); e != nil {
		t.Fatal(e)
	} else if present {
		_, _, _, _, r.afterEnergy, e = state.Components()
		if e != nil {
			t.Fatal(e)
		}
	}
	return before.Revision()
}

func (r *nha135Run) activateResult(t *testing.T, derived, selected bool, accessory string) (configactivation.Result, error) {
	t.Helper()
	if err := os.WriteFile(r.path, nha135Operational(derived, selected, accessory), 0600); err != nil {
		t.Fatal(err)
	}
	author, err := storage.NewConfigAuthor("nha135:trusted-test-composition")
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.NewConfigSource("nha135:synthetic-workspace")
	if err != nil {
		t.Fatal(err)
	}
	request, err := configactivation.NewWorkspaceRequest(configactivation.RetainDefinitions(), author, source, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r.owner.Activate(r.ctx, request)
}

func (r *nha135Run) activate(t *testing.T, derived, selected bool, accessory string) uint64 {
	t.Helper()
	before := r.mark(t)
	result, err := r.activateResult(t, derived, selected, accessory)
	if err != nil || result.Outcome() != configactivation.Published {
		t.Fatalf("configuration publication: %v %v", result.Outcome(), err)
	}
	r.powerEnabled = derived
	return before
}

func (r *nha135Run) report(t *testing.T, by time.Duration, activity *domain.ClimateActivity, availability domain.Availability, freshness domain.Freshness, provenance domain.Provenance) uint64 {
	t.Helper()
	before := r.mark(t)
	r.source.clock.advance(by)
	report := nha135Report{activity, availability, freshness, provenance, make(chan error, 1)}
	select {
	case r.source.reports <- report:
	case <-r.ctx.Done():
		t.Fatal(r.ctx.Err())
	}
	select {
	case err := <-report.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-r.ctx.Done():
		t.Fatal(r.ctx.Err())
	}
	return before
}

// AC1-4: source-bearing startup admission, real SQL registry and counter,
// actual supervised Host + reactive templates + energy pilot, synthetic I/O.
func TestNHA135RunQualifiedActivityPowerEnergyGapsAndRollback(t *testing.T) {
	cluster := testpg.Start(t)
	launch := nha137LaunchFixture(cluster.Port, shortPrivateDataDir(t), "ecobee_mvp_v1")
	settings, err := ecobee.ParseSettings(ecobee.Input{Instance: "nha135-synthetic", Address: "192.0.2.1:12345", AccessoryID: "nha135-thermostat", CredentialSecret: "unused-synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha135Clock{at: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	run := nha135Start(t, launch, settings, clock, true, true, true)
	run.wait(t, nil, nil, "0", 0)
	heating, cooling, fan, idle := domain.ClimateActivityHeating, domain.ClimateActivityCooling, domain.ClimateActivityFanOnly, domain.ClimateActivityIdle
	watts, total := 15400.0, 0.0
	after := run.report(t, 0, &heating, domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	run.wait(t, &watts, nil, "0", after)
	// A new observed report is a new energy bound even while heating stays
	// unchanged. Semantic template deduplication must not discard this hour.
	after = run.report(t, time.Hour, &heating, domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	total = 15.4
	run.wait(t, &watts, &total, "77/5", after)
	for _, row := range []struct {
		activity     *domain.ClimateActivity
		watts, total float64
		fraction     string
	}{
		{&cooling, 3200, 24.7, "247/10"}, {&fan, 400, 26.5, "53/2"}, {&idle, 0, 26.7, "267/10"},
	} {
		after = run.report(t, time.Hour, row.activity, domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		run.wait(t, &row.watts, &row.total, row.fraction, after)
	}
	// Re-observed idle remains a valid zero-power interval, rather than a hole.
	after = run.report(t, time.Hour, &idle, domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	idleWatts, idleTotal := 0.0, 26.7
	run.wait(t, &idleWatts, &idleTotal, "267/10", after)
	// Each invalid source variant is separated by a valid recovery. The first
	// recovery is only a new bound, never a trapezoid bridging the missing hour.
	for _, row := range []struct {
		activity     *domain.ClimateActivity
		availability domain.Availability
		freshness    domain.Freshness
		provenance   domain.Provenance
	}{
		{nil, domain.AvailabilityUnavailable, domain.FreshnessStale, domain.ProvenanceObserved},
		{&idle, domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceObserved},
		{nil, domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved},
		{&idle, domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceRestored},
	} {
		after = run.report(t, time.Hour, row.activity, row.availability, row.freshness, row.provenance)
		checkpoint := run.wait(t, nil, nil, "267/10", after)
		if checkpoint.Coverage() != energy.CoverageDegraded {
			t.Fatal("source hole hidden from coverage")
		}
		after = run.report(t, time.Hour, &heating, domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		run.wait(t, &watts, nil, "267/10", after)
	}
	after = run.report(t, time.Hour, &cooling, domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	watts, total = 3200, 36
	run.wait(t, &watts, &total, "36", after)
	// Disable and roll back through the actual NHA-029 hot refresh owner.
	// Keep the synthetic Ecobee pilot unchanged: no production client is made.
	after = run.report(t, 0, &cooling, domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceObserved)
	run.wait(t, nil, nil, "36", after)
	after = run.activate(t, false, true, "nha135-thermostat")
	run.wait(t, nil, nil, "36", after)
	// New observations while disabled must not retroactively produce energy.
	run.report(t, 12*time.Hour, &fan, domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	after = run.activate(t, true, true, "nha135-thermostat")
	watts = 400
	run.wait(t, &watts, nil, "36", after)
	after = run.report(t, time.Hour, &idle, domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	watts, total = 0, 36.2
	run.wait(t, &watts, &total, "181/5", after)
	// A different source key cannot take over the already bound alias. This
	// fails during registry preparation, before any replacement pilot starts.
	stamp, err := run.owner.CurrentConfig(run.ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, refusal := run.activateResult(t, true, true, "unapproved-replacement")
	if !errors.Is(refusal, registry.ErrAliasConflict) || result.Outcome() != configactivation.RejectedBeforeCommit {
		t.Fatalf("replacement source refusal: outcome=%v err=%v", result.Outcome(), refusal)
	}
	current, err := run.owner.CurrentConfig(run.ctx)
	if err != nil || current != stamp {
		t.Fatal("refused source replacement changed the active generation")
	}
	// Remove the configured Ecobee while retaining the formula. Its publisher
	// must be replaced with a closed source proof; the last idle is not current.
	after = run.activate(t, true, false, "nha135-thermostat")
	run.wait(t, nil, nil, "181/5", after)
	power, output := run.power, run.output
	run.close(t)
	// Cold replay of the now source-free active configuration retains totals
	// and identities across process restart.
	clock.advance(12 * time.Hour)
	restarted := nha135Start(t, launch, settings, clock, true, false, false)
	restarted.wait(t, nil, nil, "181/5", 0)
	if restarted.power != power || restarted.output != output {
		t.Fatal("restart replaced durable identities")
	}

}

func TestNHA135RunAliasAndObservedQualityCannotQualifyAnotherProvider(t *testing.T) {
	cluster := testpg.Start(t)
	launch := nha137LaunchFixture(cluster.Port, shortPrivateDataDir(t), "ecobee_mvp_v1")
	settings, err := ecobee.ParseSettings(ecobee.Input{Instance: "nha135-impostor", Address: "192.0.2.1:12345", AccessoryID: "unqualified-source", CredentialSecret: "unused-synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha135Clock{at: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	run := nha135Start(t, launch, settings, clock, true, false, true)
	run.wait(t, nil, nil, "0", 0)
	heating := domain.ClimateActivityHeating
	after := run.report(t, time.Hour, &heating, domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	run.wait(t, nil, nil, "0", after)
	// Prove the negative path exercised a live, fresh climate.home, rather than
	// passing because discovery or source publication failed.
	alias, err := registry.ParseHAAlias(ecobee.Alias)
	if err != nil {
		t.Fatal(err)
	}
	entity, err := run.runtime.sequence.ResolveCommandAlias(alias)
	if err != nil {
		t.Fatal(err)
	}
	state, present, err := run.runtime.sequence.State(entity)
	if err != nil || !present {
		t.Fatalf("impostor source was not published: %v", err)
	}
	value, fresh := state.CriticalValue()
	if _, climate := value.Climate(); !fresh || !climate {
		t.Fatal("negative source was not available/fresh climate")
	}
}
