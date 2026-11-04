package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// This opt-in fixture exercises the real existing MCP audience authenticator
// with an issued key and the shared resource handler. No CRUD MCP tool or MCP
// credential admission to production Connect routes is installed by this test.
func TestNHA203IntegrationMCPBoundaryPreservesAudienceCredentialAndRevocationTLS(t *testing.T) {
	f := nha148StartTLS(t)
	coordinator := f.run.observed.sessions
	provider := &sessionAuth{store: coordinator.store, coordinator: coordinator}
	plan, err := NewPublicPlan(provider, provider, accountTransportAudit{store: coordinator.store})
	if err != nil {
		t.Fatal(err)
	}
	const createPath = "/nha203.IntegrationFixture/Create"
	const updatePath = "/nha203.IntegrationFixture/Update"
	const getPath = "/nha203.IntegrationFixture/GetOperation"
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		t.Fatal(err)
	}
	get := func(ctx context.Context, request *connect.Request[longrunningpb.GetOperationRequest]) (*connect.Response[longrunningpb.Operation], error) {
		name, err := apitypes.ParseOperationName(request.Msg.Name)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		operation, err := f.integrationHandler.readOperation(ctx, name, nil)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		return connect.NewResponse(operation), nil
	}
	for path, handler := range map[string]*connect.Handler{
		createPath: connect.NewUnaryHandler(createPath, f.integrationHandler.CreateIntegrationInstance),
		updatePath: connect.NewUnaryHandler(updatePath, f.integrationHandler.UpdateIntegrationInstance),
		getPath:    connect.NewUnaryHandler(getPath, get),
	} {
		if err := plan.Register(RouteKindMCP, RouteMethodPost, path, RouteAccessProtected, RouteFlowUnary, RouteTransportTLSRequired, handler); err != nil {
			t.Fatal(err)
		}
		if err := plan.BindPermission(RouteKindMCP, RouteMethodPost, path, permission); err != nil {
			t.Fatal(err)
		}
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(compiled.handler(settings, newPublicAdmissions()))
	t.Cleanup(server.Close)
	create := connect.NewClient[api.CreateIntegrationInstanceRequest, longrunningpb.Operation](server.Client(), server.URL+createPath)
	update := connect.NewClient[api.UpdateIntegrationInstanceRequest, longrunningpb.Operation](server.Client(), server.URL+updatePath)
	read := connect.NewClient[longrunningpb.GetOperationRequest, longrunningpb.Operation](server.Client(), server.URL+getPath)
	nativeHTTP := nha162RunClient(t, f.roots, f.run.address)
	origin := "https://" + f.run.address.String()
	issuer := authv1connect.NewMcpKeyServiceClient(nativeHTTP, origin)
	issue := func(label string, scope auth.ScopeOperation, lifetime *durationpb.Duration) *auth.CreateMcpKeyResponse {
		t.Helper()
		response, err := issuer.CreateMcpKey(f.ctx, nha056SessionRequest(&auth.CreateMcpKeyRequest{ClientName: label, Scopes: []*auth.Scope{{Family: auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, Operation: scope}}, Lifetime: lifetime}, f.token))
		if err != nil {
			t.Fatal(err)
		}
		return response.Msg
	}
	first := issue("integration admin first", auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil)
	second := issue("integration admin second", auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil)
	readOnly := issue("integration read only", auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	home, err := apitypes.ParseHomeName(nha203HomeName(t, f.ctx, f.run))
	if err != nil {
		t.Fatal(err)
	}
	typ, err := integrationTypeName(home, "workday")
	if err != nil {
		t.Fatal(err)
	}
	input := &api.CreateIntegrationInstanceRequest{Parent: home.String(), IntegrationInstance: &api.IntegrationInstance{InstanceKey: "homeward-workday-v1", IntegrationType: typ.String(), DesiredState: api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED, Configuration: &api.IntegrationInstance_Editable{Editable: &api.IntegrationConfiguration{SchemaVersion: "homeward.integration/workday/1", YamlSource: []byte("timezone: America/Toronto\n")}}}}
	if _, err := apiv2connect.NewIntegrationServiceClient(nativeHTTP, origin).CreateIntegrationInstance(f.ctx, nha056SessionRequest(input, first.Secret)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("MCP key entered native Connect audience", err)
	}
	if _, err := create.CallUnary(f.ctx, nha056SessionRequest(&api.CreateIntegrationInstanceRequest{}, readOnly.Secret)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("scope checked after body diagnostics", err)
	}
	created, err := create.CallUnary(f.ctx, nha056SessionRequest(input, first.Secret))
	if err != nil {
		t.Fatal("real MCP admin-only create", err)
	}
	definition := nha203IntegrationResult(t, created.Msg)
	request := &api.UpdateIntegrationInstanceRequest{IntegrationInstance: &api.IntegrationInstance{Name: definition.Name, Etag: definition.Etag}, UpdateMask: &fieldmaskpb.FieldMask{}, RequestId: "018e363a-8e16-44a4-9aa6-f623ecb0b541"}
	noChange, err := update.CallUnary(f.ctx, nha056SessionRequest(request, first.Secret))
	if err != nil {
		t.Fatal("MCP no-op", err)
	}
	if _, err := update.CallUnary(f.ctx, nha056SessionRequest(request, second.Secret)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("other MCP key replayed origin receipt", err)
	}
	stored, err := read.CallUnary(f.ctx, nha056SessionRequest(&longrunningpb.GetOperationRequest{Name: noChange.Msg.Name}, first.Secret))
	if err != nil || !proto.Equal(stored.Msg, noChange.Msg) {
		t.Fatal("MCP original receipt unavailable", err)
	}
	if _, err := read.CallUnary(f.ctx, nha056SessionRequest(&longrunningpb.GetOperationRequest{Name: noChange.Msg.Name}, second.Secret)); err == nil {
		t.Fatal("other MCP key read origin receipt")
	}
	expiring := issue("integration expiring", auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, durationpb.New(200*time.Millisecond))
	if wait := time.Until(expiring.Key.ExpiresAt.AsTime()) + 5*time.Millisecond; wait > 0 {
		time.Sleep(wait)
	}
	if _, err := create.CallUnary(f.ctx, nha056SessionRequest(&api.CreateIntegrationInstanceRequest{}, expiring.Secret)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("expired MCP credential reached handler", err)
	}
	if _, err := issuer.RevokeMcpKey(f.ctx, nha056SessionRequest(&auth.RevokeMcpKeyRequest{KeyId: first.Key.Id}, f.token)); err != nil {
		t.Fatal(err)
	}
	if _, err := read.CallUnary(f.ctx, nha056SessionRequest(&longrunningpb.GetOperationRequest{Name: noChange.Msg.Name}, first.Secret)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("revoked MCP credential read result", err)
	}
}
