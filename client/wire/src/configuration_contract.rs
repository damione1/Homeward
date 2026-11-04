//! Configuration protocol admission. No transport, UI, credentials or source log.
use crate::home::api::v2 as wire;
use buffa::EnumValue;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConfigurationAdmissionError {
    Invalid,
}

pub enum ConfigurationRequest<'a> {
    Revision(&'a wire::GetConfigurationRevisionRequest),
    Generation(&'a wire::GetConfigurationGenerationRequest),
    Schema(&'a wire::GetConfigurationSchemaRequest),
    Read(&'a wire::GetConfigurationDocumentRequest),
    Documents(&'a wire::ListConfigurationDocumentsRequest),
    Versions(&'a wire::ListConfigurationRevisionsRequest),
    Generations(&'a wire::ListConfigurationGenerationsRequest),
    Schemas(&'a wire::ListConfigurationSchemasRequest),
    Write(&'a wire::PublishConfigurationDocumentRequest),
    Restore(&'a wire::RollbackConfigurationDocumentRequest),
    Watch(&'a wire::WatchConfigurationRequest),
}

pub struct AdmittedConfigurationRequest<'a> {
    wire: ConfigurationRequest<'a>,
}
impl<'a> AdmittedConfigurationRequest<'a> {
    pub fn wire(&self) -> &ConfigurationRequest<'a> {
        &self.wire
    }
}

pub(crate) fn hash(value: &[u8]) -> bool {
    value.len() == 32 && value.iter().any(|byte| *byte != 0)
}
pub(crate) fn id(value: &wire::DocumentId) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && !value.value.trim().is_empty()
        && value.value.len() <= 255
        && !value.value.contains(['\0', '\r', '\n'])
}
pub(crate) fn version(value: &wire::DocumentVersion) -> bool {
    value.__buffa_unknown_fields.is_empty() && value.revision > 0 && hash(&value.source_sha256)
}
pub(crate) fn generation(value: &wire::ActiveGeneration) -> bool {
    value.__buffa_unknown_fields.is_empty() && value.revision > 0 && hash(&value.manifest_sha256)
}
fn page(size: i32, token: &str) -> bool {
    size >= 0 && token.len() <= 1200
}
pub fn home_name(value: &str) -> bool {
    let parts: Vec<_> = value.split('/').collect();
    parts.len() == 2 && parts[0] == "homes" && resource_id(parts[1])
}
fn resource_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 63
        && !value.starts_with('-')
        && !value.ends_with('-')
        && value
            .bytes()
            .all(|v| v.is_ascii_lowercase() || v.is_ascii_digit() || v == b'-')
}
pub(crate) fn resource_name(value: &str, collection: &str) -> bool {
    let segment = format!("/{collection}/");
    let Some((parent, id)) = value.split_once(&segment) else {
        return false;
    };
    home_name(parent) && resource_id(id)
}
pub fn document_name(value: &str) -> bool {
    resource_name(value, "configurationDocuments")
}

pub(crate) fn requested_resource_id(value: &str) -> bool {
    value.is_empty() || (resource_id(value) && value.as_bytes()[0].is_ascii_lowercase())
}
fn canonical_revision(value: &str, prefix: &str) -> Option<i64> {
    let raw = value.strip_prefix(prefix)?;
    let number = raw.parse::<i64>().ok()?;
    (number > 0 && raw == number.to_string()).then_some(number)
}
fn revision_name(value: &str) -> Option<i64> {
    let (parent, id) = value.split_once("/revisions/")?;
    if !document_name(parent) {
        return None;
    };
    canonical_revision(id, "r-")
}
fn generation_name(value: &str) -> Option<i64> {
    let (parent, id) = value.split_once("/configurationGenerations/")?;
    if !home_name(parent) {
        return None;
    };
    canonical_revision(id, "g-")
}

pub fn admit_configuration_request(
    value: ConfigurationRequest<'_>,
) -> Result<AdmittedConfigurationRequest<'_>, ConfigurationAdmissionError> {
    let valid = match &value {
        ConfigurationRequest::Revision(v) => {
            v.__buffa_unknown_fields.is_empty() && revision_name(&v.name).is_some()
        }
        ConfigurationRequest::Generation(v) => {
            v.__buffa_unknown_fields.is_empty() && generation_name(&v.name).is_some()
        }
        ConfigurationRequest::Schema(v) => {
            v.__buffa_unknown_fields.is_empty() && resource_name(&v.name, "configurationSchemas")
        }
        ConfigurationRequest::Read(v) => {
            v.__buffa_unknown_fields.is_empty() && document_name(&v.name)
        }
        ConfigurationRequest::Documents(v) => {
            v.__buffa_unknown_fields.is_empty()
                && page(v.page_size, &v.page_token)
                && home_name(&v.parent)
        }
        ConfigurationRequest::Versions(v) => {
            v.__buffa_unknown_fields.is_empty()
                && document_name(&v.parent)
                && page(v.page_size, &v.page_token)
        }
        ConfigurationRequest::Generations(v) => {
            v.__buffa_unknown_fields.is_empty()
                && page(v.page_size, &v.page_token)
                && home_name(&v.parent)
        }
        ConfigurationRequest::Schemas(v) => {
            v.__buffa_unknown_fields.is_empty()
                && page(v.page_size, &v.page_token)
                && home_name(&v.parent)
        }
        ConfigurationRequest::Write(v) => {
            v.__buffa_unknown_fields.is_empty()
                && (v.request_id.is_empty()
                    || home_client_model::api::RequestId::try_from(v.request_id.as_str()).is_ok())
                && document_name(&v.name)
                && v.expected.as_option().is_some_and(version)
                && hash(&v.expected_workspace_sha256)
                && (1..=1_048_576).contains(&v.exact_yaml_source.len())
        }
        ConfigurationRequest::Restore(v) => {
            v.__buffa_unknown_fields.is_empty()
                && (v.request_id.is_empty()
                    || home_client_model::api::RequestId::try_from(v.request_id.as_str()).is_ok())
                && document_name(&v.name)
                && v.expected.as_option().is_some_and(version)
                && hash(&v.expected_workspace_sha256)
                && v.historical.as_option().is_some_and(generation)
        }
        ConfigurationRequest::Watch(v) => {
            v.__buffa_unknown_fields.is_empty()
                && home_name(&v.parent)
                && v.resume_token.len() == 118
        }
    };
    if valid {
        Ok(AdmittedConfigurationRequest { wire: value })
    } else {
        Err(ConfigurationAdmissionError::Invalid)
    }
}

fn dirty(value: EnumValue<wire::DirtyState>) -> bool {
    match value {
        EnumValue::Known(
            wire::DirtyState::Clean
            | wire::DirtyState::Unvalidated
            | wire::DirtyState::Rejected
            | wire::DirtyState::Reconciling
            | wire::DirtyState::Unavailable,
        ) => true,
        EnumValue::Known(wire::DirtyState::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
pub fn timestamp(value: &buffa_types::google::protobuf::Timestamp) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && (-62_135_596_800..=253_402_300_799).contains(&value.seconds)
        && (0..1_000_000_000).contains(&value.nanos)
        && (value.seconds != -62_135_596_800 || value.nanos != 0)
}
fn workspace_diagnostic(
    document: &wire::ConfigurationDocument,
    workspace: &wire::WorkspaceObservation,
) -> bool {
    let Some(diagnostic) = workspace.diagnostic.as_option() else {
        return true;
    };
    if !diagnostic.__buffa_unknown_fields.is_empty()
        || workspace.dirty == EnumValue::Known(wire::DirtyState::Clean)
        || !diagnostic.document.as_option().is_some_and(id)
        || diagnostic.document.value != document.id.value
        || !hash(&diagnostic.observed_source_sha256)
        || diagnostic.observed_source_sha256 != workspace.source_sha256
        || !diagnostic.observed_at.as_option().is_some_and(timestamp)
        || diagnostic.observed_at != workspace.updated_at
    {
        return false;
    }
    match diagnostic.code {
        EnumValue::Known(wire::ConfigurationDiagnosticCode::CandidateRejected) => matches!(
            workspace.dirty,
            EnumValue::Known(wire::DirtyState::Rejected | wire::DirtyState::Reconciling)
        ),
        EnumValue::Known(
            wire::ConfigurationDiagnosticCode::WorkspaceChanged
            | wire::ConfigurationDiagnosticCode::ReconciliationRequired
            | wire::ConfigurationDiagnosticCode::ActivationUnavailable,
        ) => true,
        EnumValue::Known(wire::ConfigurationDiagnosticCode::Unspecified)
        | EnumValue::Unknown(_) => false,
    }
}

fn schema(value: &wire::ConfigurationDocument) -> bool {
    match value.kind {
        EnumValue::Known(wire::DocumentKind::YamlSource) => {
            value.schema_version == "ha-declarative-1"
                && value.syntax_profile == "ha-2026.10/declarative-1"
                || value.schema_version == "ha-declarative-2"
                    && value.syntax_profile == "ha-2026.10/declarative-2"
        }
        EnumValue::Known(wire::DocumentKind::AuthorizationPolicy) => {
            value.schema_version == "homeward.authorization/1"
                && value.syntax_profile == "homeward.authorization-yaml/1"
        }
        EnumValue::Known(wire::DocumentKind::MfaPolicy) => {
            value.schema_version == "homeward.mfa-policy/1"
                && value.syntax_profile == "homeward.mfa-policy-yaml/1"
        }
        EnumValue::Known(wire::DocumentKind::OperationalYaml) => {
            value.schema_version == "homeward.operational/1"
                && value.syntax_profile == "homeward.operational-yaml/1"
        }
        EnumValue::Known(wire::DocumentKind::BootstrapLaunch) => {
            value.schema_version == "homeward.bootstrap-launch/1"
                && value.syntax_profile == "homeward.bootstrap-launch-yaml/1"
        }
        EnumValue::Known(wire::DocumentKind::DashboardSource) => {
            value.schema_version == "homeward-dashboard-1"
                && value.syntax_profile == "ha-2026.10/lovelace-1"
        }
        EnumValue::Known(
            wire::DocumentKind::AutomationSource | wire::DocumentKind::ScriptSource,
        ) => value.schema_version == "ha-logic-1" && value.syntax_profile == "ha-2026.10/logic-1",
        EnumValue::Known(wire::DocumentKind::SceneSource) => {
            value.schema_version == "ha-scene-1" && value.syntax_profile == "ha-2026.10/scene-1"
        }
        EnumValue::Known(wire::DocumentKind::OwnershipSource) => {
            value.schema_version == "homeward.heating-ownership/1"
                && value.syntax_profile == "homeward.heating-ownership-json/1"
        }
        EnumValue::Known(wire::DocumentKind::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
fn observable_document(document: &wire::ConfigurationDocument) -> bool {
    match document.kind {
        EnumValue::Known(wire::DocumentKind::OperationalYaml) => {
            document.id.value == "operational.main"
        }
        EnumValue::Known(
            wire::DocumentKind::YamlSource
            | wire::DocumentKind::AuthorizationPolicy
            | wire::DocumentKind::DashboardSource
            | wire::DocumentKind::AutomationSource
            | wire::DocumentKind::ScriptSource
            | wire::DocumentKind::SceneSource
            | wire::DocumentKind::OwnershipSource,
        ) => document.id.value != "operational.main",
        EnumValue::Known(
            wire::DocumentKind::MfaPolicy
            | wire::DocumentKind::BootstrapLaunch
            | wire::DocumentKind::Unspecified,
        )
        | EnumValue::Unknown(_) => false,
    }
}
fn admit_document(
    document: &wire::ConfigurationDocument,
) -> Result<(), ConfigurationAdmissionError> {
    if !document_name(&document.name)
        || document.etag.is_empty()
        || document.etag.len() > 256
        || !document.__buffa_unknown_fields.is_empty()
        || !document.id.as_option().is_some_and(id)
        || !document.version.as_option().is_some_and(version)
        || !document.updated_at.as_option().is_some_and(timestamp)
        || !hash(&document.protected_source_reference)
        || !schema(document)
    {
        return Err(ConfigurationAdmissionError::Invalid);
    }
    use wire::configuration_document::WorkspaceState;
    match &document.workspace_state {
        Some(WorkspaceState::Workspace(workspace)) => {
            if !workspace.__buffa_unknown_fields.is_empty()
                || !observable_document(document)
                || !hash(&workspace.source_sha256)
                || !workspace.updated_at.as_option().is_some_and(timestamp)
                || !dirty(workspace.dirty)
                || !workspace_diagnostic(document, workspace)
                || workspace.dirty == EnumValue::Known(wire::DirtyState::Clean)
                    && document.version.source_sha256 != workspace.source_sha256
            {
                return Err(ConfigurationAdmissionError::Invalid);
            }
        }
        Some(WorkspaceState::WorkspaceUnobserved(unobserved)) => {
            if !unobserved.__buffa_unknown_fields.is_empty() {
                return Err(ConfigurationAdmissionError::Invalid);
            }
            let coherent = match unobserved.reason {
                EnumValue::Known(wire::WorkspaceUnobservedReason::NoPhysicalLocator) => {
                    document.id.value != "operational.main"
                }
                EnumValue::Known(wire::WorkspaceUnobservedReason::SourceUnavailable) => {
                    observable_document(document)
                }
                EnumValue::Known(wire::WorkspaceUnobservedReason::Unspecified)
                | EnumValue::Unknown(_) => false,
            };
            if !coherent {
                return Err(ConfigurationAdmissionError::Invalid);
            }
        }
        None => return Err(ConfigurationAdmissionError::Invalid),
    }
    Ok(())
}
pub fn admit_configuration_read(
    value: &wire::ConfigurationDocument,
) -> Result<(), ConfigurationAdmissionError> {
    if !document_name(&value.name)
        || value.etag.is_empty()
        || value.etag.len() > 256
        || !value.__buffa_unknown_fields.is_empty()
        || value.resume_token.len() != 118
        || !value.active_generation.as_option().is_some_and(generation)
    {
        return Err(ConfigurationAdmissionError::Invalid);
    }
    admit_document(value)
}

pub enum ConfigurationResponse<'a> {
    Revision(&'a wire::ConfigurationRevision),
    Generation(&'a wire::ConfigurationGeneration),
    Schema(&'a wire::ConfigurationSchema),
    Read(&'a wire::ConfigurationDocument),
    Documents(&'a wire::ListConfigurationDocumentsResponse),
    Versions(&'a wire::ListConfigurationRevisionsResponse),
    Generations(&'a wire::ListConfigurationGenerationsResponse),
    Schemas(&'a wire::ListConfigurationSchemasResponse),
    Write(&'a crate::google::longrunning::Operation),
    Restore(&'a crate::google::longrunning::Operation),
    Watch(&'a wire::WatchConfigurationResponse),
}

fn admit_revision_resource(value: &wire::ConfigurationRevision) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value.id.as_option().is_some_and(id)
        && value.version.as_option().is_some_and(version)
        && match value.availability {
            EnumValue::Known(wire::VersionAvailability::Retained) => {
                value.update_time.as_option().is_some_and(timestamp)
                    && !value.schema_version.is_empty()
                    && value.schema_version.len() <= 128
            }
            EnumValue::Known(wire::VersionAvailability::Purged) => {
                !value.update_time.is_set() && value.schema_version.is_empty()
            }
            EnumValue::Known(wire::VersionAvailability::Unspecified) | EnumValue::Unknown(_) => {
                false
            }
        }
        && revision_name(&value.name).is_some_and(|number| number == value.version.revision)
}
fn admit_generation_resource(value: &wire::ConfigurationGeneration) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && resource_name(&value.name, "configurationGenerations")
        && value.generation.as_option().is_some_and(generation)
        && value.create_time.as_option().is_some_and(timestamp)
        && match value.availability {
            EnumValue::Known(
                wire::GenerationAvailability::Complete
                | wire::GenerationAvailability::PartiallyPurged,
            ) => true,
            EnumValue::Known(wire::GenerationAvailability::Unspecified) | EnumValue::Unknown(_) => {
                false
            }
        }
        && generation_name(&value.name).is_some_and(|number| number == value.generation.revision)
}
fn admit_schema_resource(value: &wire::ConfigurationSchema) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && resource_name(&value.name, "configurationSchemas")
        && schema(&wire::ConfigurationDocument {
            kind: value.kind,
            schema_version: value.schema_version.clone(),
            syntax_profile: value.syntax_profile.clone(),
            ..Default::default()
        })
        && value.editable_operational_workspace
            == (value.kind == wire::DocumentKind::OperationalYaml)
}

pub fn admit_configuration_response(
    value: ConfigurationResponse<'_>,
) -> Result<(), ConfigurationAdmissionError> {
    let valid = match value {
        ConfigurationResponse::Revision(value) => admit_revision_resource(value),
        ConfigurationResponse::Generation(value) => admit_generation_resource(value),
        ConfigurationResponse::Schema(value) => admit_schema_resource(value),
        ConfigurationResponse::Read(value) => return admit_configuration_read(value),
        ConfigurationResponse::Watch(value) => return admit_configuration_watch(value),
        ConfigurationResponse::Documents(value) => {
            value.__buffa_unknown_fields.is_empty()
                && value.documents.len() <= 100
                && value.next_page_token.len() <= 1200
                && value
                    .documents
                    .iter()
                    .all(|document| admit_document(document).is_ok())
        }
        ConfigurationResponse::Versions(value) => {
            value.__buffa_unknown_fields.is_empty()
                && value.versions.len() <= 100
                && value.next_page_token.len() <= 1200
                && value.versions.iter().all(admit_revision_resource)
        }
        ConfigurationResponse::Generations(value) => {
            value.__buffa_unknown_fields.is_empty()
                && value.generations.len() <= 100
                && value.next_page_token.len() <= 1200
                && value.generations.iter().all(admit_generation_resource)
        }
        ConfigurationResponse::Schemas(value) => {
            value.__buffa_unknown_fields.is_empty()
                && value.schemas.len() <= 100
                && value.next_page_token.len() <= 1200
                && value.schemas.iter().all(admit_schema_resource)
        }
        ConfigurationResponse::Write(value) | ConfigurationResponse::Restore(value) => {
            crate::configuration_operations_contract::admit_response(value).is_ok()
        }
    };
    if valid {
        Ok(())
    } else {
        Err(ConfigurationAdmissionError::Invalid)
    }
}
pub fn admit_configuration_activation(
    value: &wire::ActivationResult,
) -> Result<(), ConfigurationAdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || value
            .generation_revision
            .is_some_and(|revision| revision < 1)
        || value.diagnostics.len() > 4
    {
        return Err(ConfigurationAdmissionError::Invalid);
    }
    let known_transaction = match value.transaction {
        EnumValue::Known(
            wire::TransactionOutcome::NotCommitted
            | wire::TransactionOutcome::CommitUnknown
            | wire::TransactionOutcome::Committed
            | wire::TransactionOutcome::NoTransaction,
        ) => true,
        EnumValue::Known(wire::TransactionOutcome::Unspecified) | EnumValue::Unknown(_) => false,
    };
    let valid = known_transaction
        && match value.outcome {
            EnumValue::Known(
                wire::ActivationOutcome::RejectedBeforeCommit
                | wire::ActivationOutcome::RestartRequired,
            ) => {
                value.transaction == EnumValue::Known(wire::TransactionOutcome::NotCommitted)
                    || value.transaction
                        == EnumValue::Known(wire::TransactionOutcome::NoTransaction)
            }
            EnumValue::Known(wire::ActivationOutcome::NotCommitted) => {
                value.transaction == EnumValue::Known(wire::TransactionOutcome::NotCommitted)
            }
            EnumValue::Known(wire::ActivationOutcome::CommitUnknown) => {
                value.transaction == EnumValue::Known(wire::TransactionOutcome::CommitUnknown)
            }
            EnumValue::Known(
                wire::ActivationOutcome::CommittedReconciling | wire::ActivationOutcome::Published,
            ) => {
                value.generation_revision.is_some()
                    && value.transaction
                        != EnumValue::Known(wire::TransactionOutcome::NoTransaction)
            }
            EnumValue::Known(wire::ActivationOutcome::Unspecified) | EnumValue::Unknown(_) => false,
        };
    if !valid {
        return Err(ConfigurationAdmissionError::Invalid);
    }
    let mut seen = Vec::with_capacity(4);
    for code in &value.diagnostics {
        if seen.contains(code) {
            return Err(ConfigurationAdmissionError::Invalid);
        }
        let known = match code {
            EnumValue::Known(
                wire::ConfigurationDiagnosticCode::CandidateRejected
                | wire::ConfigurationDiagnosticCode::WorkspaceChanged
                | wire::ConfigurationDiagnosticCode::ReconciliationRequired
                | wire::ConfigurationDiagnosticCode::ActivationUnavailable,
            ) => true,
            EnumValue::Known(wire::ConfigurationDiagnosticCode::Unspecified)
            | EnumValue::Unknown(_) => false,
        };
        if !known {
            return Err(ConfigurationAdmissionError::Invalid);
        }
        seen.push(*code);
    }
    Ok(())
}
pub fn admit_configuration_watch(
    value: &wire::WatchConfigurationResponse,
) -> Result<(), ConfigurationAdmissionError> {
    use wire::watch_configuration_response::Result as WatchResult;
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(ConfigurationAdmissionError::Invalid);
    }
    let valid = match &value.result {
        Some(WatchResult::Ready(ready)) => {
            ready.__buffa_unknown_fields.is_empty() && ready.resume_token.len() == 118
        }
        Some(WatchResult::ResyncRequired(resync)) => {
            resync.__buffa_unknown_fields.is_empty()
                && match resync.reason {
                    EnumValue::Known(
                        wire::ResyncReason::EpochChanged
                        | wire::ResyncReason::TokenInvalid
                        | wire::ResyncReason::HistoryLost
                        | wire::ResyncReason::AuthorityChanged
                        | wire::ResyncReason::ActivationSignal
                        | wire::ResyncReason::Closed,
                    ) => true,
                    EnumValue::Known(wire::ResyncReason::Unspecified) | EnumValue::Unknown(_) => {
                        false
                    }
                }
        }
        Some(WatchResult::Changes(batch)) => {
            batch.__buffa_unknown_fields.is_empty()
                && batch.resume_token.len() == 118
                && (1..=256).contains(&batch.changes.len())
                && batch.changes.iter().all(|change| {
                    change.__buffa_unknown_fields.is_empty()
                        && document_name(&change.name)
                        && change.version.as_option().is_some_and(version)
                        && dirty(change.dirty)
                        && match &change.observed_workspace_sha256 {
                            Some(observed) => hash(observed),
                            None => {
                                change.dirty == EnumValue::Known(wire::DirtyState::Reconciling)
                                    || change.dirty
                                        == EnumValue::Known(wire::DirtyState::Unavailable)
                            }
                        }
                })
        }
        None => false,
    };
    if valid {
        Ok(())
    } else {
        Err(ConfigurationAdmissionError::Invalid)
    }
}
