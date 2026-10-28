//! CAS expectations and retained selectors are separate from diagnostic plans.
use super::{Editor, FileKey, Operation, render, wire as plan};
use crate::{
    ProAdminChoice, ProAdminField, ProAdminFieldKind,
    configuration::{
        ActiveGeneration, Anchor, Capture, Dirty, DocumentId, EditStage, LogicalPath, Outcome,
        Receipt, SourceFamily, SqlVersion, UnobservedReason, WorkspaceHash,
    },
};
use home_wire_contract::{
    configuration_apply_contract::{self, ActivationRequest},
    home::{configuration::v1 as document, configurationapply::v1 as wire},
};
use std::collections::BTreeSet;

#[derive(Clone)]
enum Observation<T> {
    Unobserved,
    Captured(T),
    Stale(T),
}
impl<T> Observation<T> {
    fn invalidate(&mut self) {
        let old = std::mem::replace(self, Self::Unobserved);
        *self = match old {
            Self::Unobserved => Self::Unobserved,
            Self::Captured(value) | Self::Stale(value) => Self::Stale(value),
        };
    }
}
#[derive(Clone)]
enum Sql {
    Existing(SqlVersion),
    ExpectedAbsent,
}
#[derive(Clone)]
enum Workspace {
    Observed { hash: WorkspaceHash, dirty: Dirty },
    Unobserved(UnobservedReason),
    ExpectedAbsent,
}
pub(super) struct SourceReference {
    id: String,
    sql: Observation<Sql>,
    workspace: Observation<Workspace>,
}
impl Default for SourceReference {
    fn default() -> Self {
        Self {
            id: String::new(),
            sql: Observation::Unobserved,
            workspace: Observation::Unobserved,
        }
    }
}
impl SourceReference {
    pub fn clear(&mut self) {
        self.sql = Observation::Unobserved;
        self.workspace = Observation::Unobserved;
    }
    pub fn stale(&mut self) {
        self.sql.invalidate();
        self.workspace.invalidate();
    }
    fn to_wire(&self, path: &str) -> Option<wire::SourcePrecondition> {
        let id = DocumentId::parse(&self.id).ok()?;
        LogicalPath::parse(path).ok()?;
        let sql = match &self.sql {
            Observation::Captured(Sql::Existing(value)) => {
                wire::source_precondition::SqlState::Existing(Box::new(value.to_wire()))
            }
            Observation::Captured(Sql::ExpectedAbsent) => {
                wire::source_precondition::SqlState::SqlAbsent(Box::default())
            }
            Observation::Unobserved | Observation::Stale(_) => return None,
        };
        let workspace = match &self.workspace {
            Observation::Captured(Workspace::Observed {
                hash,
                dirty: Dirty::Clean | Dirty::Unvalidated | Dirty::Rejected,
            }) => wire::source_precondition::WorkspaceState::WorkspaceSha256(hash.bytes().to_vec()),
            Observation::Captured(
                Workspace::Observed {
                    dirty: Dirty::Reconciling | Dirty::Unavailable,
                    ..
                }
                | Workspace::Unobserved(_),
            ) => return None,
            Observation::Captured(Workspace::ExpectedAbsent) => {
                wire::source_precondition::WorkspaceState::WorkspaceAbsent(Box::default())
            }
            Observation::Unobserved | Observation::Stale(_) => return None,
        };
        Some(wire::SourcePrecondition {
            id: id.to_wire().into(),
            logical_path: path.into(),
            sql_state: Some(sql),
            workspace_state: Some(workspace),
            ..Default::default()
        })
    }
}
pub(in crate::administration_host::configuration) struct ReferenceRead {
    pub key: FileKey,
    pub id: DocumentId,
    pub path: LogicalPath,
}
pub(in crate::administration_host::configuration) enum HistoryQuery {
    Initial { policy: Option<DocumentId> },
    Generations(String),
    Policy { id: DocumentId, page: String },
}
pub(in crate::administration_host::configuration) struct HistoryReply {
    pub generations: Option<document::ListGenerationsResponse>,
    pub versions: Option<(DocumentId, document::ListVersionsResponse)>,
}
#[derive(Clone)]
enum Historical {
    Generation(ActiveGeneration),
    Policy { id: DocumentId, version: SqlVersion },
}
#[derive(Clone, Copy)]
#[repr(i32)]
enum HistoryPage {
    Generations = 1,
    Policy = 2,
}
impl HistoryPage {
    fn admit(value: i32) -> Option<Self> {
        match value {
            1 => Some(Self::Generations),
            2 => Some(Self::Policy),
            _ => None,
        }
    }
}
#[derive(Clone, Copy)]
#[repr(i32)]
enum ExpectationChoice {
    Unobserved = 1,
    ExpectedAbsent = 2,
    Observed = 3,
}
impl ExpectationChoice {
    fn admit(value: i32) -> Option<Self> {
        match value {
            1 => Some(Self::Unobserved),
            2 => Some(Self::ExpectedAbsent),
            3 => Some(Self::Observed),
            _ => None,
        }
    }
}
struct History {
    generations: Vec<document::HistoricalGeneration>,
    versions: Vec<document::HistoricalVersion>,
    policy: Option<DocumentId>,
    generation_next: Option<String>,
    policy_next: Option<String>,
    page: HistoryPage,
    selected: Option<Historical>,
}
impl Default for History {
    fn default() -> Self {
        Self {
            generations: Vec::new(),
            versions: Vec::new(),
            policy: None,
            generation_next: None,
            policy_next: None,
            page: HistoryPage::Generations,
            selected: None,
        }
    }
}
pub(super) struct Activation {
    stage: EditStage,
    ready: bool,
    receipt: Option<Receipt>,
    history: History,
}
impl Default for Activation {
    fn default() -> Self {
        Self {
            stage: EditStage::Draft,
            ready: true,
            receipt: None,
            history: History::default(),
        }
    }
}
#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(i32)]
pub(in crate::administration_host::configuration) enum Field {
    Document = 2100,
    Sql = 2101,
    Workspace = 2102,
    References = 2103,
    Generation = 2104,
    Policy = 2105,
    Page = 2106,
    Outcome = 2107,
    Current = 2108,
}
impl Field {
    fn admit(value: i32) -> Option<Self> {
        match value {
            2100 => Some(Self::Document),
            2101 => Some(Self::Sql),
            2102 => Some(Self::Workspace),
            2103 => Some(Self::References),
            2104 => Some(Self::Generation),
            2105 => Some(Self::Policy),
            2106 => Some(Self::Page),
            2107 => Some(Self::Outcome),
            2108 => Some(Self::Current),
            _ => None,
        }
    }
    fn label(self) -> &'static str {
        match self {
            Self::Document => "Identité serveur du fichier sélectionné",
            Self::Sql => "Précondition SQL du fichier",
            Self::Workspace => "Précondition workspace du fichier",
            Self::References => "Références indépendantes du fichier sélectionné",
            Self::Generation => "Génération historique complète",
            Self::Policy => "Version historique de politique",
            Self::Page => "Historique à paginer",
            Self::Outcome => "Issue de la tentative d’activation",
            Self::Current => "Dernier actif décrit pour cette tentative",
        }
    }
}
impl Editor {
    #[cfg(test)]
    pub fn activation_outcome(&self) -> Option<Outcome> {
        self.activation
            .receipt
            .as_ref()
            .map(|receipt| receipt.outcome)
    }
    pub fn mutable(&self) -> bool {
        matches!(
            self.activation.stage,
            EditStage::Draft
                | EditStage::Completed(Outcome::RejectedBeforeCommit | Outcome::NotCommitted)
        )
    }
    pub fn set_ready(&mut self, ready: bool) {
        self.activation.ready = ready;
    }
    pub fn subscription_syncing(&mut self) {
        self.activation.ready = false;
        for file in &mut self.files {
            file.reference.stale();
        }
    }
    pub(super) fn resolve_activation(&mut self) {
        self.activation = Activation {
            ready: false,
            ..Default::default()
        };
        for file in &mut self.files {
            file.reference.clear();
        }
    }
    pub(super) fn edit_reference(&mut self, token: i32, text: &str) -> bool {
        if Field::admit(token) != Some(Field::Document) {
            return false;
        }
        if text.len() <= 255 {
            let reference = &mut self
                .files
                .iter_mut()
                .find(|file| file.key == self.selected)
                .expect("selected file")
                .reference;
            reference.id = text.into();
            reference.clear();
            self.invalidate();
            self.activation.history = History::default();
        }
        true
    }
    pub(super) fn choose_activation(&mut self, token: i32, option: i32) -> bool {
        let Some(field) = Field::admit(token) else {
            return false;
        };
        match field {
            Field::Sql | Field::Workspace => {
                if !self.mutable() {
                    return true;
                }
                let Some(option) = ExpectationChoice::admit(option) else {
                    return true;
                };
                let reference = &mut self
                    .files
                    .iter_mut()
                    .find(|file| file.key == self.selected)
                    .expect("selected file")
                    .reference;
                match (field, option) {
                    (Field::Sql, ExpectationChoice::Unobserved) => {
                        reference.sql = Observation::Unobserved
                    }
                    (Field::Sql, ExpectationChoice::ExpectedAbsent) => {
                        reference.sql = Observation::Captured(Sql::ExpectedAbsent)
                    }
                    (Field::Workspace, ExpectationChoice::Unobserved) => {
                        reference.workspace = Observation::Unobserved
                    }
                    (Field::Workspace, ExpectationChoice::ExpectedAbsent) => {
                        reference.workspace = Observation::Captured(Workspace::ExpectedAbsent)
                    }
                    (Field::Sql | Field::Workspace, ExpectationChoice::Observed)
                    | (
                        Field::Document
                        | Field::References
                        | Field::Generation
                        | Field::Policy
                        | Field::Page
                        | Field::Outcome
                        | Field::Current,
                        ExpectationChoice::Unobserved
                        | ExpectationChoice::ExpectedAbsent
                        | ExpectationChoice::Observed,
                    ) => return true,
                }
                self.notice = "Absence attendue fournie explicitement ; le serveur vérifiera cette précondition avant toute écriture.";
            }
            Field::Generation => {
                let Some(value) = choice(&self.activation.history.generations, option) else {
                    return true;
                };
                if value.availability == document::GenerationAvailability::Complete
                    && let Ok(value) = ActiveGeneration::admit(&value.generation)
                {
                    self.activation.history.selected = Some(Historical::Generation(value));
                }
            }
            Field::Policy => {
                let Some(value) = choice(&self.activation.history.versions, option) else {
                    return true;
                };
                if value.availability == document::VersionAvailability::Retained
                    && let (Some(id), Ok(version)) = (
                        self.activation.history.policy.clone(),
                        SqlVersion::admit(&value.version),
                    )
                {
                    self.activation.history.selected = Some(Historical::Policy { id, version });
                }
            }
            Field::Page => {
                if let Some(page) = HistoryPage::admit(option) {
                    self.activation.history.page = page;
                }
            }
            Field::Document | Field::References | Field::Outcome | Field::Current => (),
        }
        true
    }
    pub(super) fn source_read(&mut self) -> Option<Operation> {
        let file = self.selected();
        let (Ok(id), Ok(path)) = (
            DocumentId::parse(&file.reference.id),
            LogicalPath::parse(&file.path),
        ) else {
            self.notice = "Fournissez l’identité serveur et le chemin logique du fichier avant de lire ses références.";
            return None;
        };
        Some(Operation::References(ReferenceRead {
            key: file.key,
            id,
            path,
        }))
    }
    pub fn accept_reference(&mut self, read: ReferenceRead, capture: Capture) {
        let Some(file) = self.files.iter_mut().find(|file| {
            file.key == read.key
                && file.path == read.path.as_str()
                && file.reference.id == read.id.as_str()
        }) else {
            self.references_failed();
            return;
        };
        if capture.id() != &read.id {
            self.references_failed();
            return;
        }
        file.reference.sql = Observation::Captured(Sql::Existing(capture.sql().clone()));
        file.reference.workspace = Observation::Captured(match capture.workspace() {
            crate::configuration::Workspace::Observed { hash, dirty } => Workspace::Observed {
                hash: hash.clone(),
                dirty: *dirty,
            },
            crate::configuration::Workspace::Unobserved(reason) => Workspace::Unobserved(*reason),
        });
        if capture.active() != self.captured.active() {
            self.stale = true;
            self.notice = "Le fichier a été lu dans une autre génération. Résolvez les références globales avant de confirmer.";
        } else {
            self.notice = "Références du fichier relues. Un workspace non observé reste non observé ; aucune absence n’est déduite.";
        }
    }
    pub fn references_failed(&mut self) {
        for file in &mut self.files {
            file.reference.stale();
        }
        self.notice = "Références du fichier indisponibles. Aucune absence ni précondition fraîche n’est déduite de cet échec.";
    }
    pub(super) fn history_read(&mut self) -> Option<Operation> {
        let policy = if self.family == SourceFamily::Authorization {
            let Ok(id) = DocumentId::parse(&self.selected().reference.id) else {
                self.notice = "Fournissez l’identité serveur de la politique dont les versions doivent être lues.";
                return None;
            };
            Some(id)
        } else {
            None
        };
        Some(Operation::History(HistoryQuery::Initial { policy }))
    }
    pub(super) fn history_more(&self) -> Option<Operation> {
        let history = &self.activation.history;
        match history.page {
            HistoryPage::Generations => history
                .generation_next
                .clone()
                .map(|page| Operation::History(HistoryQuery::Generations(page))),
            HistoryPage::Policy => history
                .policy_next
                .clone()
                .zip(history.policy.clone())
                .map(|(page, id)| Operation::History(HistoryQuery::Policy { id, page })),
        }
    }
    pub fn accept_history(&mut self, reply: HistoryReply) -> bool {
        let history = &mut self.activation.history;
        if let Some(response) = reply.generations {
            let mut ids = BTreeSet::new();
            if response
                .generations
                .iter()
                .any(|value| !ids.insert(value.generation.revision))
            {
                self.history_failed();
                return false;
            }
            history.generation_next = next(&response.continuation);
            history.generations = response.generations;
        }
        if let Some((id, response)) = reply.versions {
            let mut revisions = BTreeSet::new();
            if self.family != SourceFamily::Authorization
                || !self
                    .files
                    .iter()
                    .any(|file| file.reference.id == id.as_str())
                || response.versions.iter().any(|value| {
                    value.id.value != id.as_str() || !revisions.insert(value.version.revision)
                })
            {
                self.history_failed();
                return false;
            }
            history.policy = Some(id);
            history.policy_next = next(&response.continuation);
            history.versions = response.versions;
        }
        history.selected = None;
        if history.generation_next.is_none() && history.policy_next.is_some() {
            history.page = HistoryPage::Policy;
        }
        self.notice = "Historique relu. Choisissez explicitement une référence complète ; le retour crée une nouvelle révision.";
        true
    }
    pub fn history_failed(&mut self) {
        self.activation.history = History::default();
        self.notice =
            "Historique indisponible. Aucune sélection ni tentative de rollback n’est effectuée.";
    }
    fn source_preconditions(&self) -> Option<Vec<wire::SourcePrecondition>> {
        if self.family == SourceFamily::Operational {
            return Some(Vec::new());
        }
        let values = self
            .files
            .iter()
            .map(|file| file.reference.to_wire(&file.path))
            .collect::<Option<Vec<_>>>()?;
        let mut ids = BTreeSet::new();
        let mut paths = BTreeSet::new();
        if values.iter().any(|value| {
            !ids.insert(value.id.value.as_str()) || !paths.insert(value.logical_path.as_str())
        }) {
            return None;
        }
        Some(values)
    }
    fn activation_anchor(&self) -> Option<Anchor> {
        if !self.activation.ready || self.stale || !self.mutable() {
            return None;
        }
        Anchor::capture(&self.captured).ok()
    }
    pub(super) fn prepare_apply(&mut self) -> Option<Operation> {
        let (Some(anchor), Some(sources), Ok(candidate)) = (
            self.activation_anchor(),
            self.source_preconditions(),
            self.candidate(),
        ) else {
            self.notice = "Application non envoyée : références globales et par fichier fraîches requises, sans écrasement automatique.";
            return None;
        };
        let Some(plan) = self
            .plan
            .as_ref()
            .filter(|plan| plan.state == super::wire::PlanState::Admitted)
        else {
            self.notice = "Construisez un aperçu admis pour ce candidat inchangé avant de confirmer son application.";
            return None;
        };
        let request = wire::ApplyRequest {
            candidate: candidate.to_wire().into(),
            receipt: Some(plan.receipt.clone()),
            expected: anchor.sql().to_wire().into(),
            expected_workspace_sha256: anchor.workspace().bytes().to_vec(),
            sources,
            ..Default::default()
        };
        configuration_apply_contract::admit_activation_request(ActivationRequest::Apply(&request))
            .ok()?;
        self.activation.stage = EditStage::InFlight;
        self.activation.receipt = None;
        self.notice =
            "Application confirmée en cours ; une seule tentative, aucun rejeu automatique.";
        Some(Operation::Apply(Box::new(request)))
    }
    pub(super) fn prepare_rollback(&mut self) -> Option<Operation> {
        let (Some(anchor), Some(sources), Some(historical)) = (
            self.activation_anchor(),
            self.source_preconditions(),
            self.activation.history.selected.clone(),
        ) else {
            self.notice = "Retour non envoyé : sélection historique complète et préconditions fraîches requises.";
            return None;
        };
        let mut request = wire::RollbackRequest {
            id: self.captured.id().to_wire().into(),
            expected: anchor.sql().to_wire().into(),
            expected_workspace_sha256: anchor.workspace().bytes().to_vec(),
            sources,
            family: self.family.to_wire().into(),
            identity: self.identity.clone(),
            ..Default::default()
        };
        match historical {
            Historical::Generation(value) => request.historical = value.to_wire().into(),
            Historical::Policy { id, version } => {
                if self.family != SourceFamily::Authorization
                    || !self
                        .files
                        .iter()
                        .any(|file| file.reference.id == id.as_str())
                {
                    return None;
                }
                request.historical_policy_version = version.to_wire().into();
            }
        }
        configuration_apply_contract::admit_activation_request(ActivationRequest::Rollback(
            &request,
        ))
        .ok()?;
        self.activation.stage = EditStage::InFlight;
        self.activation.receipt = None;
        self.notice = "Retour historique confirmé en cours ; une nouvelle révision sera créée, jamais un compteur reculé.";
        Some(Operation::Rollback(Box::new(request)))
    }
    pub fn complete_activation(
        &mut self,
        activation: &document::ActivationResult,
        current: Option<document::ReadDocumentResponse>,
    ) {
        if self.activation.stage != EditStage::InFlight {
            return;
        }
        match Receipt::admit(&self.captured, activation, current) {
            Ok(receipt) => {
                self.activation.stage = EditStage::Completed(receipt.outcome);
                self.notice = receipt.outcome.label();
                self.activation.receipt = Some(receipt);
            }
            Err(_) => self.activation_uncertain(),
        }
    }
    pub fn activation_conflict(&mut self) {
        self.activation.stage = EditStage::Conflict;
        self.notice = "Conflit de références avant enregistrement. Relisez et résolvez explicitement ; aucune source n’est écrasée automatiquement.";
    }
    pub fn activation_rejected(&mut self) {
        self.activation.stage = EditStage::Completed(Outcome::RejectedBeforeCommit);
        self.notice =
            "Activation refusée avant enregistrement. Une nouvelle confirmation sera nécessaire.";
    }
    pub fn activation_uncertain(&mut self) {
        self.activation.stage = EditStage::Uncertain;
        self.notice = "Résultat de l’activation inconnu. Relisez sans rejouer, puis résolvez explicitement avant toute nouvelle confirmation.";
    }
    pub(super) fn can_apply(&self) -> bool {
        self.activation_anchor().is_some()
            && self.source_preconditions().is_some()
            && self.input_complete()
            && self
                .plan
                .as_ref()
                .is_some_and(|plan| plan.state == plan::PlanState::Admitted)
    }
    pub(super) fn can_rollback(&self) -> bool {
        self.activation_anchor().is_some()
            && self.source_preconditions().is_some()
            && self.activation.history.selected.is_some()
    }
    pub(super) fn activation_fields(&self, editable: bool) -> Vec<ProAdminField> {
        let reference = &self.selected().reference;
        let mut fields = Vec::new();
        if self.family != SourceFamily::Operational {
            fields.push(field(Field::Document, reference.id.clone(), editable));
            fields.push(field(
                Field::Sql,
                "Précondition SQL à lire ou absence attendue à déclarer explicitement".into(),
                false,
            ));
            fields.push(field(
                Field::Workspace,
                "Précondition workspace à observer ou absence attendue à déclarer explicitement"
                    .into(),
                false,
            ));
            fields.push(field(
                Field::References,
                format!(
                    "SQL : {}\nWorkspace : {}",
                    sql_label(&reference.sql),
                    workspace_label(&reference.workspace)
                ),
                false,
            ));
        }
        if !self.activation.history.generations.is_empty() {
            fields.push(field(Field::Generation,"Une génération purgée n’est pas sélectionnable ; le retour crée une nouvelle révision.".into(),false));
        }
        if !self.activation.history.versions.is_empty() {
            fields.push(field(
                Field::Policy,
                format!(
                    "Politique {} · une seule référence historique sélectionnée",
                    self.activation
                        .history
                        .policy
                        .as_ref()
                        .map_or("non fournie", DocumentId::as_str)
                ),
                false,
            ));
        }
        if self.activation.history.generation_next.is_some()
            || self.activation.history.policy_next.is_some()
        {
            fields.push(field(
                Field::Page,
                "Choisissez l’historique pour la page suivante.".into(),
                false,
            ));
        }
        if let Some(receipt) = &self.activation.receipt {
            fields.push(field(
                Field::Outcome,
                format!(
                    "{}\nGénération annoncée {}\n{} diagnostics",
                    receipt.outcome.label(),
                    receipt
                        .target_generation
                        .map_or_else(|| "non fournie".into(), |value| value.to_string()),
                    receipt.diagnostics.len()
                ),
                false,
            ));
            fields.push(field(
                Field::Current,
                format!(
                    "{} · {}",
                    receipt.displayed_active().revision(),
                    render::hex(receipt.displayed_active().manifest().bytes())
                ),
                false,
            ));
        }
        fields
    }
    pub(super) fn activation_choices(&self, available: bool) -> Vec<ProAdminChoice> {
        let mut values = Vec::new();
        let reference = &self.selected().reference;
        if self.family != SourceFamily::Operational {
            values.extend([
                choice_row(
                    Field::Sql,
                    ExpectationChoice::Unobserved as i32,
                    "Non observé — lire le serveur",
                    matches!(reference.sql, Observation::Unobserved),
                    available,
                ),
                choice_row(
                    Field::Sql,
                    ExpectationChoice::ExpectedAbsent as i32,
                    "Attendre explicitement l’absence SQL",
                    matches!(reference.sql, Observation::Captured(Sql::ExpectedAbsent)),
                    available,
                ),
                choice_row(
                    Field::Sql,
                    ExpectationChoice::Observed as i32,
                    "Version SQL lue sur le serveur",
                    matches!(reference.sql, Observation::Captured(Sql::Existing(_))),
                    false,
                ),
                choice_row(
                    Field::Workspace,
                    ExpectationChoice::Unobserved as i32,
                    "Non observé — lire le serveur",
                    matches!(reference.workspace, Observation::Unobserved),
                    available,
                ),
                choice_row(
                    Field::Workspace,
                    ExpectationChoice::ExpectedAbsent as i32,
                    "Attendre explicitement l’absence du fichier",
                    matches!(
                        reference.workspace,
                        Observation::Captured(Workspace::ExpectedAbsent)
                    ),
                    available,
                ),
                choice_row(
                    Field::Workspace,
                    ExpectationChoice::Observed as i32,
                    "Observation du fichier fournie par le serveur",
                    matches!(
                        reference.workspace,
                        Observation::Captured(
                            Workspace::Observed { .. } | Workspace::Unobserved(_)
                        )
                    ),
                    false,
                ),
            ]);
        }
        for (index, value) in self.activation.history.generations.iter().enumerate() {
            values.push(choice_row(Field::Generation,i32::try_from(index+1).expect("bounded history"),&format!("Génération {} · {}",value.generation.revision,if value.availability==document::GenerationAvailability::Complete {"complète"} else {"purgée en partie"}),matches!(&self.activation.history.selected,Some(Historical::Generation(selected)) if selected.revision()==value.generation.revision),available && value.availability==document::GenerationAvailability::Complete));
        }
        for (index, value) in self.activation.history.versions.iter().enumerate() {
            values.push(choice_row(Field::Policy,i32::try_from(index+1).expect("bounded versions"),&format!("Version {} · {}",value.version.revision,if value.availability==document::VersionAvailability::Retained {"complète"} else {"purgée"}),matches!(&self.activation.history.selected,Some(Historical::Policy {version,..}) if version.revision()==value.version.revision),available && value.availability==document::VersionAvailability::Retained));
        }
        if self.activation.history.generation_next.is_some() {
            values.push(choice_row(
                Field::Page,
                HistoryPage::Generations as i32,
                "Générations",
                matches!(self.activation.history.page, HistoryPage::Generations),
                available,
            ));
        }
        if self.activation.history.policy_next.is_some() {
            values.push(choice_row(
                Field::Page,
                HistoryPage::Policy as i32,
                "Versions de politique",
                matches!(self.activation.history.page, HistoryPage::Policy),
                available,
            ));
        }
        values
    }
}
fn choice<T>(values: &[T], option: i32) -> Option<&T> {
    usize::try_from(option)
        .ok()
        .and_then(|value| value.checked_sub(1))
        .and_then(|index| values.get(index))
}
fn next(value: &document::PageContinuation) -> Option<String> {
    match &value.result {
        Some(document::page_continuation::Result::NextPageToken(value)) => Some(value.clone()),
        Some(document::page_continuation::Result::Complete(_)) | None => None,
    }
}
fn sql_label(value: &Observation<Sql>) -> String {
    let (prefix, value) = match value {
        Observation::Unobserved => return "non observé".into(),
        Observation::Captured(value) => ("", value),
        Observation::Stale(value) => ("ancienne référence à relire · ", value),
    };
    format!(
        "{}{}",
        prefix,
        match value {
            Sql::Existing(value) => format!(
                "version {} · {}",
                value.revision(),
                render::hex(value.source().bytes())
            ),
            Sql::ExpectedAbsent => "absence attendue déclarée par vous, non une observation".into(),
        }
    )
}
fn workspace_label(value: &Observation<Workspace>) -> String {
    let (prefix, value) = match value {
        Observation::Unobserved => return "non observé".into(),
        Observation::Captured(value) => ("", value),
        Observation::Stale(value) => ("ancienne référence à relire · ", value),
    };
    format!(
        "{}{}",
        prefix,
        match value {
            Workspace::Observed { hash, dirty } =>
                format!("{} · {}", dirty.label(), render::hex(hash.bytes())),
            Workspace::Unobserved(reason) => match reason {
                UnobservedReason::NoPhysicalLocator =>
                    "non observé : aucun emplacement physique".into(),
                UnobservedReason::SourceUnavailable => "non observé : source indisponible".into(),
            },
            Workspace::ExpectedAbsent =>
                "absence attendue déclarée par vous, non une observation".into(),
        }
    )
}
fn choice_row(
    field: Field,
    token: i32,
    label: &str,
    selected: bool,
    enabled: bool,
) -> ProAdminChoice {
    ProAdminChoice {
        token,
        field_token: field as i32,
        label: label.into(),
        selected,
        enabled,
    }
}
fn field(field: Field, value: String, editable: bool) -> ProAdminField {
    ProAdminField {token:field as i32,label:field.label().into(),description:"Référence SQL, fichier observé ou attente explicitement fournie ; le serveur vérifie le CAS avant l’écriture".into(),value:value.into(),kind:match field {Field::Document|Field::Sql|Field::Workspace|Field::Generation|Field::Policy|Field::Page=>ProAdminFieldKind::Plain,Field::References|Field::Outcome|Field::Current=>ProAdminFieldKind::Multiline},editable}
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ProAdminAction;
    use buffa_types::google::protobuf::Timestamp;
    fn editor() -> Editor {
        let mut editor = super::super::tests::editor();
        editor.edit(Field::Document as i32, "synthetic.dashboard".into());
        editor.accept(super::super::tests::plan());
        editor.set_ready(true);
        editor
    }
    fn absent(editor: &mut Editor) {
        editor.choose(Field::Sql as i32, ExpectationChoice::ExpectedAbsent as i32);
        editor.choose(
            Field::Workspace as i32,
            ExpectationChoice::ExpectedAbsent as i32,
        );
    }
    fn read(editor: &mut Editor, dirty: Option<document::DirtyState>) {
        let Operation::References(read) = editor.action(ProAdminAction::Refresh).unwrap() else {
            panic!("references")
        };
        let mut response = super::super::super::workflow::tests::response(7, 6);
        let value = response.document.as_option_mut().unwrap();
        value.id = read.id.to_wire().into();
        value.kind = document::DocumentKind::DashboardSource.into();
        value.schema_version = "homeward-dashboard-1".into();
        value.syntax_profile = "ha-2026.10/lovelace-1".into();
        value.workspace_state = Some(match dirty {
            Some(dirty) => document::document::WorkspaceState::Workspace(Box::new(
                document::WorkspaceObservation {
                    source_sha256: vec![
                        if dirty == document::DirtyState::Clean {
                            1
                        } else {
                            2
                        };
                        32
                    ],
                    dirty: dirty.into(),
                    updated_at: Timestamp {
                        seconds: 1_700_000_000,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
            )),
            None => document::document::WorkspaceState::WorkspaceUnobserved(Box::new(
                document::WorkspaceUnobserved {
                    reason: document::WorkspaceUnobservedReason::NoPhysicalLocator.into(),
                    ..Default::default()
                },
            )),
        });
        editor.accept_reference(read, Capture::admit(response).unwrap());
    }
    #[test]
    fn each_absence_is_explicit_and_a_confirmed_apply_seals_exact_preconditions() {
        let mut editor = editor();
        assert!(editor.action(ProAdminAction::Apply).is_none());
        editor.choose(Field::Sql as i32, ExpectationChoice::ExpectedAbsent as i32);
        assert!(editor.action(ProAdminAction::Apply).is_none());
        editor.choose(
            Field::Workspace as i32,
            ExpectationChoice::ExpectedAbsent as i32,
        );
        let Some(Operation::Apply(request)) = editor.action(ProAdminAction::Apply) else {
            panic!("apply")
        };
        assert_eq!(request.expected.revision, 7);
        assert_eq!(request.expected_workspace_sha256, vec![2; 32]);
        assert_eq!(request.sources.len(), 1);
        assert_eq!(request.sources[0].id.value, "synthetic.dashboard");
        assert!(matches!(
            request.sources[0].sql_state,
            Some(wire::source_precondition::SqlState::SqlAbsent(_))
        ));
        assert!(matches!(
            request.sources[0].workspace_state,
            Some(wire::source_precondition::WorkspaceState::WorkspaceAbsent(
                _
            ))
        ));
        assert_eq!(request.candidate.files[0].exact_source, b"views: []\n");
        editor.edit(super::super::Field::Source as i32, "late bytes".into());
        assert_eq!(&*editor.selected().source, "views: []\n");
        assert!(editor.action(ProAdminAction::Apply).is_none());
    }
    #[test]
    fn unknown_or_reconciling_workspaces_are_not_invented_as_absent_or_writable() {
        let mut editor = editor();
        read(&mut editor, None);
        assert!(matches!(
            editor.selected().reference.sql,
            Observation::Captured(Sql::Existing(_))
        ));
        assert!(editor.source_preconditions().is_none());
        editor.choose(
            Field::Workspace as i32,
            ExpectationChoice::ExpectedAbsent as i32,
        );
        assert!(editor.source_preconditions().is_some());
        read(&mut editor, Some(document::DirtyState::Reconciling));
        assert!(editor.source_preconditions().is_none());
        assert!(
            workspace_label(&editor.selected().reference.workspace)
                .contains(Dirty::Reconciling.label())
        );
        read(&mut editor, Some(document::DirtyState::Unvalidated));
        assert!(editor.source_preconditions().is_some());
    }
    #[test]
    fn watch_invalidation_freezes_cas_without_losing_an_owned_publication_reply() {
        let mut editor = editor();
        absent(&mut editor);
        assert!(matches!(
            editor.action(ProAdminAction::Apply),
            Some(Operation::Apply(_))
        ));
        editor.subscription_syncing();
        editor.observe(
            &Capture::admit(super::super::super::workflow::tests::response(9, 9)).unwrap(),
        );
        editor.complete_activation(
            &document::ActivationResult {
                outcome: document::ActivationOutcome::Published.into(),
                transaction: document::TransactionOutcome::Committed.into(),
                generation_revision: Some(8),
                ..Default::default()
            },
            Some(super::super::super::workflow::tests::response(8, 8)),
        );
        assert_eq!(
            editor.activation.stage,
            EditStage::Completed(Outcome::Published)
        );
        assert_eq!(
            editor
                .activation
                .receipt
                .as_ref()
                .unwrap()
                .target_generation,
            Some(8)
        );
        editor.set_ready(true);
        assert!(editor.source_preconditions().is_none());
        assert!(editor.action(ProAdminAction::Apply).is_none());
    }
    #[test]
    fn an_uncertain_apply_requires_explicit_resolution_and_another_confirmation() {
        let mut editor = editor();
        absent(&mut editor);
        editor.action(ProAdminAction::Apply).unwrap();
        editor.activation_uncertain();
        editor.set_ready(true);
        assert!(editor.action(ProAdminAction::Apply).is_none());
        read(&mut editor, Some(document::DirtyState::Clean));
        assert!(editor.action(ProAdminAction::Apply).is_none());
        editor
            .resolve(Capture::admit(super::super::super::workflow::tests::response(8, 8)).unwrap());
        assert!(editor.plan.is_none());
        assert!(editor.source_preconditions().is_none());
        assert_eq!(editor.activation.stage, EditStage::Draft);
        assert!(editor.action(ProAdminAction::Apply).is_none());
    }
    #[test]
    fn a_file_rebinding_cannot_accept_an_old_reference_read() {
        let mut editor = editor();
        let Operation::References(old) = editor.action(ProAdminAction::Refresh).unwrap() else {
            panic!("reference read")
        };
        editor.edit(super::super::Field::Path as i32, "replacement.yaml".into());
        let mut response = super::super::super::workflow::tests::response(7, 6);
        let document = response.document.as_option_mut().unwrap();
        document.id = old.id.to_wire().into();
        document.kind =
            home_wire_contract::home::configuration::v1::DocumentKind::DashboardSource.into();
        document.schema_version = "homeward-dashboard-1".into();
        document.syntax_profile = "ha-2026.10/lovelace-1".into();
        editor.accept_reference(old, Capture::admit(response).unwrap());
        assert!(editor.source_preconditions().is_none());
    }
    #[test]
    fn rollback_selectors_are_exclusive_and_purged_history_never_submits() {
        let mut editor = editor();
        editor.choose(super::super::Field::Family as i32, 4);
        editor.set_ready(true);
        absent(&mut editor);
        let id = DocumentId::parse("synthetic.dashboard").unwrap();
        let reply = HistoryReply {
            generations: Some(document::ListGenerationsResponse {
                generations: vec![
                    document::HistoricalGeneration {
                        generation: document::ActiveGeneration {
                            revision: 5,
                            manifest_sha256: vec![5; 32],
                            ..Default::default()
                        }
                        .into(),
                        availability: document::GenerationAvailability::PartiallyPurged.into(),
                        created_at: Timestamp {
                            seconds: 1_700_000_000,
                            ..Default::default()
                        }
                        .into(),
                        ..Default::default()
                    },
                    document::HistoricalGeneration {
                        generation: document::ActiveGeneration {
                            revision: 4,
                            manifest_sha256: vec![4; 32],
                            ..Default::default()
                        }
                        .into(),
                        availability: document::GenerationAvailability::Complete.into(),
                        created_at: Timestamp {
                            seconds: 1_700_000_000,
                            ..Default::default()
                        }
                        .into(),
                        ..Default::default()
                    },
                ],
                continuation: document::PageContinuation {
                    result: Some(document::page_continuation::Result::Complete(Box::default())),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }),
            versions: Some((
                id.clone(),
                document::ListVersionsResponse {
                    versions: vec![document::HistoricalVersion {
                        id: id.to_wire().into(),
                        version: document::DocumentVersion {
                            revision: 3,
                            source_sha256: vec![3; 32],
                            ..Default::default()
                        }
                        .into(),
                        availability: document::VersionAvailability::Retained.into(),
                        schema_version: "homeward.authorization/1".into(),
                        updated_at: Timestamp {
                            seconds: 1_700_000_000,
                            ..Default::default()
                        }
                        .into(),
                        ..Default::default()
                    }],
                    continuation: document::PageContinuation {
                        result: Some(document::page_continuation::Result::Complete(Box::default())),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
            )),
        };
        assert!(editor.accept_history(reply));
        editor.choose(Field::Generation as i32, 1);
        assert!(editor.action(ProAdminAction::Rollback).is_none());
        editor.choose(Field::Generation as i32, 2);
        assert!(matches!(
            editor.activation.history.selected,
            Some(Historical::Generation(_))
        ));
        editor.choose(Field::Policy as i32, 1);
        let Some(Operation::Rollback(request)) = editor.action(ProAdminAction::Rollback) else {
            panic!("rollback")
        };
        assert!(!request.historical.is_set());
        assert_eq!(request.historical_policy_version.revision, 3);
        assert_eq!(request.expected.revision, 7);
        assert_eq!(request.family, plan::SourceFamily::Authorization);
        assert!(editor.action(ProAdminAction::Rollback).is_none());
    }
}
