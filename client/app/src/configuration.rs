//! Configuration references and source ownership. Public metadata never becomes
//! an editable source, and an ambiguous attempt never becomes a retry.
mod sources;
#[cfg(test)]
mod tests;
pub use sources::{Candidate, LogicalPath, SourceFamily, SourceFile, SourceIdentity, UserSource};

#[cfg(test)]
pub(crate) fn operation_for_test(
    home: &str,
    published: bool,
    revision: i64,
    manifest: u8,
) -> home_wire_contract::google::longrunning::Operation {
    use buffa::Message;
    use buffa_types::google::protobuf::{Any, Timestamp};
    use home_wire_contract::google::longrunning::{self, operation};
    let generation = format!("{home}/configurationGenerations/g-{revision}");
    let metadata = wire::ConfigurationOperationMetadata {
        create_time: Timestamp {
            seconds: 1_700_000_000,
            ..Default::default()
        }
        .into(),
        commit_time: Timestamp {
            seconds: 1_700_000_001,
            ..Default::default()
        }
        .into(),
        generation: generation.clone(),
        generation_revision: revision,
        manifest_sha256: vec![manifest; 32],
        phase: if published {
            wire::ConfigurationOperationPhase::Published
        } else {
            wire::ConfigurationOperationPhase::Committed
        }
        .into(),
        ..Default::default()
    };
    longrunning::Operation {
        name: format!("{home}/operations/config-11111111111111111111111111111111"),
        metadata: Any {
            type_url: "type.googleapis.com/home.api.v2.ConfigurationOperationMetadata".into(),
            value: metadata.encode_to_vec(),
            ..Default::default()
        }
        .into(),
        done: published,
        result: published.then(|| {
            operation::Result::Response(Box::new(Any {
                type_url: "type.googleapis.com/home.api.v2.ConfigurationActivationResult".into(),
                value: wire::ConfigurationActivationResult {
                    generation,
                    generation_revision: revision,
                    manifest_sha256: vec![manifest; 32],
                    ..Default::default()
                }
                .encode_to_vec(),
                ..Default::default()
            }))
        }),
        ..Default::default()
    }
}

#[cfg(test)]
pub(crate) fn publication_for_test(
    home: &str,
    published: bool,
    revision: i64,
    manifest: u8,
) -> home_wire_contract::configuration_operations_contract::Publication {
    home_wire_contract::configuration_operations_contract::admit_response(&operation_for_test(
        home, published, revision, manifest,
    ))
    .unwrap()
}

use crate::administration::Refusal;
use buffa::EnumValue;
use home_wire_contract::{configuration_contract, home::api::v2 as wire};

macro_rules! hash_type {
    ($name:ident) => {
        #[derive(Clone, Debug, PartialEq, Eq)]
        pub struct $name([u8; 32]);
        impl $name {
            pub fn parse(value: &[u8]) -> Result<Self, Refusal> {
                let bytes: [u8; 32] = value.try_into().map_err(|_| Refusal::Invalid)?;
                if bytes.iter().all(|value| *value == 0) {
                    return Err(Refusal::Invalid);
                }
                Ok(Self(bytes))
            }
            pub fn bytes(&self) -> &[u8; 32] {
                &self.0
            }
        }
    };
}
hash_type!(SourceHash);
hash_type!(ManifestHash);
hash_type!(WorkspaceHash);

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct DocumentId(String);
impl DocumentId {
    pub fn parse(value: &str) -> Result<Self, Refusal> {
        if value.trim().is_empty() || value.len() > 255 || value.contains(['\0', '\r', '\n']) {
            return Err(Refusal::Invalid);
        }
        Ok(Self(value.into()))
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
    pub fn to_wire(&self) -> wire::DocumentId {
        wire::DocumentId {
            value: self.0.clone(),
            ..Default::default()
        }
    }
}

// Resource identity is separate from the compiler's native source binding key.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct DocumentName(String);
impl DocumentName {
    pub fn parse(value: &str) -> Result<Self, Refusal> {
        if !configuration_contract::document_name(value) {
            return Err(Refusal::Invalid);
        };
        Ok(Self(value.into()))
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
    pub fn home(&self) -> &str {
        self.0
            .split_once("/configurationDocuments/")
            .expect("admitted document name")
            .0
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SqlVersion {
    revision: i64,
    source: SourceHash,
}
impl SqlVersion {
    pub fn admit(value: &wire::DocumentVersion) -> Result<Self, Refusal> {
        if value.revision <= 0 || !value.__buffa_unknown_fields.is_empty() {
            return Err(Refusal::Invalid);
        }
        Ok(Self {
            revision: value.revision,
            source: SourceHash::parse(&value.source_sha256)?,
        })
    }
    pub fn revision(&self) -> i64 {
        self.revision
    }
    pub fn source(&self) -> &SourceHash {
        &self.source
    }
    pub fn to_wire(&self) -> wire::DocumentVersion {
        wire::DocumentVersion {
            revision: self.revision,
            source_sha256: self.source.0.to_vec(),
            ..Default::default()
        }
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ActiveGeneration {
    revision: i64,
    manifest: ManifestHash,
}
impl ActiveGeneration {
    pub fn admit(value: &wire::ActiveGeneration) -> Result<Self, Refusal> {
        if value.revision <= 0 || !value.__buffa_unknown_fields.is_empty() {
            return Err(Refusal::Invalid);
        }
        Ok(Self {
            revision: value.revision,
            manifest: ManifestHash::parse(&value.manifest_sha256)?,
        })
    }
    pub fn revision(&self) -> i64 {
        self.revision
    }
    pub fn manifest(&self) -> &ManifestHash {
        &self.manifest
    }
    pub fn to_wire(&self) -> wire::ActiveGeneration {
        wire::ActiveGeneration {
            revision: self.revision,
            manifest_sha256: self.manifest.0.to_vec(),
            ..Default::default()
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Dirty {
    Clean,
    Unvalidated,
    Rejected,
    Reconciling,
    Unavailable,
}
impl Dirty {
    pub fn label(self) -> &'static str {
        match self {
            Self::Clean => "Validated Files",
            Self::Unvalidated => "Unvalidated Changes",
            Self::Rejected => "Changes Rejected",
            Self::Reconciling => "Reconciling",
            Self::Unavailable => "File Status Unavailable",
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum UnobservedReason {
    NoPhysicalLocator,
    SourceUnavailable,
}
impl UnobservedReason {
    pub fn label(self) -> &'static str {
        match self {
            Self::NoPhysicalLocator => "Not observed: no physical location",
            Self::SourceUnavailable => "Not observed: source unavailable",
        }
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Workspace {
    Observed { hash: WorkspaceHash, dirty: Dirty },
    Unobserved(UnobservedReason),
}
impl Workspace {
    pub fn writable_hash(&self) -> Option<&WorkspaceHash> {
        match self {
            Self::Observed {
                hash,
                dirty: Dirty::Clean | Dirty::Unvalidated | Dirty::Rejected,
            } => Some(hash),
            Self::Observed {
                dirty: Dirty::Reconciling | Dirty::Unavailable,
                ..
            }
            | Self::Unobserved(_) => None,
        }
    }
}

#[derive(Clone)]
pub struct Capture {
    response: wire::ConfigurationDocument,
    id: DocumentName,
    sql: SqlVersion,
    active: ActiveGeneration,
    workspace: Workspace,
}
impl Capture {
    pub fn admit(response: wire::ConfigurationDocument) -> Result<Self, Refusal> {
        configuration_contract::admit_configuration_read(&response)
            .map_err(|_| Refusal::Invalid)?;
        let document = &response;
        let id = DocumentName::parse(&document.name)?;
        let sql = SqlVersion::admit(&document.version)?;
        let active = ActiveGeneration::admit(&response.active_generation)?;
        let workspace = match &document.workspace_state {
            Some(wire::configuration_document::WorkspaceState::Workspace(value)) => {
                let dirty = match value.dirty {
                    EnumValue::Known(wire::DirtyState::Clean) => Dirty::Clean,
                    EnumValue::Known(wire::DirtyState::Unvalidated) => Dirty::Unvalidated,
                    EnumValue::Known(wire::DirtyState::Rejected) => Dirty::Rejected,
                    EnumValue::Known(wire::DirtyState::Reconciling) => Dirty::Reconciling,
                    EnumValue::Known(wire::DirtyState::Unavailable) => Dirty::Unavailable,
                    EnumValue::Known(wire::DirtyState::Unspecified) | EnumValue::Unknown(_) => {
                        return Err(Refusal::Unsupported);
                    }
                };
                Workspace::Observed {
                    hash: WorkspaceHash::parse(&value.source_sha256)?,
                    dirty,
                }
            }
            Some(wire::configuration_document::WorkspaceState::WorkspaceUnobserved(value)) => {
                Workspace::Unobserved(match value.reason {
                    EnumValue::Known(wire::WorkspaceUnobservedReason::NoPhysicalLocator) => {
                        UnobservedReason::NoPhysicalLocator
                    }
                    EnumValue::Known(wire::WorkspaceUnobservedReason::SourceUnavailable) => {
                        UnobservedReason::SourceUnavailable
                    }
                    EnumValue::Known(wire::WorkspaceUnobservedReason::Unspecified)
                    | EnumValue::Unknown(_) => return Err(Refusal::Unsupported),
                })
            }
            None => return Err(Refusal::Invalid),
        };
        Ok(Self {
            response,
            id,
            sql,
            active,
            workspace,
        })
    }
    pub fn name(&self) -> &str {
        &self.response.name
    }
    pub fn id(&self) -> &DocumentName {
        &self.id
    }
    pub fn sql(&self) -> &SqlVersion {
        &self.sql
    }
    pub fn active(&self) -> &ActiveGeneration {
        &self.active
    }
    pub fn workspace(&self) -> &Workspace {
        &self.workspace
    }
    pub fn document(&self) -> &wire::ConfigurationDocument {
        &self.response
    }
    pub fn resume_token(&self) -> &str {
        &self.response.resume_token
    }
}

/// Captures all three references without equating their revisions or hashes.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Anchor {
    id: DocumentName,
    sql: SqlVersion,
    workspace: WorkspaceHash,
    active: ActiveGeneration,
}
impl Anchor {
    pub fn capture(value: &Capture) -> Result<Self, Refusal> {
        if value.document().kind != wire::DocumentKind::OperationalYaml {
            return Err(Refusal::ReadOnly);
        }
        Ok(Self {
            id: value.id.clone(),
            sql: value.sql.clone(),
            active: value.active.clone(),
            workspace: value
                .workspace
                .writable_hash()
                .ok_or(Refusal::ReadOnly)?
                .clone(),
        })
    }
    pub fn matches(&self, value: &Capture) -> bool {
        Self::capture(value).as_ref() == Ok(self)
    }
    pub fn sql(&self) -> &SqlVersion {
        &self.sql
    }
    pub fn workspace(&self) -> &WorkspaceHash {
        &self.workspace
    }
    pub fn active(&self) -> &ActiveGeneration {
        &self.active
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Outcome {
    RejectedBeforeCommit,
    NotCommitted,
    CommitUnknown,
    CommittedReconciling,
    RestartRequired,
    Published,
}
impl Outcome {
    pub fn label(self) -> &'static str {
        match self {
            Self::RejectedBeforeCommit => "Rejected Before Saving",
            Self::NotCommitted => "Not Saved",
            Self::CommitUnknown => "Save Outcome Unknown",
            Self::CommittedReconciling => "Saved · publishing",
            Self::RestartRequired => "Restart Required",
            Self::Published => "Published",
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Transaction {
    NotCommitted,
    Unknown,
    Committed,
    NoTransaction,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum PublicationEvidence {
    Observed(ActiveGeneration),
    Reported(ActiveGeneration),
}
pub struct Receipt {
    pub operation: home_client_model::api::OperationName,
    pub outcome: Outcome,
    pub transaction: Transaction,
    pub diagnostics: Vec<wire::ConfigurationDiagnosticCode>,
    pub target_generation: Option<i64>,
    pub publication: Option<PublicationEvidence>,
    displayed_active: ActiveGeneration,
    current: Option<Capture>,
}
impl Receipt {
    pub fn admit(
        previous: &Capture,
        admitted: &home_wire_contract::configuration_operations_contract::Publication,
        current: Option<wire::ConfigurationDocument>,
    ) -> Result<Self, Refusal> {
        use home_wire_contract::configuration_operations_contract::Phase;
        if admitted
            .name()
            .as_str()
            .split_once("/operations/")
            .map(|(home, _)| home)
            != Some(previous.id.home())
        {
            return Err(Refusal::Invalid);
        }
        let outcome = match admitted.phase() {
            Phase::Accepted => Outcome::CommitUnknown,
            Phase::Committed => Outcome::CommittedReconciling,
            Phase::Published => Outcome::Published,
            Phase::Failed => {
                if admitted.failure().is_some_and(|failure| {
                    failure.reason() == wire::ApiErrorReason::ConfigRestartRequired
                }) {
                    Outcome::RestartRequired
                } else {
                    Outcome::RejectedBeforeCommit
                }
            }
        };
        let current = current.map(Capture::admit).transpose()?;
        if current.as_ref().is_some_and(|value| {
            value.id != previous.id
                || value.active.revision < previous.active.revision
                || value.active.revision == previous.active.revision
                    && value.active != previous.active
        }) {
            return Err(Refusal::Invalid);
        }
        let publication = if admitted.phase() == Phase::Published {
            let target = admitted.target().ok_or(Refusal::Invalid)?;
            let generation = ActiveGeneration {
                revision: target.revision(),
                manifest: ManifestHash::parse(target.manifest())?,
            };
            if generation.revision < previous.active.revision {
                return Err(Refusal::Invalid);
            }
            match &current {
                Some(current) if current.active == generation => {
                    Some(PublicationEvidence::Observed(generation))
                }
                Some(_) | None => Some(PublicationEvidence::Reported(generation)),
            }
        } else {
            None
        };
        let displayed_active = if publication.is_some() {
            current
                .as_ref()
                .map_or_else(|| previous.active.clone(), |value| value.active.clone())
        } else {
            previous.active.clone()
        };
        Ok(Self {
            operation: admitted.name().clone(),
            outcome,
            transaction: match admitted.phase() {
                Phase::Accepted => Transaction::Unknown,
                Phase::Committed | Phase::Published => Transaction::Committed,
                Phase::Failed => Transaction::NotCommitted,
            },
            diagnostics: Vec::new(),
            target_generation: admitted.target().map(|target| target.revision()),
            publication,
            displayed_active,
            current,
        })
    }
    /// Last fully described generation. A reported publication can have a newer
    /// revision whose manifest still needs an explicit metadata read.
    pub fn displayed_active(&self) -> &ActiveGeneration {
        &self.displayed_active
    }
    /// A returned SQL snapshot is not itself proof of publication.
    pub fn current(&self) -> Option<&Capture> {
        self.current.as_ref()
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum EditStage {
    Draft,
    InFlight,
    Conflict,
    Uncertain,
    Completed(Outcome),
}
pub struct SourceEdit {
    previous: Capture,
    anchor: Anchor,
    source: Option<UserSource>,
    stage: EditStage,
}
impl SourceEdit {
    /// The editor deliberately starts empty: ReadDocument has no source bytes.
    pub fn open(previous: Capture) -> Result<Self, Refusal> {
        let anchor = Anchor::capture(&previous)?;
        Ok(Self {
            previous,
            anchor,
            source: None,
            stage: EditStage::Draft,
        })
    }
    pub fn stage(&self) -> EditStage {
        self.stage
    }
    pub fn source(&self) -> Option<&UserSource> {
        self.source.as_ref()
    }
    pub fn observe(&mut self, current: &Capture) {
        if matches!(
            self.stage,
            EditStage::Draft
                | EditStage::Completed(Outcome::RejectedBeforeCommit | Outcome::NotCommitted)
        ) && !self.anchor.matches(current)
        {
            self.stage = EditStage::Conflict;
        }
    }
    pub fn supply(&mut self, source: UserSource) -> Result<(), Refusal> {
        match self.stage {
            EditStage::Draft
            | EditStage::Completed(Outcome::RejectedBeforeCommit | Outcome::NotCommitted) => {
                self.source = Some(source);
                self.stage = EditStage::Draft;
                Ok(())
            }
            EditStage::InFlight
            | EditStage::Conflict
            | EditStage::Uncertain
            | EditStage::Completed(
                Outcome::CommitUnknown
                | Outcome::CommittedReconciling
                | Outcome::RestartRequired
                | Outcome::Published,
            ) => Err(Refusal::Stale),
        }
    }
    pub fn prepare_write(
        &mut self,
        current: &Capture,
    ) -> Result<wire::PublishConfigurationDocumentRequest, Refusal> {
        if self.stage != EditStage::Draft {
            return Err(Refusal::Stale);
        }
        if !self.anchor.matches(current) {
            self.stage = EditStage::Conflict;
            return Err(Refusal::Stale);
        }
        let request = wire::PublishConfigurationDocumentRequest {
            request_id: home_client_actions::new_request_id()
                .map_err(|_| Refusal::Invalid)?
                .as_str()
                .into(),
            name: self.anchor.id.as_str().into(),
            expected: self.anchor.sql.to_wire().into(),
            expected_workspace_sha256: self.anchor.workspace.0.to_vec(),
            exact_yaml_source: self
                .source
                .as_ref()
                .ok_or(Refusal::Invalid)?
                .bytes()
                .to_vec(),
            ..Default::default()
        };
        configuration_contract::admit_configuration_request(
            configuration_contract::ConfigurationRequest::Write(&request),
        )
        .map_err(|_| Refusal::Invalid)?;
        self.stage = EditStage::InFlight;
        Ok(request)
    }
    pub fn uncertain(&mut self) {
        if self.stage == EditStage::InFlight {
            self.stage = EditStage::Uncertain;
        }
    }
    pub fn restart_required(&mut self) {
        if self.stage == EditStage::InFlight {
            self.stage = EditStage::Completed(Outcome::RestartRequired);
        }
    }
    pub fn rejected_before_commit(&mut self) {
        if self.stage == EditStage::InFlight {
            self.stage = EditStage::Completed(Outcome::RejectedBeforeCommit);
        }
    }
    pub fn conflict_before_commit(&mut self) {
        if self.stage == EditStage::InFlight {
            self.stage = EditStage::Conflict;
        }
    }
    pub fn complete(
        &mut self,
        admitted: &home_wire_contract::configuration_operations_contract::Publication,
        current: Option<wire::ConfigurationDocument>,
    ) -> Result<Receipt, Refusal> {
        if self.stage != EditStage::InFlight {
            return Err(Refusal::Stale);
        }
        match Receipt::admit(&self.previous, admitted, current) {
            Ok(receipt) => {
                self.stage = EditStage::Completed(receipt.outcome);
                Ok(receipt)
            }
            Err(error) => {
                self.stage = EditStage::Uncertain;
                Err(error)
            }
        }
    }
    /// An explicit user resolution creates a fresh attempt; watches never call it.
    pub fn resolve_explicitly(&self, current: Capture) -> Result<Self, Refusal> {
        match self.stage {
            EditStage::Conflict | EditStage::Uncertain | EditStage::Completed(_) => {
                let mut value = Self::open(current)?;
                value.source = self.source.clone();
                Ok(value)
            }
            EditStage::Draft | EditStage::InFlight => Err(Refusal::Stale),
        }
    }
}
