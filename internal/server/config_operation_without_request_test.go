package server

import (
	"bytes"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/damione1/Homeward/gen/google/longrunning/longrunningpbconnect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"testing"
)

func TestNHA213ConfigurationWithoutRequestIDRetainsOperationAcrossRestart(t *testing.T) {
	f := nha055PublicationStart(t)
	before := f.read(t)
	request := &api.PublishConfigurationDocumentRequest{
		Name: before.Name, Expected: before.Version,
		ExpectedWorkspaceSha256: before.GetWorkspace().SourceSha256,
		ExactYamlSource:         append(bytes.Clone(f.raw), []byte("# synthetic unreserved operation\n")...),
	}
	result, err := f.api.PublishConfigurationDocument(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || result == nil || !result.Msg.Done {
		t.Fatal("native publication without request ID", err)
	}
	metadata := new(api.ConfigurationOperationMetadata)
	if anypb.UnmarshalTo(result.Msg.Metadata, metadata, proto.UnmarshalOptions{}) != nil || metadata.RequestId != "" || metadata.CreateTime == nil || metadata.CommitTime == nil || !proto.Equal(metadata.CreateTime, metadata.CommitTime) {
		t.Fatal("unreserved creation must equal native commit with no invented request ID")
	}
	value := new(api.PublishConfigurationDocumentResponse)
	if anypb.UnmarshalTo(result.Msg.GetResponse(), value, proto.UnmarshalOptions{}) != nil {
		t.Fatal("wrong method envelope")
	}
	if err := f.run.join(t); err != nil {
		t.Fatal(err)
	}
	f.startRun(t)
	f.login(t)
	operations := longrunningpbconnect.NewOperationsClient(f.http, f.origin)
	stored, err := operations.GetOperation(f.ctx, nha056SessionRequest(&longrunningpb.GetOperationRequest{Name: result.Msg.Name}, f.token))
	if err != nil || stored == nil || !proto.Equal(result.Msg, stored.Msg) {
		t.Fatal("restart changed original native operation", err)
	}
}
