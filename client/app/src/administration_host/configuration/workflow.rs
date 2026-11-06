//! One private editor cohort seals attempts independently of table generations.
#[cfg(test)]
mod tls_probe;
use super::{render, wire};
use crate::{
    ProAdminAction, ProAdminActionRow, ProAdminChoice, ProAdminField, ProAdminFieldKind,
    configuration::{
        ActiveGeneration, Anchor, Capture, EditStage, Outcome, Receipt, SourceEdit, UserSource,
    },
};
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
            Self::Source => "Exact Source Provided",
            Self::Sql => "Captured SQL Version",
            Self::Active => "Captured Active Generation",
            Self::Workspace => "Captured Observed Files",
            Self::Outcome => "Attempt Outcome",
            Self::CurrentActive => "Reloaded Active Generation",
            Self::History => "Historical Generation",
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
    histories: Vec<wire::ConfigurationGeneration>,
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
            self.notice =
                "References changed. Reload them, then resolve explicitly before confirming.";
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
    Plan(Box<super::planner::Operation>),
    Write(wire::PublishConfigurationDocumentRequest),
    Restore(wire::RollbackConfigurationDocumentRequest),
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
    References {
        read: super::planner::ReferenceRead,
        capture: Box<Capture>,
    },
    PlanHistory(Box<super::planner::HistoryReply>),
    Simulation(Box<home_wire_contract::home::api::v2::SimulateConfigurationPlanResponse>),
    Plan(Box<home_wire_contract::home::api::v2::ConfigurationPlan>),
    Mutation {
        activation: home_wire_contract::configuration_operations_contract::Publication,
        current: Option<Box<wire::ConfigurationDocument>>,
    },
    History(wire::ListConfigurationGenerationsResponse),
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
    Restart,
    Uncertain,
    Invalid,
}
#[derive(Clone, Copy, PartialEq, Eq)]
enum Effect {
    Read,
    Simulation,
    References,
    History,
    Mutation,
}
pub(in crate::administration_host) struct Completion {
    pub permit: Permit,
    effect: Effect,
    result: Result<Reply, Error>,
}

pub(in crate::administration_host) struct Workflow {
    planner: Option<Box<super::planner::Editor>>,
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
            planner: None,
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
        self.planner = None;
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
        self.draft.is_some() || self.planner.is_some()
    }
    pub fn observe(&mut self, capture: Capture) {
        if self.snapshot_admission(&capture) != Ok(true) {
            return;
        }
        if let Some(draft) = &mut self.draft {
            draft.observe(&capture);
        }
        if let Some(planner) = &mut self.planner {
            planner.observe(&capture);
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
        if let Some(planner) = &mut self.planner {
            planner.subscription_syncing();
        }
        true
    }
    pub fn watch_ready(&mut self, permit: super::WatchPermit) -> bool {
        if !self.watch.ready(permit) {
            return false;
        }
        self.synchronized = true;
        if let Some(planner) = &mut self.planner {
            planner.set_ready(true);
        }
        true
    }
    pub fn watch_stopped(&mut self, permit: super::WatchPermit) -> bool {
        if !self.watch.stop(permit) {
            return false;
        }
        self.synchronized = false;
        if let Some(planner) = &mut self.planner {
            planner.set_ready(false);
        }
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
                ProAdminAction::Preview,
                "Prepare a Candidate and Its Preview",
                self.synchronized && self.active.is_none(),
                false,
            ),
            action(
                ProAdminAction::Save,
                "Provide Operational Source",
                enabled,
                false,
            ),
            action(
                ProAdminAction::Restore,
                "Restore a Generation",
                enabled,
                false,
            ),
        ]
    }
    pub fn open(&mut self, action: ProAdminAction) -> bool {
        if action == ProAdminAction::Preview {
            if self.opened() || self.active.is_some() || !self.synchronized || self.scope.is_none()
            {
                return false;
            }
            let Some(capture) = self.current.clone() else {
                return false;
            };
            self.planner = Some(Box::new(super::planner::Editor::open(capture)));
            return true;
        }
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
                    "Provide the exact source. Protected metadata is not an editable source."
                }
                Mode::Restore => {
                    "Choose a complete generation. Restoring creates a new revision after confirmation."
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
        if let Some(planner) = &mut self.planner {
            planner.edit(field, text);
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
            draft.notice = "The source exceeds the 1 MiB limit.";
            return;
        }
        draft.text = text;
        draft.receipt = None;
        draft.notice = "Source explicitly provided; nothing will be sent before confirmation.";
    }
    pub fn choose(&mut self, field: i32, option: i32) {
        if self.active.is_some() {
            return;
        }
        if let Some(planner) = &mut self.planner {
            planner.choose(field, option);
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
            draft.notice = "Generation selected. Restore has not been sent yet.";
        }
    }
    pub fn editor_action(&mut self, value: ProAdminAction) -> bool {
        if !self.opened() {
            return false;
        }
        if self.planner.is_some()
            && !matches!(
                value,
                ProAdminAction::Cancel | ProAdminAction::CloseSecret | ProAdminAction::Resolve
            )
        {
            let synchronized = self.synchronized;
            if self.active.is_none()
                && let Some(operation) = self.planner.as_mut().and_then(|planner| {
                    planner.set_ready(synchronized);
                    planner.action(value)
                })
            {
                self.queue(JobKind::Plan(Box::new(operation)));
            }
            return true;
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
                    draft.notice = "Provide a nonempty source of at most 1 MiB.";
                    return;
                };
                if value.supply(source).is_err() {
                    return;
                }
                match value.prepare_write(current) {
                    Ok(request) => JobKind::Write(request),
                    Err(_) => {
                        draft.notice = "Preconditions changed. Reload and resolve explicitly.";
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
                    draft.notice = "Preconditions changed. Reload and resolve explicitly.";
                    return;
                }
                let Some(historical) = historical else {
                    draft.notice = "Select a complete generation.";
                    return;
                };
                let Ok(request_id) = home_client_actions::new_request_id() else {
                    draft.notice = "Request identity unavailable. No restore was sent.";
                    return;
                };
                let request = wire::RollbackConfigurationDocumentRequest {
                    request_id: request_id.as_str().into(),
                    name: current.name().into(),
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
        draft.notice = "Attempt in progress; no second write will be sent.";
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
            draft.notice = "Outcome unknown. Reload the state without resubmitting the write, then resolve explicitly.";
        }
    }
    pub fn accept(&mut self, completion: Completion) -> bool {
        if self.active != Some(completion.permit) || self.scope != Some(completion.permit.scope) {
            return false;
        }
        self.active = None;
        match completion.result {
            Ok(Reply::References { read, capture }) => {
                if let Some(planner) = &mut self.planner {
                    planner.accept_reference(read, *capture);
                }
            }
            Ok(Reply::PlanHistory(reply)) => {
                if let Some(planner) = &mut self.planner {
                    planner.accept_history(*reply);
                }
            }
            Ok(Reply::Simulation(response)) => {
                if let Some(planner) = &mut self.planner {
                    planner.accept_simulation(*response);
                }
            }
            Ok(Reply::Plan(plan)) => {
                if let Some(planner) = &mut self.planner {
                    planner.accept(*plan);
                }
            }
            Ok(Reply::Mutation {
                activation,
                current,
            }) => {
                let current = current.map(|value| *value);
                if let Some(planner) = &mut self.planner {
                    planner.complete_activation(&activation, current.clone());
                    if let Some(current) = current.and_then(|value| Capture::admit(value).ok()) {
                        self.observe(current);
                    }
                    return false;
                }
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
                        draft.notice = if receipt.outcome == Outcome::Published {
                            ""
                        } else {
                            receipt.outcome.label()
                        };
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
                    draft.next_history =
                        (!value.next_page_token.is_empty()).then_some(value.next_page_token);
                    draft.histories = value.configuration_generations;
                    if let Edit::Restore { historical, .. } = &mut draft.edit {
                        *historical = None;
                    }
                    draft.notice = "Choose a complete generation before confirming.";
                }
            }
            Ok(Reply::Read { purpose, capture }) => {
                if self.snapshot_admission(&capture) != Ok(true) {
                    self.synchronized = false;
                    if let Some(draft) = &mut self.draft {
                        draft.notice = "The new reading is older or contradicts known references. No resolution was applied; reload again.";
                    }
                    if let Some(planner) = &mut self.planner {
                        planner.failed();
                    }
                    return false;
                }
                self.observe(*capture);
                if matches!(purpose, ReadPurpose::Resolve)
                    && let (Some(planner), Some(current)) =
                        (&mut self.planner, self.current.clone())
                {
                    planner.resolve(current);
                }
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
                        draft.notice = "References reloaded and explicitly resolved. A new confirmation is still required.";
                    }
                }
            }
            Err(Error::Denied) => {
                self.retire();
                return true;
            }
            Err(error) => {
                self.synchronized = false;
                if let Some(planner) = &mut self.planner {
                    planner.set_ready(false);
                    match completion.effect {
                        Effect::Read => planner.failed(),
                        Effect::Simulation => planner.simulation_failed(),
                        Effect::References => planner.references_failed(),
                        Effect::History => planner.history_failed(),
                        Effect::Mutation => match error {
                            Error::Conflict => planner.activation_conflict(),
                            Error::Rejected => planner.activation_rejected(),
                            Error::Restart => planner.activation_restart_required(),
                            Error::Uncertain | Error::Invalid => planner.activation_uncertain(),
                            Error::Denied => unreachable!(),
                        },
                    }
                }
                if let Some(draft) = &mut self.draft {
                    if completion.effect == Effect::Read {
                        draft.notice = "Reading unavailable. References and the previous result are unchanged.";
                        return false;
                    }
                    match error {
                        Error::Conflict => {
                            match &mut draft.edit {
                                Edit::Write(value) => value.conflict_before_commit(),
                                Edit::Restore { stage, .. } => *stage = EditStage::Conflict,
                            }
                            draft.notice = "Reference conflict. Reload and resolve without automatic overwrite.";
                        }
                        Error::Rejected => {
                            match &mut draft.edit {
                                Edit::Write(value) => value.rejected_before_commit(),
                                Edit::Restore { stage, .. } => {
                                    *stage = EditStage::Completed(Outcome::RejectedBeforeCommit)
                                }
                            }
                            draft.notice = "Source rejected before saving. Reload the state before confirming again.";
                        }
                        Error::Restart => {
                            match &mut draft.edit {
                                Edit::Write(value) => value.restart_required(),
                                Edit::Restore { stage, .. } => {
                                    *stage = EditStage::Completed(Outcome::RestartRequired)
                                }
                            }
                            draft.notice =
                                "Server restart required. Nothing was saved; no automatic retry.";
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
        if self.planner.is_some() {
            return "Prepare a Configuration Plan";
        }
        match self.draft.as_ref().map(|value| value.mode()) {
            Some(Mode::Write) => "Explicit Operational Source",
            Some(Mode::Restore) => "Restore a Generation",
            None => "Configuration",
        }
    }
    pub fn notice(&self) -> &'static str {
        if let Some(planner) = &self.planner {
            return planner.notice();
        }
        if !self.synchronized
            && self.active.is_none()
            && self
                .draft
                .as_ref()
                .is_some_and(|draft| draft.stage() == EditStage::Draft)
        {
            return "Configuration tracking is not synchronized. Confirmation requires a valid reading and subscription.";
        }
        self.draft.as_ref().map_or("", |value| value.notice)
    }
    pub fn editable(&self) -> bool {
        if let Some(planner) = &self.planner {
            return self.active.is_none() && planner.mutable();
        }
        self.active.is_none() && self.draft.as_ref().is_some_and(|value| value.editable())
    }
    pub fn fields(&self) -> Vec<ProAdminField> {
        if let Some(planner) = &self.planner {
            let mut fields = planner.fields(self.editable());
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
            return fields;
        }
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
        fields.push(field(
            Field::Workspace,
            super::workspace_label(draft.captured.workspace()),
            false,
        ));
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
                    "{} · reported generation {}",
                    receipt.outcome.label(),
                    receipt
                        .target_generation
                        .map_or_else(|| "not provided".to_owned(), |value| value.to_string())
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
        if let Some(planner) = &self.planner {
            return planner.choices(self.editable());
        }
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
                    "Generation {} · {}",
                    value.generation.revision,
                    if value.availability == wire::GenerationAvailability::Complete {
                        "complete"
                    } else {
                        "partially purged"
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
        if let Some(planner) = &self.planner {
            return planner.actions(self.active.is_none());
        }
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
                    Mode::Write => "Confirm Write",
                    Mode::Restore => "Confirm Restore",
                },
                ready,
                true,
            ),
            action(
                ProAdminAction::Refresh,
                "Reload State Without Writing",
                available,
                false,
            ),
            action(
                ProAdminAction::Resolve,
                "Reload and Prepare a New Attempt",
                available,
                false,
            ),
            action(ProAdminAction::Cancel, "Close", true, false),
        ];
        if draft.next_history.is_some() {
            rows.push(action(
                ProAdminAction::More,
                "Next Generations",
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
            "Bytes provided by you; never reconstructed from metadata",
            ProAdminFieldKind::Multiline,
        ),
        Field::Sql | Field::Active | Field::Workspace | Field::CurrentActive | Field::History => (
            "Separate server-provided reference",
            ProAdminFieldKind::Plain,
        ),
        Field::Outcome => ("Result Reported for This Attempt", ProAdminFieldKind::Plain),
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
    if match value.api_failure() {
        Err(_) => true,
        Ok(Some(detail)) => matches!(
            detail.reason(),
            home_wire_contract::home::api::v2::ApiErrorReason::AuthorityUnavailable
                | home_wire_contract::home::api::v2::ApiErrorReason::AuthenticationRequired
                | home_wire_contract::home::api::v2::ApiErrorReason::AccessDenied
        ),
        Ok(None) => false,
    } || matches!(
        value.kind(),
        FailureKind::Rpc(RpcCode::Unauthenticated | RpcCode::PermissionDenied)
    ) {
        return Error::Denied;
    }
    if let Ok(Some(error)) = value.api_failure() {
        use home_wire_contract::home::api::v2::ApiErrorReason;
        match error.reason() {
            ApiErrorReason::EtagMismatch => return Error::Conflict,
            ApiErrorReason::ValidationFailed | ApiErrorReason::StatePreconditionFailed => {
                return Error::Rejected;
            }
            ApiErrorReason::ConfigRestartRequired => return Error::Restart,
            ApiErrorReason::AuthorityUnavailable
            | ApiErrorReason::AuthenticationRequired
            | ApiErrorReason::AccessDenied => {
                return Error::Denied;
            }
            ApiErrorReason::Unspecified
            | ApiErrorReason::ResourceNotFound
            | ApiErrorReason::ResourceAlreadyExists
            | ApiErrorReason::RequestIdReused
            | ApiErrorReason::ConfigChanged
            | ApiErrorReason::TopologyChanged
            | ApiErrorReason::CapabilityUnavailable
            | ApiErrorReason::SourceReadOnly
            | ApiErrorReason::ResyncRequired
            | ApiErrorReason::RateLimited
            | ApiErrorReason::RequestTooLarge
            | ApiErrorReason::StreamCapacity
            | ApiErrorReason::QueryBudgetExceeded
            | ApiErrorReason::ServiceUnavailable
            | ApiErrorReason::RequestCancelled
            | ApiErrorReason::RequestDeadlineExceeded
            | ApiErrorReason::CommitOutcomeUnknown
            | ApiErrorReason::EffectOutcomeUnknown
            | ApiErrorReason::InternalFailure
            | ApiErrorReason::StoredDataIntegrity
            | ApiErrorReason::MethodNotSupported => {}
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
        JobKind::Plan(operation) => match **operation {
            super::planner::Operation::Build(_) | super::planner::Operation::Revalidate(_) => {
                Effect::Read
            }
            super::planner::Operation::Simulate(_) => Effect::Simulation,
            super::planner::Operation::References(_) => Effect::References,
            super::planner::Operation::History(_) => Effect::History,
            super::planner::Operation::Apply(_) | super::planner::Operation::Rollback(_) => {
                Effect::Mutation
            }
        },
    };
    let result = tokio::select! {biased;()=parent.cancelled()=>Err(Error::Uncertain),result=execute_inner(session,connection,expected,job)=>result};
    Completion {
        permit,
        effect,
        result,
    }
}
async fn follow_configuration_operation(
    session: &SessionCoordinator,
    connection: &Connection,
    lease: &home_client_session::AccessLease,
    cancel: &CancellationToken,
    home: &str,
    request_id: &str,
    raw: home_wire_contract::google::longrunning::Operation,
) -> Result<home_wire_contract::configuration_operations_contract::Publication, Error> {
    use home_wire_contract::{
        configuration_operations_contract::{self, Phase},
        google::longrunning::WaitOperationRequest,
    };
    let admitted =
        configuration_operations_contract::admit_response(&raw).map_err(|_| Error::Invalid)?;
    let expected_request =
        home_client_model::api::RequestId::try_from(request_id).map_err(|_| Error::Invalid)?;
    if admitted.request_id() != Some(&expected_request) {
        return Err(Error::Invalid);
    }
    if admitted
        .name()
        .as_str()
        .split_once("/operations/")
        .map(|(parent, _)| parent)
        != Some(home)
    {
        return Err(Error::Invalid);
    }
    if matches!(admitted.phase(), Phase::Published | Phase::Failed) {
        return Ok(admitted);
    }
    let transport = connection.operations();
    let request = WaitOperationRequest {
        name: admitted.name().as_str().into(),
        timeout: buffa_types::google::protobuf::Duration {
            seconds: 1,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let result = tokio::select! { biased;
        () = cancel.cancelled() => return Err(Error::Uncertain),
        () = lease.cancellation.cancelled() => return Err(Error::Denied),
        result = transport.wait_operation(request, lease.token(), Duration::from_secs(3), cancel) => result,
    };
    match result {
        Ok(raw) => {
            match configuration_operations_contract::admit_operation(&raw, admitted.name()) {
                Ok(next) if next.follows(&admitted) => Ok(next),
                // Refuse malformed, retargeted or regressing status. The last
                // admitted COMMIT stays pending; no new payload is projected.
                Ok(_) | Err(_) => Ok(admitted),
            }
        }
        Err(error) => {
            let classification = failure(&error);
            let _ = session.observe_failure(lease.generation, &error).await;
            if matches!(classification, Error::Denied) {
                return Err(Error::Denied);
            }
            // Failure of this read does not erase the already admitted COMMIT
            // and never retries the mutation that allocated the operation.
            Ok(admitted)
        }
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
        || authority.identity.account != *lease.account_name()
        || authority.identity.session != *lease.session_name()
    {
        return Err(Error::Denied);
    }
    let api = connection.configuration();
    let home = super::home_name(session, connection, &lease, &job.cancel)
        .await
        .map_err(|_| Error::Uncertain)?;
    macro_rules! call {($client:expr,$method:ident,$request:expr)=>{{
        let client = $client;
        let result=tokio::select! {biased;()=job.cancel.cancelled()=>return Err(Error::Uncertain),()=lease.cancellation.cancelled()=>return Err(Error::Denied),result=client.$method($request,lease.token(),Duration::from_secs(4),&job.cancel)=>result};
        match result {Ok(value)=>value,Err(error)=>{let classified=failure(&error);let _=session.observe_failure(lease.generation,&error).await;return Err(classified);}}
    }}}
    match job.kind {
        JobKind::Plan(operation) => {
            use home_wire_contract::{
                config_plans::{self, PlanResponse},
                home::api::v2 as plan,
            };
            let api = connection.configuration_plans();
            let (family, identity, value) = match *operation {
                super::planner::Operation::References(read) => {
                    let name = super::source_document_name(
                        session,
                        connection,
                        &lease,
                        &job.cancel,
                        read.id.as_str(),
                    )
                    .await
                    .map_err(|_| Error::Invalid)?;
                    let response = call!(
                        connection.configuration(),
                        read_document,
                        wire::GetConfigurationDocumentRequest {
                            name,
                            ..Default::default()
                        }
                    );
                    let capture = Capture::admit(response).map_err(|_| Error::Invalid)?;
                    if capture.document().id.value != read.id.as_str() {
                        return Err(Error::Invalid);
                    }
                    return Ok(Reply::References {
                        read,
                        capture: Box::new(capture),
                    });
                }
                super::planner::Operation::History(query) => {
                    use super::planner::{HistoryQuery, HistoryReply};
                    let (generations, policy) = match query {
                        HistoryQuery::Initial { policy } => {
                            (Some(None), policy.map(|id| (id, None)))
                        }
                        HistoryQuery::Generations(page) => (Some(Some(page)), None),
                        HistoryQuery::Policy { id, page } => (None, Some((id, Some(page)))),
                    };
                    let mut reply = HistoryReply {
                        generations: None,
                        versions: None,
                    };
                    if let Some(page) = generations {
                        let response = call!(
                            connection.configuration(),
                            list_generations,
                            wire::ListConfigurationGenerationsRequest {
                                parent: home.clone(),
                                page_size: 50,
                                page_token: page.unwrap_or_default(),
                                ..Default::default()
                            }
                        );
                        configuration_contract::admit_configuration_response(
                            ConfigurationResponse::Generations(&response),
                        )
                        .map_err(|_| Error::Invalid)?;
                        reply.generations = Some(response);
                    }
                    if let Some((id, page)) = policy {
                        let response = call!(
                            connection.configuration(),
                            list_versions,
                            wire::ListConfigurationRevisionsRequest {
                                parent: id.as_str().into(),
                                page_size: 50,
                                page_token: page.unwrap_or_default(),
                                ..Default::default()
                            }
                        );
                        configuration_contract::admit_configuration_response(
                            ConfigurationResponse::Versions(&response),
                        )
                        .map_err(|_| Error::Invalid)?;
                        reply.versions = Some((id, response));
                    }
                    return Ok(Reply::PlanHistory(Box::new(reply)));
                }
                super::planner::Operation::Apply(request) => {
                    let request_id = request.request_id.clone();
                    let response = call!(connection.configuration_apply(), apply, *request);
                    let response = follow_configuration_operation(
                        session,
                        connection,
                        &lease,
                        &job.cancel,
                        &home,
                        &request_id,
                        response,
                    )
                    .await?;
                    return Ok(Reply::Mutation {
                        activation: response,
                        current: None,
                    });
                }
                super::planner::Operation::Rollback(request) => {
                    let request_id = request.request_id.clone();
                    let response = call!(connection.configuration_apply(), rollback, *request);
                    let response = follow_configuration_operation(
                        session,
                        connection,
                        &lease,
                        &job.cancel,
                        &home,
                        &request_id,
                        response,
                    )
                    .await?;
                    return Ok(Reply::Mutation {
                        activation: response,
                        current: None,
                    });
                }
                super::planner::Operation::Simulate(request) => {
                    let response = call!(
                        connection.configuration_simulation(),
                        simulate_plan,
                        request
                    );
                    home_wire_contract::configuration_simulation_contract::admit_simulation_response(&response).map_err(|_|Error::Invalid)?;
                    return Ok(Reply::Simulation(Box::new(response)));
                }
                super::planner::Operation::Build(mut request) => {
                    request.parent = home.clone();
                    config_plans::admit_plan_request(config_plans::PlanRequest::Build(&request))
                        .map_err(|_| Error::Invalid)?;
                    let family = request.configuration_plan.candidate.family;
                    let identity = request.configuration_plan.candidate.identity.clone();
                    let response = call!(api, build_plan, request);
                    config_plans::admit_plan_response(PlanResponse::Build(&response))
                        .map_err(|_| Error::Invalid)?;
                    (family, identity, response)
                }
                super::planner::Operation::Revalidate(request) => {
                    let family = request.candidate.family;
                    let identity = request.candidate.identity.clone();
                    let response = call!(api, revalidate_plan, request);
                    config_plans::admit_plan_response(PlanResponse::Revalidate(&response))
                        .map_err(|_| Error::Invalid)?;
                    (family, identity, response)
                }
            };
            if value.family != family
                || value.identity != identity
                || value.author != lease.account_name().as_str()
                || value.family == plan::ConfigurationPlanSourceFamily::Unspecified
            {
                return Err(Error::Invalid);
            }
            Ok(Reply::Plan(Box::new(value)))
        }
        JobKind::Write(request) => {
            let request_id = request.request_id.clone();
            let response = call!(api, write_document, request);
            let response = follow_configuration_operation(
                session,
                connection,
                &lease,
                &job.cancel,
                &home,
                &request_id,
                response,
            )
            .await?;
            Ok(Reply::Mutation {
                activation: response,
                current: None,
            })
        }
        JobKind::Restore(request) => {
            let request_id = request.request_id.clone();
            let response = call!(api, restore_document, request);
            let response = follow_configuration_operation(
                session,
                connection,
                &lease,
                &job.cancel,
                &home,
                &request_id,
                response,
            )
            .await?;
            Ok(Reply::Mutation {
                activation: response,
                current: None,
            })
        }
        JobKind::History(page) => {
            let response = call!(
                api,
                list_generations,
                wire::ListConfigurationGenerationsRequest {
                    parent: home.clone(),
                    page_size: 50,
                    page_token: page.unwrap_or_default(),
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
            let capture = super::operational_capture(session, connection, &lease, &job.cancel)
                .await
                .map_err(|_| Error::Uncertain)?;
            Ok(Reply::Read {
                purpose,
                capture: Box::new(capture),
            })
        }
    }
}

#[cfg(test)]
pub(super) mod tests {
    use super::*;
    use buffa_types::google::protobuf::Timestamp;
    pub(in crate::administration_host::configuration) fn response(
        revision: i64,
        generation: i64,
    ) -> wire::ConfigurationDocument {
        wire::ConfigurationDocument {
            name: "homes/test/configurationDocuments/operational".into(),
            etag: "test-etag".into(),
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
            workspace_state: Some(wire::configuration_document::WorkspaceState::Workspace(
                Box::new(wire::WorkspaceObservation {
                    source_sha256: vec![2; 32],
                    updated_at: Timestamp {
                        seconds: 1_700_000_001,
                        ..Default::default()
                    }
                    .into(),
                    dirty: wire::DirtyState::Unvalidated.into(),
                    ..Default::default()
                }),
            )),
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
        fork.version.as_option_mut().unwrap().source_sha256 = vec![9; 32];
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
        assert!(source.description.starts_with("Bytes provided by you"));
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
                activation: crate::configuration::publication_for_test("homes/test", true, 8, 8),
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
        assert!(workflow.draft.as_ref().unwrap().notice.is_empty());
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
    fn an_old_or_contradictory_resolution_cannot_rearm_an_uncertain_write() {
        let mut workflow = ready();
        let job = write(&mut workflow);
        workflow.accept(Completion {
            permit: job.permit,
            effect: Effect::Mutation,
            result: Err(Error::Uncertain),
        });
        workflow.observe(Capture::admit(response(9, 9)).unwrap());
        for mut capture in [response(8, 8), response(9, 9)] {
            if capture.version.revision == 9 {
                capture.version.as_option_mut().unwrap().source_sha256 = vec![9; 32];
            }
            workflow.editor_action(ProAdminAction::Resolve);
            let read = workflow.take_pending().unwrap();
            workflow.accept(Completion {
                permit: read.permit,
                effect: Effect::Read,
                result: Ok(Reply::Read {
                    purpose: ReadPurpose::Resolve,
                    capture: Box::new(Capture::admit(capture).unwrap()),
                }),
            });
            assert_eq!(
                workflow.draft.as_ref().unwrap().stage(),
                EditStage::Uncertain
            );
            assert_eq!(workflow.current.as_ref().unwrap().sql().revision(), 9);
            workflow.editor_action(ProAdminAction::Save);
            assert!(workflow.pending.is_none());
        }
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
        let generations = wire::ListConfigurationGenerationsResponse {
            configuration_generations: vec![
                wire::ConfigurationGeneration {
                    name: "homes/test/configurationGenerations/g-4".into(),
                    generation: wire::ActiveGeneration {
                        revision: 4,
                        manifest_sha256: vec![5; 32],
                        ..Default::default()
                    }
                    .into(),
                    availability: wire::GenerationAvailability::PartiallyPurged.into(),
                    create_time: Timestamp {
                        seconds: 1_700_000_000,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
                wire::ConfigurationGeneration {
                    name: "homes/test/configurationGenerations/g-5".into(),
                    generation: wire::ActiveGeneration {
                        revision: 5,
                        manifest_sha256: vec![6; 32],
                        ..Default::default()
                    }
                    .into(),
                    availability: wire::GenerationAvailability::Complete.into(),
                    create_time: Timestamp {
                        seconds: 1_700_000_000,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
            ],
            next_page_token: String::new(),
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
