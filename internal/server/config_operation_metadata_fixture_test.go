package server

import (
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"testing"
)

func nha203PublishedMetadata(t *testing.T, operation *longrunningpb.Operation) *api.ConfigurationOperationMetadata {
	t.Helper()
	if operation == nil {
		t.Fatal("missing configuration operation")
	}
	name, err := apitypes.ParseOperationName(operation.Name)
	if err != nil || contracts.AdmitConfigurationOperation(operation, name) != nil || !operation.Done {
		t.Fatal("operation did not establish native publication", err)
	}
	metadata := new(api.ConfigurationOperationMetadata)
	if err := anypb.UnmarshalTo(operation.Metadata, metadata, proto.UnmarshalOptions{}); err != nil {
		t.Fatal(err)
	}
	return metadata
}
