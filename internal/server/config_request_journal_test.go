package server

import (
	"bytes"
	"os"
	"sync"
	"testing"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestNHA203ConfigurationRequestConcurrentDuplicateAndConflictTLS(t *testing.T) {
	f := nha149StartTLS(t)
	before := f.read(t)
	request := &api.PublishConfigurationDocumentRequest{
		Name: before.Name, Expected: before.Version,
		ExpectedWorkspaceSha256: before.GetWorkspace().SourceSha256,
		ExactYamlSource:         append(bytes.Clone(f.raw), []byte("# one durable request\n")...),
		RequestId:               "018e363a-8e16-44a4-9aa6-f623ecb0b502",
	}
	start := make(chan struct{})
	var group sync.WaitGroup
	var operations [2]*longrunningpb.Operation
	var failures [2]error
	for i := range operations {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			response, err := f.api.PublishConfigurationDocument(f.ctx, nha056SessionRequest(proto.Clone(request).(*api.PublishConfigurationDocumentRequest), f.token))
			failures[i] = err
			if response != nil {
				operations[i] = response.Msg
			}
		}(i)
	}
	close(start)
	group.Wait()
	for i := range operations {
		if failures[i] != nil || operations[i] == nil {
			t.Fatal("concurrent duplicate lost original operation", failures[i])
		}
	}
	if operations[0].Name != operations[1].Name {
		t.Fatal("duplicate allocated a second operation")
	}
	after := f.read(t)
	if after.ActiveGeneration.Revision != before.ActiveGeneration.Revision+1 {
		t.Fatal("duplicate performed an additional native activation")
	}
	changed := proto.Clone(request).(*api.PublishConfigurationDocumentRequest)
	changed.ExactYamlSource = append(changed.ExactYamlSource, []byte("# different intent\n")...)
	if _, err := f.api.PublishConfigurationDocument(f.ctx, nha056SessionRequest(changed, f.token)); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatal("reused request ID accepted a different normalized intent", err)
	}
	workspace, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(workspace, request.ExactYamlSource) {
		t.Fatal("conflicting duplicate reached workspace staging", err)
	}
}

func TestNHA203ConfigurationRequestDirtyRejectionIsStableTLS(t *testing.T) {
	f := nha149StartTLS(t)
	before := f.read(t)
	request := &api.PublishConfigurationDocumentRequest{
		Name: before.Name, Expected: before.Version,
		ExpectedWorkspaceSha256: before.GetWorkspace().SourceSha256,
		ExactYamlSource:         []byte("external_workers: [\n# rejected dirty request\n"),
		RequestId:               "018e363a-8e16-44a4-9aa6-f623ecb0b503",
	}
	first, err := f.api.PublishConfigurationDocument(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || first == nil || !first.Msg.Done {
		t.Fatal("known precommit rejection was not retained", err)
	}
	failure, err := apierrors.AdmitStatus(first.Msg.GetError())
	if err != nil || failure.Reason() != api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED {
		t.Fatal("dirty rejection did not retain a canonical failure", err)
	}
	metadata := new(api.ConfigurationOperationMetadata)
	if anypb.UnmarshalTo(first.Msg.Metadata, metadata, proto.UnmarshalOptions{}) != nil || metadata.Phase != api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_FAILED || metadata.GenerationRevision != 0 || metadata.CommitTime != nil {
		t.Fatal("rejected workspace claimed a native commit")
	}
	second, err := f.api.PublishConfigurationDocument(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || second == nil || !proto.Equal(first.Msg, second.Msg) {
		t.Fatal("retry changed or replayed a rejected request", err)
	}
	workspace, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(workspace, request.ExactYamlSource) {
		t.Fatal("rejection lost native dirty workspace behavior", err)
	}
	if after := f.read(t); after.ActiveGeneration.Revision != before.ActiveGeneration.Revision {
		t.Fatal("dirty request published an activation")
	}
}
