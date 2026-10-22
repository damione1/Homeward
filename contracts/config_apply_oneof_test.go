package contracts

import (
	"errors"
	"testing"

	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	plan "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestNHA055SourcePreconditionsRejectTypedNilOneofsWithoutPanic(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*wire.SourcePrecondition)
	}{
		{"typed-nil-existing", func(v *wire.SourcePrecondition) { v.SqlState = (*wire.SourcePrecondition_Existing)(nil) }},
		{"typed-nil-sql-absent", func(v *wire.SourcePrecondition) { v.SqlState = (*wire.SourcePrecondition_SqlAbsent)(nil) }},
		{"typed-nil-workspace-hash", func(v *wire.SourcePrecondition) { v.WorkspaceState = (*wire.SourcePrecondition_WorkspaceSha256)(nil) }},
		{"typed-nil-workspace-absent", func(v *wire.SourcePrecondition) { v.WorkspaceState = (*wire.SourcePrecondition_WorkspaceAbsent)(nil) }},
		{"nil-existing-value", func(v *wire.SourcePrecondition) { v.SqlState = &wire.SourcePrecondition_Existing{} }},
		{"nil-sql-absent-value", func(v *wire.SourcePrecondition) { v.SqlState = &wire.SourcePrecondition_SqlAbsent{} }},
		{"nil-workspace-absent-value", func(v *wire.SourcePrecondition) { v.WorkspaceState = &wire.SourcePrecondition_WorkspaceAbsent{} }},
		{"nil-workspace-hash-value", func(v *wire.SourcePrecondition) { v.WorkspaceState = &wire.SourcePrecondition_WorkspaceSha256{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := nha055PolicyHistoryRequest()
			test.change(request.Sources[0])
			if err := AdmitConfigurationApplyRequest(request); !errors.Is(err, ErrInvalidConfigurationApplyContract) {
				t.Fatalf("malformed oneof returned %v", err)
			}
		})
	}
	request := nha055PolicyHistoryRequest()
	request.Sources[0].SqlState = &wire.SourcePrecondition_SqlAbsent{SqlAbsent: &emptypb.Empty{}}
	request.Sources[0].WorkspaceState = &wire.SourcePrecondition_WorkspaceSha256{WorkspaceSha256: request.ExpectedWorkspaceSha256}
	if err := AdmitConfigurationApplyRequest(request); err != nil {
		t.Fatal("independent valid SQL absence and workspace presence were rejected", err)
	}
}

func TestNHA055OperationalRollbackRefusesIgnoredSourcePreconditions(t *testing.T) {
	request := nha055PolicyHistoryRequest()
	request.Family = plan.SourceFamily_SOURCE_FAMILY_OPERATIONAL
	request.Identity = "operational.main"
	request.HistoricalPolicyVersion = nil
	request.Historical = &configuration.ActiveGeneration{Revision: 1, ManifestSha256: request.ExpectedWorkspaceSha256}
	if err := AdmitConfigurationApplyRequest(request); !errors.Is(err, ErrInvalidConfigurationApplyContract) {
		t.Fatal("operational rollback admitted source CAS that its writer does not consume", err)
	}
	request.Sources = nil
	if err := AdmitConfigurationApplyRequest(request); err != nil {
		t.Fatal("ordinary operational rollback refused", err)
	}
}
