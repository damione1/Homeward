package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	history "github.com/damione1/Homeward/gen/home/history/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA164PartialCompositeDeniesBeforeReaderAndRemovalFencesSQL(t *testing.T) {
	handler, ctx, _, spy := nha164Handler(t, auth.AccountRole_ACCOUNT_ROLE_USER, auth.ScopeFamily_SCOPE_FAMILY_HISTORY, auth.ScopeOperation_SCOPE_OPERATION_READ)
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha164-catalogue")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	device, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := binding.Source()
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("switch.nha164_fixture")
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.Sequencer().CaptureEntityCatalogue(entity, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	public := snapshot.Descriptions()[0]
	private := proto.Clone(public).(*registry.CapabilityDescription)
	private.Endpoint = "private"
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{public, private})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.Discover(context.Background(), []integrations.Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	handler.runtime.sequence = fixture.Sequencer()
	account, _ := contracts.ParseAccountID("nha164-account")
	source, err := authorization.AdmitPolicySource(account, []byte("schema: homeward.authorization/1\naccount_id: nha164-account\ngrants:\n  - scope: {family: history, operation: read}\n    objects:\n      entities:\n        all:\n          capabilities:\n            explicit:\n              - {schema_id: core.switch.v1, endpoint: relay}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err = handler.runtime.authority.Publish(nha164AuthorityState(t, source, auth.AccountRole_ACCOUNT_ROLE_USER, 2)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	name, _ := entity.Value()
	request := connect.NewRequest(&history.QueryObservationsRequest{EntityId: name, Resolution: history.Resolution_RESOLUTION_RAW, Period: &history.Period{Start: timestamppb.New(now.Add(-time.Hour)), End: timestamppb.New(now)}})
	if response, err := handler.QueryObservations(ctx, request); response != nil || connect.CodeOf(err) != connect.CodePermissionDenied || spy.calls.Load() != 0 {
		t.Fatalf("partial composite entered storage: %v", err)
	}
	full, err := authorization.AdmitPolicySource(account, []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: nha164-account\ngrants:\n  - scope: {family: history, operation: read}\n    objects:\n      entities: {all: {capabilities: {all: {}}}}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err = handler.runtime.authority.Publish(nha164AuthorityState(t, full, auth.AccountRole_ACCOUNT_ROLE_USER, 3)); err != nil {
		t.Fatal(err)
	}
	t.Run("second-permit-capacity-preserves-classification", func(t *testing.T) {
		principal, err := authorization.PrincipalFromContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_HISTORY, auth.ScopeOperation_SCOPE_OPERATION_READ)
		if err != nil {
			t.Fatal(err)
		}
		slot, err := authorization.NewCapabilitySlot(public.SchemaId, public.Endpoint)
		if err != nil {
			t.Fatal(err)
		}
		object, err := authorization.EntityObject(entity, slot)
		if err != nil {
			t.Fatal(err)
		}
		held := make([]*authorization.Permit, 0, authorization.MaxPermits-1)
		defer func() {
			for _, permit := range held {
				permit.Release()
			}
		}()
		for i := 0; i < authorization.MaxPermits-1; i++ {
			permit, err := handler.runtime.authority.Permit(ctx, principal, permission, object)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, permit)
		}
		response, err := handler.QueryObservations(ctx, request)
		if response != nil || connect.CodeOf(err) != connect.CodeResourceExhausted || spy.calls.Load() != 0 {
			t.Fatalf("cleanup hid second permit capacity or entered reader: %v", err)
		}
		typed, ok := err.(*connect.Error)
		if !ok {
			t.Fatal("missing Connect capacity error")
		}
		capacity := false
		for _, detail := range typed.Details() {
			wire, err := detail.Value()
			if err != nil {
				t.Fatal(err)
			}
			if rejected, ok := wire.(*history.HistoryError); ok && rejected.Code == history.ErrorCode_ERROR_CODE_CAPACITY {
				capacity = true
			}
		}
		if !capacity {
			t.Fatal("typed capacity classification lost")
		}
	})
	entered, release := make(chan struct{}), make(chan struct{})
	spy.recorder = func(context.Context, historyquery.Query, *storage.RecorderQueryCursor) (storage.RecorderQueryPage, error) {
		close(entered)
		<-release
		return storage.RecorderQueryPage{NotRetained: true, RawCutoff: now.Add(time.Hour), AsOf: now}, nil
	}
	returned := make(chan error, 1)
	go func() {
		response, err := handler.QueryObservations(ctx, request)
		if response != nil {
			err = fmt.Errorf("removed catalogue emitted response")
		}
		returned <- err
	}()
	<-entered
	if err = fixture.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-returned:
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("catalogue removal did not fence SQL: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not stop")
	}
}
