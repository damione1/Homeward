package contracts

import (
	"bytes"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestNHA204ConfigurationGetNamesAndPurgedRevision(t *testing.T) {
	for _, request := range []proto.Message{&api.GetConfigurationRevisionRequest{Name: "homes/test/configurationDocuments/document/revisions/r-1"}, &api.GetConfigurationGenerationRequest{Name: "homes/test/configurationGenerations/g-1"}, &api.GetConfigurationSchemaRequest{Name: "homes/test/configurationSchemas/s-1-aa"}} {
		if err := AdmitConfigurationRequest(request); err != nil {
			t.Fatal(err)
		}
	}
	for _, request := range []proto.Message{&api.GetConfigurationRevisionRequest{Name: "homes/test/configurationDocuments/document/revisions/r-01"}, &api.GetConfigurationGenerationRequest{Name: "homes/test/configurationGenerations/g-0"}, &api.GetConfigurationSchemaRequest{Name: "homes/test/configurationSchemas/../bad"}} {
		if AdmitConfigurationRequest(request) == nil {
			t.Fatal("noncanonical name admitted")
		}
	}
	revision := &api.ConfigurationRevision{Name: "homes/test/configurationDocuments/document/revisions/r-1", Id: &api.DocumentId{Value: "native-document"}, Version: &api.DocumentVersion{Revision: 1, SourceSha256: bytes.Repeat([]byte{1}, 32)}, Availability: api.VersionAvailability_VERSION_AVAILABILITY_PURGED}
	if err := AdmitConfigurationResponse(revision); err != nil {
		t.Fatal(err)
	}
	revision.SchemaVersion = "invented-after-purge"
	if AdmitConfigurationResponse(revision) == nil {
		t.Fatal("purged revision acquired invented schema")
	}
}
