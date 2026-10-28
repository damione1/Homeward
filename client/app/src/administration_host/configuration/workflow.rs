//! One private editor cohort seals attempts independently of table generations.
use super::{render, wire};
use crate::{
    ProAdminAction, ProAdminActionRow, ProAdminChoice, ProAdminField, ProAdminFieldKind,
    configuration::{
        ActiveGeneration, Anchor, Capture, EditStage, Outcome, Receipt, SourceEdit, UserSource,
    },
};
use buffa::EnumValue;
use home_client_session::{Generation, SessionCoordinator};
use home_client_transport::{Connection, Failure, FailureKind, RpcCode};
use home_wire_contract::configuration_contract::{
    self, ConfigurationRequest, ConfigurationResponse,
};
use std::time::Duration;
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Scope(u64);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Attempt(u64);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(in crate::administration_host) struct Permit {
    scope: Scope,
    attempt: Attempt,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(in crate::administration_host) enum Mode {
    Write,
    Restore,
}
#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(i32)]
enum Field {
    Source = 1000,
    Sql = 1001,
    Active = 1002,
    Workspace = 1003,
    Outcome = 1004,
    CurrentActive = 1005,
    History = 1100,
}
impl Field {
    fn admit(value: i32) -> Option<Self> {
        match value {
            1000 => Some(Self::Source),
            1001 => Some(Self::Sql),
            1002 => Some(Self::Active),
            1003 => Some(Self::Workspace),
            1004 => Some(Self::Outcome),
            1005 => Some(Self::CurrentActive),
            1100 => Some(Self::History),
            _ => None,
        }
    }
    fn label(self) -> &'static str {
        match self {
            Self::Source => "Source exacte fournie",
            Self::Sql => "Version SQL capturée",
            Self::Active => "Génération active capturée",
            Self::Workspace => "Fichiers observés capturés",
            Self::Outcome => "Issue de la tentative",
            Self::CurrentActive => "Génération active relue",
            Self::History => "Génération historique",
        }
    }
}
enum Edit {
    Write(Box<SourceEdit>),
    Restore {
        anchor: Anchor,
        stage: EditStage,
        historical: Option<ActiveGeneration>,
    },
}
struct Draft {
    captured: Capture,
    edit: Edit,
    text: Zeroizing<String>,
    histories: Vec<wire::HistoricalGeneration>,
    next_history: Option<String>,
    receipt: Option<Receipt>,
    notice: &'static str,
}
impl Draft {
    fn stage(&self) -> EditStage {
        match &self.edit {
            Edit::Write(value) => value.stage(),
            Edit::Restore { stage, .. } => *stage,
        }
    }
    fn editable(&self) -> bool {
        matches!(
            self.stage(),
            EditStage::Draft
                | EditStage::Completed(Outcome::RejectedBeforeCommit | Outcome::NotCommitted)
        )
    }
    fn observe(&mut self, current: &Capture) {
        match &mut self.edit {
            Edit::Write(value) => value.observe(current),
            Edit::Restore { anchor, stage, .. } => {
                if matches!(
                    stage,
                    EditStage::Draft
                        | EditStage::Completed(
                            Outcome::RejectedBeforeCommit | Outcome::NotCommitted
                        )
                ) && !anchor.matches(current)
                {
                    *stage = EditStage::Conflict;
                }
            }
        }
        if self.stage() == EditStage::Conflict {
            self.notice = "Les références ont changé. Relisez-les, puis résolvez explicitement avant de confirmer.";
        }
    }
    fn mode(&self) -> Mode {
        match &self.edit {
            Edit::Write(_) => Mode::Write,
            Edit::Restore { .. } => Mode::Restore,
        }
    }
}
enum JobKind {
    Write(wire::WriteDocumentRequest),
    Restore(wire::RestoreDocumentRequest),
    History(Option<String>),
    Read(ReadPurpose),
}
#[derive(Clone, Copy)]
enum ReadPurpose {
    Observe,
    Resolve,
}
pub(in crate::administration_host) struct Job {
    pub permit: Permit,
    kind: JobKind,
    cancel: CancellationToken,
}
enum Reply {
    Mutation {
        activation: wire::ActivationResult,
        current: Option<Box<wire::ReadDocumentResponse>>,
    },
    History(wire::ListGenerationsResponse),
    Read {
        purpose: ReadPurpose,
        capture: Box<Capture>,
    },
}
#[derive(Clone, Copy)]
enum Error {
    Denied,
    Conflict,
    Rejected,
    Uncertain,
    Invalid,
}
#[derive(Clone, Copy, PartialEq, Eq)]
enum Effect {
    Read,
    Mutation,
}
pub(in crate::administration_host) struct Completion {
    pub permit: Permit,
    effect: Effect,
    result: Result<Reply, Error>,
}

pub(in crate::administration_host) struct Workflow {
    watch: super::watch::Cohort,
    scope: Option<Scope>,
    next_attempt: u64,
    cancel: CancellationToken,
    current: Option<Capture>,
    draft: Option<Box<Draft>>,
    pending: Option<Job>,
    active: Option<Permit>,
    synchronized: bool,
}
impl Default for Workflow {
    fn default() -> Self {
        Self {
            watch: super::watch::Cohort::default(),
            scope: Some(Scope(1)),
            next_attempt: 0,
            cancel: CancellationToken::new(),
            current: None,
            draft: None,
            pending: None,
            active: None,
            synchronized: false,
        }
    }
}
impl Workflow {
    pub fn retire(&mut self) {
        self.watch.restart();
        self.cancel.cancel();
        self.cancel = CancellationToken::new();
        self.scope = self
            .scope
            .and_then(|value| value.0.checked_add(1).map(Scope));
        self.current = None;
        self.draft = None;
        self.pending = None;
        self.active = None;
        self.synchronized = false;
    }
    pub fn opened(&self) -> bool {
        self.draft.is_some()
    }
    pub fn observe(&mut self, capture: Capture) {
        if self.snapshot_admission(&capture) != Ok(true) {
            return;
        }
        if let Some(draft) = &mut self.draft {
            draft.observe(&capture);
        }
        self.current = Some(capture);
        self.synchronized = self.watch.live();
    }
    pub fn snapshot_admission(
        &self,
        capture: &Capture,
    ) -> Result<bool, crate::administration::Refusal> {
        let Some(previous) = &self.current else {
            return Ok(true);
        };
        if capture.id() != previous.id()
            || (capture.sql().revision() == previous.sql().revision()
                && capture.sql().source() != previous.sql().source())
            || (capture.active().revision() == previous.active().revision()
                && capture.active().manifest() != previous.active().manifest())
        {
            return Err(crate::administration::Refusal::Invalid);
        }
        Ok(capture.sql().revision() >= previous.sql().revision()
            && capture.active().revision() >= previous.active().revision())
    }
    pub fn take_watch(&mut self) -> Option<super::watch::Job> {
        if self.current.is_none() || self.scope.is_none() {
            return None;
        }
        self.watch.start()
    }
    pub fn watch_admits(&self, permit: super::WatchPermit) -> bool {
        self.watch.admits(permit)
    }
    pub fn watch_syncing(&mut self, permit: super::WatchPermit) -> bool {
        if !self.watch.syncing(permit) {
            return false;
        }
        self.synchronized = false;
        true
    }
    pub fn watch_ready(&mut self, permit: super::WatchPermit) -> bool {
        if !self.watch.ready(permit) {
            return false;
        }
        self.synchronized = true;
        true
    }
    pub fn watch_stopped(&mut self, permit: super::WatchPermit) -> bool {
        if !self.watch.stop(permit) {
            return false;
        }
        self.synchronized = false;
        true
    }
    fn queue(&mut self, kind: JobKind) -> bool {
        if self.active.is_some() || self.pending.is_some() {
            return false;
        }
        let Some(scope) = self.scope else {
            return false;
        };
        let Some(attempt) = self.next_attempt.checked_add(1) else {
            self.retire();
            self.scope = None;
            return false;
        };
        self.next_attempt = attempt;
        let permit = Permit {
            scope,
            attempt: Attempt(attempt),
        };
        self.active = Some(permit);
        self.pending = Some(Job {
            permit,
            kind,
            cancel: self.cancel.child_token(),
        });
        true
    }
    pub fn take_pending(&mut self) -> Option<Job> {
        self.pending.take()
    }
    pub fn actions(&self) -> Vec<ProAdminActionRow> {
        if self.opened() || self.current.is_none() || self.scope.is_none() {
            return Vec::new();
        }
        let enabled = self.synchronized
            && self.active.is_none()
            && self
                .current
                .as_ref()
                .is_some_and(|value| Anchor::capture(value).is_ok());
        vec![
            action(
                ProAdminAction::Save,
                "Fournir la source opérationnelle",
                enabled,
                false,
            ),
            action(
                ProAdminAction::Restore,
                "Restaurer une génération",
                enabled,
                false,
            ),
        ]
    }
    pub fn open(&mut self, action: ProAdminAction) -> bool {
        let mode = match action {
            ProAdminAction::Save => Mode::Write,
            ProAdminAction::Restore => Mode::Restore,
            ProAdminAction::Refresh
            | ProAdminAction::Create
            | ProAdminAction::Remove
            | ProAdminAction::Cancel
            | ProAdminAction::More
            | ProAdminAction::Enable
            | ProAdminAction::Disable
            | ProAdminAction::Revoke
            | ProAdminAction::CloseSecret
            | ProAdminAction::Preview
            | ProAdminAction::Revalidate
            | ProAdminAction::Simulate
            | ProAdminAction::Apply
            | ProAdminAction::Rollback
            | ProAdminAction::AddFile
            | ProAdminAction::RemoveFile
            | ProAdminAction::NextFile
            | ProAdminAction::Resolve => return false,
        };
        if self.opened() || self.active.is_some() || !self.synchronized || self.scope.is_none() {
            return false;
        }
        let Some(captured) = self.current.clone() else {
            return false;
        };
        let Ok(anchor) = Anchor::capture(&captured) else {
            return false;
        };
        let edit = match mode {
            Mode::Write => match SourceEdit::open(captured.clone()) {
                Ok(value) => Edit::Write(Box::new(value)),
                Err(_) => return false,
            },
            Mode::Restore => Edit::Restore {
                anchor,
                stage: EditStage::Draft,
                historical: None,
            },
        };
        self.draft = Some(Box::new(Draft {
            captured,
            edit,
            text: Zeroizing::new(String::new()),
            histories: Vec::new(),
            next_history: None,
            receipt: None,
            notice: match mode {
                Mode::Write => {
                    "Fournissez la source exacte. Les métadonnées protégées ne sont pas une source éditable."
                }
                Mode::Restore => {
                    "Choisissez une génération complète. La restauration crée une nouvelle révision après confirmation."
                }
            },
        }));
        if mode == Mode::Restore {
            self.queue(JobKind::History(None));
        }
        true
    }
    pub fn edit(&mut self, field: i32, text: String) {
        if self.active.is_some() {
            return;
        }
        let Some(draft) = &mut self.draft else {
            return;
        };
        if Field::admit(field) != Some(Field::Source)
            || !draft.editable()
            || draft.mode() != Mode::Write
        {
            return;
        }
        let text = Zeroizing::new(text);
        if text.len() > 1_048_576 {
            draft.notice = "La source dépasse la limite de 1 Mio.";
            return;
        }
        draft.text = text;
        draft.receipt = None;
        draft.notice = "Source fournie explicitement ; rien n’est envoyé avant confirmation.";
    }
    pub fn choose(&mut self, field: i32, option: i32) {
        if self.active.is_some() {
            return;
        }
        let Some(draft) = &mut self.draft else {
            return;
        };
        if Field::admit(field) != Some(Field::History) || !draft.editable() {
            return;
        }
        let Some(value) = usize::try_from(option)
            .ok()
            .and_then(|value| value.checked_sub(1))
            .and_then(|index| draft.histories.get(index))
        else {
            return;
        };
        if value.availability != wire::GenerationAvailability::Complete {
            return;
        }
        let Ok(value) = ActiveGeneration::admit(&value.generation) else {
            return;
        };
        if let Edit::Restore { historical, .. } = &mut draft.edit {
            *historical = Some(value);
            draft.notice = "Génération sélectionnée. La restauration n’est pas encore envoyée.";
        }
    }
    pub fn editor_action(&mut self, value: ProAdminAction) -> bool {
        if !self.opened() {
            return false;
        }
        match value {
            ProAdminAction::Cancel | ProAdminAction::CloseSecret => {
                let current = self.current.clone();
                self.retire();
                if let Some(value) = current {
                    self.observe(value);
                }
                true
            }
            ProAdminAction::Refresh => {
                if self.queue(JobKind::Read(ReadPurpose::Observe)) {
                    self.watch.restart();
                    self.synchronized = false;
                }
                true
            }
            ProAdminAction::Resolve => {
                if self.queue(JobKind::Read(ReadPurpose::Resolve)) {
                    self.watch.restart();
                    self.synchronized = false;
                }
                true
            }
            ProAdminAction::More => {
                let next = self
                    .draft
                    .as_ref()
                    .and_then(|value| value.next_history.clone());
                if let Some(next) = next {
                    self.queue(JobKind::History(Some(next)));
                }
                true
            }
            ProAdminAction::Save => {
                self.submit();
                true
            }
            ProAdminAction::Create
            | ProAdminAction::Remove
            | ProAdminAction::Enable
            | ProAdminAction::Disable
            | ProAdminAction::Revoke
            | ProAdminAction::Preview
            | ProAdminAction::Revalidate
            | ProAdminAction::Simulate
            | ProAdminAction::Apply
            | ProAdminAction::Restore
            | ProAdminAction::Rollback
            | ProAdminAction::AddFile
            | ProAdminAction::RemoveFile
            | ProAdminAction::NextFile => true,
        }
    }
    fn submit(&mut self) {
        if self.active.is_some() || !self.synchronized {
            return;
        }
        let Some(current) = &self.current else {
            return;
        };
        let Some(draft) = &mut self.draft else {
            return;
        };
        if !draft.editable() {
            return;
        }
        let kind = match &mut draft.edit {
            Edit::Write(value) => {
                let Ok(source) = UserSource::supplied(draft.text.as_bytes().to_vec()) else {
                    draft.notice = "Fournissez une source non vide de 1 Mio maximum.";
                    return;
                };
                if value.supply(source).is_err() {
                    return;
                }
                match value.prepare_write(current) {
                    Ok(request) => JobKind::Write(request),
                    Err(_) => {
                        draft.notice =
                            "Les préconditions ont changé. Relisez et résolvez explicitement.";
                        return;
                    }
                }
            }
            Edit::Restore {
                anchor,
                stage,
                historical,
            } => {
                if !anchor.matches(current) {
                    *stage = EditStage::Conflict;
                    draft.notice =
                        "Les préconditions ont changé. Relisez et résolvez explicitement.";
                    return;
                }
                let Some(historical) = historical else {
                    draft.notice = "Sélectionnez une génération complète.";
                    return;
                };
                let request = wire::RestoreDocumentRequest {
                    id: current.id().to_wire().into(),
                    expected: anchor.sql().to_wire().into(),
                    expected_workspace_sha256: anchor.workspace().bytes().to_vec(),
                    historical: historical.to_wire().into(),
                    ..Default::default()
                };
                if configuration_contract::admit_configuration_request(
                    ConfigurationRequest::Restore(&request),
                )
                .is_err()
                {
                    return;
                }
                *stage = EditStage::InFlight;
                JobKind::Restore(request)
            }
        };
        draft.notice = "Tentative en cours ; aucune seconde écriture ne sera envoyée.";
        if !self.queue(kind) {
            self.uncertain();
        }
    }
    fn uncertain(&mut self) {
        if let Some(draft) = &mut self.draft {
            match &mut draft.edit {
                Edit::Write(value) => value.uncertain(),
                Edit::Restore { stage, .. } => *stage = EditStage::Uncertain,
            }
            draft.notice = "Résultat inconnu. Relisez l’état sans renvoyer l’écriture, puis résolvez explicitement.";
        }
    }
    pub fn accept(&mut self, completion: Completion) -> bool {
        if self.active != Some(completion.permit) || self.scope != Some(completion.permit.scope) {
            return false;
        }
        self.active = None;
        match completion.result {
            Ok(Reply::Mutation {
                activation,
                current,
            }) => {
                let current = current.map(|value| *value);
                let Some(draft) = &mut self.draft else {
                    return false;
                };
                let result = match &mut draft.edit {
                    Edit::Write(value) => value.complete(&activation, current.clone()),
                    Edit::Restore { stage, .. } => {
                        match Receipt::admit(&draft.captured, &activation, current.clone()) {
                            Ok(receipt) => {
                                *stage = EditStage::Completed(receipt.outcome);
                                Ok(receipt)
                            }
                            Err(error) => {
                                *stage = EditStage::Uncertain;
                                Err(error)
                            }
                        }
                    }
                };
                match result {
                    Ok(receipt) => {
                        draft.notice = receipt.outcome.label();
                        draft.receipt = Some(receipt);
                    }
                    Err(_) => self.uncertain(),
                }
                if let Some(current) = current.and_then(|value| Capture::admit(value).ok()) {
                    self.observe(current);
                }
            }
            Ok(Reply::History(value)) => {
                if let Some(draft) = &mut self.draft {
                    draft.next_history = match value.continuation.result {
                        Some(wire::page_continuation::Result::NextPageToken(ref value)) => {
                            Some(value.clone())
                        }
                        Some(wire::page_continuation::Result::Complete(_)) | None => None,
                    };
                    draft.histories = value.generations;
                    if let Edit::Restore { historical, .. } = &mut draft.edit {
                        *historical = None;
                    }
                    draft.notice = "Choisissez une génération complète avant de confirmer.";
                }
            }
            Ok(Reply::Read { purpose, capture }) => {
                self.observe(*capture);
                if matches!(purpose, ReadPurpose::Resolve)
                    && let (Some(current), Some(draft)) = (self.current.clone(), &mut self.draft)
                {
                    let fresh = match &draft.edit {
                        Edit::Write(_) => SourceEdit::open(current.clone())
                            .map(|value| Edit::Write(Box::new(value))),
                        Edit::Restore { historical, .. } => {
                            Anchor::capture(&current).map(|anchor| Edit::Restore {
                                anchor,
                                stage: EditStage::Draft,
                                historical: historical.clone(),
                            })
                        }
                    };
                    if let Ok(edit) = fresh {
                        draft.edit = edit;
                        draft.captured = current;
                        draft.receipt = None;
                        draft.notice = "Références relues et résolution explicite. Une nouvelle confirmation reste nécessaire.";
                    }
                }
            }
            Err(Error::Denied) => {
                self.retire();
                return true;
            }
            Err(error) => {
                self.synchronized = false;
                if let Some(draft) = &mut self.draft {
                    if completion.effect == Effect::Read {
                        draft.notice = "Lecture indisponible. Les références et le résultat précédent ne sont pas modifiés.";
                        return false;
                    }
                    match error {
                        Error::Conflict => {
                            match &mut draft.edit {
                                Edit::Write(value) => value.conflict_before_commit(),
                                Edit::Restore { stage, .. } => *stage = EditStage::Conflict,
                            }
                            draft.notice = "Conflit de références. Relisez et résolvez sans écraser automatiquement.";
                        }
                        Error::Rejected => {
                            match &mut draft.edit {
                                Edit::Write(value) => value.rejected_before_commit(),
                                Edit::Restore { stage, .. } => {
                                    *stage = EditStage::Completed(Outcome::RejectedBeforeCommit)
                                }
                            }
                            draft.notice = "Source refusée avant enregistrement. Relisez l’état avant une nouvelle confirmation.";
                        }
                        Error::Uncertain | Error::Invalid => self.uncertain(),
                        Error::Denied => unreachable!(),
                    }
                }
            }
        }
        false
    }
    pub fn title(&self) -> &'static str {
        match self.draft.as_ref().map(|value| value.mode()) {
            Some(Mode::Write) => "Source opérationnelle explicite",
            Some(Mode::Restore) => "Restaurer une génération",
            None => "Configuration",
        }
    }
    pub fn notice(&self) -> &'static str {
        if !self.synchronized
            && self.active.is_none()
            && self
                .draft
                .as_ref()
                .is_some_and(|draft| draft.stage() == EditStage::Draft)
        {
            return "Suivi de configuration non synchronisé. Aucune confirmation n’est admise avant une lecture et un abonnement valides.";
        }
        self.draft.as_ref().map_or("", |value| value.notice)
    }
    pub fn editable(&self) -> bool {
        self.active.is_none() && self.draft.as_ref().is_some_and(|value| value.editable())
    }
    pub fn fields(&self) -> Vec<ProAdminField> {
        let Some(draft) = &self.draft else {
            return Vec::new();
        };
        let mut fields = vec![
            field(
                Field::Sql,
                format!(
                    "{} · {}",
                    draft.captured.sql().revision(),
                    render::hex(draft.captured.sql().source().bytes())
                ),
                false,
            ),
            field(
                Field::Active,
                format!(
                    "{} · {}",
                    draft.captured.active().revision(),
                    render::hex(draft.captured.active().manifest().bytes())
                ),
                false,
            ),
        ];
        if let Some(hash) = draft.captured.workspace().writable_hash() {
            fields.push(field(Field::Workspace, render::hex(hash.bytes()), false));
        }
        if draft.mode() == Mode::Write {
            fields.push(field(
                Field::Source,
                draft.text.to_string(),
                self.editable(),
            ));
        }
        if let Some(receipt) = &draft.receipt {
            fields.push(field(
                Field::Outcome,
                format!(
                    "{} · génération annoncée {}",
                    receipt.outcome.label(),
                    receipt
                        .target_generation
                        .map_or_else(|| "non fournie".to_owned(), |value| value.to_string())
                ),
                false,
            ));
        }
        if let Some(current) = &self.current {
            fields.push(field(
                Field::CurrentActive,
                format!(
                    "{} · {}",
                    current.active().revision(),
                    render::hex(current.active().manifest().bytes())
                ),
                false,
            ));
        }
        fields
    }
    pub fn choices(&self) -> Vec<ProAdminChoice> {
        let Some(draft) = &self.draft else {
            return Vec::new();
        };
        let selected = match &draft.edit {
            Edit::Restore { historical, .. } => historical.as_ref(),
            Edit::Write(_) => None,
        };
        draft
            .histories
            .iter()
            .enumerate()
            .map(|(index, value)| ProAdminChoice {
                token: i32::try_from(index + 1).expect("bounded history"),
                field_token: Field::History as i32,
                label: format!(
                    "Génération {} · {}",
                    value.generation.revision,
                    if value.availability == wire::GenerationAvailability::Complete {
                        "complète"
                    } else {
                        "partiellement purgée"
                    }
                )
                .into(),
                selected: selected
                    .is_some_and(|selected| selected.revision() == value.generation.revision),
                enabled: self.editable()
                    && value.availability == wire::GenerationAvailability::Complete,
            })
            .collect()
    }
    pub fn editor_actions(&self) -> Vec<ProAdminActionRow> {
        let Some(draft) = &self.draft else {
            return Vec::new();
        };
        let available = self.active.is_none();
        let ready = self.synchronized
            && self.editable()
            && match &draft.edit {
                Edit::Write(_) => !draft.text.is_empty(),
                Edit::Restore { historical, .. } => historical.is_some(),
            };
        let mut rows = vec![
            action(
                ProAdminAction::Save,
                match draft.mode() {
                    Mode::Write => "Confirmer l’écriture",
                    Mode::Restore => "Confirmer la restauration",
                },
                ready,
                true,
            ),
            action(
                ProAdminAction::Refresh,
                "Relire l’état sans écrire",
                available,
                false,
            ),
            action(
                ProAdminAction::Resolve,
                "Relire et préparer une nouvelle tentative",
                available,
                false,
            ),
            action(ProAdminAction::Cancel, "Fermer", true, false),
        ];
        if draft.next_history.is_some() {
            rows.push(action(
                ProAdminAction::More,
                "Générations suivantes",
                available,
                false,
            ));
        }
        rows
    }
}
fn action(
    action: ProAdminAction,
    label: &str,
    enabled: bool,
    destructive: bool,
) -> ProAdminActionRow {
    ProAdminActionRow {
        action,
        label: label.into(),
        enabled,
        destructive,
    }
}
fn field(field: Field, value: String, editable: bool) -> ProAdminField {
    let (description, kind) = match field {
        Field::Source => (
            "Octets fournis par vous ; jamais reconstruits depuis les métadonnées",
            ProAdminFieldKind::Multiline,
        ),
        Field::Sql | Field::Active | Field::Workspace | Field::CurrentActive | Field::History => (
            "Référence distincte fournie par le serveur",
            ProAdminFieldKind::Plain,
        ),
        Field::Outcome => (
            "Résultat communiqué pour cette tentative",
            ProAdminFieldKind::Plain,
        ),
    };
    ProAdminField {
        token: field as i32,
        label: field.label().into(),
        description: description.into(),
        value: value.into(),
        kind,
        editable,
    }
}

fn failure(value: &Failure) -> Error {
    if !matches!(value.authorization_termination(), Ok(None))
        || matches!(
            value.kind(),
            FailureKind::Rpc(RpcCode::Unauthenticated | RpcCode::PermissionDenied)
        )
    {
        return Error::Denied;
    }
    if let Ok(Some(error)) = value.configuration_error() {
        if !error.__buffa_unknown_fields.is_empty() {
            return Error::Uncertain;
        }
        match error.code {
            EnumValue::Known(wire::ErrorCode::Conflict) => return Error::Conflict,
            EnumValue::Known(wire::ErrorCode::InvalidArgument) => return Error::Rejected,
            EnumValue::Known(
                wire::ErrorCode::Unspecified
                | wire::ErrorCode::Denied
                | wire::ErrorCode::NotFound
                | wire::ErrorCode::Unavailable
                | wire::ErrorCode::ReconciliationRequired
                | wire::ErrorCode::IncompleteGeneration
                | wire::ErrorCode::Capacity,
            )
            | EnumValue::Unknown(_) => {}
        }
    }
    Error::Uncertain
}
pub(in crate::administration_host) async fn execute(
    session: &SessionCoordinator,
    connection: &Connection,
    expected: Generation,
    job: Job,
    parent: &CancellationToken,
) -> Completion {
    let permit = job.permit;
    let effect = match &job.kind {
        JobKind::Write(_) | JobKind::Restore(_) => Effect::Mutation,
        JobKind::History(_) | JobKind::Read(_) => Effect::Read,
    };
    let result = tokio::select! {biased;()=parent.cancelled()=>Err(Error::Uncertain),result=execute_inner(session,connection,expected,job)=>result};
    Completion {
        permit,
        effect,
        result,
    }
}
async fn execute_inner(
    session: &SessionCoordinator,
    connection: &Connection,
    expected: Generation,
    job: Job,
) -> Result<Reply, Error> {
    let lease = super::super::read::admitted_lease(session, expected, &job.cancel)
        .await
        .map_err(|_| Error::Denied)?;
    let authority = super::super::read::probe(session, connection, expected, &job.cancel)
        .await
        .map_err(|_| Error::Denied)?;
    if !authority.administrator()
        || authority.identity.account != *lease.account_id()
        || authority.identity.session != *lease.session_id()
    {
        return Err(Error::Denied);
    }
    let api = connection.configuration();
    macro_rules! call {($method:ident,$request:expr)=>{{
        let result=tokio::select! {biased;()=job.cancel.cancelled()=>return Err(Error::Uncertain),()=lease.cancellation.cancelled()=>return Err(Error::Denied),result=api.$method($request,lease.token(),Duration::from_secs(4),&job.cancel)=>result};
        match result {Ok(value)=>value,Err(error)=>{let classified=failure(&error);let _=session.observe_failure(lease.generation,&error).await;return Err(classified);}}
    }}}
    match job.kind {
        JobKind::Write(request) => {
            let response = call!(write_document, request);
            configuration_contract::admit_configuration_response(ConfigurationResponse::Write(
                &response,
            ))
            .map_err(|_| Error::Invalid)?;
            Ok(Reply::Mutation {
                activation: response.activation.into_option().ok_or(Error::Invalid)?,
                current: response.current.into_option().map(Box::new),
            })
        }
        JobKind::Restore(request) => {
            let response = call!(restore_document, request);
            configuration_contract::admit_configuration_response(ConfigurationResponse::Restore(
                &response,
            ))
            .map_err(|_| Error::Invalid)?;
            Ok(Reply::Mutation {
                activation: response.activation.into_option().ok_or(Error::Invalid)?,
                current: response.current.into_option().map(Box::new),
            })
        }
        JobKind::History(page) => {
            let response = call!(
                list_generations,
                wire::ListGenerationsRequest {
                    page: wire::PageRequest {
                        page_size: 50,
                        page_token: page,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                }
            );
            configuration_contract::admit_configuration_response(
                ConfigurationResponse::Generations(&response),
            )
            .map_err(|_| Error::Invalid)?;
            Ok(Reply::History(response))
        }
        JobKind::Read(purpose) => {
            let response = call!(
                read_document,
                wire::ReadDocumentRequest {
                    id: wire::DocumentId {
                        value: "operational.main".into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                }
            );
            let capture = Capture::admit(response).map_err(|_| Error::Invalid)?;
            if capture.id().as_str() != "operational.main" {
                return Err(Error::Invalid);
            }
            Ok(Reply::Read {
                purpose,
                capture: Box::new(capture),
            })
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use buffa_types::google::protobuf::Timestamp;
    fn response(revision: i64, generation: i64) -> wire::ReadDocumentResponse {
        wire::ReadDocumentResponse {
            document: wire::Document {
                id: wire::DocumentId {
                    value: "operational.main".into(),
                    ..Default::default()
                }
                .into(),
                kind: wire::DocumentKind::OperationalYaml.into(),
                schema_version: "homeward.operational/1".into(),
                syntax_profile: "homeward.operational-yaml/1".into(),
                version: wire::DocumentVersion {
                    revision,
                    source_sha256: vec![1; 32],
                    ..Default::default()
                }
                .into(),
                updated_at: Timestamp {
                    seconds: 1_700_000_000,
                    ..Default::default()
                }
                .into(),
                active: true,
                protected_source_reference: vec![3; 32],
                workspace_state: Some(wire::document::WorkspaceState::Workspace(Box::new(
                    wire::WorkspaceObservation {
                        source_sha256: vec![2; 32],
                        updated_at: Timestamp {
                            seconds: 1_700_000_001,
                            ..Default::default()
                        }
                        .into(),
                        dirty: wire::DirtyState::Unvalidated.into(),
                        ..Default::default()
                    },
                ))),
                ..Default::default()
            }
            .into(),
            active_generation: wire::ActiveGeneration {
                revision: generation,
                manifest_sha256: vec![4; 32],
                ..Default::default()
            }
            .into(),
            resume_token: "x".repeat(118),
            ..Default::default()
        }
    }
    fn ready() -> Workflow {
        let mut workflow = Workflow::default();
        workflow.observe(Capture::admit(response(7, 6)).unwrap());
        let job = workflow.take_watch().unwrap();
        workflow.watch_ready(job.permit);
        workflow
    }
    fn write(workflow: &mut Workflow) -> Job {
        if let Some(job) = workflow.take_watch() {
            workflow.watch_ready(job.permit);
        }
        assert!(workflow.open(ProAdminAction::Save));
        workflow.edit(Field::Source as i32, "workday: {}\n".into());
        workflow.editor_action(ProAdminAction::Save);
        workflow.take_pending().unwrap()
    }
    #[test]
    fn snapshots_require_live_readiness_and_never_rebase_a_dirty_source() {
        let mut workflow = Workflow::default();
        workflow.observe(Capture::admit(response(7, 6)).unwrap());
        assert!(!workflow.open(ProAdminAction::Save));
        let watch = workflow.take_watch().unwrap();
        assert!(!workflow.open(ProAdminAction::Save));
        workflow.watch_ready(watch.permit);
        assert!(workflow.open(ProAdminAction::Save));
        workflow.edit(Field::Source as i32, "# retained draft\n".into());
        workflow.watch_syncing(watch.permit);
        workflow.editor_action(ProAdminAction::Save);
        assert!(workflow.pending.is_none());
        workflow.observe(Capture::admit(response(8, 8)).unwrap());
        assert_eq!(
            workflow.draft.as_ref().unwrap().stage(),
            EditStage::Conflict
        );
        assert_eq!(
            &*workflow.draft.as_ref().unwrap().text,
            "# retained draft\n"
        );
        workflow.watch_ready(watch.permit);
        workflow.editor_action(ProAdminAction::Save);
        assert!(workflow.pending.is_none());
        assert_eq!(
            workflow.draft.as_ref().unwrap().captured.sql().revision(),
            7
        );
    }
    #[test]
    fn immutable_reference_forks_and_older_snapshots_cannot_replace_current_metadata() {
        let mut workflow = ready();
        let old = Capture::admit(response(6, 5)).unwrap();
        assert_eq!(workflow.snapshot_admission(&old), Ok(false));
        let mut fork = response(7, 6);
        let mut document = fork.document.into_option().unwrap();
        let mut version = document.version.into_option().unwrap();
        version.source_sha256 = vec![9; 32];
        document.version = version.into();
        fork.document = document.into();
        let fork = Capture::admit(fork).unwrap();
        assert!(workflow.snapshot_admission(&fork).is_err());
        workflow.observe(fork);
        workflow.observe(old);
        assert_eq!(workflow.current.as_ref().unwrap().sql().revision(), 7);
        assert_eq!(
            workflow.current.as_ref().unwrap().sql().source().bytes(),
            &[1; 32]
        );
    }
    #[test]
    fn an_explicit_source_and_confirmation_seal_one_exact_attempt() {
        let mut workflow = ready();
        assert!(workflow.open(ProAdminAction::Save));
        assert!(workflow.draft.as_ref().unwrap().text.is_empty());
        workflow.editor_action(ProAdminAction::Save);
        assert!(workflow.pending.is_none());
        workflow.edit(Field::Source as i32, "# exact supplied bytes\n".into());
        workflow.editor_action(ProAdminAction::Save);
        let job = workflow.take_pending().unwrap();
        let JobKind::Write(request) = job.kind else {
            panic!("write")
        };
        assert_eq!(request.exact_yaml_source, b"# exact supplied bytes\n");
        assert_eq!(request.expected.revision, 7);
        assert_eq!(request.expected_workspace_sha256, vec![2; 32]);
        workflow.editor_action(ProAdminAction::Save);
        assert!(workflow.pending.is_none());
        workflow.edit(Field::Source as i32, "late replacement".into());
        let fields = workflow.fields();
        let source = fields
            .iter()
            .find(|value| value.token == Field::Source as i32)
            .unwrap();
        assert!(!source.editable);
        assert!(source.description.starts_with("Octets fournis par vous"));
        assert_eq!(
            &*workflow.draft.as_ref().unwrap().text,
            "# exact supplied bytes\n"
        );
    }
    #[test]
    fn a_metadata_update_before_the_reply_keeps_the_owned_completion_and_newer_active() {
        let mut workflow = ready();
        let job = write(&mut workflow);
        workflow.observe(Capture::admit(response(9, 9)).unwrap());
        assert!(!workflow.accept(Completion {
            permit: job.permit,
            effect: Effect::Mutation,
            result: Ok(Reply::Mutation {
                activation: wire::ActivationResult {
                    outcome: wire::ActivationOutcome::Published.into(),
                    transaction: wire::TransactionOutcome::Committed.into(),
                    generation_revision: Some(8),
                    ..Default::default()
                },
                current: Some(Box::new(response(8, 8)))
            })
        }));
        assert_eq!(workflow.current.as_ref().unwrap().active().revision(), 9);
        assert_eq!(
            workflow
                .draft
                .as_ref()
                .unwrap()
                .receipt
                .as_ref()
                .unwrap()
                .outcome,
            Outcome::Published
        );
        assert_eq!(
            workflow.draft.as_ref().unwrap().stage(),
            EditStage::Completed(Outcome::Published)
        );
        workflow.editor_action(ProAdminAction::Save);
        assert!(workflow.pending.is_none());
    }
    #[test]
    fn a_retired_attempt_cannot_complete_or_unlock_a_replacement_editor() {
        let mut workflow = ready();
        let old = write(&mut workflow);
        workflow.retire();
        workflow.observe(Capture::admit(response(7, 6)).unwrap());
        let current = write(&mut workflow);
        assert_ne!(old.permit, current.permit);
        workflow.accept(Completion {
            permit: old.permit,
            effect: Effect::Mutation,
            result: Err(Error::Rejected),
        });
        assert_eq!(workflow.active, Some(current.permit));
        assert_eq!(
            workflow.draft.as_ref().unwrap().stage(),
            EditStage::InFlight
        );
        assert!(old.cancel.is_cancelled());
    }
    #[test]
    fn an_unknown_attempt_is_only_resolved_by_read_then_a_separate_confirmation() {
        let mut workflow = ready();
        let job = write(&mut workflow);
        workflow.accept(Completion {
            permit: job.permit,
            effect: Effect::Mutation,
            result: Err(Error::Uncertain),
        });
        assert_eq!(
            workflow.draft.as_ref().unwrap().stage(),
            EditStage::Uncertain
        );
        workflow.editor_action(ProAdminAction::Save);
        assert!(workflow.pending.is_none());
        workflow.editor_action(ProAdminAction::Refresh);
        let read = workflow.take_pending().unwrap();
        workflow.accept(Completion {
            permit: read.permit,
            effect: Effect::Read,
            result: Ok(Reply::Read {
                purpose: ReadPurpose::Observe,
                capture: Box::new(Capture::admit(response(8, 8)).unwrap()),
            }),
        });
        workflow.editor_action(ProAdminAction::Save);
        assert!(workflow.pending.is_none());
        workflow.editor_action(ProAdminAction::Resolve);
        let resolve = workflow.take_pending().unwrap();
        workflow.accept(Completion {
            permit: resolve.permit,
            effect: Effect::Read,
            result: Ok(Reply::Read {
                purpose: ReadPurpose::Resolve,
                capture: Box::new(Capture::admit(response(8, 8)).unwrap()),
            }),
        });
        assert!(workflow.pending.is_none());
        assert_eq!(workflow.draft.as_ref().unwrap().stage(), EditStage::Draft);
        let watch = workflow.take_watch().unwrap();
        workflow.watch_ready(watch.permit);
        workflow.editor_action(ProAdminAction::Save);
        let next = workflow.take_pending().unwrap();
        let JobKind::Write(request) = next.kind else {
            panic!("write")
        };
        assert_eq!(request.expected.revision, 8);
    }
    #[test]
    fn history_read_failure_does_not_claim_an_ambiguous_write() {
        let mut workflow = ready();
        workflow.open(ProAdminAction::Restore);
        let read = workflow.take_pending().unwrap();
        workflow.accept(Completion {
            permit: read.permit,
            effect: Effect::Read,
            result: Err(Error::Uncertain),
        });
        assert_eq!(workflow.draft.as_ref().unwrap().stage(), EditStage::Draft);
        assert!(workflow.draft.as_ref().unwrap().receipt.is_none());
        assert!(!workflow.synchronized);
    }
    #[test]
    fn restore_requires_an_explicit_complete_generation_and_keeps_current_cas() {
        let mut workflow = ready();
        workflow.open(ProAdminAction::Restore);
        let history = workflow.take_pending().unwrap();
        let generations = wire::ListGenerationsResponse {
            generations: vec![
                wire::HistoricalGeneration {
                    generation: wire::ActiveGeneration {
                        revision: 4,
                        manifest_sha256: vec![5; 32],
                        ..Default::default()
                    }
                    .into(),
                    availability: wire::GenerationAvailability::PartiallyPurged.into(),
                    created_at: Timestamp {
                        seconds: 1_700_000_000,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
                wire::HistoricalGeneration {
                    generation: wire::ActiveGeneration {
                        revision: 5,
                        manifest_sha256: vec![6; 32],
                        ..Default::default()
                    }
                    .into(),
                    availability: wire::GenerationAvailability::Complete.into(),
                    created_at: Timestamp {
                        seconds: 1_700_000_000,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
            ],
            continuation: wire::PageContinuation {
                result: Some(wire::page_continuation::Result::Complete(Box::default())),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        workflow.accept(Completion {
            permit: history.permit,
            effect: Effect::Read,
            result: Ok(Reply::History(generations)),
        });
        workflow.choose(1100, 1);
        workflow.editor_action(ProAdminAction::Save);
        assert!(workflow.pending.is_none());
        workflow.choose(1100, 2);
        workflow.editor_action(ProAdminAction::Save);
        let restore = workflow.take_pending().unwrap();
        let JobKind::Restore(request) = restore.kind else {
            panic!("restore")
        };
        assert_eq!(request.expected.revision, 7);
        assert_eq!(request.expected.source_sha256, vec![1; 32]);
        assert_eq!(request.expected_workspace_sha256, vec![2; 32]);
        assert_eq!(request.historical.revision, 5);
    }
}
