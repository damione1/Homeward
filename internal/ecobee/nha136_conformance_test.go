package ecobee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/sdk/conformance"
	"github.com/mctofu/homekit/client"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fixtureControl struct {
	Ctx                       context.Context
	command                   integrations.AdmittedCommand
	fault                     bool
	partial                   []byte
	put, get, ack             chan struct{}
	read                      chan conformance.Stimulus
	putOnce, getOnce, ackOnce sync.Once
	getHandled                atomic.Bool
}

func (c *fixtureControl) intercept(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/characteristics" {
		return false
	}
	if r.Method == http.MethodPut {
		c.putOnce.Do(func() { close(c.put) })
		select {
		case <-c.ack:
			return false
		case <-c.Ctx.Done():
			http.Error(w, "fixture canceled", http.StatusServiceUnavailable)
			return true
		}
	}
	if r.Method == http.MethodGet {
		if !c.getHandled.CompareAndSwap(false, true) {
			return false
		}
		if c.fault {
			_ = c.command.ConfirmProtocol(nha136AckOnlyProof{})
		}
		c.getOnce.Do(func() { close(c.get) })
		select {
		case stimulus := <-c.read:
			if stimulus == conformance.StimulusPartialClimate {
				w.Header().Set("Content-Type", "application/hap+json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(c.partial)
				return true
			}
			return false
		case <-c.Ctx.Done():
			http.Error(w, "fixture canceled", http.StatusServiceUnavailable)
			return true
		}
	}
	return false
}

// Deliberately faulty trusted driver, used only to prove the suite catches ACK proof.
type nha136AckOnlyProof struct{}

func (nha136AckOnlyProof) QualifyProtocolConfirmation(integrations.AdmittedCommand) error { return nil }

type climateTestAdapter struct {
	fixture       *ordinaryFixture
	t             *testing.T
	value         storage.SecretValue
	settings      Settings
	fault         bool
	mu            sync.Mutex
	current       *Pilot
	controls      map[domain.CommandID]*fixtureControl
	changed       chan struct{}
	ready         chan struct{}
	startupErrors chan error
	Ctx           context.Context
	Cancel        context.CancelFunc
	active        atomic.Pointer[fixtureControl]
	clock         *schedulerClock
	periodicAt    []time.Time
}

func newClimateAdapter(t *testing.T, fault bool) *climateTestAdapter {
	return newClimateAdapterWithClock(t, fault, nil)
}
func newClimateAdapterWithClock(t *testing.T, fault bool, clock *schedulerClock) *climateTestAdapter {
	f := newOrdinaryFixture(t)
	input := PairingInput{Version: 1, ControllerID: f.Controller.DeviceID, ControllerPublicKey: f.Controller.PublicKey, ControllerPrivateKey: f.Controller.PrivateKey, AccessoryID: f.Pairing.DeviceID, AccessoryPublicKey: f.Pairing.PublicKey}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	value, err := storage.NewSecretValue(raw)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := ParseSettings(Input{Instance: "nha095-synthetic", Address: f.Address, AccessoryID: f.Pairing.DeviceID, CredentialSecret: "synthetic-pairing"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.Ctx)
	adapter := &climateTestAdapter{fixture: f, t: t, value: value, settings: settings, fault: fault, clock: clock, controls: make(map[domain.CommandID]*fixtureControl), changed: make(chan struct{}), ready: make(chan struct{}, 8), startupErrors: make(chan error, 8), Ctx: ctx, Cancel: cancel}
	f.SetInterceptor(func(w http.ResponseWriter, r *http.Request) bool {
		control := adapter.active.Load()
		if clock != nil && r.URL.Path == "/characteristics" && r.Method == http.MethodGet && (control == nil || control.getHandled.Load()) {
			adapter.mu.Lock()
			adapter.periodicAt = append(adapter.periodicAt, clock.Now())
			adapter.mu.Unlock()
		}
		return control != nil && control.intercept(w, r)
	})
	return adapter
}
func (a *climateTestAdapter) Spec(instance conformance.IntegrationInstanceID) (conformance.PilotSpec, error) {
	own, _ := a.settings.Instance()
	if own != instance {
		return conformance.PilotSpec{}, ErrSettings
	}
	var factory integrations.PilotFactory
	var err error
	if a.clock != nil {
		factory, err = NewPilotFactoryWithClock(a.settings, a.clock)
	} else {
		factory, err = NewPilotFactory(a.settings)
	}
	if err != nil {
		return conformance.PilotSpec{}, err
	}
	return integrations.NewPilotSpec(instance, func() integrations.Pilot {
		p := factory().(*Pilot)
		a.mu.Lock()
		a.current = p
		a.mu.Unlock()
		return &observedClimatePilot{inner: p, adapter: a}
	})
}
func (a *climateTestAdapter) Completion() conformance.CompletionSemantics {
	return conformance.CompletionAcknowledged
}
func (a *climateTestAdapter) Ready(ctx context.Context) error {
	// A real Start/task/initial Host failure must not turn into an opaque Ready timeout.
	select {
	case err := <-a.startupErrors:
		return err
	default:
	}
	select {
	case err := <-a.startupErrors:
		return err
	case <-a.ready:
		if a.clock != nil {
			a.clock.advance(14 * time.Second)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (a *climateTestAdapter) control(ctx context.Context, id domain.CommandID) (*fixtureControl, error) {
	for {
		a.mu.Lock()
		control := a.controls[id]
		changed := a.changed
		a.mu.Unlock()
		if control != nil {
			return control, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
func (a *climateTestAdapter) Delivered(ctx context.Context, id conformance.CommandID) error {
	control, err := a.control(ctx, id)
	if err != nil {
		return err
	}
	select {
	case <-control.put:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (a *climateTestAdapter) Apply(ctx context.Context, id conformance.CommandID, stimulus conformance.Stimulus) error {
	control, err := a.control(ctx, id)
	if err != nil {
		return err
	}
	switch stimulus {
	case conformance.StimulusAcknowledge:
		if a.clock != nil {
			a.clock.advance(6 * time.Second)
		}
		control.ackOnce.Do(func() { close(control.ack) })
		select {
		case <-control.get:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case conformance.StimulusMatch, conformance.StimulusPartialClimate:
		select {
		case control.read <- stimulus:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case conformance.StimulusUnspecified, conformance.StimulusUncorrelated, conformance.StimulusWrongCorrelation, conformance.StimulusStale, conformance.StimulusPartialLight:
		return ErrSettings
	default:
		return ErrSettings
	}
}
func (a *climateTestAdapter) Crash(context.Context) error {
	a.mu.Lock()
	p := a.current
	a.mu.Unlock()
	if p == nil {
		return ErrUnavailable
	}
	a.active.Store(nil)
	return p.Stop(context.Background())
}
func (a *climateTestAdapter) Deliveries() int { return int(a.fixture.CharacteristicPUT.Load()) }
func (a *climateTestAdapter) Close() error {
	a.Cancel()
	a.active.Store(nil)
	a.mu.Lock()
	p := a.current
	a.mu.Unlock()
	var err error
	if p != nil {
		err = p.Stop(context.Background())
	}
	a.fixture.Close(a.t)
	return err
}

type observedClimatePilot struct {
	inner   *Pilot
	adapter *climateTestAdapter
}

func (p *observedClimatePilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	startup := &nha136StartupWitness{adapter: p.adapter}
	err := p.inner.Start(ctx, &climateSecretHost{SessionHost: host, adapter: p.adapter, startup: startup}, nha136ObservedTasks{Tasks: tasks, startup: startup})
	startup.report("Start", err)
	return err
}
func (p *observedClimatePilot) Stop(ctx context.Context) error            { return p.inner.Stop(ctx) }
func (p *observedClimatePilot) CommandDriver() integrations.CommandDriver { return p }
func (p *observedClimatePilot) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	id, _, err := command.RouteIdentity()
	if err != nil {
		return err
	}
	control := &fixtureControl{Ctx: p.adapter.Ctx, command: command, fault: p.adapter.fault, put: make(chan struct{}), get: make(chan struct{}), ack: make(chan struct{}), read: make(chan conformance.Stimulus, 1)}
	request, err := command.Request()
	if err != nil {
		return err
	}
	climate := request.Arguments.GetSetClimate()
	if climate == nil {
		return ErrSettings
	}
	if climate.SetpointCelsius == nil {
		// Range/fan cases own their real HAP interceptor; the SDK partial
		// witness applies only to its setpoint command shape.
		return p.inner.Dispatch(ctx, command)
	}
	target, ok := p.adapter.fixture.CharacteristicIdentity("35")
	if !ok {
		return ErrSettings
	}
	// One commanded field agrees; commanded mode is deliberately absent.
	control.partial = []byte(fmt.Sprintf(`{"characteristics":[{"aid":%d,"iid":%d,"value":%g}]}`, target.AccessoryID, target.CharacteristicID, *request.Arguments.GetSetClimate().SetpointCelsius))
	p.adapter.mu.Lock()
	p.adapter.controls[id] = control
	close(p.adapter.changed)
	p.adapter.changed = make(chan struct{})
	p.adapter.mu.Unlock()
	p.adapter.active.Store(control)
	err = p.inner.Dispatch(ctx, command)
	p.adapter.active.CompareAndSwap(control, nil)
	return err
}

// This test-only secret provider substitutes a synthetic credential while every
// discovery, state, command and proof still crosses the real session/Host. The
// server integration tests exercise the actual encrypted SQL provider separately.
type climateSecretHost struct {
	integrations.SessionHost
	adapter *climateTestAdapter
	once    sync.Once
	startup *nha136StartupWitness
}

func (h *climateSecretHost) Secrets() (integrations.SecretProvider, error) { return h, nil }
func (h *climateSecretHost) Read(ctx context.Context, handle storage.SecretHandle) (storage.SecretValue, error) {
	if ctx.Err() != nil {
		return storage.SecretValue{}, ctx.Err()
	}
	if handle != h.adapter.settings.CredentialHandle() {
		return storage.SecretValue{}, integrations.ErrForeignSecretHandle
	}
	return h.adapter.value, nil
}
func (h *climateSecretHost) Discover(ctx context.Context, candidates []integrations.Candidate) ([]integrations.Binding, error) {
	bindings, err := h.SessionHost.Discover(ctx, candidates)
	h.startup.report("Host.Discover", err)
	return bindings, err
}
func (h *climateSecretHost) Publish(ctx context.Context, b integrations.Binding, s integrations.StateObservation) error {
	if err := h.SessionHost.Publish(ctx, b, s); err != nil {
		h.startup.report("initial Host.Publish", err)
		return err
	}
	h.startup.published.Store(true)
	h.once.Do(func() { h.adapter.ready <- struct{}{} })
	return nil
}

// Fixture-only observation. Each supervisor Start gets its own witness, so an
// intentional stop after a successful publication cannot poison reconnect Ready.
// The existing task owner still starts, joins and receives the exact task error.
type nha136StartupFailure struct {
	phase string
	cause error
}

func (e *nha136StartupFailure) Error() string {
	return "NHA136 startup " + e.phase + ": " + e.cause.Error()
}
func (e *nha136StartupFailure) Unwrap() error { return e.cause }

type nha136StartupWitness struct {
	adapter   *climateTestAdapter
	published atomic.Bool
	reported  atomic.Bool
}

func (s *nha136StartupWitness) report(phase string, err error) {
	if err == nil || s.published.Load() || !s.reported.CompareAndSwap(false, true) {
		return
	}
	select {
	case s.adapter.startupErrors <- &nha136StartupFailure{phase: phase, cause: err}:
	default:
		s.adapter.t.Error("startup diagnostic buffer unexpectedly full")
	}
}

type nha136ObservedTasks struct {
	integrations.Tasks
	startup *nha136StartupWitness
}

func (t nha136ObservedTasks) Go(task integrations.Task) error {
	err := t.Tasks.Go(func(ctx context.Context) error {
		err := task(ctx)
		if err != nil {
			t.startup.report("task", err)
		} else if !t.startup.published.Load() && ctx.Err() == nil {
			t.startup.report("task exited before initial publication", ErrUnavailable)
		}
		return err
	})
	t.startup.report("Tasks.Go admission", err)
	return err
}

func TestNHA136RealPilotUsesPublicHostConformance(t *testing.T) {
	profile, err := conformance.NewClimateFixtureProfile(conformance.FixtureSource{Device: "CA:FE:00:11:22:33", Entity: "thermostat", Endpoint: Endpoint, Schema: contracts.ClimateSchema})
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []bool{false, true} {
		t.Run(fmt.Sprintf("fault-%v", fault), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			failures := conformance.Check(ctx, func(conformance.Case, string) (conformance.Adapter, error) { return newClimateAdapter(t, fault), nil }, conformance.Options{WorkDir: t.TempDir(), Timeout: 15 * time.Second, Profile: &profile})
			if !fault {
				if len(failures) != 0 {
					t.Fatal(failures)
				}
				return
			}
			if len(failures) != 4 {
				t.Fatalf("fault was not caught in all four cases: %v", failures)
			}
			for _, failure := range failures {
				if failure.Code() != conformance.FailureUnprovenConfirmation {
					t.Fatalf("wrong failure: %v", failure)
				}
			}
		})
	}
	for _, status := range []int{-70404, 0} {
		name := "write-refused"
		if status == 0 {
			name = "ack-without-state-change"
		}
		t.Run(name, func(t *testing.T) { nha136NegativeReadback(t, status) })
	}
	for _, shape := range []string{"partial-missing", "partial-different", "complete"} {
		t.Run("range-fan-"+shape, func(t *testing.T) { nha136ReadbackCase(t, 0, shape) })
	}
}

type nha136Generation struct{}

func (nha136Generation) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(7)
}

type nha136NoJitter struct{}

func (nha136NoJitter) Offset(time.Duration) time.Duration { return 0 }

// Both negative responses cross the actual encrypted HAP client, production
// Pilot and public Host action ledger. The fake never applies the received PUT.
func nha136NegativeReadback(t *testing.T, hapStatus int) { nha136ReadbackCase(t, hapStatus, "") }

func nha136ReadbackCase(t *testing.T, hapStatus int, readback string) {
	t.Helper()
	adapter := newClimateAdapter(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var puts atomic.Int32
	getEntered, releaseRead := make(chan struct{}), make(chan struct{})
	var getOnce sync.Once
	adapter.fixture.SetInterceptor(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/characteristics" {
			return false
		}
		if r.Method == http.MethodPut {
			var request client.CharacteristicsWriteRequest
			raw, readErr := io.ReadAll(io.LimitReader(r.Body, 4096))
			if err := json.Unmarshal(raw, &request); readErr != nil || err != nil || len(request.Characteristics) == 0 {
				http.Error(w, "invalid synthetic write", http.StatusBadRequest)
				return true
			}
			puts.Add(1)
			if readback != "" {
				// Literal HAP types and expected values, independent of commandValues/write.
				want := map[string]float64{"12": 19.5, "D": 25.5, "BF": 0}
				if len(request.Characteristics) != 3 {
					t.Error("range/fan wire field count differs")
				}
				for _, row := range request.Characteristics {
					matched := false
					for typ, value := range want {
						id, ok := adapter.fixture.CharacteristicIdentity(typ)
						if ok && id.AccessoryID == row.AccessoryID && id.CharacteristicID == row.CharacteristicID {
							if number, ok := row.Value.(float64); !ok || number != value {
								t.Error("range/fan wire value differs", typ, row.Value)
							}
							delete(want, typ)
							matched = true
							break
						}
					}
					if !matched {
						t.Error("range/fan unexpected wire identity")
					}
				}
				if len(want) != 0 {
					t.Error("range/fan wire field missing")
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
				return false // Let the real fake hc apply all three writes.
			}
			rows := make([]client.CharacteristicWriteResponse, len(request.Characteristics))
			for i, row := range request.Characteristics {
				rows[i] = client.CharacteristicWriteResponse{AccessoryID: row.AccessoryID, CharacteristicID: row.CharacteristicID, Status: &hapStatus}
			}
			w.Header().Set("Content-Type", "application/hap+json")
			w.WriteHeader(http.StatusMultiStatus)
			if err := json.NewEncoder(w).Encode(struct {
				Characteristics []client.CharacteristicWriteResponse `json:"characteristics"`
			}{rows}); err != nil {
				t.Error("synthetic write response", err)
			}
			return true
		}
		if r.Method == http.MethodGet && puts.Load() > 0 {
			getOnce.Do(func() { close(getEntered) })
			select {
			case <-releaseRead:
				if readback != "" {
					values := map[string]float64{"F": 0, "33": 1, "11": 21.5, "35": 21.5, "36": 0, "10": 45, "12": 19.5, "D": 25.5, "AF": 0, "BF": 0}
					if readback == "partial-missing" {
						delete(values, "D")
					}
					if readback == "partial-different" {
						values["D"] = 24.5
					}
					rows := make([]client.CharacteristicReadResponse, 0, len(values))
					for typ, value := range values {
						id, ok := adapter.fixture.CharacteristicIdentity(typ)
						if !ok {
							t.Error("literal fake identity absent", typ)
							continue
						}
						rows = append(rows, client.CharacteristicReadResponse{AccessoryID: id.AccessoryID, CharacteristicID: id.CharacteristicID, Value: []byte(fmt.Sprintf("%g", value))})
					}
					w.Header().Set("Content-Type", "application/hap+json")
					if err := json.NewEncoder(w).Encode(struct {
						Characteristics []client.CharacteristicReadResponse `json:"characteristics"`
					}{rows}); err != nil {
						t.Error(err)
					}
					return true
				}
				return false // The real encrypted GET reads the unchanged accessory.
			case <-adapter.Ctx.Done():
				http.Error(w, "fixture canceled", http.StatusServiceUnavailable)
				return true
			}
		}
		return false
	})
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := adapter.settings.Instance()
	key, _ := integrations.NewSourceKey(adapter.fixture.Pairing.DeviceID, "thermostat")
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias(Alias)
	record, err := integrations.NewRegistryRecord(ids[0].Device, ids[0].Entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	service, err := integrations.NewVolatileCommandService(host, nha136Generation{}, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	spec, err := adapter.Spec(instance)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := integrations.NewRetryPolicy(0, time.Second, time.Second, 5*time.Second)
	supervisor, err := integrations.NewSupervisor(host, []integrations.PilotSpec{spec}, policy, integrations.RealTimer{}, nha136NoJitter{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adapter.Cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), 7*time.Second)
		defer stop()
		if err := supervisor.Stop(stopCtx); err != nil {
			t.Error("negative readback supervisor join", err)
		}
		if err := adapter.Close(); err != nil {
			t.Error("negative readback fixture close", err)
		}
	})
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	epoch, err := fixture.CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := ids[0].Entity.Value()
	actor, _ := domain.ParseActorID("nha136-negative-actor")
	id, _ := domain.ParseCommandID("nha136-negative-readback")
	idText, _ := id.Value()
	setpoint := 22.5
	request := &actions.CommandRequest{CommandId: idText, AdmissionEpoch: epoch, ConfigRevision: 7, Target: &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entity}}, CapabilitySchemaId: contracts.ClimateSchema, Endpoint: Endpoint, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: &setpoint}}}, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
	if readback != "" {
		low, high, fan := 19.5, 25.5, registry.ClimateFanTarget_CLIMATE_FAN_TARGET_ON
		request.Arguments = &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{HeatThresholdCelsius: &low, CoolThresholdCelsius: &high, FanTarget: &fan}}}
	}

	type result struct {
		response *actions.CommandResponse
		err      error
	}
	done := make(chan result, 1)
	go func() { response, err := service.Dispatch(ctx, actor, request); done <- result{response, err} }()
	if hapStatus == 0 {
		waitNHA136PollSignal(t, getEntered, "separate unchanged GET entered after ACK")
		progress, err := host.ActionStatus(id)
		if err != nil || progress.Status != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
			t.Fatal("ACK without matching observation confirmed or lost pending status", progress, err)
		}
		close(releaseRead)
	}
	select {
	case actual := <-done:
		if readback == "complete" {
			if actual.err != nil || actual.response == nil || actual.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				t.Fatal("complete range/fan readback failed", actual.response, actual.err)
			}
		} else if !errors.Is(actual.err, integrations.ErrCommandOutcomeIndeterminate) || actual.response == nil || actual.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
			t.Fatal("refused/unchanged write did not remain indeterminate", actual.response, actual.err)
		}
	case <-ctx.Done():
		t.Fatal("negative readback dispatch join", ctx.Err())
	}
	progress, err := host.ActionStatus(id)
	wantStatus := actions.ActionStatus_ACTION_STATUS_INDETERMINATE
	if readback == "complete" {
		wantStatus = actions.ActionStatus_ACTION_STATUS_CONFIRMED
	}
	if err != nil || progress.Status != wantStatus {
		t.Fatal("refused/unchanged write became confirmed", progress, err)
	}
	wantApplied := int32(0)
	if readback != "" {
		wantApplied = 1
	}
	if puts.Load() != 1 || adapter.fixture.CharacteristicPUT.Load() != wantApplied {
		t.Fatal("negative write replayed or applied to synthetic accessory")
	}
	if hapStatus != 0 {
		select {
		case <-getEntered:
			t.Fatal("refused PUT was treated as an ACK/readback")
		default:
		}
	}
}
