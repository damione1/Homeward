package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

const nha166PreparationLogin = "NHA166 offline isolated admin"
const nha166PreparationPassword = "NHA166_SYNTHETIC_OFFLINE_PASSWORD"

func nha166Mapping(t *testing.T, device, unique, alias string) MQTTReadOnlyObservationMapping {
	t.Helper()
	mapping, err := NewMQTTReadOnlyObservationMapping(device, unique, alias)
	if err != nil {
		t.Fatal(err)
	}
	return mapping
}
func nha166PreparationInput(t *testing.T) MQTTReadOnlyPreparationInput {
	t.Helper()
	return nha166InputWithMappings(t, nha166Mapping(t, "configured-ikea-wire-id", "synthetic-battery-unique", "sensor.ikea_sw_01_battery"))
}
func nha166InputWithMappings(t *testing.T, mappings ...MQTTReadOnlyObservationMapping) MQTTReadOnlyPreparationInput {
	t.Helper()
	input, err := NewMQTTReadOnlyPreparationInput(testbroker.PilotUser, testbroker.PilotPassword, nha166PreparationLogin, "NHA166 isolated observer", nha166PreparationPassword, mappings...)
	if err != nil {
		t.Fatal(err)
	}
	return input
}
func nha166PhysicalPreparation(t *testing.T, fixture nha029RunFixture) config.StartupInputs {
	t.Helper()
	source, err := fixture.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	return startup.WithMQTTReadOnlyObservation()
}
func nha166PreparedStore(t *testing.T, startup config.StartupInputs) (*storage.Store, *storage.SecretRepository) {
	t.Helper()
	launch, err := startup.LaunchConfig()
	if err != nil {
		t.Fatal(err)
	}
	settings, err := launch.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	admitted, _, err := configactivation.ReconcileStartup(context.Background(), store, startup)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := admitted.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	return store, secrets
}

type nha166PreparationSnapshot struct {
	account                    storage.AccountRecord
	version                    authorization.Version
	policy                     [32]byte
	verifier                   accounts.Verifier
	user, pass                 string
	userRevision, passRevision storage.SecretRevision
	marker                     string
	markerRevision             storage.SecretRevision
}

func nha166PreparationFacts(t *testing.T, store *storage.Store, secrets *storage.SecretRepository, startup config.StartupInputs, input MQTTReadOnlyPreparationInput) nha166PreparationSnapshot {
	t.Helper()
	ctx := context.Background()
	account, err := store.CredentialByLogin(ctx, input.login)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := account.ID()
	snapshot, err := store.AuthorizationPolicy(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	version, _ := snapshot.Version()
	source, _ := snapshot.Source()
	policy, _ := source.SemanticSHA256()
	verifier, _ := account.Verifier()
	_, _, _, err = verifier.StorageValues()
	if err != nil {
		t.Fatal(err)
	}
	workspace, _ := startup.CaptureWorkspace()
	operational, _ := workspace.OperationalConfig()
	mqtt, _ := operational.MQTTSettings()
	username, password, err := mqtt.CredentialHandles()
	if err != nil {
		t.Fatal(err)
	}
	userValue, userRevision, err := secrets.ReadCredentialVersioned(ctx, username)
	if err != nil {
		t.Fatal(err)
	}
	passValue, passRevision, err := secrets.ReadCredentialVersioned(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	// Comparisons stay in memory. Failure messages never print this transcript.
	userBytes, _ := userValue.Bytes()
	defer clear(userBytes)
	passBytes, _ := passValue.Bytes()
	defer clear(passBytes)
	instance, _ := mqtt.Instance()
	name, _ := storage.ParseSecretName(observationPreparationSecret)
	binding, _ := storage.NewSecretHandle(instance, name)
	marker, markerRevision, err := secrets.ReadCredentialVersioned(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	markerBytes, _ := marker.Bytes()
	defer clear(markerBytes)
	return nha166PreparationSnapshot{account, version, policy, verifier, string(userBytes), string(passBytes), userRevision, passRevision, string(markerBytes), markerRevision}
}
func TestNHA166PreparationOpaqueInputAndPhysicalGuardRequired(t *testing.T) {
	input := nha166PreparationInput(t)
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{fmt.Sprint(input), fmt.Sprintf("%#v", input), string(encoded)} {
		for _, secret := range []string{testbroker.PilotUser, testbroker.PilotPassword, nha166PreparationLogin, nha166PreparationPassword, "configured-ikea-wire-id", "synthetic-battery-unique", "sensor.ikea_sw_01_battery"} {
			if strings.Contains(text, secret) {
				t.Fatal("opaque input disclosed private data")
			}
		}
	}
	mapping := input.mappings[0]
	mappingJSON, _ := json.Marshal(mapping)
	for _, text := range []string{fmt.Sprint(mapping), fmt.Sprintf("%#v", mapping), string(mappingJSON)} {
		if strings.Contains(text, "configured-ikea-wire-id") || strings.Contains(text, "synthetic-battery-unique") || strings.Contains(text, "sensor.ikea_sw_01_battery") {
			t.Fatal("opaque mapping disclosed identifiers")
		}
	}
	if _, err := NewMQTTReadOnlyPreparationInput("", "x", "x", "x", nha166PreparationPassword); !errors.Is(err, ErrMQTTObservationPreparationInvalid) {
		t.Fatal("empty MQTT username admitted")
	}
	if err := PrepareMQTTReadOnlyObservation(context.Background(), config.StartupInputs{}, input); !errors.Is(err, ErrMQTTObservationPreparationInvalid) {
		t.Fatal("invalid physical startup admitted", err)
	}
}
func TestNHA166PreparationFreshResumeConflictsAndNoBrokerContact(t *testing.T) {
	cluster := testpg.Start(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var contacts atomic.Int32
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			contacts.Add(1)
			connection.Close()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-joined })
	fixture := nha166Config(t, cluster.Port, listener.Addr().String())
	startup := nha166PhysicalPreparation(t, fixture)
	input := nha166PreparationInput(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := PrepareMQTTReadOnlyObservation(ctx, fixture.startup.WithMQTTReadOnlyObservation(), input); !errors.Is(err, ErrMQTTObservationPreparationInvalid) {
		t.Fatal("detached source admitted", err)
	}
	if err := PrepareMQTTReadOnlyObservation(ctx, startup, input); err != nil {
		t.Fatal("fresh preparation", err)
	}
	store, secrets := nha166PreparedStore(t, startup)
	before := nha166PreparationFacts(t, store, secrets, startup, input)
	physicalPath, _ := startup.BootstrapPath()
	if _, err := os.Stat(physicalPath); err != nil || startup.ConfirmBootstrapSource() != nil {
		t.Fatal("physical bootstrap sidecar missing")
	}
	// Reload both physical files as a new process would. Account/policy writes
	// changed the SQL generation, but must not change the stable intent binding.
	workspaceSource, _ := startup.CaptureWorkspace()
	workspacePath, _ := workspaceSource.PhysicalPath()
	resumed, err := config.LoadStartupPaths(workspacePath, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	resumed = resumed.WithMQTTReadOnlyObservation()
	for attempt := 0; attempt < 2; attempt++ {
		if err := PrepareMQTTReadOnlyObservation(ctx, resumed, input); err != nil {
			t.Fatal("same-input physical restart", err)
		}
	}
	if after := nha166PreparationFacts(t, store, secrets, startup, input); after != before {
		t.Fatal("idempotent resume changed existing account/policy/credentials")
	}
	for _, change := range []func(*MQTTReadOnlyPreparationInput){func(i *MQTTReadOnlyPreparationInput) {
		i.password, _ = storage.NewSecretValue([]byte("different-canary"))
	}, func(i *MQTTReadOnlyPreparationInput) { i.adminPassword = "DIFFERENT_SYNTHETIC_PASSWORD" }, func(i *MQTTReadOnlyPreparationInput) {
		i.login, _ = accounts.ParseLoginName("different isolated account")
	}, func(i *MQTTReadOnlyPreparationInput) { i.display, _ = accounts.ParseDisplayName("different display") }} {
		different := input
		change(&different)
		if err := PrepareMQTTReadOnlyObservation(ctx, resumed, different); !errors.Is(err, ErrMQTTObservationPreparationConflict) {
			t.Fatal("different input did not refuse", err)
		}
		if nha166PreparationFacts(t, store, secrets, startup, input) != before {
			t.Fatal("conflicting input overwrote installed facts")
		}
	}
	runtime, err := store.AcquireCommandRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := PrepareMQTTReadOnlyObservation(ctx, resumed, input); !errors.Is(err, ErrMQTTObservationPreparationConflict) {
		t.Fatal("concurrent runtime/preparation admitted", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if contacts.Load() != 0 {
		t.Fatal("offline helper contacted MQTT")
	}
	// A broadened operator policy is refused without being silently narrowed.
	record, _ := store.CredentialByLogin(ctx, input.login)
	id, _ := record.ID()
	policy, _ := store.AuthorizationPolicy(ctx, id)
	version, _ := policy.Version()
	expected, _ := authorization.ExpectVersion(version)
	idText, _ := id.Value()
	broad, err := authorization.AdmitPolicySource(id, []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n - scope: {family: state_registry, operation: read}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n - scope: {family: actions, operation: write}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n", idText)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceAuthorizationPolicy(ctx, id, id, broad, expected); err != nil {
		t.Fatal(err)
	}
	changed := nha166PreparationFacts(t, store, secrets, startup, input)
	if err := PrepareMQTTReadOnlyObservation(ctx, resumed, input); !errors.Is(err, ErrMQTTObservationPreparationConflict) {
		t.Fatal("unexpected policy admitted", err)
	}
	if nha166PreparationFacts(t, store, secrets, startup, input) != changed {
		t.Fatal("unexpected policy overwritten")
	}
	if contacts.Load() != 0 {
		t.Fatal("conflicting preparation contacted MQTT")
	}
}
func TestNHA166PreparationIntentResumesBeforeAccountAndRejectsDifferentInput(t *testing.T) {
	cluster := testpg.Start(t)
	startup := nha166PhysicalPreparation(t, nha166Config(t, cluster.Port, "127.0.0.1:1"))
	input := nha166InputWithMappings(t, nha166Mapping(t, "configured-ikea-wire-id", "synthetic-battery-unique", "sensor.ikea_sw_01_battery"), nha166Mapping(t, "configured-motion-wire-id", "synthetic-motion-unique", "binary_sensor.bathroom_motion_occupancy"))
	ctx := context.Background()
	launch, _ := startup.LaunchConfig()
	settings, _ := launch.PostgresSettings()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	admitted, _, err := configactivation.ReconcileStartup(ctx, store, startup)
	if err != nil {
		t.Fatal(err)
	}
	secrets, _ := admitted.Secrets()
	source, _ := startup.CaptureWorkspace()
	operational, _ := source.OperationalConfig()
	mqtt, _ := operational.MQTTSettings()
	user, pass, _ := mqtt.CredentialHandles()
	intent, err := observationPreparationIntent(ctx, source.SHA256(), user, pass, input)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := mqtt.Instance()
	name, _ := storage.ParseSecretName(observationPreparationSecret)
	binding, _ := storage.NewSecretHandle(instance, name)
	if _, err := secrets.InitializeCredentialIfAbsent(ctx, binding, intent); err != nil {
		t.Fatal(err)
	}
	revision, _ := launch.ScopeRevision()
	manifest, err := registry.LoadManifest(revision)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	firstAlias, _ := input.mappings[0].alias.Value()
	firstIdentity, err := repository.ConfigureBinding(ctx, instance, input.mappings[0].key, firstAlias)
	if err != nil {
		t.Fatal(err)
	}
	marker, markerVersion, err := secrets.ReadCredentialVersioned(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	markerBytes, _ := marker.Bytes()
	defer clear(markerBytes)
	if len(markerBytes) != 58 || string(markerBytes[:8]) != "N166KDF1" {
		t.Fatal("intent did not persist the account KDF verifier format")
	}
	different := input
	different.adminPassword = "OTHER_SYNTHETIC_PASSWORD"
	if err := PrepareMQTTReadOnlyObservation(ctx, startup, different); !errors.Is(err, ErrMQTTObservationPreparationConflict) {
		t.Fatal("different pending intent admitted", err)
	}
	if _, err := store.CredentialByLogin(ctx, input.login); !errors.Is(err, storage.ErrAccountNotFound) {
		t.Fatal("conflicting partial preparation created an account")
	}
	// Alter only the uncommitted part of the set. Registry prevalidation
	// permits it; the full-set KDF marker must still reject it before writes.
	extra := nha166Mapping(t, "configured-extra-wire-id", "synthetic-extra-unique", "switch.chauffe_eau")
	changed := nha166InputWithMappings(t, input.mappings[0], extra)
	removed := nha166InputWithMappings(t, input.mappings[0])
	added := nha166InputWithMappings(t, append(append([]MQTTReadOnlyObservationMapping(nil), input.mappings...), extra)...)
	for _, different := range []MQTTReadOnlyPreparationInput{changed, removed, added} {
		if err := PrepareMQTTReadOnlyObservation(ctx, startup, different); !errors.Is(err, ErrMQTTObservationPreparationConflict) {
			t.Fatal("pending exact mapping set changed", err)
		}
		records, err := repository.List(ctx)
		if err != nil || len(records) != 1 {
			t.Fatal("conflicting mapping set wrote registry", err)
		}
		if _, err := store.CredentialByLogin(ctx, input.login); !errors.Is(err, storage.ErrAccountNotFound) {
			t.Fatal("conflicting mapping set created admin")
		}
		value, version, err := secrets.ReadCredentialVersioned(ctx, binding)
		if err != nil || version != markerVersion {
			t.Fatal("conflicting mapping set changed intent revision", err)
		}
		encoded, _ := value.Bytes()
		same := bytes.Equal(encoded, markerBytes)
		clear(encoded)
		if !same {
			t.Fatal("conflicting mapping set overwrote intent")
		}
		for _, handle := range []storage.SecretHandle{user, pass} {
			if _, err := secrets.Read(ctx, handle); err == nil {
				t.Fatal("conflicting pending mapping set initialized MQTT credentials")
			}
		}
	}
	if err := PrepareMQTTReadOnlyObservation(ctx, startup, input); err != nil {
		t.Fatal("partial intent did not resume", err)
	}
	resolved, err := repository.Resolve(ctx, instance, []integrations.SourceKey{input.mappings[0].key, input.mappings[1].key})
	if err != nil || len(resolved) != 2 || resolved[0] != firstIdentity {
		t.Fatal("partial resume changed committed identity", err)
	}
	before := nha166PreparationFacts(t, store, secrets, startup, input)
	reversed := nha166InputWithMappings(t, input.mappings[1], input.mappings[0])
	if err := PrepareMQTTReadOnlyObservation(ctx, startup, reversed); err != nil {
		t.Fatal("same set reordered refused", err)
	}
	if nha166PreparationFacts(t, store, secrets, startup, input) != before {
		t.Fatal("same set reordered changed existing facts")
	}
	again, err := repository.Resolve(ctx, instance, []integrations.SourceKey{input.mappings[0].key, input.mappings[1].key})
	if err != nil || len(again) != len(resolved) || again[0] != resolved[0] || again[1] != resolved[1] {
		t.Fatal("same set reordered changed registry IDs", err)
	}
	if err := PrepareMQTTReadOnlyObservation(ctx, startup, added); !errors.Is(err, ErrMQTTObservationPreparationConflict) {
		t.Fatal("installed set silently expanded", err)
	}
	if nha166PreparationFacts(t, store, secrets, startup, input) != before {
		t.Fatal("installed set expansion changed existing facts")
	}
	records, err := repository.List(ctx)
	if err != nil || len(records) != 2 {
		t.Fatal("installed set expansion wrote registry", err)
	}
}
func TestNHA166PreparationRealGuardedTLSLoginAndCanaryState(t *testing.T) {
	broker := testbroker.Start(t)
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	original := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	source, _ := original.startup.CaptureWorkspace()
	mqtt := nha166Config(t, cluster.Port, broker.Address)
	mqttSource, _ := mqtt.startup.CaptureWorkspace()
	mqttYAML := strings.SplitN(string(mqttSource.Bytes()), "mqtt:\n", 2)[1]
	mqttYAML = strings.Replace(mqttYAML, "['nha068/state/#']", "['homeassistant/#', 'zigbee2mqtt/#']", 1)
	fixture := nha029RunFixtureFromRaw(t, append(bytes.Clone(source.Bytes()), []byte("mqtt:\n"+mqttYAML)...))
	startup := nha166PhysicalPreparation(t, fixture)
	input := nha166PreparationInput(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := PrepareMQTTReadOnlyObservation(ctx, startup, input); err != nil {
		t.Fatal("offline TLS preparation", err)
	}
	if len(broker.Server.Clients.GetAll()) != 0 {
		t.Fatal("offline preparation connected to MQTT")
	}
	broker.Publish(t, "homeassistant/sensor/ikea-battery/config", []byte(`{"unique_id":"synthetic-battery-unique","device":{"identifiers":["configured-ikea-wire-id"]},"device_class":"battery","unit_of_measurement":"%","state_topic":"zigbee2mqtt/IKEA-SW-01/battery"}`), true)
	running := nha162StartDefaultRun(t, startup)
	client := nha162RunClient(t, roots, running.address)
	base := "https://" + running.address.String()
	tokens := nha162RunLogin(t, ctx, authv1connect.NewAuthServiceClient(client, base), nha166PreparationLogin, nha166PreparationPassword)
	health, err := observabilityv1connect.NewObservabilityServiceClient(client, base).GetHealth(ctx, nha056SessionRequest(&obs.GetHealthRequest{}, tokens.AccessToken))
	if err != nil || health.Header().Get("Homeward-Observation-Profile") != "mqtt-passive-readonly-v1" || health.Header().Get("Homeward-Command-Driver-Entries") != "0" {
		t.Fatal("prepared login/guard attestation failed", err)
	}
	api := statev1connect.NewStateServiceClient(client, base)
	deadline := time.Now().Add(8 * time.Second)
	for {
		broker.Publish(t, "zigbee2mqtt/IKEA-SW-01/battery", []byte("7"), false)
		page, err := api.OpenView(ctx, nha056SessionRequest(&state.OpenViewRequest{PageSize: 100, Filter: &state.ViewFilter{}}, tokens.AccessToken))
		if err != nil {
			t.Fatal("prepared READ grant unavailable", err)
		}
		for _, entity := range page.Msg.Entities {
			if entity.CurrentAlias == "sensor.ikea_sw_01_battery" && entity.State.GetMeasurement() != nil && entity.State.GetMeasurement().Value == 7 {
				if err := PrepareMQTTReadOnlyObservation(ctx, startup, input); !errors.Is(err, ErrMQTTObservationPreparationConflict) {
					t.Fatal("preparation admitted alongside serving runtime", err)
				}
				final, err := observabilityv1connect.NewObservabilityServiceClient(client, base).GetHealth(ctx, nha056SessionRequest(&obs.GetHealthRequest{}, tokens.AccessToken))
				if err != nil || final.Header().Get("Homeward-Command-Driver-Entries") != "0" {
					t.Fatal("prepared run entered a command driver", err)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("prepared account did not observe actual MQTT canary state")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNHA166PreparationMappingValidationPrecedesStorageAndWrites(t *testing.T) {
	base := nha166Mapping(t, "configured-ikea-wire-id", "synthetic-battery-unique", "sensor.ikea_sw_01_battery")
	differentAlias := nha166Mapping(t, "configured-ikea-wire-id", "synthetic-battery-unique", "switch.chauffe_eau")
	aliasDuplicate := nha166Mapping(t, "other-wire", "other-unique", "sensor.ikea_sw_01_battery")
	ambiguous := nha166Mapping(t, "other-wire", "synthetic-battery-unique", "binary_sensor.bathroom_motion_occupancy")
	oversized := make([]MQTTReadOnlyObservationMapping, 257)
	for i := range oversized {
		oversized[i] = base
	}
	for _, mappings := range [][]MQTTReadOnlyObservationMapping{nil, {base, base}, {base, differentAlias}, {base, aliasDuplicate}, {base, ambiguous}, {MQTTReadOnlyObservationMapping{}}, oversized} {
		if _, err := NewMQTTReadOnlyPreparationInput(testbroker.PilotUser, testbroker.PilotPassword, nha166PreparationLogin, "NHA166 isolated observer", nha166PreparationPassword, mappings...); !errors.Is(err, ErrMQTTObservationPreparationInvalid) {
			t.Fatal("invalid/ambiguous mapping set admitted", err)
		}
	}
	for _, bad := range [][3]string{{"", "unique", "sensor.ikea_sw_01_battery"}, {"wire", "", "sensor.ikea_sw_01_battery"}, {"wire", "unique", "invalid alias"}, {"wire\x00", "unique", "sensor.ikea_sw_01_battery"}} {
		if _, err := NewMQTTReadOnlyObservationMapping(bad[0], bad[1], bad[2]); !errors.Is(err, ErrMQTTObservationPreparationInvalid) {
			t.Fatal("invalid mapping admitted", err)
		}
	}
	owned := []MQTTReadOnlyObservationMapping{base}
	input := nha166InputWithMappings(t, owned...)
	owned[0] = differentAlias
	if input.mappings[0] != base {
		t.Fatal("private input did not copy mapping set")
	}
	// A TCP trap doubles PostgreSQL. Rejection must occur before storage.Open,
	// so even a connection attempt is a failure, not merely a refused SQL write.
	trap, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var contacts atomic.Int32
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		for {
			connection, err := trap.Accept()
			if err != nil {
				return
			}
			contacts.Add(1)
			connection.Close()
		}
	}()
	t.Cleanup(func() { trap.Close(); <-joined })
	fixture := nha166Config(t, trap.Addr().(*net.TCPAddr).Port, "127.0.0.1:1")
	startup := nha166PhysicalPreparation(t, fixture)
	outside := nha166InputWithMappings(t, nha166Mapping(t, "outside-wire", "outside-unique", "sensor.nha166_outside_manifest"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := PrepareMQTTReadOnlyObservation(ctx, startup, outside); !errors.Is(err, ErrMQTTObservationPreparationInvalid) {
		t.Fatal("outside-manifest mapping admitted", err)
	}
	forged := input
	forged.mappings = []MQTTReadOnlyObservationMapping{base, ambiguous}
	if err := PrepareMQTTReadOnlyObservation(ctx, startup, forged); !errors.Is(err, ErrMQTTObservationPreparationInvalid) {
		t.Fatal("helper trusted ambiguous private fixture", err)
	}
	if contacts.Load() != 0 {
		t.Fatal("pure mapping rejection entered storage")
	}
	path, _ := startup.BootstrapPath()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected mapping initialized physical sidecar")
	}
	data, _ := fixture.DataDir()
	entries, err := os.ReadDir(data)
	if err != nil || len(entries) != 0 {
		t.Fatal("rejected mapping initialized local credentials", err)
	}
}

func TestNHA166PreparationExistingBindingConflictsRefuseBeforeDomainWrites(t *testing.T) {
	cluster := testpg.Start(t)
	fixture := nha166Config(t, cluster.Port, "127.0.0.1:1")
	startup := nha166PhysicalPreparation(t, fixture)
	input := nha166PreparationInput(t)
	ctx := context.Background()
	launch, _ := startup.LaunchConfig()
	settings, _ := launch.PostgresSettings()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	revision, _ := launch.ScopeRevision()
	manifest, err := registry.LoadManifest(revision)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := startup.CaptureWorkspace()
	operational, _ := source.OperationalConfig()
	settingsMQTT, _ := operational.MQTTSettings()
	instance, _ := settingsMQTT.Instance()
	otherInstance, err := domain.ParseIntegrationInstanceID("nha166-other-instance")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _ := integrations.NewSourceKey("foreign-wire", "foreign-unique")
	if _, err := repository.ConfigureBinding(ctx, otherInstance, otherKey, "switch.chauffe_eau"); err != nil {
		t.Fatal(err)
	}
	assertRefused := func(candidate MQTTReadOnlyPreparationInput) {
		t.Helper()
		before, err := repository.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := PrepareMQTTReadOnlyObservation(ctx, startup, candidate); !errors.Is(err, ErrMQTTObservationPreparationConflict) {
			t.Fatal("conflicting registry admitted", err)
		}
		after, err := repository.List(ctx)
		if err != nil || len(after) != len(before) {
			t.Fatal("conflict changed registry size", err)
		}
		for index := range before {
			if !before[index].Equal(after[index]) {
				t.Fatal("conflict changed durable identity/alias")
			}
		}
		if _, err := store.CredentialByLogin(ctx, input.login); !errors.Is(err, storage.ErrAccountNotFound) {
			t.Fatal("registry conflict initialized admin")
		}
		path, _ := startup.BootstrapPath()
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("registry conflict initialized config sidecar")
		}
		data, _ := launch.DataDir()
		entries, err := os.ReadDir(data)
		if err != nil || len(entries) != 0 {
			t.Fatal("registry conflict initialized secret key", err)
		}
	}
	assertRefused(nha166InputWithMappings(t, nha166Mapping(t, "alias-clash-wire", "alias-clash-unique", "switch.chauffe_eau")))
	firstAlias, _ := input.mappings[0].alias.Value()
	identity, err := repository.ConfigureBinding(ctx, instance, input.mappings[0].key, firstAlias)
	if err != nil {
		t.Fatal(err)
	}
	assertRefused(nha166InputWithMappings(t, nha166Mapping(t, "new-wire", "new-unique", "sensor.ikea_sw_01_battery")))
	assertRefused(nha166InputWithMappings(t, nha166Mapping(t, "configured-ikea-wire-id", "synthetic-battery-unique", "binary_sensor.bathroom_motion_occupancy")))
	if err := repository.Rename(ctx, identity.Entity, "sensor.nha166_renamed"); err != nil {
		t.Fatal(err)
	}
	assertRefused(input)
	if err := repository.Tombstone(ctx, identity.Entity); err != nil {
		t.Fatal(err)
	}
	assertRefused(input)
}

func TestNHA166PreparationRejectsPhysicalSourceDriftBeforeImport(t *testing.T) {
	cluster := testpg.Start(t)
	fixture := nha166Config(t, cluster.Port, "127.0.0.1:1")
	startup := nha166PhysicalPreparation(t, fixture)
	input := nha166PreparationInput(t)
	ctx := context.Background()
	launch, _ := startup.LaunchConfig()
	settings, _ := launch.PostgresSettings()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	// The interrupted operator process completed only SQL source + key/sidecar.
	// Neither preparation intent, credentials, account nor binding exists.
	admitted, _, err := configactivation.ReconcileStartup(ctx, store, startup)
	if err != nil {
		t.Fatal(err)
	}
	physical, err := startup.CaptureWorkspace()
	if err != nil || admitted.SourceSHA256() != physical.SHA256() {
		t.Fatal("fresh admitted source does not bind exact physical bytes", err)
	}
	before, found, err := store.ReadActiveConfig(ctx)
	if err != nil || !found {
		t.Fatal(err)
	}
	beforeBootstrap, found, err := store.ReadActiveBootstrap(ctx)
	if err != nil || !found {
		t.Fatal(err)
	}
	secrets, _ := admitted.Secrets()
	operational, _ := admitted.OperationalConfig()
	mqtt, _ := operational.MQTTSettings()
	user, pass, _ := mqtt.CredentialHandles()
	instance, _ := mqtt.Instance()
	name, _ := storage.ParseSecretName(observationPreparationSecret)
	marker, _ := storage.NewSecretHandle(instance, name)
	revision, _ := launch.ScopeRevision()
	manifest, err := registry.LoadManifest(revision)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := physical.PhysicalPath()
	changed := bytes.Replace(physical.Bytes(), []byte("mqtt://127.0.0.1:1"), []byte("mqtt://127.0.0.1:2"), 1)
	if bytes.Equal(changed, physical.Bytes()) {
		t.Fatal("drift fixture did not change broker")
	}
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	// Also change one inert bootstrap listener value. The precheck must refuse
	// before ReconcileStartup would publish this separate bootstrap change.
	bootstrapPath, _ := startup.BootstrapPath()
	bootstrapRaw, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	changedBootstrap := bytes.Replace(bootstrapRaw, []byte("127.0.0.1:0"), []byte("127.0.0.1:1"), 1)
	if bytes.Equal(changedBootstrap, bootstrapRaw) {
		t.Fatal("drift fixture did not change bootstrap")
	}
	if err := os.WriteFile(bootstrapPath, changedBootstrap, 0600); err != nil {
		t.Fatal(err)
	}
	drift, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	drift = drift.WithMQTTReadOnlyObservation()
	if err := PrepareMQTTReadOnlyObservation(ctx, drift, input); !errors.Is(err, ErrMQTTObservationPreparationConflict) {
		t.Fatal("physical broker drift imported credentials into replayed SQL source", err)
	}
	after, found, err := store.ReadActiveConfig(ctx)
	if err != nil || !found || before.Revision() != after.Revision() || before.ManifestSHA256() != after.ManifestSHA256() {
		t.Fatal("drift refusal changed active SQL generation", err)
	}
	afterBootstrap, found, err := store.ReadActiveBootstrap(ctx)
	if err != nil || !found || beforeBootstrap.Ref() != afterBootstrap.Ref() || beforeBootstrap.SHA256() != afterBootstrap.SHA256() {
		t.Fatal("drift refusal changed SQL bootstrap", err)
	}
	if _, err := store.CredentialByLogin(ctx, input.login); !errors.Is(err, storage.ErrAccountNotFound) {
		t.Fatal("source drift initialized admin")
	}
	for _, handle := range []storage.SecretHandle{user, pass, marker} {
		_, err := secrets.Read(ctx, handle)
		var stored *storage.Error
		if !errors.As(err, &stored) || stored.Kind() != storage.SecretNotFound {
			t.Fatal("source drift initialized private credential/intent")
		}
	}
	records, err := repository.List(ctx)
	if err != nil || len(records) != 0 {
		t.Fatal("source drift initialized registry binding", err)
	}
	if err := os.WriteFile(path, physical.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootstrapPath, bootstrapRaw, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	restored = restored.WithMQTTReadOnlyObservation()
	if err := PrepareMQTTReadOnlyObservation(ctx, restored, input); err != nil {
		t.Fatal("exact interrupted source did not resume", err)
	}
	// Account + READ-policy configuration revisions now exist. Their source
	// does not redefine the operational-source hash or prevent an exact retry.
	_, found, err = store.ReadActiveConfig(ctx)
	if err != nil || !found {
		t.Fatal("prepared generation unavailable after admin/policy", err)
	}
	record, err := store.CredentialByLogin(ctx, input.login)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := record.ID()
	policy, err := store.AuthorizationPolicy(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	version, _ := policy.Version()
	number, _, err := version.Values()
	if err != nil || number < 2 {
		t.Fatal("fixture did not write admin/READ policy revisions", err)
	}
	replayed, _, err := configactivation.ReconcileStartup(ctx, store, restored)
	if err != nil || replayed.SourceSHA256() != physical.SHA256() {
		t.Fatal("admin/policy changed operational-source SHA", err)
	}
	beforeFacts := nha166PreparationFacts(t, store, secrets, restored, input)
	if err := PrepareMQTTReadOnlyObservation(ctx, restored, input); err != nil {
		t.Fatal("same-source retry after admin/policy refused", err)
	}
	if nha166PreparationFacts(t, store, secrets, restored, input) != beforeFacts {
		t.Fatal("same-source retry changed installed facts")
	}
}
