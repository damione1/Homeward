package server

import (
	"errors"
	"testing"

	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	planwire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/internal/configplans"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestNHA055NativeJSONPreconditionBelongsOnlyToOwnership(t *testing.T) {
	input := &wire.SourcePrecondition{Id: &configuration.DocumentId{Value: "ownership.heating"}, LogicalPath: "ownership.json",
		SqlState:       &wire.SourcePrecondition_SqlAbsent{SqlAbsent: &emptypb.Empty{}},
		WorkspaceState: &wire.SourcePrecondition_WorkspaceAbsent{WorkspaceAbsent: &emptypb.Empty{}}}
	for _, family := range []planwire.SourceFamily{planwire.SourceFamily_SOURCE_FAMILY_OWNERSHIP, planwire.SourceFamily_SOURCE_FAMILY_DASHBOARD, planwire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION, planwire.SourceFamily_SOURCE_FAMILY_AUTOMATION} {
		values, err := configApplySourceExpected([]*wire.SourcePrecondition{input}, family)
		if family == planwire.SourceFamily_SOURCE_FAMILY_OWNERSHIP {
			if err != nil || len(values) != 1 || values[0].Binding().Path() != "ownership.json" {
				t.Fatalf("native ownership binding rejected: %v", err)
			}
		} else if !errors.Is(err, configplans.ErrInvalid) || values != nil {
			t.Fatalf("native ownership JSON entered family %v: %v", family, err)
		}
	}
}
