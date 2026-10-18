package hilo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/damione1/Homeward/contracts"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/energy"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

type nha090BlockedSession struct {
	integrations.SessionHost
	entered chan struct{}
	release <-chan struct{}
}

type nha090TypedNilHost struct{ integrations.SessionHost }
type nha090TypedNilTasks struct{ integrations.Tasks }
type nha090TypedNilExchanger struct {
	integrations.CloudAuthorizationExchanger
}

func TestNHA090TypedNilBoundaries(t *testing.T) {
	if err := (&Pilot{}).Start(context.Background(), (*nha090TypedNilHost)(nil), (*nha090TypedNilTasks)(nil)); !errors.Is(err, ErrSettings) {
		t.Fatalf("typed nil Host/Tasks was admitted: %v", err)
	}
	profile, err := integrations.ParseCloudAuthorizationProfileID("hilo-android-b2c-v1")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewAuthorizationProvider(profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Authorize(context.Background(), (*nha090TypedNilExchanger)(nil), integrations.CloudAuthorizationInput{}); !errors.Is(err, ErrAuthorizationProtocol) {
		t.Fatalf("typed nil exchanger was admitted: %v", err)
	}
	if err := (&LeaseRegistry{}).PublishPower(context.Background(), (*nha090TypedNilHost)(nil), integrations.Binding{}, integrations.StateObservation{}, domain.EntityID{}, 0); !errors.Is(err, ErrLeaseEvidence) {
		t.Fatalf("typed nil lease publication Host was admitted: %v", err)
	}
}

func TestNHA090DeviceHubAndSSEOverSyntheticTLS(t *testing.T) {
	const locationHiloID = "urn:hilo:synthetic:location"
	const meterHiloID = "urn:hilo:synthetic:meter"
	var firstNegotiate, secondNegotiate, snapshotAttempts, sseOpens atomic.Int32
	var sseBurst = make(chan struct{})
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/DeviceHub/negotiate" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Ocp-Apim-Subscription-Key") != "" {
				firstNegotiate.Add(1)
				_, _ = io.WriteString(w, `{"url":"`+server.URL+`/DeviceHub","accessToken":"synthetic-edge-token"}`)
			} else if r.Header.Get("Authorization") == "Bearer synthetic-edge-token" {
				secondNegotiate.Add(1)
				_, _ = io.WriteString(w, `{"connectionToken":"synthetic-connection"}`)
			} else {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}
		case r.URL.Path == "/DeviceHub" && r.Method == http.MethodGet && r.URL.Query().Get("id") == "synthetic-connection":
			connection, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			_, handshake, err := connection.Read(r.Context())
			if err != nil || string(handshake) != "{\"protocol\":\"json\",\"version\":1}\x1e" {
				return
			}
			if err := connection.Write(r.Context(), websocket.MessageText, []byte("{}\x1e")); err != nil {
				return
			}
			_, subscribe, err := connection.Read(r.Context())
			if err != nil || !strings.Contains(string(subscribe), "SubscribeToLocation") || !strings.Contains(string(subscribe), "10000") {
				return
			}
			inventory := `{"type":1,"target":"DeviceListInitialValuesReceived","arguments":[[{"id":1,"hiloId":"` + meterHiloID + `","type":"Meter","modelNumber":"","supportedAttributesList":["Power","Disconnected"]}]]}` + "\x1e"
			if connection.Write(r.Context(), websocket.MessageText, []byte(inventory)) != nil {
				return
			}
			values := fmt.Sprintf(`{"type":1,"target":"DevicesValuesReceived","arguments":[[{"deviceId":1,"hiloId":%q,"locationId":10000,"attribute":"Power","valueType":"Watt","value":355,"timeStampUTC":%q},{"deviceId":1,"hiloId":%q,"locationId":10000,"attribute":"Disconnected","valueType":"null","value":false,"timeStampUTC":%q}]]}`+"\x1e", meterHiloID, time.Now().UTC().Format(time.RFC3339Nano), meterHiloID, time.Now().UTC().Format(time.RFC3339Nano))
			if connection.Write(r.Context(), websocket.MessageText, []byte(values)) != nil {
				return
			}
			for {
				if _, _, err := connection.Read(r.Context()); err != nil {
					return
				}
			}
		case r.URL.Path == "/api/digital-twin/v3/graphql" && r.Method == http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			if r.Header.Get("Accept") == "text/event-stream" {
				sseOpens.Add(1)
				if !strings.Contains(string(body), `"locationHiloId":"`+locationHiloID+`"`) || !strings.Contains(string(body), "persistedQuery") {
					http.Error(w, "invalid SSE query", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"data":{"onAnyDeviceUpdated":{"deviceType":"BasicSmartMeter","locationHiloId":"`+locationHiloID+`","transmissionTime":"`+time.Now().UTC().Format(time.RFC3339Nano)+`","device":{"hiloId":"`+meterHiloID+`","connectionStatus":1,"power":{"value":0.5,"kind":"KILOWATT"}}}}}`+"\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-sseBurst:
				case <-r.Context().Done():
					return
				}
				for i := 0; i < 40; i++ {
					_, _ = io.WriteString(w, "data: {}\n\n")
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			if snapshotAttempts.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"errors":[{"message":"PersistedQueryNotFound"}]}`)
				return
			}
			if !strings.Contains(string(body), "query getLocation") {
				http.Error(w, "missing APQ fallback", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"getLocation":{"hiloId":"`+locationHiloID+`","devices":[{"deviceType":"BasicSmartMeter","hiloId":"`+meterHiloID+`","connectionStatus":1,"power":{"value":0.355,"kind":"KILOWATT"}}]}}}`)
		default:
			http.Error(w, "unconfigured synthetic path", http.StatusNotFound)
		}
	}))
	t.Cleanup(func() { server.CloseClientConnections(); server.Close() })
	input := Input{Instance: "nha090-synthetic-tls", AccountID: "synthetic-account", LocationID: 10000, LocationHiloID: locationHiloID, CredentialSecret: "hilo-credential", DailyLimit: 500, EnergyMaxGapSeconds: 300, APIBaseURL: server.URL, PlatformBaseURL: server.URL}
	for _, entry := range retainedProfile {
		if entry.Kind.Direct() && entry.Kind != AliasWeather && entry.Kind != AliasNotifications {
			input.Bindings = append(input.Bindings, BindingInput{Alias: entry.Alias, DeviceID: len(input.Bindings) + 1})
		}
	}
	settings, err := ParseSettings(input)
	if err != nil {
		t.Fatal(err)
	}
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	cluster := testpg.Start(t)
	storageSettings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), storageSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	secrets, err := store.BootstrapSecrets(context.Background(), filepath.Join(t.TempDir(), "synthetic-secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachSecrets(secrets); err != nil {
		t.Fatal(err)
	}
	instance, _ := settings.Instance()
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	account, err := integrations.NewCloudAccountBinding(instance, settings.Account())
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewCloudPolicyWithStreams(integrations.DefaultCloudLimits(), integrations.DefaultCloudStreamLimits(), []integrations.CloudAccountBinding{account}, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	transport := server.Client().Transport.(*http.Transport).Clone()
	client, err := integrations.NewCloudClient(session, policy, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	pilot := &Pilot{settings: settings, client: client}
	token := credentialJSON{Version: 1, Access: "synthetic-access", Refresh: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour), Scope: clientScope, Profile: "hilo-android-b2c-v1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, pending, err := pilot.openDeviceHub(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(context.Background())
	inventory, pending, err := pilot.awaitInventory(ctx, ws, pending)
	if err != nil || len(inventory) != 1 || inventory[0].HiloID != meterHiloID || len(pending) != 0 {
		t.Fatalf("DeviceHub inventory: %+v %d %v", inventory, len(pending), err)
	}
	frames, err := readHub(ctx, ws)
	if err != nil || len(frames) != 1 || frames[0].Target != "DevicesValuesReceived" || len(frames[0].Arguments) != 1 {
		t.Fatal("pinned RAW DeviceHub values frame was not delivered")
	}
	hubValues, err := decodeHubValues(frames[0].Arguments[0])
	if err != nil || len(hubValues) != 2 || hubValues[0].reading.power.value == nil || *hubValues[0].reading.power.value != 355 || hubValues[1].reading.disconnected.value == nil || *hubValues[1].reading.disconnected.value {
		t.Fatal("pinned RAW attribute/valueType/timeStampUTC DTO was not decoded")
	}
	snapshot, _, err := graphqlSnapshot(ctx, client, settings, token)
	if err != nil || len(snapshot) != 1 {
		t.Fatalf("GraphQL APQ snapshot: %d %v", len(snapshot), err)
	}
	sse, err := openGraphQLStream(ctx, client, settings, token, false)
	if err != nil {
		t.Fatal(err)
	}
	defer sse.Close(context.Background())
	message, err := sse.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := message.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	id, reading, _, retry, err := decodeGraphQLUpdate(raw, locationHiloID)
	if err != nil || retry || id != meterHiloID || reading.power.value == nil || *reading.power.value != 500 {
		t.Fatalf("SSE observation: %q %+v %v", id, reading, err)
	}
	if firstNegotiate.Load() != 1 || secondNegotiate.Load() != 1 || snapshotAttempts.Load() != 2 || sseOpens.Load() != 1 {
		t.Fatalf("unexpected cloud attempts: gateway=%d edge=%d snapshot=%d sse=%d", firstNegotiate.Load(), secondNegotiate.Load(), snapshotAttempts.Load(), sseOpens.Load())
	}
	// A stream reader can discover delivery loss while its error event is
	// trapped behind queued data. The lease must be gone before any drain.
	key, _ := integrations.NewSourceKey("synthetic-hilo-meter", "power")
	desc := &registrywire.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Synthetic Hilo power", Endpoint: "power", Constraints: &registrywire.CapabilityDescription_Measurement{Measurement: &registrywire.MeasurementConstraints{Unit: "W", Precision: 0, Semantic: registrywire.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registrywire.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}
	candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{desc})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	_, source, err := bindings[0].IDs()
	if err != nil {
		t.Fatal(err)
	}
	output, _ := domain.ParseEntityID("nha090-queued-loss-energy")
	profile, err := energy.HiloLeftV1(source, output, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sub, _, err := fixture.Sequencer().SubscribeRecorder()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	leasing, err := NewLeaseRegistry(fixture.Sequencer(), instance, 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	defer pumpCancel()
	fence := &cycleFence{leases: leasing, cancel: pumpCancel}
	generation, err := fence.activate()
	if err != nil {
		t.Fatal(err)
	}
	if err := leasing.PublishPower(ctx, session, bindings[0], nha090PowerObservation(t, 100), source, generation); err != nil {
		t.Fatal(err)
	}
	report, reportTick := nha090NextReport(t, sub, source)
	if _, ok, err := leasing.Lease(ctx, profile, report, reportTick, reportTick+120*time.Second); err != nil || !ok {
		t.Fatal("pre-loss lease was not available")
	}
	events := make(chan streamEvent, 1)
	done := make(chan struct{})
	go func() { pumpStream(pumpCtx, sse, integrations.CloudStreamSSE, events, fence); close(done) }()
	close(sseBurst)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("overflow reader did not finish")
	}
	if fence.failure() == nil || len(events) != 1 {
		t.Fatal("overflow did not fence before draining the full queue")
	}
	if _, ok, err := leasing.Lease(context.Background(), profile, report, reportTick, reportTick+120*time.Second); err != nil || ok {
		t.Fatalf("queued stream error left a renewable power proof: %v %v", ok, err)
	}
}

func (h nha090BlockedSession) Publish(ctx context.Context, binding integrations.Binding, observation integrations.StateObservation) error {
	close(h.entered)
	select {
	case <-h.release:
		return h.SessionHost.Publish(ctx, binding, observation)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type nha090LostAckSession struct{ integrations.SessionHost }

func (h nha090LostAckSession) Publish(ctx context.Context, binding integrations.Binding, observation integrations.StateObservation) error {
	if err := h.SessionHost.Publish(ctx, binding, observation); err != nil {
		return err
	}
	return errors.New("synthetic admission acknowledgement loss")
}

func nha090PowerObservation(t *testing.T, power float64) integrations.StateObservation {
	t.Helper()
	unit, _ := domain.NewUnitCatalog().Parse("W")
	measurement, err := domain.NewMeasurement(power, unit, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := integrations.NewStateObservation("power", quality, &value, nil, time.Now().UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func nha090NextReport(t *testing.T, sub *sequencer.Subscription, source domain.EntityID) (energy.ReportIdentity, time.Duration) {
	t.Helper()
	wait, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		delivery, err := sub.Next(wait)
		if err != nil {
			t.Fatal(err)
		}
		entry, ok := delivery.Entry()
		if !ok {
			continue
		}
		entity, _, _, isState := entry.State()
		if !isState || entity != source {
			continue
		}
		identity, err := energy.NewReportIdentity(entry)
		if err != nil {
			t.Fatal(err)
		}
		return identity, entry.AdmissionTick()
	}
}

func TestNHA090RetainedProfileAndExplicitLocalPolicy(t *testing.T) {
	counts := map[AliasKind]int{}
	seen := map[string]bool{}
	input := Input{Instance: "nha090-test", AccountID: "synthetic-account", LocationID: 10000, LocationHiloID: "urn:hilo:synthetic:location", CredentialSecret: "hilo-credential", DailyLimit: 500, EnergyMaxGapSeconds: 300}
	for _, entry := range retainedProfile {
		if seen[entry.Alias] {
			t.Fatalf("duplicate %s", entry.Alias)
		}
		seen[entry.Alias] = true
		counts[entry.Kind]++
		if entry.Kind.Direct() && entry.Kind != AliasWeather && entry.Kind != AliasNotifications {
			input.Bindings = append(input.Bindings, BindingInput{Alias: entry.Alias, DeviceID: len(input.Bindings) + 1})
		}
		if entry.Kind == AliasEnergy && classifyAlias(entry.Source).Kind != AliasPower {
			t.Fatalf("unbound energy %s", entry.Alias)
		}
	}
	if len(seen) != 58 || counts[AliasClimate] != 8 || counts[AliasPower] != 10 || counts[AliasTemperature] != 17 || counts[AliasSwitch] != 1 || counts[AliasGateway] != 1 || counts[AliasNotifications] != 1 || counts[AliasWeather] != 1 || counts[AliasEnergy] != 10 || counts[AliasDeferred] != 9 {
		t.Fatalf("wrong classification: %v", counts)
	}
	settings, err := ParseSettings(input)
	if err != nil || !settings.Valid() || len(settings.Bindings()) != 48 || len(settings.EnergyPairs()) != 10 || settings.LeaseWindow() != 180*time.Second || settings.EnergyMaxGap() != 300*time.Second {
		t.Fatalf("settings: %v", err)
	}
	input.EnergyMaxGapSeconds = 120
	if _, err := ParseSettings(input); err == nil {
		t.Fatal("max_gap silently weaker than held-power window")
	}
	input.EnergyMaxGapSeconds = 300
	input.Bindings = append(input.Bindings, BindingInput{Alias: "sensor.defi_hilo", DeviceID: 123})
	if _, err := ParseSettings(input); err == nil {
		t.Fatal("deferred alias admitted as a source")
	}
	input.Bindings = input.Bindings[:len(input.Bindings)-1]
	input.APIBaseURL = "https://foreign.example"
	if _, err := ParseSettings(input); err == nil {
		t.Fatal("foreign API origin admitted for a bearer token")
	}
}

func TestNHA090B2CCallbackOpaqueTokensAndRedaction(t *testing.T) {
	callback := redirectURI + "?code=opaque-code&state=expected-state"
	if code, err := checkedCallback(callback, "expected-state"); err != nil || code != "opaque-code" {
		t.Fatalf("valid callback: %q %v", code, err)
	}
	for _, raw := range []string{
		redirectURI + "?code=opaque-code&state=wrong",
		redirectURI + "?code=opaque-code&state=expected-state&state=expected-state",
		redirectURI + "?code=opaque-code&state=expected-state&extra=x",
		redirectURI + "?code=opaque-code&state=expected-state&extra=%zz",
		"https://evil.example/login-callback?code=opaque-code&state=expected-state",
		redirectURI + "/extra?code=opaque-code&state=expected-state",
	} {
		if _, err := checkedCallback(raw, "expected-state"); err == nil {
			t.Fatalf("accepted callback %q", raw)
		}
	}
	raw := []byte(`{"access_token":"access-canary","refresh_token":"refresh-canary","id_token":"identity-canary","expires_in":3600,"scope":"` + clientScope + `","token_type":"Bearer"}`)
	issuedAt := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	credential, err := decodeTokenResponse(raw, "", issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if want := issuedAt.Add(time.Hour); !credential.ExpiresAt.Equal(want) || !credential.ExpiresAt.Add(-tokenRenewalMargin).Equal(issuedAt.Add(time.Hour-30*time.Second)) {
		t.Fatalf("token expiry or sole renewal margin changed: got %s, want %s", credential.ExpiresAt, want)
	}
	value, err := credential.secretValue()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := value.Bytes()
	if err != nil || strings.Contains(string(stored), "identity-canary") || !strings.Contains(string(stored), "access-canary") {
		t.Fatal("id_token persisted or opaque access missing")
	}
	diagnostic, err := json.Marshal(credential)
	if err != nil || strings.Contains(string(diagnostic), "canary") || strings.Contains(fmt.Sprintf("%+v", credential), "canary") {
		t.Fatal("credential diagnostic exposed a token")
	}
	if _, err := decodeTokenResponse([]byte(`{"access_token":"x","expires_in":3600,"token_type":"Bearer"}`), "", time.Now()); err == nil {
		t.Fatal("missing refresh token admitted")
	}
}

func TestNHA090WirePresenceAndCompleteNotificationList(t *testing.T) {
	id, withZero, err := decodeGraphQLDevice(json.RawMessage(`{"hiloId":"urn:hilo:device","power":{"value":0,"kind":"Watt"},"connectionStatus":"Connected"}`))
	if err != nil || id == "" || withZero.power.value == nil || *withZero.power.value != 0 || withZero.disconnected.value == nil || *withZero.disconnected.value {
		t.Fatal("present zero/connected lost")
	}
	_, withNull, err := decodeGraphQLDevice(json.RawMessage(`{"hiloId":"urn:hilo:device","power":{"value":null,"kind":"Watt"}}`))
	if err != nil || !withNull.power.present || withNull.power.value != nil {
		t.Fatal("null invented zero")
	}
	_, unknownUnit, err := decodeGraphQLDevice(json.RawMessage(`{"hiloId":"urn:hilo:device","power":{"value":2,"kind":"megawatt"}}`))
	if err != nil || unknownUnit.power.value != nil {
		t.Fatal("unknown power unit admitted")
	}
	if count, err := decodeNotificationCount([]byte(`[ {"viewed":false}, {"viewed":true}, {"viewed":false} ]`)); err != nil || count != 2 {
		t.Fatalf("notification count %v %v", count, err)
	}
	if _, err := decodeNotificationCount([]byte(`[ {"viewed":false}, {} ]`)); err == nil {
		t.Fatal("partial notification list became a count")
	}
	if count, err := decodeNotificationCount([]byte(`[]`)); err != nil || count != 0 {
		t.Fatal("complete empty notification list was lost")
	}
	if _, err := decodeNotificationCount([]byte(`null`)); err == nil {
		t.Fatal("null notification list became a false zero")
	}
	if parseHubNumber(json.RawMessage(`null`)) != nil || parseHubBool(json.RawMessage(`null`)) != nil || decodeStateBool(json.RawMessage(`null`)).value != nil {
		t.Fatal("null DeviceHub scalar became zero or false")
	}
	zero, off := parseHubNumber(json.RawMessage(`0`)), parseHubBool(json.RawMessage(`false`))
	if zero == nil || *zero != 0 || off == nil || *off {
		t.Fatal("present DeviceHub zero or false was lost")
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	updates, err := decodeHubValues(json.RawMessage(fmt.Sprintf(`[{"deviceId":1,"attribute":"Power","valueType":"Watt","timeStampUTC":%q,"value":null},{"deviceId":1,"attribute":"Disconnected","valueType":"null","timeStampUTC":%q,"value":null}]`, stamp, stamp)))
	if err != nil || len(updates) != 2 || !updates[0].reading.power.present || updates[0].reading.power.value != nil || !updates[1].reading.disconnected.present || updates[1].reading.disconnected.value != nil {
		t.Fatal("null DeviceHub fields did not remain present and unknown")
	}
	if _, err := decodeHubValues(json.RawMessage(`[{"deviceId":1,"attributeType":"Power","value":0}]`)); err == nil {
		t.Fatal("invented attributeType wire shape was accepted")
	}
	if heating, err := decodeHubValues(json.RawMessage(fmt.Sprintf(`[{"deviceId":1,"attribute":"Heating","valueType":"Percentage","timeStampUTC":%q,"value":1}]`, stamp))); err != nil || len(heating) != 1 || heating[0].reading.heating.value == nil || !*heating[0].reading.heating.value {
		t.Fatal("numeric source Heating=1 was lost")
	}
	inventory, err := decodeHubInventory(json.RawMessage(`[{"id":1,"hiloId":"urn:hilo:meter","type":"Thermostat","modelNumber":"Model_43076","supportedAttributesList":["Power","Disconnected"]}]`))
	if err != nil || len(inventory) != 1 || inventory[0].Model != "43076" {
		t.Fatal("Model_ normalization disagrees with the pinned HiloDevice update")
	}
	if _, err := decodeWeather([]byte(`{}`)); err == nil {
		t.Fatal("missing weather became zero")
	}
	if temperature, err := decodeWeather([]byte(`[{"temperature":-9,"condition":"Foggy"}]`)); err != nil || temperature != -9 {
		t.Fatalf("documented weather list lost its current Celsius reading: %v %v", temperature, err)
	}
}

func TestNHA090GraphQLPinnedSchemaAndPassiveUpdate(t *testing.T) {
	body, err := graphqlBody(graphqlSnapshotQuery, "urn:hilo:location", true)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Query     string `json:"query"`
		Variables struct {
			LocationHiloID string `json:"locationHiloId"`
		} `json:"variables"`
	}
	if json.Unmarshal(body, &request) != nil || request.Variables.LocationHiloID != "urn:hilo:location" || !strings.Contains(request.Query, "probeTemp") || !strings.Contains(request.Query, "deviceType") {
		t.Fatal("snapshot query does not follow the pinned digital-twin schema")
	}
	if !strings.Contains(graphqlSubscriptionQuery, "onAnyDeviceUpdated(locationHiloId: $locationHiloId)") {
		t.Fatal("subscription uses a non-provider argument")
	}
	passive := []byte(`{"data":{"onAnyDeviceUpdated":{"deviceType":"BasicSmartMeter","locationHiloId":"urn:hilo:location","transmissionTime":"2026-10-18T12:00:00Z","device":{"deviceType":"BasicSmartMeter","hiloId":"urn:hilo:meter","connectionStatus":"CONNECTED","power":{"value":0.355,"kind":"KILOWATT"}}}}}`)
	id, reading, sourceTime, retry, err := decodeGraphQLUpdate(passive, "urn:hilo:location")
	if err != nil || retry || id != "urn:hilo:meter" || sourceTime == nil || reading.power.value == nil || *reading.power.value != 355 {
		t.Fatalf("passive update lost a measured power value: %q %+v %v", id, reading, err)
	}
	for _, raw := range [][]byte{
		[]byte(`{"data":{"onAnyDeviceUpdated":{"locationHiloId":"urn:hilo:location","operationId":"op-rejected","status":"REJECTED","device":{"hiloId":"urn:hilo:meter","power":{"value":1,"kind":"WATT"}}}}}`),
		[]byte(`{"data":{"onAnyDeviceUpdated":{"locationHiloId":"urn:hilo:location","device":{"hiloId":"urn:hilo:meter"}}}}`),
	} {
		id, _, _, _, err := decodeGraphQLUpdate(raw, "urn:hilo:location")
		if err != nil || id != "" {
			t.Fatalf("operation status or id-only event invented a measurement: %q %v", id, err)
		}
	}
	if graphQLDeviceType(json.RawMessage(`{"deviceType":"Gateway","hiloId":"urn:hilo:gateway"}`)) != "Gateway" {
		t.Fatal("gateway type discriminator lost")
	}
	_, wrongUnit, err := decodeGraphQLDevice(json.RawMessage(`{"hiloId":"urn:hilo:thermostat","ambientTemperature":{"value":21,"kind":"KELVIN"}}`))
	if err != nil || wrongUnit.ambient.value != nil {
		t.Fatal("non-Celsius temperature admitted")
	}
	_, fieldCelsius, err := decodeGraphQLDevice(json.RawMessage(`{"hiloId":"urn:hilo:thermostat","ambientTemperature":{"value":21}}`))
	if err != nil || fieldCelsius.ambient.value == nil || *fieldCelsius.ambient.value != 21 {
		t.Fatal("temperature field's pinned Celsius semantics were lost")
	}
	_, offlineMeter, err := decodeGraphQLDevice(json.RawMessage(`{"hiloId":"urn:hilo:meter","connectionStatus":2}`))
	if err != nil || offlineMeter.disconnected.value == nil || !*offlineMeter.disconnected.value {
		t.Fatal("numeric meter disconnection from the pinned mapper was lost")
	}
}

func TestNHA090SSETimestampWatermarkDoesNotRefreshAReplay(t *testing.T) {
	pilot := &Pilot{
		settings: Settings{locationHiloID: "urn:hilo:location"},
		devices:  map[int]hubDevice{1: {ID: 1, HiloID: "urn:hilo:meter", Type: "Meter"}},
		states:   make(map[int]deviceReading), sseTimes: make(map[int]time.Time),
	}
	packet := func(at string) []byte {
		return []byte(fmt.Sprintf(`{"data":{"onAnyDeviceUpdated":{"deviceType":"BasicSmartMeter","locationHiloId":"urn:hilo:location","transmissionTime":%q,"device":{"hiloId":"urn:hilo:meter","power":{"value":1,"kind":"WATT"}}}}}`, at))
	}
	current := time.Now().UTC().Truncate(time.Second)
	if err := pilot.applyGraphQLUpdate(context.Background(), nil, packet(current.Format(time.RFC3339))); err != nil {
		t.Fatal(err)
	}
	if !pilot.sseTimes[1].Equal(current) {
		t.Fatal("valid supplier timestamp did not become the cycle watermark")
	}
	for _, old := range []time.Time{current, current.Add(-time.Second)} {
		if err := pilot.applyGraphQLUpdate(context.Background(), nil, packet(old.Format(time.RFC3339))); err != nil || !pilot.sseTimes[1].Equal(current) {
			t.Fatal("duplicate or backward supplier timestamp refreshed the watermark")
		}
	}
	for _, invalid := range [][]byte{packet("invalid"), packet(current.Add(-25 * time.Hour).Format(time.RFC3339)), packet(current.Add(3 * time.Minute).Format(time.RFC3339)), []byte(`{"data":{"onAnyDeviceUpdated":{"deviceType":"BasicSmartMeter","locationHiloId":"urn:hilo:location","device":{"hiloId":"urn:hilo:meter","power":{"value":1,"kind":"WATT"}}}}}`)} {
		if err := pilot.applyGraphQLUpdate(context.Background(), nil, invalid); err == nil || !pilot.sseTimes[1].Equal(current) {
			t.Fatal("missing, invalid, stale or future transmission time refreshed the source")
		}
	}
}

func TestNHA090HubRawTimestampWatermark(t *testing.T) {
	pilot := &Pilot{
		settings: Settings{locationID: 10000},
		devices:  map[int]hubDevice{1: {ID: 1, HiloID: "urn:hilo:meter", Type: "Meter"}},
		states:   make(map[int]deviceReading), hubTimes: make(map[hubTimeKey]time.Time),
	}
	packet := func(at time.Time, power float64) hubFrame {
		raw := fmt.Sprintf(`[{"deviceId":1,"hiloId":"urn:hilo:meter","locationId":10000,"attribute":"Power","valueType":"Watt","value":%v,"timeStampUTC":%q}]`, power, at.Format(time.RFC3339Nano))
		return hubFrame{Type: 1, Target: "DevicesValuesReceived", Arguments: []json.RawMessage{json.RawMessage(raw)}}
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Second)
	for _, item := range []struct {
		at   time.Time
		watt float64
		want float64
	}{{base, 100, 100}, {base, 200, 100}, {base.Add(-time.Second), 300, 100}, {base.Add(time.Second), 200, 200}} {
		if err := pilot.applyHubEvent(context.Background(), nil, []hubFrame{packet(item.at, item.watt)}); err != nil || pilot.states[1].power.value == nil || *pilot.states[1].power.value != item.want {
			t.Fatalf("RAW Hub replay altered the admitted value: %v %+v", err, pilot.states[1])
		}
	}
	if err := pilot.applyHubEvent(context.Background(), nil, []hubFrame{packet(base.Add(-25*time.Hour), 400)}); err == nil {
		t.Fatal("stale RAW Hub timestamp was admitted as a fresh push")
	}
}

func TestNHA090LeaseRevokeBeforeNewPublicationCannotMaskGap(t *testing.T) {
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha090-lease-source")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("synthetic-hilo-device", "power")
	desc := &registrywire.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Synthetic Hilo power", Endpoint: "power", Constraints: &registrywire.CapabilityDescription_Measurement{Measurement: &registrywire.MeasurementConstraints{Unit: "W", Precision: 0, Semantic: registrywire.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registrywire.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}
	candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{desc})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	_, source, err := bindings[0].IDs()
	if err != nil {
		t.Fatal(err)
	}
	output, _ := domain.ParseEntityID("nha090-derived")
	profile, err := energy.HiloLeftV1(source, output, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sub, _, err := fixture.Sequencer().SubscribeRecorder()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	leasing, err := NewLeaseRegistry(fixture.Sequencer(), instance, 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	firstGeneration, err := leasing.Activate()
	if err != nil {
		t.Fatal(err)
	}
	publish := func(power float64, generation uint64) (energy.ReportIdentity, time.Duration) {
		observation := nha090PowerObservation(t, power)
		if err := leasing.PublishPower(context.Background(), session, bindings[0], observation, source, generation); err != nil {
			t.Fatal(err)
		}
		return nha090NextReport(t, sub, source)
	}
	first, firstTick := publish(100, firstGeneration)
	if _, ok, err := leasing.Lease(context.Background(), profile, first, firstTick, firstTick+120*time.Second); err != nil || !ok {
		t.Fatalf("held constant power rejected: %v %v", ok, err)
	}
	// A null DeviceHub power/disconnect must retire the actual Host source and
	// its energy proof, not publish a fabricated 0 W/Connected observation.
	nullUpdates, err := decodeHubValues(json.RawMessage(fmt.Sprintf(`[{"deviceId":1,"attribute":"Power","valueType":"Watt","timeStampUTC":%q,"value":null}]`, time.Now().UTC().Format(time.RFC3339Nano))))
	if err != nil {
		t.Fatal(err)
	}
	projection := &Pilot{
		settings: Settings{bindings: []Binding{{alias: "sensor.meter00_power", kind: AliasPower, deviceID: 1}}},
		bindings: map[string]integrations.Binding{"sensor.meter00_power": bindings[0]},
		devices:  map[int]hubDevice{1: {ID: 1, HiloID: "urn:hilo:synthetic:meter", Type: "Meter"}},
		states:   make(map[int]deviceReading), leases: leasing, generation: firstGeneration,
	}
	if err := projection.applyReading(context.Background(), session, 1, nullUpdates[0].reading, false, &nullUpdates[0].at); err != nil {
		t.Fatal(err)
	}
	_, _ = nha090NextReport(t, sub, source)
	if _, ok, err := leasing.Lease(context.Background(), profile, first, firstTick, firstTick+120*time.Second); err != nil || ok {
		t.Fatalf("null Hub observation kept an energy proof: %v %v", ok, err)
	}
	leasing.Revoke()
	newGeneration, err := leasing.Activate()
	if err != nil {
		t.Fatal(err)
	}
	second, secondTick := publish(200, newGeneration)
	if _, ok, err := leasing.Lease(context.Background(), profile, first, firstTick, firstTick+120*time.Second); err != nil || ok {
		t.Fatalf("new generation masked old gap: %v %v", ok, err)
	}
	if _, ok, err := leasing.Lease(context.Background(), profile, second, secondTick, secondTick+120*time.Second); err != nil || !ok {
		t.Fatalf("new report unavailable: %v %v", ok, err)
	}
	if _, ok, err := leasing.Lease(context.Background(), profile, second, secondTick, secondTick+180*time.Second); err != nil || ok {
		t.Fatalf("expired source renewed: %v %v", ok, err)
	}

	// The next report is admitted while a held-power tick is waiting for the
	// registry. Supersession is visible through the Host revision, and the new
	// report has an atomic evidence record when publication returns.
	entered, release := make(chan struct{}), make(chan struct{})
	thirdPublished := make(chan error, 1)
	thirdObservation := nha090PowerObservation(t, 300)
	go func() {
		thirdPublished <- leasing.PublishPower(context.Background(), nha090BlockedSession{SessionHost: session, entered: entered, release: release}, bindings[0], thirdObservation, source, newGeneration)
	}()
	<-entered
	type leaseResult struct {
		ok  bool
		err error
	}
	tickDone := make(chan leaseResult, 1)
	go func() {
		_, ok, err := leasing.Lease(context.Background(), profile, second, secondTick, secondTick+120*time.Second)
		tickDone <- leaseResult{ok: ok, err: err}
	}()
	close(release)
	select {
	case err := <-thirdPublished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked publication deadlocked")
	}
	third, thirdTick := nha090NextReport(t, sub, source)
	select {
	case result := <-tickDone:
		if result.err != nil || result.ok {
			t.Fatalf("superseded report was renewed: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent tick deadlocked")
	}
	if _, ok, err := leasing.Lease(context.Background(), profile, third, thirdTick, thirdTick+120*time.Second); err != nil || !ok {
		t.Fatalf("normal update caused a false gap: %v %v", ok, err)
	}

	// A stream loss may race an in-flight Host admission. Revoke waits for the
	// publication fence, then erases both old and new evidence before the
	// unavailable report and any subsequent snapshot.
	lostEntered, lostRelease := make(chan struct{}), make(chan struct{})
	fourthPublished := make(chan error, 1)
	fourthObservation := nha090PowerObservation(t, 400)
	go func() {
		fourthPublished <- leasing.PublishPower(context.Background(), nha090BlockedSession{SessionHost: session, entered: lostEntered, release: lostRelease}, bindings[0], fourthObservation, source, newGeneration)
	}()
	<-lostEntered
	lossTick := make(chan leaseResult, 1)
	go func() {
		_, ok, err := leasing.Lease(context.Background(), profile, third, thirdTick, thirdTick+120*time.Second)
		lossTick <- leaseResult{ok: ok, err: err}
	}()
	revoked := make(chan struct{})
	go func() { leasing.Revoke(); close(revoked) }()
	close(lostRelease)
	select {
	case err := <-fourthPublished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("publication and loss deadlocked")
	}
	fourth, fourthTick := nha090NextReport(t, sub, source)
	select {
	case <-revoked:
	case <-time.After(time.Second):
		t.Fatal("loss fence deadlocked")
	}
	select {
	case result := <-lossTick:
		if result.err != nil {
			t.Fatalf("loss tick: %v", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("loss tick deadlocked")
	}
	for _, report := range []struct {
		identity energy.ReportIdentity
		tick     time.Duration
	}{{third, thirdTick}, {fourth, fourthTick}} {
		if _, ok, err := leasing.Lease(context.Background(), profile, report.identity, report.tick, report.tick+120*time.Second); err != nil || ok {
			t.Fatalf("post-loss interval admitted: %v %v", ok, err)
		}
	}
	unavailable, _ := domain.NewQuality(domain.AvailabilityUnavailable, domain.FreshnessStale, domain.ProvenanceObserved)
	missing, err := integrations.NewStateObservation("power", unavailable, nil, nil, time.Now().UTC(), nil)
	if err != nil || session.Publish(context.Background(), bindings[0], missing) != nil {
		t.Fatal("unavailable report was not admitted")
	}
	_, _ = nha090NextReport(t, sub, source)
	reconnectedGeneration, err := leasing.Activate()
	if err != nil {
		t.Fatal(err)
	}
	fifth, fifthTick := publish(500, reconnectedGeneration)
	if _, ok, err := leasing.Lease(context.Background(), profile, fifth, fifthTick, fifthTick+120*time.Second); err != nil || !ok {
		t.Fatalf("reconnect did not establish a new proof: %v %v", ok, err)
	}

	// The Host may admit a report and lose only its acknowledgement. The old
	// proof must be fenced immediately, before the pilot can process the error.
	if err := leasing.PublishPower(context.Background(), nha090LostAckSession{session}, bindings[0], nha090PowerObservation(t, 600), source, reconnectedGeneration); err == nil {
		t.Fatal("lost admission acknowledgement was accepted")
	}
	sixth, sixthTick := nha090NextReport(t, sub, source)
	for _, report := range []struct {
		identity energy.ReportIdentity
		tick     time.Duration
	}{{fifth, fifthTick}, {sixth, sixthTick}} {
		if _, ok, err := leasing.Lease(context.Background(), profile, report.identity, report.tick, report.tick+120*time.Second); err != nil || ok {
			t.Fatalf("lost acknowledgement left a renewable proof: %v %v", ok, err)
		}
	}
	if err := projection.markUnavailable(context.Background(), nha090LostAckSession{session}); err == nil {
		t.Fatal("lost acknowledgement of unavailable was hidden from the run fence")
	}
	_, _ = nha090NextReport(t, sub, source)
	if err := session.Withdraw(context.Background(), bindings[0]); err != nil {
		t.Fatal(err)
	}
	_, _ = nha090NextReport(t, sub, source)
	if _, ok, err := leasing.Lease(context.Background(), profile, sixth, sixthTick, sixthTick+120*time.Second); err != nil || ok {
		t.Fatalf("retired report was renewed: %v %v", ok, err)
	}
}
