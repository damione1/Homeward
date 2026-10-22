package contracts

import (
	wire "github.com/damione1/Homeward/gen/home/configuration/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func configurationTime(t *timestamppb.Timestamp) bool {
	return t != nil && t.CheckValid() == nil && !t.AsTime().IsZero()
}
func configurationDirty(d wire.DirtyState) bool {
	switch d {
	case wire.DirtyState_DIRTY_STATE_CLEAN, wire.DirtyState_DIRTY_STATE_UNVALIDATED, wire.DirtyState_DIRTY_STATE_REJECTED, wire.DirtyState_DIRTY_STATE_RECONCILING, wire.DirtyState_DIRTY_STATE_UNAVAILABLE:
		return true
	case wire.DirtyState_DIRTY_STATE_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func configurationSchema(kind wire.DocumentKind, schema, profile string) bool {
	switch kind {
	case wire.DocumentKind_DOCUMENT_KIND_YAML_SOURCE:
		return schema == "ha-declarative-1" && profile == "ha-2026.10/declarative-1" || schema == "ha-declarative-2" && profile == "ha-2026.10/declarative-2"
	case wire.DocumentKind_DOCUMENT_KIND_AUTHORIZATION_POLICY:
		return schema == "homeward.authorization/1" && profile == "homeward.authorization-yaml/1"
	case wire.DocumentKind_DOCUMENT_KIND_MFA_POLICY:
		return schema == "homeward.mfa-policy/1" && profile == "homeward.mfa-policy-yaml/1"
	case wire.DocumentKind_DOCUMENT_KIND_OPERATIONAL_YAML:
		return schema == "homeward.operational/1" && profile == "homeward.operational-yaml/1"
	case wire.DocumentKind_DOCUMENT_KIND_BOOTSTRAP_LAUNCH:
		return schema == "homeward.bootstrap-launch/1" && profile == "homeward.bootstrap-launch-yaml/1"
	case wire.DocumentKind_DOCUMENT_KIND_DASHBOARD_SOURCE:
		return schema == "homeward-dashboard-1" && profile == "ha-2026.10/lovelace-1"
	case wire.DocumentKind_DOCUMENT_KIND_AUTOMATION_SOURCE, wire.DocumentKind_DOCUMENT_KIND_SCRIPT_SOURCE:
		return schema == "ha-logic-1" && profile == "ha-2026.10/logic-1"
	case wire.DocumentKind_DOCUMENT_KIND_SCENE_SOURCE:
		return schema == "ha-scene-1" && profile == "ha-2026.10/scene-1"
	case wire.DocumentKind_DOCUMENT_KIND_OWNERSHIP_SOURCE:
		return schema == "homeward.heating-ownership/1" && profile == "homeward.heating-ownership-json/1"
	case wire.DocumentKind_DOCUMENT_KIND_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func configurationObservableDocument(document *wire.Document) bool {
	if document == nil || document.Id == nil {
		return false
	}
	switch document.Kind {
	case wire.DocumentKind_DOCUMENT_KIND_OPERATIONAL_YAML:
		return document.Id.Value == "operational.main"
	case wire.DocumentKind_DOCUMENT_KIND_YAML_SOURCE, wire.DocumentKind_DOCUMENT_KIND_AUTHORIZATION_POLICY,
		wire.DocumentKind_DOCUMENT_KIND_DASHBOARD_SOURCE, wire.DocumentKind_DOCUMENT_KIND_AUTOMATION_SOURCE,
		wire.DocumentKind_DOCUMENT_KIND_SCRIPT_SOURCE, wire.DocumentKind_DOCUMENT_KIND_SCENE_SOURCE,
		wire.DocumentKind_DOCUMENT_KIND_OWNERSHIP_SOURCE:
		return document.Id.Value != "operational.main"
	case wire.DocumentKind_DOCUMENT_KIND_MFA_POLICY, wire.DocumentKind_DOCUMENT_KIND_BOOTSTRAP_LAUNCH, wire.DocumentKind_DOCUMENT_KIND_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func configurationDocument(d *wire.Document) bool {
	if d == nil || !configurationID(d.Id) || !configurationVersion(d.Version) || !configurationSchema(d.Kind, d.SchemaVersion, d.SyntaxProfile) || !configurationTime(d.UpdatedAt) || !configurationHash(d.ProtectedSourceReference) {
		return false
	}
	switch state := d.WorkspaceState.(type) {
	case *wire.Document_Workspace:
		if state == nil || state.Workspace == nil {
			return false
		}
		workspace := state.Workspace
		if !configurationObservableDocument(d) || !configurationDirty(workspace.Dirty) || !configurationHash(workspace.SourceSha256) || !configurationTime(workspace.UpdatedAt) {
			return false
		}
		if !configurationWorkspaceDiagnostic(d.Id, workspace) {
			return false
		}
		if workspace.Dirty == wire.DirtyState_DIRTY_STATE_CLEAN && string(d.Version.SourceSha256) != string(workspace.SourceSha256) {
			return false
		}
	case *wire.Document_WorkspaceUnobserved:
		if state == nil || state.WorkspaceUnobserved == nil {
			return false
		}
		switch state.WorkspaceUnobserved.Reason {
		case wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_NO_PHYSICAL_LOCATOR:
			if d.Id.Value == "operational.main" {
				return false
			}
		case wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_SOURCE_UNAVAILABLE:
			if !configurationObservableDocument(d) {
				return false
			}
		case wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_UNSPECIFIED:
			return false
		default:
			return false
		}
	case nil:
		return false
	default:
		return false
	}
	return true
}
func configurationRead(r *wire.ReadDocumentResponse) bool {
	return r != nil && configurationDocument(r.Document) && configurationGeneration(r.ActiveGeneration) && len(r.ResumeToken) == 118
}
func configurationContinuation(c *wire.PageContinuation) bool {
	if c == nil {
		return false
	}
	switch value := c.Result.(type) {
	case *wire.PageContinuation_Complete:
		return value != nil && value.Complete != nil
	case *wire.PageContinuation_NextPageToken:
		return value != nil && len(value.NextPageToken) > 0 && len(value.NextPageToken) <= 1200
	case nil:
		return false
	default:
		return false
	}
}
func configurationActivation(a *wire.ActivationResult) bool {
	if a == nil || a.GenerationRevision != nil && *a.GenerationRevision < 1 || len(a.Diagnostics) > 4 {
		return false
	}
	transaction := false
	switch a.Transaction {
	case wire.TransactionOutcome_TRANSACTION_OUTCOME_NOT_COMMITTED, wire.TransactionOutcome_TRANSACTION_OUTCOME_COMMIT_UNKNOWN, wire.TransactionOutcome_TRANSACTION_OUTCOME_COMMITTED, wire.TransactionOutcome_TRANSACTION_OUTCOME_NO_TRANSACTION:
		transaction = true
	case wire.TransactionOutcome_TRANSACTION_OUTCOME_UNSPECIFIED:
		return false
	default:
		return false
	}
	if !transaction {
		return false
	}
	switch a.Outcome {
	case wire.ActivationOutcome_ACTIVATION_OUTCOME_REJECTED_BEFORE_COMMIT, wire.ActivationOutcome_ACTIVATION_OUTCOME_RESTART_REQUIRED:
		if a.Transaction != wire.TransactionOutcome_TRANSACTION_OUTCOME_NOT_COMMITTED && a.Transaction != wire.TransactionOutcome_TRANSACTION_OUTCOME_NO_TRANSACTION {
			return false
		}
	case wire.ActivationOutcome_ACTIVATION_OUTCOME_NOT_COMMITTED:
		if a.Transaction != wire.TransactionOutcome_TRANSACTION_OUTCOME_NOT_COMMITTED {
			return false
		}
	case wire.ActivationOutcome_ACTIVATION_OUTCOME_COMMIT_UNKNOWN:
		if a.Transaction != wire.TransactionOutcome_TRANSACTION_OUTCOME_COMMIT_UNKNOWN {
			return false
		}
	case wire.ActivationOutcome_ACTIVATION_OUTCOME_COMMITTED_RECONCILING, wire.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED:
		if a.GenerationRevision == nil || a.Transaction == wire.TransactionOutcome_TRANSACTION_OUTCOME_NO_TRANSACTION {
			return false
		}
	case wire.ActivationOutcome_ACTIVATION_OUTCOME_UNSPECIFIED:
		return false
	default:
		return false
	}
	seen := make(map[wire.DiagnosticCode]bool, len(a.Diagnostics))
	for _, code := range a.Diagnostics {
		if seen[code] {
			return false
		}
		seen[code] = true
		switch code {
		case wire.DiagnosticCode_DIAGNOSTIC_CODE_CANDIDATE_REJECTED, wire.DiagnosticCode_DIAGNOSTIC_CODE_WORKSPACE_CHANGED, wire.DiagnosticCode_DIAGNOSTIC_CODE_RECONCILIATION_REQUIRED, wire.DiagnosticCode_DIAGNOSTIC_CODE_ACTIVATION_UNAVAILABLE:
		case wire.DiagnosticCode_DIAGNOSTIC_CODE_UNSPECIFIED:
			return false
		default:
			return false
		}
	}
	return true
}
func configurationWatch(w *wire.WatchConfigurationResponse) bool {
	if w == nil {
		return false
	}
	switch value := w.Result.(type) {
	case *wire.WatchConfigurationResponse_Ready:
		return value != nil && value.Ready != nil && len(value.Ready.ResumeToken) == 118
	case *wire.WatchConfigurationResponse_ResyncRequired:
		if value == nil || value.ResyncRequired == nil {
			return false
		}
		switch value.ResyncRequired.Reason {
		case wire.ResyncReason_RESYNC_REASON_EPOCH_CHANGED, wire.ResyncReason_RESYNC_REASON_TOKEN_INVALID, wire.ResyncReason_RESYNC_REASON_HISTORY_LOST, wire.ResyncReason_RESYNC_REASON_AUTHORITY_CHANGED, wire.ResyncReason_RESYNC_REASON_ACTIVATION_SIGNAL, wire.ResyncReason_RESYNC_REASON_CLOSED:
			return true
		case wire.ResyncReason_RESYNC_REASON_UNSPECIFIED:
			return false
		default:
			return false
		}
	case *wire.WatchConfigurationResponse_Changes:
		if value == nil || value.Changes == nil || len(value.Changes.ResumeToken) != 118 || len(value.Changes.Changes) < 1 || len(value.Changes.Changes) > 256 {
			return false
		}
		for _, change := range value.Changes.Changes {
			if change == nil || !configurationID(change.Id) || !configurationVersion(change.Version) || !configurationDirty(change.Dirty) {
				return false
			}
			if change.ObservedWorkspaceSha256 != nil {
				if !configurationHash(change.ObservedWorkspaceSha256) {
					return false
				}
			} else if change.Dirty != wire.DirtyState_DIRTY_STATE_RECONCILING && change.Dirty != wire.DirtyState_DIRTY_STATE_UNAVAILABLE {
				return false
			}
		}
		return true
	case nil:
		return false
	default:
		return false
	}
}
func AdmitConfigurationResponse(message proto.Message) error {
	if message == nil || !configurationKnown(message.ProtoReflect()) {
		return ErrInvalidConfigurationContract
	}
	valid := false
	switch value := message.(type) {
	case *wire.ReadDocumentResponse:
		valid = configurationRead(value)
	case *wire.ListDocumentsResponse:
		valid = value != nil && len(value.Documents) <= 100 && configurationContinuation(value.Continuation)
		if valid {
			for _, document := range value.Documents {
				if !configurationDocument(document) {
					valid = false
					break
				}
			}
		}
	case *wire.ListVersionsResponse:
		valid = value != nil && len(value.Versions) <= 100 && configurationContinuation(value.Continuation)
		if valid {
			for _, version := range value.Versions {
				if version == nil || !configurationID(version.Id) || !configurationVersion(version.Version) {
					valid = false
					break
				}
				switch version.Availability {
				case wire.VersionAvailability_VERSION_AVAILABILITY_RETAINED:
					valid = configurationTime(version.UpdatedAt) && len(version.SchemaVersion) > 0 && len(version.SchemaVersion) <= 128
				case wire.VersionAvailability_VERSION_AVAILABILITY_PURGED:
					valid = version.UpdatedAt == nil && version.SchemaVersion == ""
				case wire.VersionAvailability_VERSION_AVAILABILITY_UNSPECIFIED:
					valid = false
				default:
					valid = false
				}
				if !valid {
					break
				}
			}
		}
	case *wire.ListGenerationsResponse:
		valid = value != nil && len(value.Generations) <= 100 && configurationContinuation(value.Continuation)
		if valid {
			for _, generation := range value.Generations {
				if generation == nil || !configurationGeneration(generation.Generation) || !configurationTime(generation.CreatedAt) {
					valid = false
					break
				}
				switch generation.Availability {
				case wire.GenerationAvailability_GENERATION_AVAILABILITY_COMPLETE, wire.GenerationAvailability_GENERATION_AVAILABILITY_PARTIALLY_PURGED:
				case wire.GenerationAvailability_GENERATION_AVAILABILITY_UNSPECIFIED:
					valid = false
				default:
					valid = false
				}
				if !valid {
					break
				}
			}
		}
	case *wire.ListSchemasResponse:
		valid = value != nil && len(value.Schemas) <= 100 && configurationContinuation(value.Continuation)
		if valid {
			for _, schema := range value.Schemas {
				if schema == nil || !configurationSchema(schema.Kind, schema.SchemaVersion, schema.SyntaxProfile) || schema.EditableOperationalWorkspace != (schema.Kind == wire.DocumentKind_DOCUMENT_KIND_OPERATIONAL_YAML) {
					valid = false
					break
				}
			}
		}
	case *wire.WriteDocumentResponse:
		valid = value != nil && configurationActivation(value.Activation) && (value.Current == nil || configurationRead(value.Current))
	case *wire.RestoreDocumentResponse:
		valid = value != nil && configurationActivation(value.Activation) && (value.Current == nil || configurationRead(value.Current))
	case *wire.WatchConfigurationResponse:
		valid = configurationWatch(value)
	default:
		return ErrInvalidConfigurationContract
	}
	if !valid {
		return ErrInvalidConfigurationContract
	}
	return nil
}
