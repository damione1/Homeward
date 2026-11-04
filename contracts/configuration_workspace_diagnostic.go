package contracts

import (
	"bytes"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
)

func configurationWorkspaceDiagnostic(document *wire.DocumentId, workspace *wire.WorkspaceObservation) bool {
	diagnostic := workspace.Diagnostic
	if diagnostic == nil {
		return true
	}
	if workspace.Dirty == wire.DirtyState_DIRTY_STATE_CLEAN || !configurationID(diagnostic.Document) || diagnostic.Document.Value != document.Value || !configurationHash(diagnostic.ObservedSourceSha256) || !bytes.Equal(diagnostic.ObservedSourceSha256, workspace.SourceSha256) || !configurationTime(diagnostic.ObservedAt) || !diagnostic.ObservedAt.AsTime().Equal(workspace.UpdatedAt.AsTime()) {
		return false
	}
	switch diagnostic.Code {
	case wire.ConfigurationDiagnosticCode_CONFIGURATION_DIAGNOSTIC_CODE_CANDIDATE_REJECTED:
		return workspace.Dirty == wire.DirtyState_DIRTY_STATE_REJECTED || workspace.Dirty == wire.DirtyState_DIRTY_STATE_RECONCILING
	case wire.ConfigurationDiagnosticCode_CONFIGURATION_DIAGNOSTIC_CODE_WORKSPACE_CHANGED, wire.ConfigurationDiagnosticCode_CONFIGURATION_DIAGNOSTIC_CODE_RECONCILIATION_REQUIRED, wire.ConfigurationDiagnosticCode_CONFIGURATION_DIAGNOSTIC_CODE_ACTIVATION_UNAVAILABLE:
		return true
	case wire.ConfigurationDiagnosticCode_CONFIGURATION_DIAGNOSTIC_CODE_UNSPECIFIED:
		return false
	default:
		return false
	}
}
