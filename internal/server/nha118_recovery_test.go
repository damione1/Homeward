package server

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/backups/installation"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA118RestoredRunFencesDispatchedIntentAndRejectsOldVolatileEpoch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	r.run.waitReady(t)
	nha034WaitSignal(t, r.pilot.ready, "source simulated pilot")
	oldEpoch, err := r.identity.Sequence().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	actor, err := r.environment.principal.Actor()
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.ParseCommandID("nha118-dispatched-before-loss")
	_, created, outcome, err := r.store.AcceptCommandIntent(ctx, id, actor, oldEpoch, []byte("nha118-synthetic-crash-fixture"))
	if err != nil || !created || outcome != storage.TransactionCommitted {
		t.Fatal("cannot arrange admitted crash state", err)
	}
	moved, outcome, err := r.store.MoveCommandIntent(ctx, id, storage.IntentAccepted, storage.IntentDispatching, nil, storage.IntentNoError)
	if err != nil || !moved || outcome != storage.TransactionCommitted {
		t.Fatal("cannot arrange dispatched crash state", err)
	}
	// A volatile command prepared by a client in the old epoch may still be
	// queued outside the process when the installation is recovered.
	queued := &actions.CommandRequest{CommandId: "nha118-old-queued-volatile", AdmissionEpoch: oldEpoch, Target: &registrywire.EntityRef{Reference: &registrywire.EntityRef_HaAlias{HaAlias: nha042ServerAlias}}, CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay", Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}}, ExpiresAt: timestamppb.New(time.Now().Add(2 * time.Minute))}
	data, err := r.environment.config.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := r.store.BootstrapSecretsForStartup(ctx, data, storage.RequireEstablishedSecretStartup())
	if err != nil {
		t.Fatal(err)
	}
	public, private := nha035Recovery(t)
	service, err := installation.NewService(r.store, secrets, public, filepath.Join(data, "nha118-pending"))
	if err != nil {
		t.Fatal(err)
	}
	artifact, _ := backups.NewArtifactID()
	sealed, err := service.Create(ctx, nha035Tools(t), artifact)
	if err != nil {
		t.Fatal(err)
	}
	payload := nha035Export(t, sealed)
	r.run.cancel()
	if err = r.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	if len(r.driver.snapshot()) != 0 {
		t.Fatal("source fixture unexpectedly emitted an action")
	}
	target := testpg.Start(t)
	parent := shortPrivateDataDir(t)
	destination := filepath.Join(parent, "recovered")
	bootstrap := nha035Bootstrap(target.Port, destination)
	result, err := installation.Restore(ctx, bytes.NewReader(payload), artifact, private, parent, "recovered", nha035Tools(t), bootstrap, nha029TestEnv)
	if err != nil || result.State() != installation.Restored || !result.Measurement().WithinTargets() {
		t.Fatal("isolated recovery failed", err)
	}
	// Use the actual restored sources and explicit target launch sidecar.
	// A fresh legacy workspace would name an absent default sidecar, which
	// cannot pass the established installation's physical bootstrap CAS.
	choice, err := config.ExistingBootstrap(filepath.Join(destination, "bootstrap.launch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(filepath.Join(destination, "configuration.yaml"), choice, nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	launch, err := startup.LaunchConfig()
	if err != nil {
		t.Fatal(err)
	}
	configuration := nha029RunFixture{Config: launch, startup: startup}
	environment := &nha042ServerEnvironment{config: configuration, account: r.environment.account, principal: r.environment.principal, observer: nha030ServerObserver(t, target.Port), driverObserver: nha030ServerObserver(t, target.Port)}
	var owner *configactivation.Owner
	recovered := nha042StartServer(t, environment, nha029OwnerParent, nha029OwnerChild, nha042ServerOptions{sharedJobs: true, change: func(plan *RuntimePlan) {
		// Match029's real SQL registry composition without replacing the
		// restored source locators or manufacturing a new activation.
		identity := plan.Identity.(*nha044Identity)
		plan.Identity = identity.repo
		plan.ConfigureAuthorization = func(_ context.Context, composition AuthorizationComposition) error {
			actual := composition.(runtimeAuthorizationComposition)
			identity.mu.Lock()
			identity.sequence = actual.views.source
			identity.mu.Unlock()
			return nil
		}
		plan.ConfigureActivation = func(_ context.Context, actual *configactivation.Owner) error {
			owner = actual
			return nil
		}
	}})
	recovered.run.waitReady(t)
	nha034WaitSignal(t, recovered.pilot.ready, "restored simulated pilot")
	if owner == nil {
		t.Fatal("restored runtime did not compose its real generation authority")
	}
	record, present, err := recovered.store.CommandIntent(ctx, id)
	var response actions.CommandResponse
	if err != nil || !present || record.State() != storage.IntentIndeterminate || proto.Unmarshal(record.Response(), &response) != nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatal("restored dispatched command lost its uncertainty", err)
	}
	newEpoch, err := recovered.identity.Sequence().CurrentAdmissionEpoch()
	if err != nil || newEpoch == oldEpoch {
		t.Fatal("recovery reused old admission epoch")
	}
	stamp, err := owner.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queued.ConfigRevision, _, err = stamp.Components()
	if err != nil {
		t.Fatal(err)
	}
	router, err := integrations.NewCommandRouter(recovered.host, owner)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := authorization.WithPrincipal(ctx, environment.principal)
	if err != nil {
		t.Fatal(err)
	}
	refused, err := router.Dispatch(caller, actor, queued)
	if err != nil || refused == nil || refused.Error == nil || refused.Error.Code != actions.ErrorCode_ERROR_CODE_STALE_EPOCH {
		t.Fatalf("old volatile epoch was not explicitly refused: %v %v", refused, err)
	}
	if len(recovered.driver.snapshot()) != 0 {
		t.Fatal("recovered runtime replayed a physical action")
	}
	queuedID, _ := domain.ParseCommandID(queued.CommandId)
	if _, present, err = recovered.store.CommandIntent(ctx, queuedID); err != nil || present {
		t.Fatal("volatile refusal created a durable intention", err)
	}
	recovered.run.cancel()
	if err = recovered.run.waitDone(t); err != nil {
		t.Fatal(err)
	}
	t.Logf("NHA118 MEASURED_SYNTHETIC cut=%s intervention=%s completed=%s snapshot_age=%s restore_duration=%s; dispatched remains indeterminate; queued old volatile epoch rejected; fake effects=0", result.Measurement().CutUTC.Format(time.RFC3339Nano), result.Measurement().InterventionUTC.Format(time.RFC3339Nano), result.Measurement().CompletedUTC.Format(time.RFC3339Nano), result.Measurement().DataAge, result.Measurement().RestoreDuration)
}

func TestNHA118ColdStartWithoutDatabaseNeverCreatesAdmissionOrReady(t *testing.T) {
	cluster := testpg.Start(t)
	startup := nha029RunStartupFromRaw(t, nha035Bootstrap(cluster.Port, shortPrivateDataDir(t)))
	if err := cluster.Stop(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	factoryCalls, listenerCalls, readyCalls := 0, 0, 0
	factory := func(context.Context, *storage.Store) (RuntimePlan, error) {
		factoryCalls++
		return RuntimePlan{Identity: unconfiguredIdentity{}}, nil
	}
	listen := func(context.Context, string, string) (net.Listener, error) {
		listenerCalls++
		return nil, os.ErrPermission
	}
	err := RunWithStartupFactory(ctx, startup, listen, func(net.Addr) { readyCalls++ }, factory)
	if err == nil || ctx.Err() != nil || factoryCalls != 0 || listenerCalls != 0 || readyCalls != 0 {
		t.Fatalf("database loss admitted services: err=%v factory=%d listener=%d ready=%d", err, factoryCalls, listenerCalls, readyCalls)
	}
}
