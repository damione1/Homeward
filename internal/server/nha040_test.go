package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	mochi "github.com/mochi-mqtt/server/v2"
)

type nha040RuntimeResolver struct{}

func (nha040RuntimeResolver) ResolveEntity(domain.HAAlias) (domain.EntityID, error) {
	return domain.EntityID{}, ErrInvalidTriggerComposition
}
func (nha040RuntimeResolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return domain.DeviceID{}, ErrInvalidTriggerComposition
}
func (nha040RuntimeResolver) ResolveCalendar(domain.HAAlias) (automationtriggers.CalendarSourceID, error) {
	return automationtriggers.CalendarSourceID{}, ErrInvalidTriggerComposition
}

func nha040RuntimeAutomation(t *testing.T) ir.Automation {
	t.Helper()
	entity, err := ir.NewEntity("switch.synthetic", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := parser.NewSourceFile("nha040-runtime.yaml", []byte("id: nha040_runtime\ntriggers:\n- trigger: mqtt\n  topic: zigbee2mqtt/+/action\n  payload: pressed\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.synthetic}\n  data: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.nha040_runtime", "nha040-runtime.yaml", []parser.SourceFile{source}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatalf("runtime trigger compile: %v, rejected=%+v", err, compiled.Rejected())
	}
	return compiled.Accepted()[0]
}

func nha040RuntimeConfig(t *testing.T, port int, broker string) config.Config {
	t.Helper()
	const instance = "nha040-native-mqtt"
	data := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nmqtt:\n  profile: '5'\n  broker: 'mqtt://%s'\n  instance: '%s'\n  mode: passive\n  username_secret: mqtt-username\n  password_secret: mqtt-password\n  subscriptions: ['zigbee2mqtt/+/action']\n", shortPrivateDataDir(t), port, broker, instance))
	c, err := config.ParseWithEnv(data, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	pg, err := c.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), pg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(context.Background()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	secrets, err := store.BootstrapSecrets(context.Background(), cDataDir(t, c))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	for name, value := range map[string]string{"mqtt-username": testbroker.PilotUser, "mqtt-password": testbroker.PilotPassword} {
		handle := nha051ServerHandle(t, instance, name)
		secret, err := storage.NewSecretValue([]byte(value))
		if err != nil {
			store.Close()
			t.Fatal(err)
		}
		if err := secrets.RotateCredential(context.Background(), handle, secret); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	store.Close()
	return c
}

func cDataDir(t *testing.T, c config.Config) string {
	t.Helper()
	directory, err := c.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestNHA040RunNativeMQTTResultsRetainedReconnectAndRecorder(t *testing.T) {
	broker := testbroker.Start(t)
	cluster := testpg.Start(t)
	cfg := nha040RuntimeConfig(t, cluster.Port, broker.Address)
	settings, enabled := cfg.MQTTSettings()
	if !enabled {
		t.Fatal("test MQTT settings were disabled")
	}
	automation := nha040RuntimeAutomation(t)
	broker.Publish(t, "zigbee2mqtt/button/action", []byte("pressed"), true)
	results := make(chan AutomationTriggerResults, 1)
	factory := RuntimeFactory(func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
		manifest, err := registry.ParseManifest(scope.ManifestV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		triggerPlan, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: []ir.Automation{automation}, Zone: "America/Toronto", Resolver: nha040RuntimeResolver{}, MQTTSettings: settings})
		if err != nil {
			return RuntimePlan{}, err
		}
		pilotFactory, err := mqtt.NewDiscoveryPilotFactoryWithTriggerEdges(settings, repository, triggerPlan.MQTTFilters())
		if err != nil {
			return RuntimePlan{}, err
		}
		instance, err := settings.Instance()
		if err != nil {
			return RuntimePlan{}, err
		}
		spec, err := integrations.NewPilotSpec(instance, pilotFactory)
		if err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: repository, Pilots: []integrations.PilotSpec{spec}, AutomationTriggers: &triggerPlan,
			ConfigureAutomationTriggerResults: func(_ context.Context, reader AutomationTriggerResults) error {
				if _, mutable := reader.(*automationtriggers.Service); mutable {
					return ErrInvalidTriggerComposition
				}
				results <- reader
				return nil
			}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- RunWithFactory(ctx, cfg, (&net.ListenConfig{}).Listen, func(a net.Addr) { ready <- a }, factory)
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run exited before ready: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not start")
	}
	var reader AutomationTriggerResults
	select {
	case reader = <-results:
	case <-time.After(time.Second):
		t.Fatal("Run did not install its trigger results consumer")
	}
	const instance = "nha040-native-mqtt"
	digest := sha256.Sum256([]byte(instance))
	clientID := "homeward-" + hex.EncodeToString(digest[:12])
	connected := func(previous *mochi.Client) *mochi.Client {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if client, ok := broker.Server.Clients.Get(clientID); ok && client != previous && !client.Closed() && client.State.Subscriptions.Len() > 0 {
				return client
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("native MQTT pilot did not subscribe")
		return nil
	}
	firstClient := connected(nil)
	assertNoResult := func() {
		t.Helper()
		waitCtx, stop := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer stop()
		if result, err := reader.NextResult(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("retained/reconnect packet became a trigger: kind=%v err=%v", result.Kind(), err)
		}
	}
	assertFire := func() {
		t.Helper()
		waitCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		result, err := reader.NextResult(waitCtx)
		if err != nil || result.Kind() != automationtriggers.ResultFired || result.Automation() != "nha040_runtime" || string(result.Payload()) != "pressed" {
			t.Fatalf("native packet did not fire admitted trigger: kind=%v automation=%q payload=%q err=%v", result.Kind(), result.Automation(), result.Payload(), err)
		}
	}
	assertNoResult() // The retained value was restored on subscribe.
	broker.Publish(t, "zigbee2mqtt/button/action", []byte("pressed"), false)
	assertFire()
	broker.Publish(t, "zigbee2mqtt/button/action", []byte("pressed"), false)
	assertFire() // Equal live payloads are separate packets and source sequences.
	firstClient.Stop(errors.New("synthetic network loss"))
	_ = connected(firstClient)
	assertNoResult() // Replayed retained value after a clean reconnect is not an edge.
	broker.Publish(t, "zigbee2mqtt/button/action", []byte("pressed"), false)
	assertFire()
	if count := broker.PublishCount(clientID); count != 0 {
		t.Fatalf("trigger engine emitted %d application PUBLISH packets", count)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not join its native trigger runtime")
	}
	select {
	case <-reader.Done():
	default:
		t.Fatal("trigger reader remained active after Run")
	}
	observer := nha030ServerObserver(t, cluster.Port)
	var checkpoint, excluded string
	var clean bool
	if err := observer.QueryRow(context.Background(), `SELECT checkpoint::text,policy_excluded::text,clean_close FROM public.recorder_epochs ORDER BY run_id DESC LIMIT 1`).Scan(&checkpoint, &excluded, &clean); err != nil {
		t.Fatal(err)
	}
	if checkpoint == "0" || excluded == "0" || !clean {
		t.Fatalf("native-only recorder did not close an excluded checkpoint: checkpoint=%s excluded=%s clean=%v", checkpoint, excluded, clean)
	}
}

func TestNHA040RunClosesTriggerRuntimeWhenConsumerConfigurationFails(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	entity, err := ir.NewEntity("switch.synthetic", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := parser.NewSourceFile("nha040-failure.yaml", []byte("id: nha040_failure\ntriggers:\n- trigger: time_pattern\n  seconds: '/1'\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.synthetic}\n  data: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.nha040_failure", "nha040-failure.yaml", []parser.SourceFile{source}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatalf("failure-path trigger compile: %v, rejected=%+v", err, compiled.Rejected())
	}
	triggerPlan, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: compiled.Accepted(), Zone: "America/Toronto", Resolver: nha040RuntimeResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("nha040 configured consumer failed")
	var reader AutomationTriggerResults
	factory := RuntimeFactory(func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, AutomationTriggers: &triggerPlan,
			ConfigureAutomationTriggerResults: func(_ context.Context, results AutomationTriggerResults) error {
				reader = results
				return failure
			}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	var listener net.ListenConfig
	err = RunWithFactory(ctx, cfg, listener.Listen, nil, factory)
	if !errors.Is(err, failure) || reader == nil {
		t.Fatalf("consumer failure did not propagate after trigger start: %v", err)
	}
	select {
	case <-reader.Done():
	default:
		t.Fatal("trigger engine was not drained on consumer failure")
	}
	observer := nha030ServerObserver(t, cluster.Port)
	count, clean, err := nha030EpochState(observer)
	if err != nil || count != 1 || !clean {
		t.Fatalf("Recorder epoch leaked after trigger consumer failure: count=%d clean=%v err=%v", count, clean, err)
	}
}

type nha040DeferredSun struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (s *nha040DeferredSun) NextSunset(ctx context.Context, _ time.Time, _ string, _ automationtriggers.SolarPosition) (automationtriggers.SunResult, error) {
	if s.calls.Add(1) == 1 {
		return automationtriggers.SunEventAt(time.Now().Add(250 * time.Millisecond))
	}
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release // Deliberately ignore cancellation until the test releases the port.
	return automationtriggers.SunResult{}, ctx.Err()
}

func nha040RuntimeSunAutomation(t *testing.T) ir.Automation {
	t.Helper()
	entity, err := ir.NewEntity("switch.synthetic", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := parser.NewSourceFile("nha040-sun.yaml", []byte("id: nha040_sun\ntriggers:\n- trigger: sun\n  event: sunset\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.synthetic}\n  data: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.nha040_sun", "nha040-sun.yaml", []parser.SourceFile{source}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatalf("sun trigger compile: %v, rejected=%+v", err, compiled.Rejected())
	}
	return compiled.Accepted()[0]
}

func TestNHA040RunRetainsStoreUntilDeferredSunExits(t *testing.T) {
	for _, mode := range []string{"normal-stop", "consumer-failure"} {
		t.Run(mode, func(t *testing.T) {
			cluster := testpg.Start(t)
			cfg := nha030ServerConfig(t, cluster.Port)
			sun := &nha040DeferredSun{entered: make(chan struct{}, 1), release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(sun.release) }) }
			position, err := automationtriggers.NewSolarPosition(43.65, -79.38)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := automationtriggers.NewPlan(automationtriggers.PlanInput{
				Automations: []ir.Automation{nha040RuntimeSunAutomation(t)}, Zone: "America/Toronto",
				Resolver: nha040RuntimeResolver{}, SunSource: sun, SolarPosition: position,
			})
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("nha040 consumer configuration failure")
			stores := make(chan *storage.Store, 1)
			readers := make(chan AutomationTriggerResults, 1)
			factory := RuntimeFactory(func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
				stores <- store
				return RuntimePlan{Identity: unconfiguredIdentity{}, AutomationTriggers: &plan,
					ConfigureAutomationTriggerResults: func(_ context.Context, reader AutomationTriggerResults) error {
						readers <- reader
						if mode == "consumer-failure" {
							select {
							case <-sun.entered:
								return failure
							case <-time.After(2 * time.Second):
								return errors.New("sun source was not reached")
							}
						}
						return nil
					}}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			runDone := make(chan error, 1)
			finished := false
			t.Cleanup(func() {
				cancel()
				release()
				if !finished {
					select {
					case <-runDone:
					case <-time.After(8 * time.Second):
						t.Error("Run remained blocked after deferred source release")
					}
				}
			})
			ready := make(chan struct{}, 1)
			go func() {
				runDone <- RunWithFactory(ctx, cfg, (&net.ListenConfig{}).Listen, func(net.Addr) { ready <- struct{}{} }, factory)
			}()
			var reader AutomationTriggerResults
			select {
			case reader = <-readers:
			case err := <-runDone:
				finished = true
				t.Fatalf("Run ended before exposing trigger results: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not expose trigger results")
			}
			if mode == "normal-stop" {
				select {
				case <-ready:
				case <-time.After(5 * time.Second):
					t.Fatal("Run did not begin serving")
				}
				select {
				case <-sun.entered:
				case <-time.After(2 * time.Second):
					t.Fatal("sun source was not reached after serving")
				}
				cancel()
			}
			var runErr error
			select {
			case runErr = <-runDone:
				finished = true
			case <-time.After(8 * time.Second):
				t.Fatal("Run ignored its bounded stop deadline")
			}
			if mode == "consumer-failure" && !errors.Is(runErr, failure) {
				t.Fatalf("consumer failure was lost: %v", runErr)
			}
			if mode == "normal-stop" && !errors.Is(runErr, context.DeadlineExceeded) {
				t.Fatalf("Run did not report its bounded trigger join timeout: %v", runErr)
			}
			select {
			case <-reader.Done():
				t.Fatal("trigger Done closed while source callback is still in flight")
			default:
			}
			store := <-stores
			checkCtx, checkCancel := context.WithTimeout(context.Background(), time.Second)
			_, _, err = store.ReadActiveConfig(checkCtx)
			checkCancel()
			if err != nil {
				t.Fatalf("Store closed before trigger callback exited: %v", err)
			}
			release()
			select {
			case <-reader.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("trigger Done did not join released source callback")
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				checkCtx, checkCancel := context.WithTimeout(context.Background(), time.Second)
				_, _, err = store.ReadActiveConfig(checkCtx)
				checkCancel()
				if err != nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("Store remained open after all drains completed")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
