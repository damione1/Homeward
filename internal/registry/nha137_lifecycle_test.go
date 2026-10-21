package registry_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// This executor has no physical or network output. Its counter is shared by
// successive cold Hosts so startup and readmission cannot hide a replay.
type nha137Executor struct{ calls atomic.Int32 }

func (e *nha137Executor) Dispatch(_ context.Context, _ integrations.AdmittedCommand) error {
	e.calls.Add(1)
	return nil
}

type nha137Generation struct{}

func (nha137Generation) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(1)
}

func nha137Repository(t *testing.T, store *storage.Store, revision scope.Revision) *registry.Repository {
	t.Helper()
	manifest, err := registry.LoadManifest(revision)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

type nha137Host struct {
	host    *integrations.Manager
	session *integrations.Session
	sink    *fake.Fixture
	epoch   string
}

func nha137ColdHost(t *testing.T, ctx context.Context, repo *registry.Repository, instance domain.IntegrationInstanceID, executor *nha137Executor) nha137Host {
	t.Helper()
	_, sink, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(repo, sink, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AttachProjection(ctx, sink); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachCommandDriver(session, executor); err != nil {
		t.Fatal(err)
	}
	epoch, err := sink.CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Revoke(session) })
	return nha137Host{host, session, sink, epoch}
}

func nha137Candidate(t *testing.T, key integrations.SourceKey, climate bool) integrations.Candidate {
	t.Helper()
	description := &wireregistry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Synthetic baseline switch", Endpoint: "relay", Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: true}}}
	if climate {
		description = &wireregistry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, DisplayName: "Synthetic Ecobee thermostat", Endpoint: "climate", Constraints: &wireregistry.CapabilityDescription_Climate{Climate: &wireregistry.ClimateConstraints{SetpointCelsius: &wireregistry.NumberBounds{Minimum: 5, Maximum: 30}, ObservedTemperatureCelsius: &wireregistry.ClimateTemperatureObservation{Bounds: &wireregistry.NumberBounds{Minimum: -20, Maximum: 60}}}}}
	}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func nha137Request(identity integrations.ResolvedIdentity, epoch, id string, climate bool) *actions.CommandRequest {
	entity, _ := identity.Entity.Value()
	request := &actions.CommandRequest{CommandId: id, AdmissionEpoch: epoch, ConfigRevision: 1, Target: &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: entity}}, CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay", Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}}, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
	if climate {
		setpoint := 20.0
		request.CapabilitySchemaId, request.Endpoint = contracts.ClimateSchema, "climate"
		request.Arguments = &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: &setpoint}}}
	}
	return request
}

// Compare durable rows, rather than just counts: IDs, timestamps, aliases,
// source keys, recorder payloads/digests and completed intents must be exact.
func nha137DurableImage(t *testing.T, ctx context.Context, store *storage.Store) string {
	t.Helper()
	var image string
	err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT jsonb_build_array(
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM public.registry_devices x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY instance_id,source_device),'[]'::jsonb) FROM public.registry_device_sources x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM public.registry_entities x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY instance_id,source_device,source_entity),'[]'::jsonb) FROM public.registry_source_bindings x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY alias),'[]'::jsonb) FROM public.registry_aliases x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY epoch,sequence),'[]'::jsonb) FROM public.state_history x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY epoch),'[]'::jsonb) FROM public.recorder_epochs x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY command_id),'[]'::jsonb) FROM public.command_intents x)
		)::text`).Scan(&image)
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func nha137PersistClimateHistory(t *testing.T, ctx context.Context, store *storage.Store, rig nha137Host, binding integrations.Binding, instance domain.IntegrationInstanceID) {
	t.Helper()
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	current, err := domain.NewCelsiusTemperature(21)
	if err != nil {
		t.Fatal(err)
	}
	climate, err := domain.NewClimateValue(domain.ClimateValueInput{CurrentTemperature: &current})
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewClimateStateValue(climate)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	observation, err := integrations.NewStateObservation("climate", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.session.Publish(ctx, binding, observation); err != nil {
		t.Fatal(err)
	}
	_, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	state, present := rig.sink.State(entity)
	if !present {
		t.Fatal("synthetic climate observation missing")
	}
	epoch, err := storage.ParseRecorderEpoch("13713713713713713713713713713713")
	if err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := store.OpenRecorderEpoch(ctx, epoch, at); outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("recorder epoch: %v %v", outcome, err)
	}
	row, err := storage.NewRecorderRecord(1, instance, state, domain.Causality{}, at, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := storage.NewRecorderBatch(epoch, 0, 1, []storage.RecorderRecord{row}, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := store.PersistRecorderBatch(ctx, batch); outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("persist climate history: %v %v", outcome, err)
	}
}

func TestNHA137SameDatabaseBaselineAmendmentReturnAndReadmission(t *testing.T) {
	store, ctx, settings := nha015Store(t)
	instance := nha015Instance(t, "nha137-synthetic")
	baselineKey := nha015Source(t, "fixture-relay", "relay")
	ecobeeKey := nha015Source(t, "fixture-ecobee", "thermostat")
	base := nha137Repository(t, store, scope.BaselineV1())
	other, err := base.ConfigureBinding(ctx, instance, baselineKey, "switch.chauffe_eau")
	if err != nil {
		t.Fatal(err)
	}
	// Reserve the inactive original before there is any Ecobee SQL row.
	before := nha137DurableImage(t, ctx, store)
	if err := base.Rename(ctx, other.Entity, "climate.home"); !errors.Is(err, registry.ErrAliasConflict) {
		t.Fatalf("inactive unbound original stolen: %v", err)
	}
	if _, err := base.ConfigureBinding(ctx, instance, ecobeeKey, "climate.home"); !errors.Is(err, registry.ErrOutsideManifest) {
		t.Fatalf("baseline configured Ecobee: %v", err)
	}
	if got := nha137DurableImage(t, ctx, store); got != before {
		t.Fatal("baseline refusal wrote durable rows")
	}

	amended := nha137Repository(t, store, scope.EcobeeMVPV1())
	ecobee, err := amended.ConfigureBinding(ctx, instance, ecobeeKey, "climate.home")
	if err != nil {
		t.Fatal(err)
	}
	if err := amended.Rename(ctx, ecobee.Entity, "climate.nha137_previous"); err != nil {
		t.Fatal(err)
	}
	if err := amended.Rename(ctx, ecobee.Entity, "climate.nha137_current"); err != nil {
		t.Fatal(err)
	}
	executor := &nha137Executor{}
	rig := nha137ColdHost(t, ctx, amended, instance, executor)
	bindings, err := rig.session.Discover(ctx, []integrations.Candidate{nha137Candidate(t, ecobeeKey, true), nha137Candidate(t, baselineKey, false)})
	if err != nil || len(bindings) != 2 {
		t.Fatalf("amended discovery: %v %v", bindings, err)
	}
	nha137PersistClimateHistory(t, ctx, store, rig, bindings[0], instance)
	service, err := integrations.NewDurableCommandService(ctx, rig.host, nha137Generation{}, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	actor, err := domain.ParseActorID("nha137-trusted-fixture")
	if err != nil {
		t.Fatal(err)
	}
	request := nha137Request(ecobee, rig.epoch, "nha137-completed-climate", true)
	response, err := service.Dispatch(ctx, actor, request)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || executor.calls.Load() != 1 {
		t.Fatalf("synthetic command: %v %v sends=%d", response, err, executor.calls.Load())
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rig.host.Revoke(rig.session); err != nil {
		t.Fatal(err)
	}
	stable := nha137DurableImage(t, ctx, store)

	// A fresh pool and Repository point at the SAME cluster and all prior rows.
	reopened, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	returned := nha137Repository(t, reopened, scope.BaselineV1())
	baselineRig := nha137ColdHost(t, ctx, returned, instance, executor)
	records, err := returned.List(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("baseline projection unusable: %d %v", len(records), err)
	}
	_, projected, err := records[0].IDs()
	if err != nil || projected != other.Entity {
		t.Fatalf("baseline identity changed: %v %v", projected, err)
	}
	if got, err := returned.Resolve(ctx, instance, []integrations.SourceKey{baselineKey}); err != nil || len(got) != 1 || got[0] != other {
		t.Fatalf("other baseline source unavailable: %v %v", got, err)
	}
	if _, err := returned.Resolve(ctx, instance, []integrations.SourceKey{ecobeeKey}); !errors.Is(err, registry.ErrOutsideManifest) {
		t.Fatalf("inactive source resolved: %v", err)
	}
	if _, err := returned.ConfigureBinding(ctx, instance, ecobeeKey, "climate.home"); !errors.Is(err, registry.ErrOutsideManifest) {
		t.Fatalf("inactive source rebound: %v", err)
	}
	if err := returned.Rename(ctx, ecobee.Entity, "climate.nha137_forbidden"); !errors.Is(err, registry.ErrOutsideManifest) {
		t.Fatalf("inactive source renamed: %v", err)
	}
	if err := returned.Rename(ctx, other.Entity, "climate.home"); !errors.Is(err, registry.ErrAliasConflict) {
		t.Fatalf("inactive original stolen: %v", err)
	}
	if _, err := baselineRig.session.Discover(ctx, []integrations.Candidate{nha137Candidate(t, ecobeeKey, true)}); !errors.Is(err, registry.ErrOutsideManifest) {
		t.Fatalf("Host rediscovered inactive source: %v", err)
	}
	baselineBindings, err := baselineRig.session.Discover(ctx, []integrations.Candidate{nha137Candidate(t, baselineKey, false)})
	if err != nil || len(baselineBindings) != 1 {
		t.Fatalf("baseline discovery: %v %v", baselineBindings, err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	value, at := domain.NewSwitchValue(false), time.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := baselineRig.session.Publish(ctx, baselineBindings[0], observation); err != nil {
		t.Fatal(err)
	}
	baselineCommands, err := integrations.NewVolatileCommandService(baselineRig.host, nha137Generation{}, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"", "climate.home", "climate.nha137_previous", "climate.nha137_current"} {
		refused := nha137Request(ecobee, baselineRig.epoch, "nha137-refused-"+label, true)
		if label != "" {
			refused.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_HaAlias{HaAlias: label}}
		}
		result, err := baselineCommands.Dispatch(ctx, actor, refused)
		if err != nil || result == nil || result.Status != actions.ActionStatus_ACTION_STATUS_FAILED || result.Error == nil || executor.calls.Load() != 1 {
			t.Fatalf("inactive command admitted for %q: %v %v sends=%d", label, result, err, executor.calls.Load())
		}
	}
	if got := nha137DurableImage(t, ctx, reopened); got != stable {
		t.Fatal("return to baseline changed identities, aliases, history or completed intents")
	}
	// Prove the remaining baseline entity still has a usable command path.
	if result, err := baselineCommands.Dispatch(ctx, actor, nha137Request(other, baselineRig.epoch, "nha137-baseline-still-usable", false)); err != nil || result == nil || result.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || executor.calls.Load() != 2 {
		t.Fatalf("other baseline command refused: %v %v", result, err)
	}
	if err := baselineRig.host.Revoke(baselineRig.session); err != nil {
		t.Fatal(err)
	}

	readmitted := nha137Repository(t, reopened, scope.EcobeeMVPV1())
	if got, err := readmitted.ConfigureBinding(ctx, instance, ecobeeKey, "climate.home"); err != nil || got != ecobee {
		t.Fatalf("readmission changed IDs: %v %v", got, err)
	}
	if got, err := readmitted.Resolve(ctx, instance, []integrations.SourceKey{ecobeeKey}); err != nil || len(got) != 1 || got[0] != ecobee {
		t.Fatalf("readmission resolve changed IDs: %v %v", got, err)
	}
	finalRig := nha137ColdHost(t, ctx, readmitted, instance, executor)
	if _, err := finalRig.session.Discover(ctx, []integrations.Candidate{nha137Candidate(t, ecobeeKey, true)}); err != nil {
		t.Fatal(err)
	}
	finalService, err := integrations.NewDurableCommandService(ctx, finalRig.host, nha137Generation{}, reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := finalService.Close(cleanup); err != nil {
			t.Error(err)
		}
	}()
	if executor.calls.Load() != 2 {
		t.Fatal("scope readmission replayed a command")
	}
	commandID, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	status, present, err := finalService.Status(ctx, commandID)
	if err != nil || !present || status.State != storage.IntentResult || !proto.Equal(status.Response, response) {
		t.Fatalf("completed command history changed: %+v %t %v", status, present, err)
	}
	if result, err := finalService.Dispatch(ctx, actor, request); err != nil || !proto.Equal(result, response) || executor.calls.Load() != 2 {
		t.Fatalf("completed intent resent after readmission: %v %v sends=%d", result, err, executor.calls.Load())
	}
	if got := nha137DurableImage(t, ctx, reopened); got != stable {
		t.Fatal("readmission rewrote durable history")
	}
	for _, label := range []string{"climate.home", "climate.nha137_previous", "climate.nha137_current"} {
		alias, err := readmitted.LookupAlias(ctx, label)
		if err != nil || alias.Entity != ecobee.Entity || alias.Current != (label == "climate.nha137_current") || alias.Retired {
			t.Fatalf("alias history changed: %q %+v %v", label, alias, err)
		}
	}
}

type nha137ProjectionCounter struct{ publications int }

func (s *nha137ProjectionCounter) PublishRegistry(context.Context, []integrations.RegistryRecord) error {
	s.publications++
	return nil
}
func (*nha137ProjectionCounter) AcceptDiscovery(context.Context, []integrations.DiscoveryDeclaration) error {
	return nil
}
func (*nha137ProjectionCounter) InvalidateRegistry() {}

func TestNHA137InactiveLineageNeverHidesForeignOriginalOrCorruption(t *testing.T) {
	store, ctx, _ := nha015Store(t)
	instance := nha015Instance(t, "nha137-integrity")
	key := nha015Source(t, "synthetic-ecobee", "thermostat")
	amended := nha137Repository(t, store, scope.EcobeeMVPV1())
	ecobee, err := amended.ConfigureBinding(ctx, instance, key, "climate.home")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := amended.ConfigureBinding(ctx, instance, nha015Source(t, "synthetic-switch", "relay"), "switch.chauffe_eau"); err != nil {
		t.Fatal(err)
	}
	entity, _ := ecobee.Entity.Value()
	for _, tc := range []struct {
		name, corrupt, restore string
		want                   error
	}{
		{"foreign-original", `UPDATE public.registry_entities SET manifest_alias='climate.foreign_thermostat' WHERE id=$1`, `UPDATE public.registry_entities SET manifest_alias='climate.home' WHERE id=$1`, registry.ErrOutsideManifest},
		{"excluded-homekit-original", `UPDATE public.registry_entities SET manifest_alias='sensor.home_current_temperature' WHERE id=$1`, `UPDATE public.registry_entities SET manifest_alias='climate.home' WHERE id=$1`, registry.ErrOutsideManifest},
		{"malformed-original", `UPDATE public.registry_entities SET manifest_alias='INVALID ALIAS' WHERE id=$1`, `UPDATE public.registry_entities SET manifest_alias='climate.home' WHERE id=$1`, registry.ErrOutsideManifest},
		{"missing-original-alias", `UPDATE public.registry_aliases SET alias='climate.synthetic_without_original' WHERE entity_id=$1`, `UPDATE public.registry_aliases SET alias='climate.home' WHERE entity_id=$1`, registry.ErrStorage},
		{"missing-current-alias", `UPDATE public.registry_aliases SET current=false WHERE entity_id=$1`, `UPDATE public.registry_aliases SET current=true WHERE entity_id=$1`, registry.ErrStorage},
		{"malformed-alias", `UPDATE public.registry_aliases SET alias='INVALID ALIAS' WHERE entity_id=$1`, `UPDATE public.registry_aliases SET alias='climate.home' WHERE entity_id=$1`, registry.ErrStorage},
		{"stolen-lineage-alias", `INSERT INTO public.registry_aliases(alias,entity_id,current) VALUES('climate.chambre_eugenie',$1,false)`, `DELETE FROM public.registry_aliases WHERE entity_id=$1 AND alias='climate.chambre_eugenie'`, registry.ErrAliasConflict},
		{"malformed-source-key", `UPDATE public.registry_source_bindings SET source_entity=repeat('x',257) WHERE entity_id=$1`, `UPDATE public.registry_source_bindings SET source_entity='thermostat' WHERE entity_id=$1`, registry.ErrStorage},
		{"missing-binding", `DELETE FROM public.registry_source_bindings WHERE entity_id=$1`, `INSERT INTO public.registry_source_bindings(instance_id,source_device,source_entity,entity_id) VALUES('nha137-integrity','synthetic-ecobee','thermostat',$1)`, registry.ErrStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutate := func(statement string) {
				t.Helper()
				if err := store.InTransaction(ctx, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, statement, entity); return err }); err != nil {
					t.Fatal(err)
				}
			}
			mutate(tc.corrupt)
			defer mutate(tc.restore)
			corruptImage := nha137DurableImage(t, ctx, store)
			for _, revision := range []scope.Revision{scope.BaselineV1(), scope.EcobeeMVPV1()} {
				repo := nha137Repository(t, store, revision)
				if got, err := repo.List(ctx); !errors.Is(err, tc.want) || len(got) != 0 {
					t.Fatalf("corrupt image admitted or partially projected: len=%d err=%v want=%v", len(got), err, tc.want)
				}
				sink := &nha137ProjectionCounter{}
				if err := repo.AttachProjection(ctx, sink); !errors.Is(err, tc.want) || sink.publications != 0 {
					t.Fatalf("corrupt projection published: count=%d err=%v", sink.publications, err)
				}
				if tc.want == registry.ErrOutsideManifest {
					if err := repo.Rename(ctx, ecobee.Entity, "climate.cannot_launder_original"); !errors.Is(err, registry.ErrOutsideManifest) {
						t.Fatalf("foreign original renamed: %v", err)
					}
				}
			}
			if got := nha137DurableImage(t, ctx, store); got != corruptImage {
				t.Fatal("validation rewrote corrupt rows")
			}
		})
		if got, err := nha137Repository(t, store, scope.BaselineV1()).List(ctx); err != nil || len(got) != 1 {
			t.Fatalf("restored baseline projection unavailable: %d %v", len(got), err)
		}
	}
}
