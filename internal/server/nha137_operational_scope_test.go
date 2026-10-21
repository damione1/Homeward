package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/report"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// This pilot exposes only an in-memory relay. A real Run-owned command
// service can reach its counter, but no network or physical output exists.
type nha137OwnerPilot struct {
	host   *integrations.Manager
	key    integrations.SourceKey
	driver *nha034JobDriver
	ready  chan struct{}
}

func (p *nha137OwnerPilot) Start(ctx context.Context, source integrations.SessionHost, _ integrations.Tasks) error {
	session, ok := source.(*integrations.Session)
	if !ok || session == nil {
		return fmt.Errorf("NHA137 fixture pilot Session: %w", integrations.ErrInvalidInput)
	}
	candidate, err := integrations.NewCandidate(p.key, []*registrywire.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "NHA137 simulated relay", Constraints: &registrywire.CapabilityDescription_Switch{Switch: &registrywire.SwitchConstraints{SetOn: true}}}})
	if err != nil {
		return fmt.Errorf("NHA137 fixture pilot NewCandidate: %w", err)
	}
	bindings, err := source.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil {
		return fmt.Errorf("NHA137 fixture pilot Discover: %w", err)
	}
	if len(bindings) != 1 {
		return fmt.Errorf("NHA137 fixture pilot Discover returned %d bindings: %w", len(bindings), integrations.ErrInvalidInput)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return fmt.Errorf("NHA137 fixture pilot NewQuality: %w", err)
	}
	value, at := domain.NewSwitchValue(false), time.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		return fmt.Errorf("NHA137 fixture pilot NewStateObservation: %w", err)
	}
	if err := source.Publish(ctx, bindings[0], observation); err != nil {
		return fmt.Errorf("NHA137 fixture pilot Publish: %w", err)
	}
	if err := p.host.AttachCommandDriver(session, p.driver); err != nil {
		return fmt.Errorf("NHA137 fixture pilot AttachCommandDriver: %w", err)
	}
	close(p.ready)
	return nil
}
func (*nha137OwnerPilot) Stop(context.Context) error { return nil }

func nha137SeedOldInternalIntents(ctx context.Context, store *storage.Store, entity domain.EntityID) error {
	slot, err := authorization.NewCapabilitySlot(contracts.SwitchSchema, "relay")
	if err != nil {
		return fmt.Errorf("NHA137 fixture intents NewCapabilitySlot: %w", err)
	}
	target, err := authorization.NewCommandTarget(entity, slot)
	if err != nil {
		return fmt.Errorf("NHA137 fixture intents NewCommandTarget: %w", err)
	}
	for _, state := range []string{"accepted", "dispatching", "result"} {
		id, err := domain.ParseCommandID("nha137-old-" + state)
		if err != nil {
			return fmt.Errorf("NHA137 fixture intents ParseCommandID(%s): %w", state, err)
		}
		key, err := storage.NewInternalAutomationOriginKey("nha137-old-occurrence-" + state)
		if err != nil {
			return fmt.Errorf("NHA137 fixture intents NewInternalAutomationOriginKey(%s): %w", state, err)
		}
		fingerprint, err := storage.NewInternalAutomationFingerprint(key, []byte("nha137-synthetic-old-wire-"+state))
		if err != nil {
			return fmt.Errorf("NHA137 fixture intents NewInternalAutomationFingerprint(%s): %w", state, err)
		}
		_, created, outcome, err := store.AcceptInternalAutomationIntent(ctx, id, key, "nha137-old-process", fingerprint, target)
		if err != nil {
			return fmt.Errorf("NHA137 fixture intents AcceptInternalAutomationIntent(%s): %w", state, err)
		}
		if !created || outcome != storage.TransactionCommitted {
			return fmt.Errorf("NHA137 fixture intents AcceptInternalAutomationIntent(%s): created=%t outcome=%v", state, created, outcome)
		}
		if state == "accepted" {
			continue
		}
		if moved, outcome, err := store.MoveCommandIntent(ctx, id, storage.IntentAccepted, storage.IntentDispatching, nil, storage.IntentNoError); err != nil {
			return fmt.Errorf("NHA137 fixture intents MoveCommandIntent(%s dispatching): %w", state, err)
		} else if !moved || outcome != storage.TransactionCommitted {
			return fmt.Errorf("NHA137 fixture intents MoveCommandIntent(%s dispatching): moved=%t outcome=%v", state, moved, outcome)
		}
		if state == "result" {
			encoded, err := proto.Marshal(&actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_DISPATCHED})
			if err != nil {
				return fmt.Errorf("NHA137 fixture intents Marshal(result): %w", err)
			}
			if moved, outcome, err := store.MoveCommandIntent(ctx, id, storage.IntentDispatching, storage.IntentResult, encoded, storage.IntentNoError); err != nil {
				return fmt.Errorf("NHA137 fixture intents MoveCommandIntent(result): %w", err)
			} else if !moved || outcome != storage.TransactionCommitted {
				return fmt.Errorf("NHA137 fixture intents MoveCommandIntent(result): moved=%t outcome=%v", moved, outcome)
			}
		}
	}
	return nil
}

func nha137IntentImage(t *testing.T, ctx context.Context, store *storage.Store) string {
	t.Helper()
	for _, expected := range []struct {
		name  string
		state storage.IntentState
	}{{"accepted", storage.IntentAccepted}, {"dispatching", storage.IntentIndeterminate}, {"result", storage.IntentResult}} {
		id, err := domain.ParseCommandID("nha137-old-" + expected.name)
		if err != nil {
			t.Fatal(err)
		}
		record, present, err := store.CommandIntent(ctx, id)
		if err != nil || !present || record.State() != expected.state || record.OriginKind() != storage.IntentOriginInternalAutomation {
			t.Fatalf("startup replayed or lost old %s intent: state=%v present=%t err=%v", expected.name, record.State(), present, err)
		}
	}
	var image string
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY command_id),'[]'::jsonb)::text FROM public.command_intents x`).Scan(&image)
	}); err != nil {
		t.Fatal(err)
	}
	return image
}

func nha137OperationalRequest(t *testing.T) configactivation.Request {
	t.Helper()
	author, err := storage.NewConfigAuthor("nha137:trusted-device-free-fixture")
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.NewConfigSource("nha137:physical-workspace")
	if err != nil {
		t.Fatal(err)
	}
	request, err := configactivation.NewWorkspaceRequest(configactivation.RetainDefinitions(), author, source, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func nha137ActiveOperational(t *testing.T, ctx context.Context, store *storage.Store) storage.ConfigGenerationSnapshot {
	t.Helper()
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Availability() != storage.ConfigGenerationComplete || active.ManifestFormat() != storage.ConfigManifestCompiledV2 {
		t.Fatalf("actual operational authority unavailable: present=%t err=%v", present, err)
	}
	if len(active.Versions()) != 1 {
		t.Fatal("fixture must contain only one protected operational source")
	}
	return active
}

// This report consumes the exact immutable field of the actual compiler that
// Run attached to its Owner. No test-selected Manifest is passed to BuildMVP.
func nha137AssertComposedScopeAndReport(t *testing.T, ctx context.Context, compiler *configuredOperationalCompiler, actual *observationRuntime, selected scope.Revision) {
	t.Helper()
	if compiler == nil || actual == nil || compiler.store != actual.store || compiler.sequence != actual.sequence || compiler.host != actual.host {
		t.Fatal("test did not retain Run's actual operational compiler and owners")
	}
	owned, err := compiler.repository.ScopeManifest()
	if err != nil || !reflect.DeepEqual(owned, compiler.manifest) {
		t.Fatalf("repository/compiler admission snapshots differ: %v", err)
	}
	revision, ok := compiler.manifest.Revision()
	if !ok || revision != selected {
		t.Fatalf("operational YAML changed cold scope: %v", revision)
	}
	active := nha137ActiveOperational(t, ctx, actual.store)
	configuration, err := report.NewConfigurationRef("nha137-actual-run", uint64(active.Revision().Number()), nha047.Digest(active.ManifestSHA256()))
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := nha047.NewEvidence(nil)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := report.BuildMVPWithManifest(compiler.manifest, catalogue, configuration, nil, evidence, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := rendered.ScopeRevision(); !ok || got != selected || rendered.Configuration() != configuration || len(rendered.Objects()) != 17 || len(rendered.Deferred()) != 72 {
		t.Fatal("actual runtime report lost scope/config/logic provenance")
	}
	for _, object := range rendered.Objects() {
		if object.Activatable() {
			t.Fatal("empty operational fixture activated a documentary object")
		}
	}
	data, err := report.JSON(rendered)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Manifest       string `json:"manifest_ids_sha256"`
		ConfigRevision uint64 `json:"configuration_revision"`
		Scope          struct {
			Revision string `json:"revision"`
			Digest   string `json:"effective_ids_sha256"`
			Entities int    `json:"entity_count"`
		} `json:"scope"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	wantName, _ := selected.Value()
	wantDigest, _ := compiler.manifest.Digest()
	wantCount := 850
	if selected == scope.EcobeeMVPV1() {
		wantCount++
	}
	if wire.Scope.Revision != wantName || wire.Scope.Entities != wantCount || wire.Manifest != hex.EncodeToString(wantDigest[:]) || wire.Scope.Digest != wire.Manifest || wire.ConfigRevision != uint64(active.Revision().Number()) {
		t.Fatal("report JSON selected an operational/historical scope")
	}
	var text bytes.Buffer
	if err := report.RenderText(&text, rendered); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), wantName) || !strings.Contains(text.String(), wire.Manifest) {
		t.Fatal("report text lost selected runtime scope")
	}
}

func TestNHA137OwnerReplacementAndAuthenticatedRestoreKeepColdScopeAndReport(t *testing.T) {
	for _, cold := range []scope.Revision{scope.BaselineV1(), scope.EcobeeMVPV1()} {
		name, _ := cold.Value()
		t.Run(name, func(t *testing.T) {
			cluster := testpg.Start(t)
			data := shortPrivateDataDir(t)
			workspace := filepath.Join(t.TempDir(), "configuration.yaml")
			bootstrap := workspace + ".bootstrap.yaml"
			other := "ecobee_mvp_v1"
			if cold == scope.EcobeeMVPV1() {
				other = "v1"
			}
			initialRaw := []byte("scope_revision: " + other + "\nexternal_workers: []\n# authenticated source A\n")
			if err := os.WriteFile(workspace, initialRaw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bootstrap, nha137LaunchFixture(cluster.Port, data, name), 0600); err != nil {
				t.Fatal(err)
			}
			bootstrapChoice, err := config.ExistingBootstrap(bootstrap)
			if err != nil {
				t.Fatal(err)
			}
			startup, err := config.LoadStartupPaths(workspace, bootstrapChoice, func(key string) (string, bool) { return testpg.Password(), key == "HOMEWARD_TEST_PASSWORD" })
			if err != nil {
				t.Fatal(err)
			}
			selected, err := registry.LoadManifest(cold)
			if err != nil {
				t.Fatal(err)
			}
			owners, compilers, observations := make(chan *configactivation.Owner, 1), make(chan *configuredOperationalCompiler, 1), make(chan *observationRuntime, 1)
			driver := new(nha034JobDriver)
			pilot := &nha137OwnerPilot{driver: driver, ready: make(chan struct{})}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			runCtx, stop := context.WithCancel(ctx)
			done, ready := make(chan error, 1), make(chan net.Addr, 1)
			factory := func(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
				repository, err := registry.NewRepository(store, selected)
				if err != nil {
					return RuntimePlan{}, fmt.Errorf("NHA137 fixture factory NewRepository: %w", err)
				}
				instance, err := domain.ParseIntegrationInstanceID("nha137-owner-synthetic")
				if err != nil {
					return RuntimePlan{}, fmt.Errorf("NHA137 fixture factory ParseIntegrationInstanceID: %w", err)
				}
				key, err := integrations.NewSourceKey("synthetic-relay", "relay")
				if err != nil {
					return RuntimePlan{}, fmt.Errorf("NHA137 fixture factory NewSourceKey: %w", err)
				}
				binding, err := repository.ConfigureBinding(ctx, instance, key, "switch.chauffe_eau")
				if err != nil {
					return RuntimePlan{}, fmt.Errorf("NHA137 fixture factory ConfigureBinding: %w", err)
				}
				if err := nha137SeedOldInternalIntents(ctx, store, binding.Entity); err != nil {
					return RuntimePlan{}, err
				}
				pilot.key = key
				spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
				if err != nil {
					return RuntimePlan{}, fmt.Errorf("NHA137 fixture factory NewPilotSpec: %w", err)
				}
				commands, err := NewDurableCommandsComposition(DurableCommandsInput{Generation: &nha034Generation{revision: 1}, Clock: automationtriggers.RealClock{}})
				if err != nil {
					return RuntimePlan{}, fmt.Errorf("NHA137 fixture factory NewDurableCommandsComposition: %w", err)
				}
				return RuntimePlan{Identity: repository,
					Pilots: []integrations.PilotSpec{spec}, DurableCommands: commands,
					Configure: func(_ context.Context, host *integrations.Manager, _ *storage.Store) error {
						pilot.host = host
						return nil
					},
					ConfigureAuthorization: func(_ context.Context, composition AuthorizationComposition) error {
						actual, ok := composition.(runtimeAuthorizationComposition)
						if !ok || actual.views == nil || actual.views.source == nil {
							return fmt.Errorf("NHA137 fixture authorization composition: %w", registry.ErrInvalidInput)
						}
						// authorizationComposition already attached this Repository
						// before invoking the callback. AttachProjection is one-shot.
						image, err := actual.views.source.Capture()
						if err != nil {
							return fmt.Errorf("NHA137 fixture authorization Capture: %w", err)
						}
						if _, present := image.Registry[binding.Entity]; !image.RegistryReady || !present || len(image.Registry) != 1 {
							return fmt.Errorf("NHA137 fixture authorization initial projection: ready=%t present=%t count=%d: %w", image.RegistryReady, present, len(image.Registry), registry.ErrInvalidInput)
						}
						return nil
					},
					ConfigureActivation:        func(_ context.Context, owner *configactivation.Owner) error { owners <- owner; return nil },
					observeOperationalCompiler: func(actual *configuredOperationalCompiler) { compilers <- actual },
				}, nil
			}
			go func() {
				done <- runWithDependencies(runCtx, startup, (&net.ListenConfig{}).Listen, func(address net.Addr) { ready <- address }, factory, runDependencies{observeRuntime: func(actual *observationRuntime) { observations <- actual }})
			}()
			joined := false
			defer func() {
				stop()
				if !joined {
					select {
					case err := <-done:
						if err != nil {
							t.Error(err)
						}
					case <-time.After(12 * time.Second):
						t.Error("Run failed to join")
					}
				}
			}()
			select {
			case <-ready:
			case err := <-done:
				joined = true
				t.Fatal("Run before ready:", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			owner, compiler, actual := <-owners, <-compilers, <-observations
			if owner == nil {
				t.Fatal("Run did not attach its actual Owner")
			}
			select {
			case <-pilot.ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			intents := nha137IntentImage(t, ctx, actual.store)
			if driver.effects.Load() != 0 {
				t.Fatal("cold Run replayed an old internal command")
			}
			a := nha137ActiveOperational(t, ctx, actual.store)
			nha137AssertComposedScopeAndReport(t, ctx, compiler, actual, cold)
			changed := []byte("scope_revision: " + name + "\nexternal_workers: []\n# replacement source B\n")
			if err := os.WriteFile(workspace, changed, 0600); err != nil {
				t.Fatal(err)
			}
			published, err := owner.Activate(ctx, nha137OperationalRequest(t))
			if err != nil || published.Outcome() != configactivation.Published || published.AttemptOutcome() != storage.TransactionCommitted {
				t.Fatalf("real operational replacement failed: outcome=%v err=%v", published.Outcome(), err)
			}
			b := nha137ActiveOperational(t, ctx, actual.store)
			if b.Revision().Number() <= a.Revision().Number() || b.Versions()[0].SHA256() == a.Versions()[0].SHA256() {
				t.Fatal("replacement did not publish a new operational authority")
			}
			nha137AssertComposedScopeAndReport(t, ctx, compiler, actual, cold)
			if driver.effects.Load() != 0 || nha137IntentImage(t, ctx, actual.store) != intents {
				t.Fatal("operational replacement replayed or rewrote old internal commands")
			}
			// Restore authenticated protected SQL A, rather than replaying a
			// retained test byte slice or manufacturing an alternate Owner.
			clear(initialRaw)
			initialRaw = nil
			replay, err := storage.NewConfigReplayRepository(actual.store, compiler.secrets)
			if err != nil {
				t.Fatal(err)
			}
			historical, err := replay.ReadHistoricalProtectedSource(ctx, a.Revision(), a.ManifestSHA256(), a.Versions()[0].Ref())
			if err != nil || historical.SHA256() != a.Versions()[0].SHA256() {
				t.Fatalf("SQL A restoration authentication failed: %v", err)
			}
			restored := historical.Bytes()
			if err := os.WriteFile(workspace, restored, 0600); err != nil {
				t.Fatal(err)
			}
			clear(restored)
			published, err = owner.Activate(ctx, nha137OperationalRequest(t))
			if err != nil || published.Outcome() != configactivation.Published || published.AttemptOutcome() != storage.TransactionCommitted {
				t.Fatalf("actual Owner restore failed: outcome=%v err=%v", published.Outcome(), err)
			}
			c := nha137ActiveOperational(t, ctx, actual.store)
			if c.Revision().Number() <= b.Revision().Number() || c.Versions()[0].SHA256() != a.Versions()[0].SHA256() || c.Versions()[0].Ref().Revision().Number() <= b.Versions()[0].Ref().Revision().Number() {
				t.Fatal("restore did not create C from exact protected historical A")
			}
			nha137AssertComposedScopeAndReport(t, ctx, compiler, actual, cold)
			if driver.effects.Load() != 0 || nha137IntentImage(t, ctx, actual.store) != intents {
				t.Fatal("authenticated restore replayed or rewrote old internal commands")
			}
			next, err := config.ParseOperationalWithEnv(historical.Bytes(), func(string) (string, bool) { return "", false })
			if err != nil {
				t.Fatalf("historical known launch selector was rejected: %v", err)
			}
			if _, err := next.ScopeRevision(); err == nil {
				t.Fatal("historical operational Config acquired cold selection authority")
			}
			stop()
			select {
			case err := <-done:
				joined = true
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if driver.effects.Load() != 0 {
				t.Fatal("shutdown replayed an old internal command")
			}
		})
	}
}
