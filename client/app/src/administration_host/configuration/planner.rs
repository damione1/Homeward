//! Explicit multi-file inputs and diagnostic previews. A plan never authorizes
//! a mutation and never supplies editable bytes to a source field.
mod activation;
mod simulation;
use super::render;
use crate::{
    ProAdminAction, ProAdminActionRow, ProAdminChoice, ProAdminField, ProAdminFieldKind,
    configuration::{Candidate, Capture, SourceFamily, SourceFile, SourceIdentity, UserSource},
};
#[cfg(test)]
pub(super) use activation::Field as ActivationField;
pub(super) use activation::{HistoryQuery, HistoryReply, ReferenceRead};
use buffa::EnumValue;
use home_wire_contract::home::api::v2 as wire;
#[cfg(test)]
pub(super) use simulation::Field as SimulationField;
use zeroize::Zeroizing;

const MAX_BYTES: usize = 1_048_576;
#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(i32)]
pub(super) enum Field {
    Family = 1200,
    Identity = 1201,
    Entry = 1202,
    Path = 1203,
    Source = 1204,
    Files = 1205,
    Plan = 1206,
    Base = 1207,
    Changes = 1208,
    Diagnostics = 1209,
    Captured = 1210,
}
impl Field {
    fn admit(value: i32) -> Option<Self> {
        match value {
            1200 => Some(Self::Family),
            1201 => Some(Self::Identity),
            1202 => Some(Self::Entry),
            1203 => Some(Self::Path),
            1204 => Some(Self::Source),
            1205 => Some(Self::Files),
            1206 => Some(Self::Plan),
            1207 => Some(Self::Base),
            1208 => Some(Self::Changes),
            1209 => Some(Self::Diagnostics),
            1210 => Some(Self::Captured),
            _ => None,
        }
    }
    fn label(self) -> &'static str {
        match self {
            Self::Family => "Source Family",
            Self::Identity => "Logical Identity",
            Self::Entry => "Entry File",
            Self::Path => "Selected File’s Logical Path",
            Self::Source => "Exact Bytes Provided",
            Self::Files => "Candidate Files",
            Self::Plan => "Server Preview",
            Self::Base => "Plan Comparison Baseline",
            Self::Changes => "Reported Changes",
            Self::Diagnostics => "Plan Diagnostics",
            Self::Captured => "Captured References",
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) struct FileKey(u64);
struct File {
    reference: activation::SourceReference,
    key: FileKey,
    path: String,
    source: Zeroizing<String>,
}
impl File {
    fn empty(key: FileKey) -> Self {
        Self {
            reference: activation::SourceReference::default(),
            key,
            path: String::new(),
            source: Zeroizing::new(String::new()),
        }
    }
}
pub(super) enum Operation {
    References(ReferenceRead),
    History(HistoryQuery),
    Apply(Box<home_wire_contract::home::api::v2::ApplyConfigurationPlanRequest>),
    Rollback(Box<home_wire_contract::home::api::v2::RollbackConfigurationRequest>),
    Build(wire::CreateConfigurationPlanRequest),
    Revalidate(wire::RevalidateConfigurationPlanRequest),
    Simulate(home_wire_contract::home::api::v2::SimulateConfigurationPlanRequest),
}
pub(super) struct Editor {
    activation: activation::Activation,
    simulation: simulation::Simulation,
    captured: Capture,
    family: SourceFamily,
    identity: String,
    entry: String,
    files: Vec<File>,
    selected: FileKey,
    next_file: Option<u64>,
    plan: Option<wire::ConfigurationPlan>,
    stale: bool,
    notice: &'static str,
}
impl Editor {
    pub fn open(captured: Capture) -> Self {
        Self {
            activation: activation::Activation::default(),
            simulation: simulation::Simulation::default(),
            captured,
            family: SourceFamily::Operational,
            identity: String::new(),
            entry: String::new(),
            files: vec![File::empty(FileKey(1))],
            selected: FileKey(1),
            next_file: Some(2),
            plan: None,
            stale: false,
            notice: "Provide your exact files. Building or revalidating a preview activates no configuration.",
        }
    }
    pub fn observe(&mut self, current: &Capture) {
        if self.captured.sql() != current.sql()
            || self.captured.active() != current.active()
            || self.captured.workspace() != current.workspace()
        {
            self.stale = true;
            self.notice = "References changed. The candidate is retained; reload and resolve explicitly before a future activation.";
        }
    }
    pub fn resolve(&mut self, current: Capture) {
        self.resolve_activation();
        self.simulation.clear();
        self.captured = current;
        self.stale = false;
        self.plan = None;
        self.notice = "References explicitly reloaded. Build a new preview for these files.";
    }
    pub fn notice(&self) -> &'static str {
        self.notice
    }
    #[cfg(test)]
    pub fn plan_state(&self) -> Option<wire::ConfigurationPlanState> {
        self.plan.as_ref().and_then(|plan| match plan.state {
            EnumValue::Known(state) => Some(state),
            EnumValue::Unknown(_) => None,
        })
    }
    #[cfg(test)]
    pub fn simulation_report(
        &self,
    ) -> Option<&home_wire_contract::home::api::v2::ConfigurationSimulationReport> {
        self.simulation.report()
    }
    fn selected(&self) -> &File {
        self.files
            .iter()
            .find(|file| file.key == self.selected)
            .expect("selected file")
    }
    fn invalidate(&mut self) {
        self.simulation.clear();
        self.plan = None;
        self.notice = "Candidate changed; the previous preview was removed. No file was sent.";
    }
    pub fn edit(&mut self, token: i32, text: String) {
        if !self.mutable() {
            let _ = Zeroizing::new(text);
            return;
        }
        if self.edit_reference(token, &text) {
            return;
        }
        if let Some(field) = simulation::Field::admit(token) {
            self.simulation.edit(field, text);
            self.notice = "Simulation settings changed; the previous report was removed. No device was queried.";
            return;
        }
        let text = Zeroizing::new(text);
        let Some(field) = Field::admit(token) else {
            return;
        };
        match field {
            Field::Identity => {
                if text.len() > 255 {
                    return;
                }
                self.identity = text.to_string();
            }
            Field::Entry => {
                if text.len() > 4096 {
                    return;
                }
                self.entry = text.to_string();
            }
            Field::Path => {
                if text.len() > 4096 {
                    return;
                }
                let file = self
                    .files
                    .iter_mut()
                    .find(|file| file.key == self.selected)
                    .expect("selected file");
                file.path = text.to_string();
                file.reference.clear();
            }
            Field::Source => {
                let other_bytes: usize = self
                    .files
                    .iter()
                    .filter(|file| file.key != self.selected)
                    .map(|file| file.source.len())
                    .sum();
                if text.len() > MAX_BYTES - other_bytes {
                    self.notice = "The candidate exceeds the 1 MiB total limit; the previous input is retained.";
                    return;
                }
                self.files
                    .iter_mut()
                    .find(|file| file.key == self.selected)
                    .expect("selected file")
                    .source = text;
            }
            Field::Family
            | Field::Files
            | Field::Plan
            | Field::Base
            | Field::Changes
            | Field::Diagnostics
            | Field::Captured => return,
        }
        self.invalidate();
    }
    pub fn choose(&mut self, token: i32, option: i32) {
        if self.choose_activation(token, option) {
            return;
        }
        if !self.mutable() {
            return;
        }
        if Field::admit(token) != Some(Field::Family) {
            return;
        }
        let Some(family) = usize::try_from(option)
            .ok()
            .and_then(|value| value.checked_sub(1))
            .and_then(|index| SourceFamily::ALL.get(index))
            .copied()
        else {
            return;
        };
        if family != self.family {
            self.family = family;
            self.resolve_activation();
            self.invalidate();
        }
    }
    pub fn candidate(&self) -> Result<Candidate, crate::administration::Refusal> {
        let files = self
            .files
            .iter()
            .map(|file| {
                SourceFile::supplied(
                    &file.path,
                    UserSource::supplied(file.source.as_bytes().to_vec())?,
                )
            })
            .collect::<Result<Vec<_>, _>>()?;
        Candidate::supplied(
            self.family,
            SourceIdentity::parse(&self.identity)?,
            &self.entry,
            files,
        )
    }
    pub fn action(&mut self, action: ProAdminAction) -> Option<Operation> {
        if !self.mutable() && matches!(action, ProAdminAction::AddFile | ProAdminAction::RemoveFile)
        {
            return None;
        }
        match action {
            ProAdminAction::Refresh => return self.source_read(),
            ProAdminAction::Restore => return self.history_read(),
            ProAdminAction::More => return self.history_more(),
            ProAdminAction::Apply => return self.prepare_apply(),
            ProAdminAction::Rollback => return self.prepare_rollback(),
            ProAdminAction::Simulate => {
                let request = self.candidate().and_then(|candidate| {
                    self.simulation
                        .request(&candidate, self.captured.id().home())
                });
                match request {
                    Ok(request) => {
                        self.notice =
                            "Simulating with your synthetic states; no physical commands.";
                        return Some(Operation::Simulate(request));
                    }
                    Err(_) => {
                        self.notice = "Simulation not sent: check the logic family, files, clock, events and JSON test doubles.";
                        return None;
                    }
                }
            }
            ProAdminAction::AddFile => {
                if self.files.len() >= 64 {
                    self.notice = "The candidate is limited to 64 files.";
                    return None;
                }
                let Some(next) = self.next_file else {
                    self.notice = "Cannot open another file in this form.";
                    return None;
                };
                self.next_file = next.checked_add(1);
                self.selected = FileKey(next);
                self.files.push(File::empty(self.selected));
                self.invalidate();
            }
            ProAdminAction::RemoveFile => {
                if self.files.len() > 1 {
                    self.files.retain(|file| file.key != self.selected);
                    self.selected = self.files[0].key;
                    self.invalidate();
                }
            }
            ProAdminAction::NextFile => {
                let index = self
                    .files
                    .iter()
                    .position(|file| file.key == self.selected)
                    .expect("selected file");
                self.selected = self.files[(index + 1) % self.files.len()].key;
            }
            ProAdminAction::Preview | ProAdminAction::Revalidate => {
                let Ok(candidate) = self.candidate() else {
                    self.notice = "Complete the identity, entry point and every file; paths must be unique, the entry point must exist, and the total must not exceed 1 MiB.";
                    return None;
                };
                if action == ProAdminAction::Preview {
                    self.notice = "Building preview; nothing will be activated.";
                    let mut request = candidate.build_request();
                    request.request_id =
                        home_client_actions::new_request_id().ok()?.as_str().into();
                    return Some(Operation::Build(request));
                }
                let Some(plan) = &self.plan else {
                    return None;
                };
                if plan.receipt.is_empty() {
                    return None;
                }
                self.notice = "Revalidating preview; nothing will be activated.";
                return Some(Operation::Revalidate(
                    wire::RevalidateConfigurationPlanRequest {
                        request_id: home_client_actions::new_request_id().ok()?.as_str().into(),
                        candidate: candidate.to_wire().into(),
                        name: plan.name.clone(),
                        etag: plan.etag.clone(),
                        ..Default::default()
                    },
                ));
            }
            ProAdminAction::Create
            | ProAdminAction::Save
            | ProAdminAction::Remove
            | ProAdminAction::Cancel
            | ProAdminAction::Enable
            | ProAdminAction::Disable
            | ProAdminAction::Revoke
            | ProAdminAction::CloseSecret
            | ProAdminAction::Resolve => (),
        }
        None
    }
    pub fn accept(&mut self, plan: wire::ConfigurationPlan) -> bool {
        if plan.family != self.family.to_wire() || plan.identity != self.identity {
            self.failed();
            return false;
        }
        self.notice = match plan.state {
            EnumValue::Known(wire::ConfigurationPlanState::Admitted) => {
                "Preview accepted. Its receipt is diagnostic only; no configuration is activated."
            }
            EnumValue::Known(wire::ConfigurationPlanState::Rejected) => {
                "Candidate rejected. Review the diagnostics; nothing was activated."
            }
            EnumValue::Known(wire::ConfigurationPlanState::Stale) => {
                "Preview outdated. Reload the references and build a new preview."
            }
            EnumValue::Known(wire::ConfigurationPlanState::Unspecified) | EnumValue::Unknown(_) => {
                self.failed();
                return false;
            }
        };
        self.plan = Some(plan);
        true
    }
    pub fn failed(&mut self) {
        self.plan = None;
        self.notice = "Preview unavailable or unsupported. The candidate stays private; no activation was attempted.";
    }
    pub fn simulation_failed(&mut self) {
        self.simulation.clear();
        self.notice = "Simulation unavailable or unsupported. The preview remains separate; no activation was attempted.";
    }
    pub fn accept_simulation(
        &mut self,
        response: home_wire_contract::home::api::v2::SimulateConfigurationPlanResponse,
    ) {
        if self.simulation.accept(response) {
            self.notice = "Simulation report received. Actions are synthetic; no configuration was published and no device was validated.";
        } else {
            self.simulation_failed();
        }
    }
    fn input_complete(&self) -> bool {
        // Keep source-sized copies and wire admission off the keystroke path.
        let mut paths = std::collections::BTreeSet::new();
        !self.identity.is_empty()
            && self.identity.trim() == self.identity
            && !self.identity.contains(['\0', '\r', '\n'])
            && self.files.iter().all(|file| {
                !file.source.is_empty()
                    && !file.path.is_empty()
                    && file.path.trim() == file.path
                    && !file.path.contains(['\0', '\r', '\n'])
                    && paths.insert(file.path.as_str())
            })
            && paths.contains(self.entry.as_str())
    }
    pub fn actions(&self, available: bool) -> Vec<ProAdminActionRow> {
        let valid = available && self.input_complete();
        [
            (ProAdminAction::Preview, "Build Preview", valid),
            (
                ProAdminAction::Refresh,
                "Read File References",
                available && self.family != SourceFamily::Operational,
            ),
            (
                ProAdminAction::Restore,
                "Load History for Restore",
                available,
            ),
            (
                ProAdminAction::More,
                "Next History Page",
                available && self.history_more().is_some(),
            ),
            (
                ProAdminAction::Apply,
                "Confirm Applying This Candidate",
                available && self.can_apply(),
            ),
            (
                ProAdminAction::Rollback,
                "Confirm Selected Historical Restore",
                available && self.can_rollback(),
            ),
            (
                ProAdminAction::Simulate,
                "Simulate With Provided Test Doubles",
                valid && self.family.simulatable(),
            ),
            (
                ProAdminAction::Revalidate,
                "Revalidate Preview",
                valid
                    && self
                        .plan
                        .as_ref()
                        .is_some_and(|plan| !plan.receipt.is_empty()),
            ),
            (
                ProAdminAction::AddFile,
                "Add File",
                available && self.mutable() && self.files.len() < 64,
            ),
            (
                ProAdminAction::RemoveFile,
                "Remove This File",
                available && self.mutable() && self.files.len() > 1,
            ),
            (
                ProAdminAction::NextFile,
                "Next File",
                available && self.files.len() > 1,
            ),
            (
                ProAdminAction::Resolve,
                "Reload and Resolve References",
                available,
            ),
            (ProAdminAction::Cancel, "Close", true),
        ]
        .into_iter()
        .map(|(action, label, enabled)| ProAdminActionRow {
            action,
            label: label.into(),
            enabled,
            destructive: matches!(action, ProAdminAction::Apply | ProAdminAction::Rollback),
        })
        .collect()
    }
    pub fn fields(&self, editable: bool) -> Vec<ProAdminField> {
        let editable = editable && self.mutable();
        let mut fields = vec![
            field(Field::Family, self.family.label().into(), false),
            field(Field::Identity, self.identity.clone(), editable),
            field(Field::Entry, self.entry.clone(), editable),
            field(
                Field::Files,
                self.files
                    .iter()
                    .map(|file| {
                        format!(
                            "{}{} · {} bytes",
                            if file.key == self.selected {
                                "→ "
                            } else {
                                ""
                            },
                            file.path,
                            file.source.len()
                        )
                    })
                    .collect::<Vec<_>>()
                    .join("\n"),
                false,
            ),
            field(Field::Path, self.selected().path.clone(), editable),
            field(Field::Source, self.selected().source.to_string(), editable),
            field(
                Field::Captured,
                format!(
                    "SQL {} · {}\nActive {} · {}\nWorkspace: {}\n{}",
                    self.captured.sql().revision(),
                    render::hex(self.captured.sql().source().bytes()),
                    self.captured.active().revision(),
                    render::hex(self.captured.active().manifest().bytes()),
                    super::workspace_label(self.captured.workspace()),
                    if self.stale {
                        "Snapshot to Resolve"
                    } else {
                        "Initial or explicitly resolved snapshot"
                    }
                ),
                false,
            ),
        ];
        if let Some(plan) = &self.plan {
            let state = match plan.state {
                EnumValue::Known(wire::ConfigurationPlanState::Admitted) => "Accepted",
                EnumValue::Known(wire::ConfigurationPlanState::Rejected) => "Rejected",
                EnumValue::Known(wire::ConfigurationPlanState::Stale) => "Outdated",
                EnumValue::Known(wire::ConfigurationPlanState::Unspecified)
                | EnumValue::Unknown(_) => "Unsupported",
            };
            fields.push(field(Field::Plan,format!("{} · {}\nAuthor {}\nFamily {} · identity {}\nScope: {}\nSource {}\nSemantics {}\nGeneration {} · {}\nPlan workspace {}\nDiagnostic receipt: {}\nThis preview proves neither saving nor publication",state,plan.schema_version,plan.author,self.family.label(),plan.identity,effect(plan.effect),render::hex(&plan.source_sha256),plan.semantic_sha256.as_ref().map_or_else(||"missing".into(),|value|render::hex(value)),plan.generation_revision,render::hex(&plan.manifest_sha256),render::hex(&plan.observed_workspace_sha256),if plan.receipt.is_empty(){"missing"}else{"provided by the server"}),false));
            let kind = match plan.base.kind {
                EnumValue::Known(wire::ConfigurationPlanBaseKind::ActiveSql) => {
                    "Active SQL Version"
                }
                EnumValue::Known(wire::ConfigurationPlanBaseKind::ImmutableReference) => {
                    "Immutable Reference"
                }
                EnumValue::Known(wire::ConfigurationPlanBaseKind::NoActiveDocument) => {
                    "No active document"
                }
                EnumValue::Known(wire::ConfigurationPlanBaseKind::Unspecified)
                | EnumValue::Unknown(_) => "Unsupported",
            };
            fields.push(field(
                Field::Base,
                format!(
                    "{}\nRevision {}\nHash {}",
                    kind,
                    plan.base
                        .revision
                        .map_or_else(|| "missing".into(), |value| value.to_string()),
                    plan.base
                        .source_sha256
                        .as_ref()
                        .map_or_else(|| "missing".into(), |value| render::hex(value))
                ),
                false,
            ));
            fields.push(field(
                Field::Changes,
                plan.changes
                    .iter()
                    .map(|change| {
                        format!(
                            "{} · {} · {} · references +{} / -{}",
                            match change.operation {
                                EnumValue::Known(wire::ConfigurationPlanChangeOperation::Added) =>
                                    "Added",
                                EnumValue::Known(
                                    wire::ConfigurationPlanChangeOperation::Removed,
                                ) => "Removed",
                                EnumValue::Known(
                                    wire::ConfigurationPlanChangeOperation::Replaced,
                                ) => "Replaced",
                                EnumValue::Known(
                                    wire::ConfigurationPlanChangeOperation::Unspecified,
                                )
                                | EnumValue::Unknown(_) => "Unsupported",
                            },
                            change.field,
                            effect(change.effect),
                            change.references_added,
                            change.references_removed
                        )
                    })
                    .collect::<Vec<_>>()
                    .join("\n"),
                false,
            ));
            fields.push(field(
                Field::Diagnostics,
                plan.diagnostics
                    .iter()
                    .map(|code| match code {
                        EnumValue::Known(wire::ConfigurationPlanDiagnosticCode::InvalidSchema) => {
                            "Invalid schema"
                        }
                        EnumValue::Known(
                            wire::ConfigurationPlanDiagnosticCode::ExcludedReference,
                        ) => "Excluded Reference",
                        EnumValue::Known(
                            wire::ConfigurationPlanDiagnosticCode::UnsupportedSource,
                        ) => "Unsupported source",
                        EnumValue::Known(wire::ConfigurationPlanDiagnosticCode::Capacity) => {
                            "Capacity exceeded"
                        }
                        EnumValue::Known(
                            wire::ConfigurationPlanDiagnosticCode::BaseUnavailable,
                        ) => "Baseline unavailable",
                        EnumValue::Known(wire::ConfigurationPlanDiagnosticCode::Unspecified)
                        | EnumValue::Unknown(_) => "Unsupported diagnostic",
                    })
                    .collect::<Vec<_>>()
                    .join("\n"),
                false,
            ));
        }
        if self.family.simulatable() {
            fields.extend(self.simulation.fields(editable));
        }
        fields.extend(self.activation_fields(editable));
        fields
    }
    pub fn choices(&self, editable: bool) -> Vec<ProAdminChoice> {
        let mut values: Vec<_> = SourceFamily::ALL
            .into_iter()
            .enumerate()
            .map(|(index, family)| ProAdminChoice {
                token: i32::try_from(index + 1).expect("nine families"),
                field_token: Field::Family as i32,
                label: family.label().into(),
                selected: family == self.family,
                enabled: editable && self.mutable(),
            })
            .collect();
        values.extend(self.activation_choices(editable));
        values
    }
}
fn effect(value: EnumValue<wire::ConfigurationPlanEffect>) -> &'static str {
    match value {
        EnumValue::Known(wire::ConfigurationPlanEffect::None) => "No changes",
        EnumValue::Known(wire::ConfigurationPlanEffect::Dashboard) => "Dashboard",
        EnumValue::Known(wire::ConfigurationPlanEffect::Configuration) => "Server Configuration",
        EnumValue::Known(wire::ConfigurationPlanEffect::HumanRights) => "User Permissions",
        EnumValue::Known(wire::ConfigurationPlanEffect::TechnicalOwnership) => {
            "Technical Ownership"
        }
        EnumValue::Known(wire::ConfigurationPlanEffect::RunTargets) => "Logic Execution Targets",
        EnumValue::Known(wire::ConfigurationPlanEffect::Unspecified) | EnumValue::Unknown(_) => {
            "Unsupported scope"
        }
    }
}
fn field(field: Field, value: String, editable: bool) -> ProAdminField {
    let (kind, description) = match field {
        Field::Source => (
            ProAdminFieldKind::Multiline,
            "Explicitly provided bytes; never reconstructed from metadata",
        ),
        Field::Identity | Field::Entry | Field::Path => (
            ProAdminFieldKind::Plain,
            "Value Explicitly Provided for This Candidate",
        ),
        Field::Family => (
            ProAdminFieldKind::Plain,
            "Existing compiler selected for this candidate",
        ),
        Field::Files => (
            ProAdminFieldKind::Multiline,
            "Files provided for this candidate, limited to 64 files and 1 MiB total",
        ),
        Field::Plan | Field::Base | Field::Changes | Field::Diagnostics => (
            ProAdminFieldKind::Multiline,
            "Server-provided diagnostic result; does not authorize activation",
        ),
        Field::Captured => (
            ProAdminFieldKind::Multiline,
            "Separate SQL references, active manifest and observed files",
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

#[cfg(test)]
mod tests {
    use super::*;
    pub(super) fn editor() -> Editor {
        let capture = Capture::admit(super::super::workflow::tests::response(7, 6)).unwrap();
        let mut editor = Editor::open(capture);
        editor.choose(Field::Family as i32, 6);
        editor.edit(Field::Identity as i32, "synthetic-preview".into());
        editor.edit(Field::Entry as i32, "main.yaml".into());
        editor.edit(Field::Path as i32, "main.yaml".into());
        editor.edit(Field::Source as i32, "views: []\n".into());
        editor
    }
    pub(super) fn plan() -> wire::ConfigurationPlan {
        wire::ConfigurationPlan {
            name: "homes/test/configurationPlans/p-synthetic".into(),
            etag: "synthetic-plan-etag".into(),
            create_time: buffa_types::google::protobuf::Timestamp {
                seconds: 1700000000,
                ..Default::default()
            }
            .into(),
            update_time: buffa_types::google::protobuf::Timestamp {
                seconds: 1700000000,
                ..Default::default()
            }
            .into(),
            expire_time: buffa_types::google::protobuf::Timestamp {
                seconds: 1700000600,
                ..Default::default()
            }
            .into(),

            state: wire::ConfigurationPlanState::Admitted.into(),
            author: "homes/synthetic/accounts/admin".into(),
            family: wire::ConfigurationPlanSourceFamily::Dashboard.into(),
            identity: "synthetic-preview".into(),
            schema_version: "homeward-dashboard-1".into(),
            source_sha256: vec![1; 32],
            semantic_sha256: Some(vec![5; 32]),
            base: wire::ConfigurationPlanBase {
                kind: wire::ConfigurationPlanBaseKind::ActiveSql.into(),
                revision: Some(7),
                source_sha256: Some(vec![1; 32]),
                ..Default::default()
            }
            .into(),
            generation_revision: 6,
            manifest_sha256: vec![4; 32],
            observed_workspace_sha256: vec![2; 32],
            effect: wire::ConfigurationPlanEffect::None.into(),
            receipt: "r".repeat(44),
            ..Default::default()
        }
    }
    #[test]
    fn an_unavailable_simulation_does_not_erase_a_distinct_diagnostic_plan() {
        let mut editor = editor();
        editor.choose(Field::Family as i32, 8);
        let mut plan = plan();
        plan.family = wire::ConfigurationPlanSourceFamily::Script.into();
        assert!(editor.accept(plan));
        editor.simulation_failed();
        assert!(editor.plan.is_some());
        editor.edit(Field::Source as i32, "sequence: []\n".into());
        assert!(editor.plan.is_none());
        assert!(matches!(
            editor.action(ProAdminAction::Simulate),
            Some(Operation::Simulate(_))
        ));
    }
    #[test]
    fn multi_file_editing_keeps_exact_bytes_and_bounds_total_during_input() {
        let mut editor = editor();
        assert!(editor.action(ProAdminAction::AddFile).is_none());
        editor.edit(Field::Path as i32, "included.yaml".into());
        editor.edit(Field::Source as i32, "x".repeat(MAX_BYTES));
        assert!(editor.selected().source.is_empty());
        editor.edit(Field::Source as i32, "# supplied include\n".into());
        let candidate = editor.candidate().unwrap().to_wire();
        assert_eq!(candidate.files.len(), 2);
        assert_eq!(candidate.files[0].exact_source, b"views: []\n");
        assert_eq!(candidate.files[1].exact_source, b"# supplied include\n");
        let removed = editor.selected;
        editor.action(ProAdminAction::RemoveFile);
        editor.action(ProAdminAction::AddFile);
        assert_ne!(editor.selected, removed);
    }
    #[test]
    fn duplicate_or_missing_entry_files_cannot_enqueue_an_opaque_preview() {
        let mut editor = editor();
        editor.edit(Field::Entry as i32, "missing.yaml".into());
        assert!(editor.action(ProAdminAction::Preview).is_none());
        editor.edit(Field::Entry as i32, "main.yaml".into());
        editor.action(ProAdminAction::AddFile);
        editor.edit(Field::Path as i32, "main.yaml".into());
        editor.edit(Field::Source as i32, "other bytes".into());
        assert!(editor.action(ProAdminAction::Preview).is_none());
    }
    #[test]
    fn a_source_change_retires_the_previous_plan_and_receipt() {
        let mut editor = editor();
        let Some(Operation::Build(request)) = editor.action(ProAdminAction::Preview) else {
            panic!("build")
        };
        assert_eq!(
            request.configuration_plan.candidate.files[0].exact_source,
            b"views: []\n"
        );
        let response = plan();
        home_wire_contract::config_plans::admit_plan_response(
            home_wire_contract::config_plans::PlanResponse::Build(&response),
        )
        .unwrap();
        assert!(editor.accept(response));
        let Some(Operation::Revalidate(request)) = editor.action(ProAdminAction::Revalidate) else {
            panic!("revalidate")
        };
        assert_eq!(request.receipt, "r".repeat(44));
        assert_eq!(
            request.configuration_plan.candidate.files[0].exact_source,
            b"views: []\n"
        );
        editor.edit(Field::Source as i32, "# replacement\nviews: []\n".into());
        assert!(editor.plan.is_none());
        assert!(editor.action(ProAdminAction::Revalidate).is_none());
        assert!(matches!(
            editor.action(ProAdminAction::Preview),
            Some(Operation::Build(_))
        ));
    }
    #[test]
    fn a_snapshot_change_preserves_inputs_and_requires_explicit_reference_resolution() {
        let mut editor = editor();
        assert!(editor.accept(plan()));
        let capture = Capture::admit(super::super::workflow::tests::response(9, 9)).unwrap();
        editor.observe(&capture);
        assert!(editor.stale);
        assert_eq!(editor.captured.sql().revision(), 7);
        assert_eq!(&*editor.selected().source, "views: []\n");
        editor.resolve(capture);
        assert!(!editor.stale);
        assert!(editor.plan.is_none());
        assert_eq!(editor.captured.sql().revision(), 9);
        assert_eq!(&*editor.selected().source, "views: []\n");
        let mut foreign = plan();
        foreign.identity = "another-preview".into();
        assert!(!editor.accept(foreign));
        assert!(editor.plan.is_none());
    }
}
