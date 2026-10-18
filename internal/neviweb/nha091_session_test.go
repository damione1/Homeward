package neviweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha091SessionRig struct {
	settings Settings
	pilot    *Pilot
	host     *integrations.Manager
	catalog  *contracts.CapabilityCatalog
	sequence *sequencer.Sequencer
	session  *integrations.Session
	secrets  *storage.SecretRepository
	quota    *storage.CloudAttemptQuota
}

func newNHA091SessionRig(t *testing.T, handler http.HandlerFunc) nha091SessionRig {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	cluster := testpg.Start(t)
	dbSettings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(key string) (string, bool) {
		return testpg.Password(), key == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dbSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	secrets, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	settings := nha091Settings(t)
	settings.baseURL, err = func() (url.URL, error) {
		parsed, e := url.Parse(upstream.URL)
		if e != nil {
			return url.URL{}, e
		}
		return *parsed, nil
	}()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range settings.bindings {
		if _, err := repository.ConfigureBinding(ctx, settings.instance, binding.key, binding.alias); err != nil {
			t.Fatal(err)
		}
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 16384)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.AttachProjection(ctx, sequence); err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(repository, sequence, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachSecrets(secrets); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(settings.instance)
	if err != nil {
		t.Fatal(err)
	}
	account, err := integrations.NewCloudAccountBinding(settings.instance, settings.account)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), []integrations.CloudAccountBinding{account}, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	quota, err := storage.NewCloudAttemptQuota(store, "123", 5000, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	client, err := integrations.NewCloudClient(session, policy, nil, quota)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	decision, err := heatingownership.Parse(scope.HeatingOwnershipV1(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewPilotFactory(settings, repository, decision, catalog, policy, quota)
	if err != nil {
		t.Fatal(err)
	}
	pilot := factory().(*Pilot)
	pilot.client = client
	return nha091SessionRig{settings: settings, pilot: pilot, host: host, catalog: catalog, sequence: sequence, session: session, secrets: secrets, quota: quota}
}

type nha091Generation struct{}

func (nha091Generation) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(7)
}

func TestNHA091CookieRotationUsesCanonicalEncryptedSession(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	rig := newNHA091SessionRig(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("Cookie"))
		index := len(sent)
		mu.Unlock()
		if index == 1 {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "response-rotated"})
		}
		_, _ = w.Write([]byte(`{}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first, err := activeSessionValue("same-session-id", "sid=durable-old", "123")
	if err != nil || rig.secrets.RotateCredential(ctx, rig.settings.session, first) != nil {
		t.Fatal("initial encrypted fixture session")
	}
	adopt := func() {
		t.Helper()
		state, e := rig.pilot.session(ctx, rig.session, false)
		if e != nil {
			t.Fatal(e)
		}
		rig.pilot.adoptSession(state)
	}
	adopt()
	for i := 0; i < 2; i++ {
		if _, err := rig.pilot.fetch(ctx, "/cookie-probe", nil); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := activeSessionValue("same-session-id", "sid=durable-new", "123")
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.secrets.RotateCredential(ctx, rig.settings.session, changed); err != nil {
		t.Fatal(err)
	}
	adopt()
	if _, err := rig.pilot.fetch(ctx, "/cookie-probe", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), sent...)
	mu.Unlock()
	if len(got) != 3 || got[0] != "sid=durable-old" || got[1] != "sid=response-rotated" || got[2] != "sid=durable-new" {
		t.Fatal("response or SQL cookie rotation did not control the next real HTTP request")
	}
	count, err := rig.quota.Today(ctx)
	if err != nil || count != 3 {
		t.Fatalf("reserved attempts=%d, error=%v", count, err)
	}
}

func TestNHA091ReadExpiryReconnectsOnceImmediately(t *testing.T) {
	var mu sync.Mutex
	var loginCount, locationCount int
	secondExpiry := make(chan struct{}, 1)
	rig := newNHA091SessionRig(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/login":
			loginCount++
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: fmt.Sprintf("fixture-%d", loginCount)})
			_, _ = fmt.Fprintf(w, `{"session":"session-%d","account":{"id":123}}`, loginCount)
		case "/api/locations":
			locationCount++
			_, _ = w.Write([]byte(`{"error":{"code":"USRSESSEXP"}}`))
			if locationCount == 2 {
				secondExpiry <- struct{}{}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, item := range []struct {
		handle storage.SecretHandle
		value  string
	}{{rig.settings.username, "fixture-user"}, {rig.settings.password, "fixture-password"}} {
		value, _ := storage.NewSecretValue([]byte(item.value))
		if err := rig.secrets.RotateCredential(ctx, item.handle, value); err != nil {
			t.Fatal(err)
		}
	}
	initial, err := InitialSessionValue()
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.secrets.RotateCredential(ctx, rig.settings.session, initial); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rig.pilot.run(runCtx, rig.session); close(done) }()
	select {
	case <-secondExpiry:
	case <-time.After(6 * time.Second):
		stop()
		<-done
		t.Fatal("read expiry did not trigger immediate bounded reconnect")
	}
	select {
	case <-time.After(150 * time.Millisecond):
	case <-done:
		t.Fatal("pilot exited instead of waiting after the second expiry")
	}
	mu.Lock()
	logins, locations := loginCount, locationCount
	mu.Unlock()
	stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("quota/session wait did not cancel")
	}
	if logins != 2 || locations != 2 {
		t.Fatalf("recovery attempts=%d logins, %d location reads", logins, locations)
	}
	count, err := rig.quota.Today(ctx)
	if err != nil || count != 4 {
		t.Fatalf("recovery quota reservations=%d, error=%v", count, err)
	}
}

func TestNHA091QualifiedSecondCycleReplacesSameHostDescriptions(t *testing.T) {
	var cycle atomic.Int32
	var unknownReads atomic.Int32
	rig := newNHA091SessionRig(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/locations":
			_, _ = w.Write([]byte(`[{"id":456}]`))
		case r.URL.Path == "/api/devices":
			pass := cycle.Add(1)
			var rows []string
			for i := 0; i < 11; i++ {
				if i != 9 {
					model := 1123
					if i == 0 && pass == 1 {
						model = 999999
					} else if i == 8 {
						model = 2151
					}
					rows = append(rows, fmt.Sprintf(`{"id":"device-%02d","signature":{"model":%d}}`, i, model))
				}
			}
			_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
		case strings.HasSuffix(r.URL.Path, "/attribute") && strings.Contains(r.URL.Path, "device-00"):
			unknownReads.Add(1)
			_, _ = w.Write([]byte(`{"roomTemperature":{"value":20},"roomSetpoint":21,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"heat"}`))
		case strings.HasSuffix(r.URL.Path, "/attribute") && strings.Contains(r.URL.Path, "device-08"):
			_, _ = w.Write([]byte(`{"onOff":"off"}`))
		case strings.HasSuffix(r.URL.Path, "/attribute"):
			_, _ = w.Write([]byte(`{"roomTemperature":{"value":20},"roomSetpoint":21,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"heat"}`))
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"status":"online"}`))
		case strings.HasSuffix(r.URL.Path, "/notifications"):
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	value, err := activeSessionValue("fixture-session", "sid=fixture-cookie", "123")
	if err != nil || rig.secrets.RotateCredential(ctx, rig.settings.session, value) != nil {
		t.Fatal("fixture session")
	}
	if err := rig.pilot.poll(ctx, rig.session); !errors.Is(err, ErrPartialObservation) {
		t.Fatalf("unqualified first cycle: %v", err)
	}
	if unknownReads.Load() != 0 {
		t.Fatal("unknown model received a speculative attribute query")
	}
	first, ok := rig.pilot.bindings[retainedAliases[0]]
	if !ok {
		t.Fatal("first binding absent")
	}
	_, entity, err := first.IDs()
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := rig.catalog.Descriptions(entity)
	if err != nil || len(capabilities) != 1 || len(capabilities[0].GetClimate().ModeOptions) != 0 {
		t.Fatalf("unqualified model exposed actions: %v", err)
	}
	if err := rig.pilot.poll(ctx, rig.session); err != nil {
		t.Fatalf("qualified second cycle: %v", err)
	}
	second := rig.pilot.bindings[retainedAliases[0]]
	if second != first {
		t.Fatal("model qualification changed the physical identity")
	}
	capabilities, err = rig.catalog.Descriptions(entity)
	if err != nil || len(capabilities) != 1 || capabilities[0].GetClimate().SetpointCelsius == nil || len(capabilities[0].GetClimate().ModeOptions) != 2 {
		t.Fatalf("qualified model was not projected into the shared catalog: %v", err)
	}
	if cycle.Load() != 2 || unknownReads.Load() != 1 {
		t.Fatal("qualified device was not queried after bounded inventory refresh")
	}
	if rig.pilot.capabilitiesChanged(cycleSnapshot{frames: map[string]attributeFrame{retainedAliases[0]: {model: rig.pilot.declared[retainedAliases[0]]}}}) {
		t.Fatal("unchanged model would rediscover every poll")
	}
}

func TestNHA091MissingDeviceDoesNotBlockOtherReadings(t *testing.T) {
	var listings, missingReads, unsupportedReads, badQuery atomic.Int32
	rig := newNHA091SessionRig(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/attribute") {
			want := "roomTemperature,roomSetpoint,roomSetpointMin,roomSetpointMax,systemMode"
			if strings.Contains(r.URL.Path, "device-08") {
				want = "onOff"
			}
			if r.URL.Query().Get("attributes") != want {
				badQuery.Add(1)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		switch {
		case r.URL.Path == "/api/locations":
			_, _ = w.Write([]byte(`[{"id":456}]`))
		case r.URL.Path == "/api/devices":
			pass := listings.Add(1)
			var rows []string
			for i := 0; i < 11; i++ {
				if i == 9 || i == 0 && pass == 1 {
					continue
				}
				model := 1123
				if i == 8 {
					model = 2151
				}
				rows = append(rows, fmt.Sprintf(`{"id":"device-%02d","signature":{"model":%d}}`, i, model))
			}
			_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
		case strings.HasSuffix(r.URL.Path, "/attribute") && strings.Contains(r.URL.Path, "device-00"):
			missingReads.Add(1)
			_, _ = w.Write([]byte(`{"roomTemperature":{"value":20},"roomSetpoint":21,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"heat"}`))
		case strings.HasSuffix(r.URL.Path, "/attribute") && strings.Contains(r.URL.Path, "device-02"):
			unsupportedReads.Add(1)
			if listings.Load() == 1 {
				_, _ = w.Write([]byte(`{"error":{"code":"DVCATTRNSPTD"}}`))
			} else {
				_, _ = w.Write([]byte(`{"roomTemperature":{"value":20},"roomSetpoint":21,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"heat"}`))
			}
		case strings.HasSuffix(r.URL.Path, "/attribute") && strings.Contains(r.URL.Path, "device-08"):
			_, _ = w.Write([]byte(`{"onOff":"off"}`))
		case strings.HasSuffix(r.URL.Path, "/attribute"):
			_, _ = w.Write([]byte(`{"roomTemperature":{"value":20},"roomSetpoint":21,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"heat"}`))
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"status":"online"}`))
		case strings.HasSuffix(r.URL.Path, "/notifications"):
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	value, err := activeSessionValue("fixture-session", "sid=fixture-cookie", "123")
	if err != nil || rig.secrets.RotateCredential(ctx, rig.settings.session, value) != nil {
		t.Fatal("fixture session")
	}
	if err := rig.pilot.poll(ctx, rig.session); !errors.Is(err, ErrPartialObservation) {
		t.Fatalf("missing device should degrade only its reading: %v", err)
	}
	if missingReads.Load() != 0 || unsupportedReads.Load() != 1 || rig.pilot.models[retainedAliases[0]].family != familyUnknown || rig.pilot.models[retainedAliases[1]].family != familyZigbee || rig.pilot.models[retainedAliases[2]].family != familyUnknown {
		t.Fatal("absent or unsupported device was read as valid or blocked its neighbour")
	}
	_, missingEntity, err := rig.pilot.bindings[retainedAliases[0]].IDs()
	if err != nil {
		t.Fatal(err)
	}
	_, availableEntity, err := rig.pilot.bindings[retainedAliases[1]].IDs()
	if err != nil {
		t.Fatal(err)
	}
	missingState, present, err := rig.sequence.State(missingEntity)
	if err != nil || !present {
		t.Fatalf("missing binding state: %v", err)
	}
	if _, hasValue := missingState.CurrentValue(); hasValue {
		t.Fatal("absent device gained a physical value")
	}
	availableState, present, err := rig.sequence.State(availableEntity)
	if err != nil || !present {
		t.Fatalf("present binding state: %v", err)
	}
	if _, hasValue := availableState.CurrentValue(); !hasValue {
		t.Fatal("present device lost its independent observation")
	}
	if err := rig.pilot.poll(ctx, rig.session); err != nil {
		t.Fatalf("device return: %v", err)
	}
	if listings.Load() != 2 || missingReads.Load() != 1 || unsupportedReads.Load() != 2 || badQuery.Load() != 0 || rig.pilot.models[retainedAliases[0]].family != familyZigbee || rig.pilot.models[retainedAliases[2]].family != familyZigbee {
		t.Fatal("returning device was not recovered on the next admitted cycle")
	}
}

func TestNHA091AdmittedClimateAndRelayCommandsUseOnePUT(t *testing.T) {
	var mu sync.Mutex
	effects := make(map[string]int)
	payloads := make(map[string]string)
	switchEntered := make(chan struct{})
	rig := newNHA091SessionRig(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("Session-Id") != "fixture-session" || r.Header.Get("Cookie") != "sid=fixture-cookie" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		effects[r.URL.Path]++
		payloads[r.URL.Path] = string(body)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/device/device-02/attribute":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"USRSESSEXP"}}`))
		case "/api/device/device-08/attribute":
			close(switchEntered)
			<-r.Context().Done()
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	snapshot := cycleSnapshot{frames: make(map[string]attributeFrame)}
	for _, binding := range rig.settings.bindings {
		if binding.kind != BindingDailyCount {
			rig.pilot.listed[binding.deviceID] = true
		}
		switch binding.kind {
		case BindingClimate:
			snapshot.frames[binding.alias] = attributeFrame{model: modelInfo{family: familyZigbee, bounded: true, minimum: 5, maximum: 30}}
		case BindingSwitch:
			snapshot.frames[binding.alias] = attributeFrame{model: modelInfo{family: familySwitch}}
		}
	}
	if err := rig.pilot.discover(ctx, rig.session, snapshot); err != nil {
		t.Fatal(err)
	}
	rig.pilot.adoptSession(sessionState{phase: sessionActive, id: "fixture-session", cookies: "sid=fixture-cookie", account: "123"})
	if err := rig.host.AttachCommandDriver(rig.session, rig.pilot); err != nil {
		t.Fatal(err)
	}
	cursor, err := rig.sequence.Start()
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := cursor.Components()
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.ParseActorID("nha091-fake-actor")
	if err != nil {
		t.Fatal(err)
	}
	service, err := integrations.NewVolatileCommandService(rig.host, nha091Generation{}, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	request := func(alias, id string, args *actions.ActionArgs) *actions.CommandRequest {
		t.Helper()
		_, entity, e := rig.pilot.bindings[alias].IDs()
		if e != nil {
			t.Fatal(e)
		}
		entityID, e := entity.Value()
		if e != nil {
			t.Fatal(e)
		}
		schema := contracts.ClimateSchema
		endpoint := "climate"
		if strings.HasPrefix(alias, "switch.") {
			schema, endpoint = contracts.SwitchSchema, "relay"
		}
		return &actions.CommandRequest{CommandId: id, AdmissionEpoch: epoch, ConfigRevision: 7, Target: &wire.EntityRef{Reference: &wire.EntityRef_EntityId{EntityId: entityID}}, CapabilitySchemaId: schema, Endpoint: endpoint, Arguments: args, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
	}
	setpoint := 22.0
	setpointRequest := request(retainedAliases[0], "nha091-setpoint", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: &setpoint}}})
	if result, err := service.Dispatch(ctx, actor, setpointRequest); err != nil || result == nil || result.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("admitted setpoint: %v %v", result, err)
	}
	mode := wire.ClimateMode_CLIMATE_MODE_OFF
	modeRequest := request(retainedAliases[1], "nha091-mode", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{Mode: &mode}}})
	if result, err := service.Dispatch(ctx, actor, modeRequest); err != nil || result == nil || result.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("admitted mode: %v %v", result, err)
	}
	combinedRequest := request(retainedAliases[3], "nha091-combined", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: &setpoint, Mode: &mode}}})
	if result, err := service.Dispatch(ctx, actor, combinedRequest); err != nil || result == nil || result.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("admitted combined climate command: %v %v", result, err)
	}
	expiredRequest := request(retainedAliases[2], "nha091-expired", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{Mode: &mode}}})
	_, _ = service.Dispatch(ctx, actor, expiredRequest)
	switchRequest := request(retainedAliases[8], "nha091-switch", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}})
	effectCtx, effectCancel := context.WithCancel(context.Background())
	type effectResult struct {
		response *actions.CommandResponse
		err      error
	}
	finished := make(chan effectResult, 1)
	go func() {
		response, e := service.Dispatch(effectCtx, actor, switchRequest)
		finished <- effectResult{response, e}
	}()
	select {
	case <-switchEntered:
	case <-time.After(5 * time.Second):
		effectCancel()
		t.Fatal("admitted relay effect did not reach local HTTP")
	}
	effectCancel()
	select {
	case result := <-finished:
		if !errors.Is(result.err, integrations.ErrCommandOutcomeIndeterminate) || result.response == nil || result.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
			t.Fatalf("ambiguous relay outcome: %v %v", result.response, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ambiguous relay effect did not terminate")
	}
	if result, err := service.Dispatch(ctx, actor, switchRequest); !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || result == nil || result.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("ambiguous relay was replayed or reclassified: %v %v", result, err)
	}
	mu.Lock()
	got := make(map[string]int, len(effects))
	for key, count := range effects {
		got[key] = count
	}
	setpointBody := payloads["/api/device/device-00/attribute"]
	modeBody := payloads["/api/device/device-01/attribute"]
	combinedBody := payloads["/api/device/device-03/attribute"]
	switchBody := payloads["/api/device/device-08/attribute"]
	mu.Unlock()
	if got["/api/device/device-00/attribute"] != 1 || got["/api/device/device-01/attribute"] != 1 || got["/api/device/device-02/attribute"] != 1 || got["/api/device/device-03/attribute"] != 1 || got["/api/device/device-08/attribute"] != 1 || len(got) != 5 || setpointBody != `{"roomSetpoint":22}` || modeBody != `{"systemMode":"off"}` || combinedBody != `{"roomSetpoint":22,"systemMode":"off"}` || switchBody != `{"onOff":"on"}` {
		t.Fatal("admitted command mapping or exactly-once HTTP boundary changed")
	}
	count, err := rig.quota.Today(ctx)
	if err != nil || count != 5 {
		t.Fatalf("action attempts=%d, error=%v", count, err)
	}
}
