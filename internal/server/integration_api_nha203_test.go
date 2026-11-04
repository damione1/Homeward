package server

import (
	"bytes"
	"os"
	"testing"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/gen/google/longrunning/longrunningpbconnect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func nha203IntegrationResult(t *testing.T, operation *longrunningpb.Operation) *api.IntegrationInstance {
	t.Helper()
	name, err := apitypes.ParseOperationName(operation.Name)
	if err != nil || contracts.AdmitIntegrationOperation(operation, name) != nil || !operation.Done {
		t.Fatal("invalid completed integration operation", err)
	}
	result := new(api.IntegrationInstance)
	if anypb.UnmarshalTo(operation.GetResponse(), result, proto.UnmarshalOptions{}) != nil {
		t.Fatal("missing integration resource result")
	}
	return result
}

func TestNHA203IntegrationCRUDNoChangeKeepsOriginalCaptureTLS(t *testing.T) {
	f := nha148StartTLS(t)
	httpClient := nha162RunClient(t, f.roots, f.run.address)
	origin := "https://" + f.run.address.String()
	client := apiv2connect.NewIntegrationServiceClient(httpClient, origin)
	operations := longrunningpbconnect.NewOperationsClient(httpClient, origin)
	home := nha203HomeName(t, f.ctx, f.run)
	types, err := client.ListIntegrationTypes(f.ctx, nha056SessionRequest(&api.ListIntegrationTypesRequest{Parent: home}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	var typ string
	for _, value := range types.Msg.IntegrationTypes {
		if value.TypeKey == "workday" {
			typ = value.Name
		}
	}
	if typ == "" {
		t.Fatal("native workday descriptor absent")
	}
	created, err := client.CreateIntegrationInstance(f.ctx, nha056SessionRequest(&api.CreateIntegrationInstanceRequest{Parent: home, IntegrationInstance: &api.IntegrationInstance{InstanceKey: "homeward-workday-v1", IntegrationType: typ, DesiredState: api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED, Configuration: &api.IntegrationInstance_Editable{Editable: &api.IntegrationConfiguration{SchemaVersion: "homeward.integration/workday/1", YamlSource: []byte("timezone: America/Toronto\n")}}}}, f.token))
	if err != nil {
		t.Fatal("create", err)
	}
	original := nha203IntegrationResult(t, created.Msg)
	current, err := client.GetIntegrationInstance(f.ctx, nha056SessionRequest(&api.GetIntegrationInstanceRequest{Name: original.Name}, f.token))
	if err != nil || current.Msg.Etag != original.Etag {
		t.Fatal("published etag does not represent original native capture", err)
	}
	before := f.read(t)
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	request := &api.UpdateIntegrationInstanceRequest{IntegrationInstance: &api.IntegrationInstance{Name: original.Name, Etag: original.Etag}, UpdateMask: &fieldmaskpb.FieldMask{}}
	var noChange [2]*longrunningpb.Operation
	for i := range noChange {
		response, err := client.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(proto.Clone(request).(*api.UpdateIntegrationInstanceRequest), f.token))
		if err != nil {
			t.Fatal("empty-mask no-op", err)
		}
		noChange[i] = response.Msg
		result := nha203IntegrationResult(t, response.Msg)
		if !proto.Equal(result, original) {
			t.Fatal("NoChange replaced original definition")
		}
		metadata := new(api.IntegrationOperationMetadata)
		if anypb.UnmarshalTo(response.Msg.Metadata, metadata, proto.UnmarshalOptions{}) != nil || metadata.Phase != api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_NO_CHANGE || metadata.RequestId != "" {
			t.Fatal("NoChange invented UUID or publication")
		}
	}
	if noChange[0].Name == noChange[1].Name {
		t.Fatal("absent UUID silently deduplicated")
	}
	after := f.read(t)
	actual, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(raw, actual) || !proto.Equal(before.Version, after.Version) || !proto.Equal(before.ActiveGeneration, after.ActiveGeneration) {
		t.Fatal("NoChange modified native data", err)
	}
	changed := proto.Clone(original).(*api.IntegrationInstance)
	changed.GetEditable().YamlSource = []byte("timezone: Europe/Paris\n")
	updated, err := client.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(&api.UpdateIntegrationInstanceRequest{IntegrationInstance: changed, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"editable"}}, RequestId: "018e363a-8e16-44a4-9aa6-f623ecb0b540"}, f.token))
	if err != nil {
		t.Fatal("update", err)
	}
	if nha203IntegrationResult(t, updated.Msg).Etag == original.Etag {
		t.Fatal("publication reused stale etag")
	}
	replayed, err := operations.GetOperation(f.ctx, nha056SessionRequest(&longrunningpb.GetOperationRequest{Name: noChange[0].Name}, f.token))
	if err != nil || !proto.Equal(replayed.Msg, noChange[0]) {
		t.Fatal("GetOperation reconstructed current state", err)
	}
	latest := nha203IntegrationResult(t, updated.Msg)
	deleted, err := client.DeleteIntegrationInstance(f.ctx, nha056SessionRequest(&api.DeleteIntegrationInstanceRequest{Name: latest.Name, Etag: latest.Etag}, f.token))
	if err != nil {
		t.Fatal("delete", err)
	}
	deletedName, err := apitypes.ParseOperationName(deleted.Msg.Name)
	if err != nil || contracts.AdmitIntegrationOperation(deleted.Msg, deletedName) != nil || !deleted.Msg.Done || deleted.Msg.GetResponse().TypeUrl != "type.googleapis.com/google.protobuf.Empty" {
		t.Fatal("delete did not publish exact Empty result", err)
	}
	if _, err := client.GetIntegrationInstance(f.ctx, nha056SessionRequest(&api.GetIntegrationInstanceRequest{Name: latest.Name}, f.token)); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatal("deleted declaration remained readable", err)
	}
	stableSession := f.tokens.Session.Id.Value
	if err := f.run.join(t); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	refreshed, err := f.authentication.RefreshSession(f.ctx, connect.NewRequest(&auth.RefreshSessionRequest{RefreshToken: f.tokens.RefreshToken}))
	if err != nil || refreshed.Msg.Tokens.Session.Id.Value != stableSession {
		t.Fatal("restart refresh replaced stable native session", err)
	}
	f.tokens, f.token = refreshed.Msg.Tokens, refreshed.Msg.Tokens.AccessToken
	operations = longrunningpbconnect.NewOperationsClient(nha162RunClient(t, f.roots, f.run.address), "https://"+f.run.address.String())
	for _, originalOperation := range []*longrunningpb.Operation{noChange[0], deleted.Msg} {
		stored, err := operations.GetOperation(f.ctx, nha056SessionRequest(&longrunningpb.GetOperationRequest{Name: originalOperation.Name}, f.token))
		if err != nil || !proto.Equal(stored.Msg, originalOperation) {
			t.Fatal("restart/refresh lost original credential-bound terminal", err)
		}
	}
	other := nha162RunLogin(t, f.ctx, f.authentication, "NHA148 admin", "NHA148 synthetic isolated password")
	if other.Session.Id.Value == stableSession {
		t.Fatal("new login reused original session")
	}
	if _, err := operations.GetOperation(f.ctx, nha056SessionRequest(&longrunningpb.GetOperationRequest{Name: noChange[0].Name}, other.AccessToken)); err == nil {
		t.Fatal("new session read another session's receipt")
	}
}
