package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func nha166Config(t *testing.T, port int, address string) nha029RunFixture {
	t.Helper()
	raw := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n host: 127.0.0.1\n port: %d\n database: postgres\n user: homeward_test\n password_env: HOMEWARD_TEST_PASSWORD\n sslmode: disable\nmqtt:\n profile: '5'\n broker: 'mqtt://%s'\n instance: nha166-passive\n mode: passive\n username_secret: mqtt-username\n password_secret: mqtt-password\n subscriptions: ['nha068/state/#']\n", shortPrivateDataDir(t), port, address))
	return nha029RunFixtureFromRaw(t, raw)
}
func nha166Provision(t *testing.T, cfg nha029RunFixture) {
	t.Helper()
	ctx := context.Background()
	settings, err := cfg.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
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
	admitted, _, err := configactivation.ReconcileStartup(ctx, store, cfg.startup)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := admitted.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	mqtt, _ := cfg.MQTTSettings()
	instance, err := mqtt.Instance()
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"mqtt-username": testbroker.PilotUser, "mqtt-password": testbroker.PilotPassword} {
		secretName, err := storage.ParseSecretName(name)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := storage.NewSecretHandle(instance, secretName)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := storage.NewSecretValue([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := secrets.InitializeCredentialIfAbsent(ctx, handle, secret); err != nil {
			t.Fatal(err)
		}
	}
}

type nha166Running struct {
	runtime *observationRuntime
	owner   *configactivation.Owner
	cancel  context.CancelFunc
	done    chan error
	joined  bool
}

func nha166Start(t *testing.T, startup config.StartupInputs) *nha166Running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := &nha166Running{cancel: cancel, done: make(chan error, 1)}
	ready := make(chan struct{})
	observed := make(chan *observationRuntime, 1)
	owners := make(chan *configactivation.Owner, 1)
	listener := &nha055DormantListener{closed: make(chan struct{})}
	go func() {
		result.done <- runWithDependencies(ctx, startup, func(context.Context, string, string) (net.Listener, error) { return listener, nil }, func(net.Addr) { close(ready) }, nil, runDependencies{observeRuntime: func(o *observationRuntime) { observed <- o }, observeRemoteActivation: func(o *configactivation.Owner) { owners <- o }})
	}()
	t.Cleanup(func() { result.close(t) })
	select {
	case <-ready:
	case err := <-result.done:
		result.joined = true
		t.Fatal("observation startup failed", err)
	case <-time.After(15 * time.Second):
		t.Fatal("observation startup timed out")
	}
	result.runtime = <-observed
	result.owner = <-owners
	return result
}
func (r *nha166Running) close(t *testing.T) {
	t.Helper()
	if r.joined {
		return
	}
	r.cancel()
	select {
	case err := <-r.done:
		r.joined = true
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	case <-time.After(15 * time.Second):
		t.Error("observation Run did not drain")
	}
}
func nha166PersistSource(t *testing.T, owner *configactivation.Owner, descriptor storage.SourceDescriptor, document, path, raw string) {
	t.Helper()
	file, err := parser.NewSourceFile(path, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	id, err := storage.NewConfigDocumentID(document)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(id, path)
	if err != nil {
		t.Fatal(err)
	}
	group, err := configactivation.NewSourceGroup(descriptor, []parser.SourceFile{file}, []storage.ConfigDocumentBinding{binding})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := owner.CompileEditableSourceGroup(context.Background(), group)
	if err != nil {
		t.Fatal("source compilation fixture", err)
	}
	author, _ := storage.NewConfigAuthor("nha166:double")
	source, _ := storage.NewConfigSource("nha166:double")
	request, err := configactivation.NewWorkspaceRequest(configactivation.RetainDefinitions(), author, source, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err = request.WithEditableSourceGroup(group, compiled)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Activate(context.Background(), request)
	if err != nil || result.Outcome() != configactivation.Published {
		t.Fatal("persisted source fixture activation", err, result.Outcome())
	}
}
func TestNHA166StartupDoesNotArmPersistedProgramsAndCannotRefresh(t *testing.T) {
	broker := testbroker.Start(t)
	cluster := testpg.Start(t)
	cfg := nha166Config(t, cluster.Port, broker.Address)
	nha166Provision(t, cfg)
	initial := nha166Start(t, cfg.startup)
	automation, err := storage.AutomationSourceDescriptor("automation.nha166_timer", "automation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	nha166PersistSource(t, initial.owner, automation, "source.nha166.automation", "automation.yaml", "id: nha166_timer\ntriggers:\n - trigger: time\n   at: '18:00:00'\nactions:\n - stop: complete\n")
	script, err := storage.ScriptSourceDescriptor("script.nha166_dormant", "script.yaml")
	if err != nil {
		t.Fatal(err)
	}
	nha166PersistSource(t, initial.owner, script, "source.nha166.script", "script.yaml", "sequence:\n - stop: complete\n")
	initial.close(t)
	selected := cfg.startup.WithMQTTReadOnlyObservation()
	running := nha166Start(t, selected)
	actual := running.runtime
	if actual.runs != nil || actual.host == nil || !actual.host.MQTTReadOnlyObservation() || actual.host.CommandDriverEntries() != 0 || actual.supervisor == nil {
		t.Fatal("observation armed executable routines or lost immutable guard")
	}
	before, err := running.owner.CurrentConfig(context.Background())
	if err != nil {
		t.Fatal("configuration reads unavailable", err)
	}
	active, found, err := actual.store.ReadActiveConfig(context.Background())
	if err != nil || !found || len(active.Versions()) != 3 {
		t.Fatal("persisted source metadata was lost", err)
	}
	author, _ := storage.NewConfigAuthor("nha166:double")
	source, _ := storage.NewConfigSource("nha166:double")
	request, err := configactivation.NewWorkspaceRequest(configactivation.RetainDefinitions(), author, source, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed, stopChanges, err := running.owner.SubscribeConfigChanges(context.Background())
	if err != nil || changed == nil {
		t.Fatal("read subscription lost in observation", err)
	}
	stopChanges()
	if _, err := running.owner.Activate(context.Background(), request); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("refresh widened frozen profile", err)
	}
	if _, err := running.owner.Reconcile(context.Background()); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("reconcile bypassed guard", err)
	}
	after, err := running.owner.CurrentConfig(context.Background())
	if err != nil || after != before {
		t.Fatal("refresh changed binding", err)
	}
	broker.Publish(t, "nha068/state/probe", []byte("synthetic-observation"), false)
	if actual.host.CommandDriverEntries() != 0 || actual.runs != nil {
		t.Fatal("MQTT ingress armed routines")
	}
}
func TestNHA166MQTTOnlyAdmissionRejectsCustomFactoryBeforeInvocation(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha166Config(t, cluster.Port, "127.0.0.1:1")
	called := false
	err := RunWithStartupFactory(context.Background(), cfg.startup.WithMQTTReadOnlyObservation(), func(context.Context, string, string) (net.Listener, error) { called = true; return nil, nil }, nil, func(context.Context, *storage.Store) (RuntimePlan, error) { called = true; return RuntimePlan{}, nil })
	if !errors.Is(err, errReadOnlyObservationScope) || called {
		t.Fatal("observation entered a custom factory or listener", err)
	}
}

func TestNHA166MQTTOnlyAdmissionClosesOtherConfiguredSources(t *testing.T) {
	base := nha166Config(t, 1, "127.0.0.1:1")
	source, err := base.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	raw := string(source.Bytes())
	cases := []struct{ name, raw string }{
		{"missing_mqtt", strings.SplitN(raw, "mqtt:\n", 2)[0]},
		{"active_mqtt", strings.Replace(raw, "mode: passive", "mode: active\n will: {topic: 'nha166/presence', payload: offline}", 1)},
		{"solar", raw + "solar: {latitude: 0, longitude: 0, timezone: UTC}\n"},
		{"workday", raw + "workday: {timezone: UTC}\n"},
		{"remote_calendar", raw + "remote_calendar: {label: Synthetic, url: 'https://example.com/calendar.ics', view_timezone: UTC}\n"},
		{"ecobee", raw + "ecobee: {instance: nha166-ecobee, address: '127.0.0.1:12345', accessory_id: 'CA:FE:00:11:22:33', credential_secret: synthetic-pairing}\n"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate, err := config.ParseWithEnv([]byte(test.raw), nha029TestEnv)
			if err != nil {
				t.Fatal("invalid negative fixture", err)
			}
			if err := validateMQTTReadOnlyObservation(candidate, base.Config, configactivation.AdmittedGeneration{}, nil); !errors.Is(err, errReadOnlyObservationScope) {
				t.Fatal("non-passive source set admitted", err)
			}
		})
	}
}
