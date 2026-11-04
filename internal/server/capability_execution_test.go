package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/gen/google/longrunning/longrunningpbconnect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Real TLS/Connect and SQL/native owners; the test interceptor supplies a
// synthetic admitted credential. Login/credential verification has separate
// tests; no fixture dispatch reaches a physical device.
func TestNHA201ExecuteCapabilityTLSRetainsAdmissionAndNeverReplays(t *testing.T) {
	f := nha165GrantSetupAuthority(t, authorization.NewAuthority())
	driver, err := fake.NewActionDriver(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(f.ctx, f.session); err != nil {
		t.Fatal(err)
	}
	if err := f.host.AttachCommandDriver(f.session, driver); err != nil {
		t.Fatal(err)
	}
	grants, err := authorization.AdmitPolicySource(f.account, []byte("schema: homeward.authorization/1\naccount_id: nha165-cache-account\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n  - scope: {family: actions, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n  - scope: {family: actions, operation: write}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.a.authority.Publish(nha164AuthorityState(t, grants, auth.AccountRole_ACCOUNT_ROLE_USER, 2)); err != nil {
		t.Fatal(err)
	}
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := integrations.NewDurableCommandService(f.ctx, f.host, nha026ServerGeneration{}, store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	coordinator, err := newSessionCoordinator(store)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.authority = f.a.authority
	home, _ := apitypes.ParseHomeName("homes/synthetic-execution")
	metadata, _ := apitypes.NewHomeMetadata(home, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	owner := nha201ProjectionOwner{home: metadata, record: f.record}
	core := &coreRPCHandler{commands: service, epoch: f.a.source}
	stamp, _ := view.NewConfigStamp(7, [32]byte{1})
	views, err := view.NewService(f.a.source, f.a, nha050ViewConfig{stamp}, sequencer.RealClock{}, core.capabilityProjector(owner, f.a.authority))
	if err != nil {
		t.Fatal(err)
	}
	core.views = views
	core.home, err = newHomeResourceHandler(owner, f.a.authority, views)
	if err != nil {
		t.Fatal(err)
	}
	operations, err := newOperationRPCHandler(core, nil, f.a.authority)
	if err != nil {
		t.Fatal(err)
	}
	interceptor := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
			authorized, authErr := authorization.WithPrincipal(ctx, f.principal)
			if authErr != nil {
				return nil, authErr
			}
			ctx = authorized
			ctx = context.WithValue(ctx, actorContextKey{}, f.actor)
			guard, guardErr := coordinator.enter(ctx, f.account)
			if guardErr != nil {
				return nil, guardErr
			}
			session, _ := contracts.ParseSessionID("nha165-cache-session")
			admitted, lease := guard.register(ctx, storage.SessionOwner{Account: f.account, Session: session})
			lease.deadline = time.Now().Add(time.Minute)
			lease.nativePrincipal = f.principal
			guard.release()
			defer lease.release()
			return next(admitted, request)
		}
	})
	mux := http.NewServeMux()
	path, handler := apiv2connect.NewCapabilityServiceHandler(core, connect.WithInterceptors(interceptor))
	mux.Handle(path, handler)
	path, handler = apiv2connect.NewHomeServiceHandler(core.home, connect.WithInterceptors(interceptor))
	mux.Handle(path, handler)
	path, handler = longrunningpbconnect.NewOperationsHandler(operations, connect.WithInterceptors(interceptor))
	mux.Handle(path, handler)
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	homeClient := apiv2connect.NewHomeServiceClient(server.Client(), server.URL)
	capabilityClient := apiv2connect.NewCapabilityServiceClient(server.Client(), server.URL)
	operationClient := longrunningpbconnect.NewOperationsClient(server.Client(), server.URL)
	_, entity, _ := f.record.IDs()
	entityName, _ := apitypes.EntityNameForIdentity(home, entity)
	list, err := homeClient.ListCapabilities(context.Background(), connect.NewRequest(&api.ListCapabilitiesRequest{Parent: entityName.String()}))
	if err != nil || len(list.Msg.Capabilities) != 1 {
		t.Fatal(list, err)
	}
	capability := list.Msg.Capabilities[0]
	command := &api.ExecuteCapabilityRequest{Name: capability.Name, Etag: capability.Etag, RequestId: "12345678-1234-4123-8123-123456789abc", ExpireTime: timestamppb.New(time.Now().Add(3 * time.Second)), Command: &api.ExecuteCapabilityRequest_Power{Power: &api.SetPowerCommand{Value: api.PowerValue_POWER_VALUE_ON}}}
	submitted, err := capabilityClient.ExecuteCapability(context.Background(), connect.NewRequest(command))
	if err != nil {
		t.Fatal(err)
	}
	if submitted.Msg.Name == "" {
		t.Fatal("missing standard handle")
	}
	duplicate, err := capabilityClient.ExecuteCapability(context.Background(), connect.NewRequest(proto.Clone(command).(*api.ExecuteCapabilityRequest)))
	if err != nil || duplicate.Msg.Name != submitted.Msg.Name {
		t.Fatal("duplicate did not reconcile", duplicate, err)
	}
	stop := time.Now().Add(6 * time.Second)
	for !submitted.Msg.Done && time.Now().Before(stop) {
		time.Sleep(20 * time.Millisecond)
		submitted, err = operationClient.GetOperation(context.Background(), connect.NewRequest(&longrunningpb.GetOperationRequest{Name: submitted.Msg.Name}))
		if err != nil {
			t.Fatal(err)
		}
	}
	if !submitted.Msg.Done || len(driver.Commands()) != 1 {
		t.Fatal("missing durable terminal or repeated effect", submitted.Msg, len(driver.Commands()))
	}
	// The same public request remains a read after its deadline. No current
	// generation/deadline rewrite may turn it into a new physical occurrence.
	if _, err := capabilityClient.ExecuteCapability(context.Background(), connect.NewRequest(command)); err != nil {
		t.Fatal("expired original intention was not reconciled", err)
	}
	if len(driver.Commands()) != 1 {
		t.Fatal("expired duplicate dispatched")
	}
	if err := f.a.authority.Publish(nha164AuthorityState(t, f.source, auth.AccountRole_ACCOUNT_ROLE_USER, 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := operationClient.GetOperation(context.Background(), connect.NewRequest(&longrunningpb.GetOperationRequest{Name: submitted.Msg.Name})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("revoked operation remained visible", err)
	}
	permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_READ)
	scopes, _ := authorization.NewScopeSet([]authorization.Permission{permission})
	key, _ := contracts.ParseMcpKeyID("synthetic-operation-read-key")
	scoped, _ := authorization.ScopedPrincipal(f.account, key, scopes)
	scopedContext, _ := authorization.WithPrincipal(context.Background(), scoped)
	scopedContext = context.WithValue(scopedContext, actorContextKey{}, f.actor)
	invalidWait := &longrunningpb.WaitOperationRequest{Name: submitted.Msg.Name, Timeout: &durationpb.Duration{Seconds: -1}}
	if _, err := operations.WaitOperation(scopedContext, connect.NewRequest(invalidWait)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("MCP coarse ceiling exposed timeout diagnostics for denied slot", err)
	}

}
