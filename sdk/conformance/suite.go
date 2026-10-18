package conformance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	fixtureInstance = "nha095-synthetic"
	fixtureActor    = "nha095-actor"
	fixtureRevision = uint64(7)
)

// FixtureSource is the exact synthetic source a candidate pilot discovers.
// A third-party pilot may support many other sources outside this suite.
type FixtureSource struct {
	Device, Entity, Endpoint, Schema string
}

// LightFixtureShape is the closed set of light argument shapes exercised by
// this SDK. Optional color temperature is never invented for a brightness-only
// capability.
type LightFixtureShape uint8

const (
	LightShapeUnspecified LightFixtureShape = iota
	LightBrightnessAndColorTemp
	LightBrightnessOnly
)

// FixtureProfile is an admitted, immutable copy of the two synthetic sources.
// The default profile preserves the original command fixtures.
type FixtureProfile struct {
	switchSource FixtureSource
	lightSource  FixtureSource
	lightShape   LightFixtureShape
}

func DefaultFixtureProfile() FixtureProfile {
	return FixtureProfile{SwitchSource(), LightSource(), LightBrightnessAndColorTemp}
}

func NewFixtureProfile(switchSource, lightSource FixtureSource, shape LightFixtureShape) (FixtureProfile, error) {
	if switchSource.Schema != contracts.SwitchSchema || lightSource.Schema != contracts.LightSchema ||
		(shape != LightBrightnessAndColorTemp && shape != LightBrightnessOnly) {
		return FixtureProfile{}, integrations.ErrInvalidInput
	}
	for _, source := range []FixtureSource{switchSource, lightSource} {
		if _, err := integrations.NewSourceKey(source.Device, source.Entity); err != nil ||
			source.Endpoint == "" || len(source.Endpoint) > 128 || !utf8.ValidString(source.Endpoint) ||
			strings.TrimSpace(source.Endpoint) != source.Endpoint {
			return FixtureProfile{}, integrations.ErrInvalidInput
		}
	}
	if switchSource.Device == lightSource.Device && switchSource.Entity == lightSource.Entity {
		return FixtureProfile{}, integrations.ErrInvalidInput
	}
	return FixtureProfile{switchSource, lightSource, shape}, nil
}

func (p FixtureProfile) Switch() FixtureSource         { return p.switchSource }
func (p FixtureProfile) Light() FixtureSource          { return p.lightSource }
func (p FixtureProfile) LightShape() LightFixtureShape { return p.lightShape }

func SwitchSource() FixtureSource {
	return FixtureSource{"synthetic-device-1", "relay-1", "relay", contracts.SwitchSchema}
}

func LightSource() FixtureSource {
	return FixtureSource{"synthetic-device-2", "light-2", "light", contracts.LightSchema}
}

// Options never embeds checkout or cache paths. WorkDir must be an existing
// absolute, caller-owned directory outside any protected repository.
type Options struct {
	WorkDir string
	Timeout time.Duration
	Profile *FixtureProfile
}

// Check runs the same Host scenarios on any Adapter. It returns typed failures
// so a caller can verify that a deliberately faulty pilot actually fails.
func Check(ctx context.Context, factory AdapterFactory, options Options) []Failure {
	if nilInterface(ctx) || factory == nil || options.WorkDir == "" || !filepath.IsAbs(options.WorkDir) {
		return []Failure{fail(CaseUnspecified, FailureSetup, "invalid conformance options")}
	}
	info, err := os.Stat(options.WorkDir)
	if err != nil || !info.IsDir() {
		return []Failure{fail(CaseUnspecified, FailureSetup, "work directory unavailable: %v", err)}
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	if timeout < 0 {
		return []Failure{fail(CaseUnspecified, FailureSetup, "negative timeout")}
	}
	profile := DefaultFixtureProfile()
	if options.Profile != nil {
		profile = *options.Profile
	}
	if _, err := NewFixtureProfile(profile.Switch(), profile.Light(), profile.LightShape()); err != nil {
		return []Failure{fail(CaseUnspecified, FailureSetup, "invalid fixture profile: %v", err)}
	}
	var failures []Failure
	for _, scenario := range []Case{CaseCore, CaseCapacity, CaseReconnect} {
		caseDir, err := os.MkdirTemp(options.WorkDir, "nha095-"+scenario.String()+"-")
		if err != nil {
			failures = append(failures, fail(scenario, FailureSetup, "create case directory: %v", err))
			continue
		}
		caseCtx, cancel := context.WithTimeout(ctx, timeout)
		failure := checkCase(caseCtx, factory, scenario, caseDir, profile)
		cancel()
		if failure != nil {
			failures = append(failures, *failure)
			if failure.Code() == FailureTeardown {
				break
			}
			continue // retain the synthetic case directory as failure evidence
		}
		if err := os.RemoveAll(caseDir); err != nil {
			failures = append(failures, fail(scenario, FailureSetup, "remove synthetic case directory: %v", err))
		}
	}
	return failures
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

type fixedGeneration struct{}

func (fixedGeneration) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(fixtureRevision)
}

type zeroJitter struct{}

func (zeroJitter) Offset(time.Duration) time.Duration { return 0 }

type rig struct {
	host       *integrations.Manager
	fixture    *fake.Fixture
	clock      *clock
	service    *integrations.VolatileCommandService
	supervisor *integrations.Supervisor
	adapter    Adapter
	entities   [2]domain.EntityID
	actor      domain.ActorID
	epoch      string
	pending    actions.ActionStatus
	profile    FixtureProfile
}

func newRig(ctx context.Context, adapter Adapter, scenario Case, profile FixtureProfile) (*rig, error) {
	var pending actions.ActionStatus
	switch adapter.Completion() {
	case CompletionTransportAccepted:
		pending = actions.ActionStatus_ACTION_STATUS_DISPATCHED
	case CompletionAcknowledged:
		pending = actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED
	case CompletionUnspecified:
		return nil, integrations.ErrInvalidInput
	default:
		return nil, integrations.ErrInvalidInput
	}
	clock, err := newClock(time.Now())
	if err != nil {
		return nil, err
	}
	host, fixture, err := fake.NewCommandFixtureWithClock(clock)
	if err != nil {
		return nil, err
	}
	if err := host.SetActionClock(clock); err != nil {
		return nil, err
	}
	instance, err := domain.ParseIntegrationInstanceID(fixtureInstance)
	if err != nil {
		return nil, err
	}
	var entities [2]domain.EntityID
	var records []integrations.RegistryRecord
	for i, source := range []FixtureSource{profile.Switch(), profile.Light()} {
		key, err := integrations.NewSourceKey(source.Device, source.Entity)
		if err != nil {
			return nil, err
		}
		resolved, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
		if err != nil {
			return nil, err
		}
		entities[i] = resolved[0].Entity
		aliasText := "switch.nha095_relay_1"
		if i == 1 {
			aliasText = "light.nha095_light_2"
		}
		alias, err := domain.ParseHAAlias(aliasText)
		if err != nil {
			return nil, err
		}
		record, err := integrations.NewRegistryRecord(resolved[0].Device, resolved[0].Entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := fixture.PublishRegistry(ctx, records); err != nil {
		return nil, err
	}
	limits := integrations.DefaultVolatileCommandLimits()
	if scenario == CaseCapacity {
		limits, err = integrations.NewVolatileCommandLimits(1, 64<<10, 64<<20)
		if err != nil {
			return nil, err
		}
	}
	service, err := integrations.NewVolatileCommandServiceWithClock(host, fixedGeneration{}, clock, limits)
	if err != nil {
		return nil, err
	}
	actor, err := domain.ParseActorID(fixtureActor)
	if err != nil {
		return nil, err
	}
	epoch, err := fixture.CurrentAdmissionEpoch()
	if err != nil {
		return nil, err
	}
	spec, err := adapter.Spec(instance)
	if err != nil {
		return nil, err
	}
	policy, err := integrations.NewRetryPolicy(1, time.Second, time.Second, 5*time.Second)
	if err != nil {
		return nil, err
	}
	supervisor, err := integrations.NewSupervisor(host, []integrations.PilotSpec{spec}, policy, clock, zeroJitter{})
	if err != nil {
		return nil, err
	}
	if err := supervisor.Start(ctx); err != nil {
		return nil, err
	}
	return &rig{host: host, fixture: fixture, clock: clock, service: service, supervisor: supervisor,
		adapter: adapter, entities: entities, actor: actor, epoch: epoch, pending: pending, profile: profile}, nil
}

func (r *rig) request(id string, index int, on bool) *actions.CommandRequest {
	entityText, _ := r.entities[index].Value()
	source := r.profile.Switch()
	args := &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: on}}}
	if index == 1 {
		source = r.profile.Light()
		brightness := 0.0
		var colorTemp *uint32
		if r.profile.LightShape() == LightBrightnessAndColorTemp {
			value := uint32(300)
			colorTemp = &value
		}
		args = &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{
			On: &on, BrightnessPercent: &brightness, ColorTempMireds: colorTemp,
		}}}
	}
	return &actions.CommandRequest{CommandId: id, AdmissionEpoch: r.epoch, ConfigRevision: fixtureRevision,
		Target:             &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entityText}},
		CapabilitySchemaId: source.Schema, Endpoint: source.Endpoint, Arguments: args,
		ExpiresAt: timestamppb.New(r.clock.Now().Add(time.Minute))}
}

type dispatchResult struct {
	response *actions.CommandResponse
	err      error
}

func (r *rig) dispatchAsync(ctx context.Context, request *actions.CommandRequest) <-chan dispatchResult {
	done := make(chan dispatchResult, 1)
	go func() {
		response, err := r.service.Dispatch(ctx, r.actor, request)
		done <- dispatchResult{response, err}
	}()
	return done
}

func (r *rig) status(id string) (actions.ActionStatus, error) {
	parsed, err := domain.ParseCommandID(id)
	if err != nil {
		return actions.ActionStatus_ACTION_STATUS_UNSPECIFIED, err
	}
	snapshot, err := r.host.ActionStatus(parsed)
	return snapshot.Status, err
}

func (r *rig) expectStatus(scenario Case, id string, want actions.ActionStatus, code FailureCode) *Failure {
	got, err := r.status(id)
	if err != nil || got != want {
		failure := fail(scenario, code, "action %s status=%s, want=%s, error=%v", id, got, want, err)
		return &failure
	}
	return nil
}

func (r *rig) command(ctx context.Context, scenario Case, id string, index int, on bool) (*actions.CommandRequest, *Failure) {
	request := r.request(id, index, on)
	commandID, err := domain.ParseCommandID(id)
	if err != nil {
		failure := fail(scenario, FailureSetup, "invalid test command ID %s: %v", id, err)
		return nil, &failure
	}
	commandCtx, cancel := context.WithCancel(ctx)
	done := r.dispatchAsync(commandCtx, request)
	delivered := make(chan error, 1)
	go func() { delivered <- r.adapter.Delivered(commandCtx, commandID) }()
	dispatchPending, deliveryPending := true, true
	defer func() {
		cancel()
		if dispatchPending {
			<-done
		}
		if deliveryPending {
			<-delivered
		}
	}()
	var early *dispatchResult
	select {
	case err := <-delivered:
		deliveryPending = false
		if err != nil {
			failure := fail(scenario, FailureDispatch, "command %s did not reach pilot: %v", id, err)
			return nil, &failure
		}
	case result := <-done:
		dispatchPending = false
		if result.err != nil || result.response == nil || result.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			failure := fail(scenario, FailureDispatch, "command %s refused before delivery: response=%v error=%v", id, result.response, result.err)
			return nil, &failure
		}
		early = &result
		if err := <-delivered; err != nil {
			deliveryPending = false
			failure := fail(scenario, FailureDispatch, "command %s returned but delivery barrier failed: %v", id, err)
			return nil, &failure
		}
		deliveryPending = false
	case <-ctx.Done():
		failure := fail(scenario, FailureDispatch, "command %s delivery timed out: %v", id, ctx.Err())
		return nil, &failure
	}
	if failure := r.expectStatus(scenario, id, actions.ActionStatus_ACTION_STATUS_DISPATCHED, FailureDispatch); failure != nil {
		return nil, failure
	}
	if err := r.adapter.Apply(ctx, commandID, StimulusAcknowledge); err != nil {
		failure := fail(scenario, FailureDispatch, "command %s ACK stimulus: %v", id, err)
		return nil, &failure
	}
	if early == nil {
		select {
		case result := <-done:
			dispatchPending = false
			if result.err != nil || result.response == nil || result.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				failure := fail(scenario, FailureDispatch, "command %s dispatch response=%v error=%v", id, result.response, result.err)
				return nil, &failure
			}
		case <-ctx.Done():
			failure := fail(scenario, FailureDispatch, "command %s dispatch did not complete: %v", id, ctx.Err())
			return nil, &failure
		}
	}
	// No state stimulus has been sent. Confirmation here is an ACK asserted as
	// physical proof, regardless of whether the pilot called ConfirmProtocol.
	if got, err := r.status(id); err != nil || got != r.pending {
		code := FailureDispatch
		if err == nil && got == actions.ActionStatus_ACTION_STATUS_CONFIRMED {
			code = FailureUnprovenConfirmation
		}
		failure := fail(scenario, code, "command %s after transport completion status=%s, want=%s, error=%v", id, got, r.pending, err)
		return nil, &failure
	}
	return request, nil
}

func (r *rig) applyAndExpect(ctx context.Context, scenario Case, id string, stimulus Stimulus, want actions.ActionStatus, code FailureCode) *Failure {
	commandID, err := domain.ParseCommandID(id)
	if err != nil {
		failure := fail(scenario, FailureSetup, "invalid test command ID %s: %v", id, err)
		return &failure
	}
	if err := r.adapter.Apply(ctx, commandID, stimulus); err != nil {
		failure := fail(scenario, code, "command %s %s stimulus: %v", id, stimulus, err)
		return &failure
	}
	return r.expectStatus(scenario, id, want, code)
}

func checkCase(ctx context.Context, factory AdapterFactory, scenario Case, dir string, profile FixtureProfile) (result *Failure) {
	adapter, err := factory(scenario, dir)
	if err != nil || nilInterface(adapter) {
		failure := fail(scenario, FailureSetup, "adapter factory: %v", err)
		return &failure
	}
	defer func() {
		if err := adapter.Close(); err != nil {
			failure := fail(scenario, FailureTeardown, "adapter close: %v; prior failure: %v", err, result)
			result = &failure
		}
	}()
	r, err := newRig(ctx, adapter, scenario, profile)
	if err != nil {
		failure := fail(scenario, FailureSetup, "production Host fixture: %v", err)
		return &failure
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		if err := r.supervisor.Stop(stopCtx); err != nil || r.supervisor.Phase() != integrations.SupervisorStopped {
			failure := fail(scenario, FailureTeardown, "pilot did not stop cleanly: %v (phase %v); prior failure: %v", err, r.supervisor.Phase(), result)
			result = &failure
		}
	}()
	if err := adapter.Ready(ctx); err != nil {
		failure := fail(scenario, FailureDiscovery, "pilot ready: %v", err)
		return &failure
	}
	if failure := r.checkInitial(scenario); failure != nil {
		return failure
	}
	switch scenario {
	case CaseCore:
		return r.checkCore(ctx)
	case CaseCapacity:
		return r.checkCapacity(ctx)
	case CaseReconnect:
		return r.checkReconnect(ctx)
	case CaseUnspecified:
		failure := fail(scenario, FailureSetup, "unspecified case")
		return &failure
	default:
		failure := fail(scenario, FailureSetup, "unknown case")
		return &failure
	}
}

func (r *rig) checkInitial(scenario Case) *Failure {
	for index, entity := range r.entities {
		state, ok := r.fixture.State(entity)
		if !ok {
			failure := fail(scenario, FailureDiscovery, "missing initial state for source %d", index)
			return &failure
		}
		value, fresh := state.CriticalValue()
		if !fresh {
			failure := fail(scenario, FailureDiscovery, "initial state for source %d is not fresh observed", index)
			return &failure
		}
		if index == 0 {
			on, isSwitch := value.Switch()
			if !isSwitch || on {
				failure := fail(scenario, FailureDiscovery, "switch initial value is not false")
				return &failure
			}
		} else {
			light, isLight := value.Light()
			if !isLight {
				failure := fail(scenario, FailureDiscovery, "light initial value absent")
				return &failure
			}
			on, brightness, colorTemp, err := light.Components()
			validColorTemp := colorTemp == nil
			if r.profile.LightShape() == LightBrightnessAndColorTemp {
				validColorTemp = colorTemp != nil && *colorTemp == 300
			}
			if err != nil || !on || brightness == nil || *brightness != 50 || !validColorTemp {
				failure := fail(scenario, FailureDiscovery, "light initial value incomplete: %v", err)
				return &failure
			}
		}
	}
	return nil
}

func (r *rig) checkCore(ctx context.Context) *Failure {
	const switchID = "nha095-core-switch"
	request, failure := r.command(ctx, CaseCore, switchID, 0, true)
	if failure != nil {
		return failure
	}
	for _, stimulus := range []Stimulus{StimulusUncorrelated, StimulusWrongCorrelation, StimulusStale} {
		if failure := r.applyAndExpect(ctx, CaseCore, switchID, stimulus, r.pending, FailureUnprovenConfirmation); failure != nil {
			return failure
		}
	}
	if failure := r.applyAndExpect(ctx, CaseCore, switchID, StimulusMatch, actions.ActionStatus_ACTION_STATUS_CONFIRMED, FailureMissingConfirmation); failure != nil {
		return failure
	}
	before := r.adapter.Deliveries()
	copyRequest := proto.Clone(request).(*actions.CommandRequest)
	response, err := r.service.Dispatch(ctx, r.actor, copyRequest)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || r.adapter.Deliveries() != before {
		failure := fail(CaseCore, FailureDuplicate, "duplicate result=%v error=%v deliveries=%d→%d", response, err, before, r.adapter.Deliveries())
		return &failure
	}
	const lightID = "nha095-core-light"
	if _, failure := r.command(ctx, CaseCore, lightID, 1, false); failure != nil {
		return failure
	}
	if failure := r.applyAndExpect(ctx, CaseCore, lightID, StimulusPartialLight, r.pending, FailureUnprovenConfirmation); failure != nil {
		return failure
	}
	if failure := r.applyAndExpect(ctx, CaseCore, lightID, StimulusMatch, actions.ActionStatus_ACTION_STATUS_CONFIRMED, FailureMissingConfirmation); failure != nil {
		return failure
	}
	const timeoutID = "nha095-core-timeout"
	if _, failure := r.command(ctx, CaseCore, timeoutID, 0, false); failure != nil {
		return failure
	}
	if err := r.clock.advance(2 * time.Minute); err != nil {
		failure := fail(CaseCore, FailureTimeout, "advance clock: %v", err)
		return &failure
	}
	if failure := r.expectStatus(CaseCore, timeoutID, actions.ActionStatus_ACTION_STATUS_INDETERMINATE, FailureTimeout); failure != nil {
		return failure
	}
	if failure := r.applyAndExpect(ctx, CaseCore, timeoutID, StimulusMatch, actions.ActionStatus_ACTION_STATUS_INDETERMINATE, FailureLateResponse); failure != nil {
		return failure
	}
	return nil
}

func (r *rig) checkCapacity(ctx context.Context) *Failure {
	const first = "nha095-capacity-first"
	if _, failure := r.command(ctx, CaseCapacity, first, 0, true); failure != nil {
		return failure
	}
	second := r.request("nha095-capacity-second", 0, true)
	response, err := r.service.Dispatch(ctx, r.actor, second)
	if !errors.Is(err, integrations.ErrVolatileLedgerFull) || response != nil {
		failure := fail(CaseCapacity, FailureCapacity, "second valid intention response=%v error=%v, want ledger full", response, err)
		return &failure
	}
	if failure := r.applyAndExpect(ctx, CaseCapacity, first, StimulusMatch, actions.ActionStatus_ACTION_STATUS_CONFIRMED, FailureMissingConfirmation); failure != nil {
		return failure
	}
	return nil
}

func (r *rig) checkReconnect(ctx context.Context) *Failure {
	before, ok := r.fixture.State(r.entities[0])
	if !ok {
		failure := fail(CaseReconnect, FailureDiscovery, "missing pre-reconnect state")
		return &failure
	}
	_, _, _, oldSource, _, err := before.Components()
	if err != nil {
		failure := fail(CaseReconnect, FailureDiscovery, "pre-reconnect source: %v", err)
		return &failure
	}
	oldSession, _, err := oldSource.Components()
	if err != nil {
		failure := fail(CaseReconnect, FailureDiscovery, "pre-reconnect session: %v", err)
		return &failure
	}
	if err := r.adapter.Crash(ctx); err != nil {
		failure := fail(CaseReconnect, FailureReconnect, "crash stimulus: %v", err)
		return &failure
	}
	if err := r.clock.waitArmed(ctx); err != nil {
		failure := fail(CaseReconnect, FailureReconnect, "retry barrier: %v", err)
		return &failure
	}
	if err := r.clock.advance(time.Second); err != nil {
		failure := fail(CaseReconnect, FailureReconnect, "retry clock: %v", err)
		return &failure
	}
	if err := r.adapter.Ready(ctx); err != nil {
		failure := fail(CaseReconnect, FailureReconnect, "new pilot ready: %v", err)
		return &failure
	}
	after, ok := r.fixture.State(r.entities[0])
	if !ok {
		failure := fail(CaseReconnect, FailureReconnect, "missing new-session state")
		return &failure
	}
	_, _, _, newSource, _, err := after.Components()
	if err != nil {
		failure := fail(CaseReconnect, FailureReconnect, "new source: %v", err)
		return &failure
	}
	newSession, _, err := newSource.Components()
	if err != nil || newSession == oldSession {
		failure := fail(CaseReconnect, FailureReconnect, "session did not change: old=%s new=%s error=%v", oldSession, newSession, err)
		return &failure
	}
	const id = "nha095-reconnect-command"
	if _, failure := r.command(ctx, CaseReconnect, id, 0, true); failure != nil {
		return failure
	}
	if failure := r.applyAndExpect(ctx, CaseReconnect, id, StimulusMatch, actions.ActionStatus_ACTION_STATUS_CONFIRMED, FailureMissingConfirmation); failure != nil {
		return failure
	}
	return nil
}

// Run reports every typed failure to a testing.TB. The same Check result is
// available directly for a negative pilot test.
func Run(t interface {
	Helper()
	Errorf(string, ...any)
	TempDir() string
	Context() context.Context
}, factory AdapterFactory) {
	t.Helper()
	for _, failure := range Check(t.Context(), factory, Options{WorkDir: t.TempDir()}) {
		t.Errorf("conformance: %s", failure.Error())
	}
}
