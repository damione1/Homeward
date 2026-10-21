package integrations_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

type nha029InternalDriver struct {
	mu      sync.Mutex
	emitted int
}

func (d *nha029InternalDriver) Dispatch(_ context.Context, command integrations.AdmittedCommand) error {
	if _, _, _, err := command.Identity(); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		return errors.New("internal command exposed a human actor")
	}
	if _, _, err := command.RouteIdentity(); err != nil || !command.ReadyForTransport() {
		return errors.New("internal mechanical route was not admitted")
	}
	d.mu.Lock()
	d.emitted++
	d.mu.Unlock()
	return nil
}

func (d *nha029InternalDriver) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.emitted
}

func TestNHA029InternalPortIsBoundToServiceOccurrenceAndLedgerOrigin(t *testing.T) {
	store, _ := nha024Store(t)
	driver := &nha029InternalDriver{}
	rig := nha050Setup(t, driver)
	service, err := integrations.NewDurableCommandService(context.Background(), rig.host, rig.gen, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	port, err := service.InternalAutomationPort()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := rig.fixture.Sequencer().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	request := nha022Request(t, rig.identities[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	request.CommandId = "nha029-internal-origin"
	id, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	var stale context.Context
	if err := port.WithOccurrence(context.Background(), "run/one/action/one", func(ctx context.Context) error {
		stale = ctx
		if !port.SameOccurrence(ctx, context.WithValue(ctx, struct{}{}, true)) {
			t.Fatal("derived execution context lost its private occurrence")
		}
		if err := port.WithOccurrence(ctx, "nested", func(context.Context) error { return nil }); !errors.Is(err, integrations.ErrInvalidInput) {
			t.Fatalf("nested occurrence admitted: %v", err)
		}
		response, err := port.Dispatch(ctx, request)
		if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("trusted internal route failed: %v %v", response, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if driver.count() != 1 || port.ValidContext(stale) || port.SameOccurrence(stale, stale) {
		t.Fatalf("effect count or stale occurrence: %d %t", driver.count(), port.ValidContext(stale))
	}
	if response, err := port.Dispatch(stale, request); response != nil || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatalf("expired occurrence crossed port: %v %v", response, err)
	}
	if status, found, err := service.Status(rig.ctx, id); found || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatalf("human status read internal origin: %+v %t %v", status, found, err)
	}
	if response, err := service.Dispatch(rig.ctx, rig.actor, request); response != nil || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatalf("human replayed internal ID: %v %v", response, err)
	}
	if err := port.WithOccurrence(context.Background(), "run/two/action/one", func(ctx context.Context) error {
		response, err := port.Dispatch(ctx, request)
		if response != nil || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
			t.Fatalf("foreign occurrence replayed ID: %v %v", response, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := port.WithOccurrence(context.Background(), "run/one/action/one", func(ctx context.Context) error {
		changed := proto.Clone(request).(*actions.CommandRequest)
		changed.ConfigRevision++
		response, err := port.Dispatch(ctx, changed)
		if response != nil || !errors.Is(err, integrations.ErrCommandIDConflict) {
			t.Fatalf("changed canonical command replayed ID: %v %v", response, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	record, found, err := store.CommandIntent(context.Background(), id)
	if err != nil || !found || record.OriginKind() != storage.IntentOriginInternalAutomation {
		t.Fatalf("internal origin missing from common ledger: %t %v %v", found, record.OriginKind(), err)
	}
	if key, present := record.InternalOriginKey(); !present || !key.Valid() {
		t.Fatal("internal occurrence provenance absent")
	}
	if driver.count() != 1 {
		t.Fatalf("replay emitted again: %d", driver.count())
	}
}

type nha029NilContext struct{ context.Context }

func TestNHA029InternalPortRejectsZeroTypedNilHumanAndForeignContext(t *testing.T) {
	var zero integrations.InternalAutomationPort
	if err := zero.WithOccurrence(context.Background(), "cause", func(context.Context) error { return nil }); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("zero port admitted occurrence: %v", err)
	}
	var typedNil *nha029NilContext
	storeA, _ := nha024Store(t)
	rigA := nha050Setup(t, &nha029InternalDriver{})
	serviceA, err := integrations.NewDurableCommandService(context.Background(), rigA.host, rigA.gen, storeA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = serviceA.Close(ctx)
	})
	portA, err := serviceA.InternalAutomationPort()
	if err != nil {
		t.Fatal(err)
	}
	if err := portA.WithOccurrence(typedNil, "cause", func(context.Context) error { return nil }); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("typed-nil context admitted: %v", err)
	}
	if err := portA.WithOccurrence(rigA.ctx, "cause", func(context.Context) error { return nil }); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatalf("human principal became internal occurrence: %v", err)
	}
	storeB, _ := nha024Store(t)
	rigB := nha050Setup(t, &nha029InternalDriver{})
	serviceB, err := integrations.NewDurableCommandService(context.Background(), rigB.host, rigB.gen, storeB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = serviceB.Close(ctx)
	})
	portB, err := serviceB.InternalAutomationPort()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := rigA.fixture.Sequencer().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	request := nha022Request(t, rigA.identities[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	request.CommandId = "nha029-foreign-port"
	if err := portB.WithOccurrence(context.Background(), "foreign", func(ctx context.Context) error {
		if portA.ValidContext(ctx) {
			t.Fatal("foreign port accepted another service's occurrence")
		}
		if response, err := portA.Dispatch(ctx, request); response != nil || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
			t.Fatalf("foreign port dispatched through service A: %v %v", response, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	alias := nha022Alias(t, "switch.nha050_target")
	if err := portA.WithOccurrence(context.Background(), "capture/A", func(ctxA context.Context) error {
		capture, err := serviceA.CaptureDirect(ctxA, alias, integrations.DirectSwitch)
		if err != nil {
			t.Fatal(err)
		}
		return portB.WithOccurrence(context.Background(), "capture/B", func(ctxB context.Context) error {
			id, err := domain.ParseCommandID("nha029-foreign-preparation")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := capture.PrepareInternalSwitch(ctxB, portB, id, time.Now().Add(time.Minute), true); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
				t.Fatalf("foreign occurrence prepared captured route: %v", err)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNHA029LegacyHostCannotExposeOrReplayInternalID(t *testing.T) {
	store, _ := nha024Store(t)
	driver := &nha022Driver{}
	rig := nha023SetupBare(t, driver)
	service := nha024Service(t, rig, store)
	port, err := service.InternalAutomationPort()
	if err != nil {
		t.Fatal(err)
	}
	request := proto.Clone(rig.request).(*actions.CommandRequest)
	request.CommandId = "nha029-legacy-host-origin"
	id, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := service.Dispatch(context.Background(), domain.ActorID{}, request); response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || response.Error.GetCode() != actions.ErrorCode_ERROR_CODE_PERMISSION_DENIED || err != nil {
		t.Fatalf("zero actor without private occurrence admitted: %v %v", response, err)
	}
	if err := port.WithOccurrence(context.Background(), "legacy-host/internal", func(ctx context.Context) error {
		response, err := port.Dispatch(ctx, request)
		if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("internal route under bare Host: %v %v", response, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if status, found, err := service.Status(context.Background(), id); found || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatalf("bare Host exposed internal status: %+v %t %v", status, found, err)
	}
	if response, err := service.Dispatch(context.Background(), nha022Actor(t), request); response != nil || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatalf("bare Host replayed internal ID: %v %v", response, err)
	}
	if driver.count() != 1 {
		t.Fatalf("bare Host replay emitted: %d", driver.count())
	}
}
