//! Configuration references and source ownership. Public metadata never becomes
//! an editable source, and an ambiguous attempt never becomes a retry.
mod sources;
#[cfg(test)]
mod tests;
pub use sources::{Candidate, LogicalPath, SourceFamily, SourceFile, SourceIdentity, UserSource};

use crate::administration::Refusal;
use buffa::EnumValue;
use home_wire_contract::{configuration_contract, home::configuration::v1 as wire};

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
            Self::Clean => "Fichiers validés",
            Self::Unvalidated => "Modifications non validées",
            Self::Rejected => "Modifications refusées",
            Self::Reconciling => "Rapprochement en cours",
            Self::Unavailable => "État des fichiers indisponible",
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
            Self::NoPhysicalLocator => "Non observé : aucun emplacement physique",
            Self::SourceUnavailable => "Non observé : source indisponible",
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
    response: wire::ReadDocumentResponse,
    id: DocumentId,
    sql: SqlVersion,
    active: ActiveGeneration,
    workspace: Workspace,
}
impl Capture {
    pub fn admit(response: wire::ReadDocumentResponse) -> Result<Self, Refusal> {
        configuration_contract::admit_configuration_read(&response)
            .map_err(|_| Refusal::Invalid)?;
        let document = &response.document;
        let id = DocumentId::parse(&document.id.value)?;
        let sql = SqlVersion::admit(&document.version)?;
        let active = ActiveGeneration::admit(&response.active_generation)?;
        let workspace = match &document.workspace_state {
            Some(wire::document::WorkspaceState::Workspace(value)) => {
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
            Some(wire::document::WorkspaceState::WorkspaceUnobserved(value)) => {
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
    pub fn id(&self) -> &DocumentId {
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
    pub fn document(&self) -> &wire::Document {
        &self.response.document
    }
    pub fn resume_token(&self) -> &str {
        &self.response.resume_token
    }
}

/// Captures all three references without equating their revisions or hashes.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Anchor {
    id: DocumentId,
    sql: SqlVersion,
    workspace: WorkspaceHash,
    active: ActiveGeneration,
}
impl Anchor {
    pub fn capture(value: &Capture) -> Result<Self, Refusal> {
        if value.id.as_str() != "operational.main"
            || value.document().kind != wire::DocumentKind::OperationalYaml
        {
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
            Self::RejectedBeforeCommit => "Refusée avant enregistrement",
            Self::NotCommitted => "Non enregistrée",
            Self::CommitUnknown => "Résultat de l’enregistrement inconnu",
            Self::CommittedReconciling => "Enregistrée · publication en cours",
            Self::RestartRequired => "Redémarrage nécessaire",
            Self::Published => "Publiée",
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
    Reported { revision: i64 },
}
pub struct Receipt {
    pub outcome: Outcome,
    pub transaction: Transaction,
    pub diagnostics: Vec<wire::DiagnosticCode>,
    pub target_generation: Option<i64>,
    pub publication: Option<PublicationEvidence>,
    displayed_active: ActiveGeneration,
    current: Option<Capture>,
}
impl Receipt {
    pub fn admit(
        previous: &Capture,
        activation: &wire::ActivationResult,
        current: Option<wire::ReadDocumentResponse>,
    ) -> Result<Self, Refusal> {
        configuration_contract::admit_configuration_activation(activation)
            .map_err(|_| Refusal::Invalid)?;
        let outcome = match activation.outcome {
            EnumValue::Known(wire::ActivationOutcome::RejectedBeforeCommit) => {
                Outcome::RejectedBeforeCommit
            }
            EnumValue::Known(wire::ActivationOutcome::NotCommitted) => Outcome::NotCommitted,
            EnumValue::Known(wire::ActivationOutcome::CommitUnknown) => Outcome::CommitUnknown,
            EnumValue::Known(wire::ActivationOutcome::CommittedReconciling) => {
                Outcome::CommittedReconciling
            }
            EnumValue::Known(wire::ActivationOutcome::RestartRequired) => Outcome::RestartRequired,
            EnumValue::Known(wire::ActivationOutcome::Published) => Outcome::Published,
            EnumValue::Known(wire::ActivationOutcome::Unspecified) | EnumValue::Unknown(_) => {
                return Err(Refusal::Unsupported);
            }
        };
        let transaction = match activation.transaction {
            EnumValue::Known(wire::TransactionOutcome::NotCommitted) => Transaction::NotCommitted,
            EnumValue::Known(wire::TransactionOutcome::CommitUnknown) => Transaction::Unknown,
            EnumValue::Known(wire::TransactionOutcome::Committed) => Transaction::Committed,
            EnumValue::Known(wire::TransactionOutcome::NoTransaction) => Transaction::NoTransaction,
            EnumValue::Known(wire::TransactionOutcome::Unspecified) | EnumValue::Unknown(_) => {
                return Err(Refusal::Unsupported);
            }
        };
        let current = current.map(Capture::admit).transpose()?;
        if current
            .as_ref()
            .is_some_and(|value| value.id != previous.id)
        {
            return Err(Refusal::Invalid);
        }
        let publication = if outcome == Outcome::Published {
            let revision = activation.generation_revision.ok_or(Refusal::Invalid)?;
            if revision < previous.active.revision {
                return Err(Refusal::Invalid);
            }
            if let Some(published) = current.as_ref() {
                if published.active.revision < previous.active.revision
                    || published.active.revision == previous.active.revision
                        && published.active != previous.active
                {
                    return Err(Refusal::Invalid);
                }
                if revision == published.active.revision {
                    Some(PublicationEvidence::Observed(published.active.clone()))
                } else {
                    // Apply and its follow-up Read are separate cuts. A later
                    // publication does not erase this attempt's known result.
                    Some(PublicationEvidence::Reported { revision })
                }
            } else {
                // Published is an authoritative business result even when the
                // following metadata read failed. Never invent its manifest.
                Some(PublicationEvidence::Reported { revision })
            }
        } else {
            None
        };
        let displayed_active = match &publication {
            Some(PublicationEvidence::Observed(value)) => value.clone(),
            Some(PublicationEvidence::Reported { .. }) => current
                .as_ref()
                .map_or_else(|| previous.active.clone(), |value| value.active.clone()),
            None => previous.active.clone(),
        };
        let diagnostics = activation
            .diagnostics
            .iter()
            .map(|value| match value {
                EnumValue::Known(value) => Ok(*value),
                EnumValue::Unknown(_) => Err(Refusal::Unsupported),
            })
            .collect::<Result<Vec<_>, _>>()?;
        Ok(Self {
            outcome,
            transaction,
            diagnostics,
            target_generation: activation.generation_revision,
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
    ) -> Result<wire::WriteDocumentRequest, Refusal> {
        if self.stage != EditStage::Draft {
            return Err(Refusal::Stale);
        }
        if !self.anchor.matches(current) {
            self.stage = EditStage::Conflict;
            return Err(Refusal::Stale);
        }
        let request = wire::WriteDocumentRequest {
            id: self.anchor.id.to_wire().into(),
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
        activation: &wire::ActivationResult,
        current: Option<wire::ReadDocumentResponse>,
    ) -> Result<Receipt, Refusal> {
        if self.stage != EditStage::InFlight {
            return Err(Refusal::Stale);
        }
        match Receipt::admit(&self.previous, activation, current) {
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
