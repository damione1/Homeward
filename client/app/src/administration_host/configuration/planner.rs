//! Explicit multi-file inputs and diagnostic previews. A plan never authorizes
//! a mutation and never supplies editable bytes to a source field.
mod simulation;
use super::render;
use crate::{
    ProAdminAction, ProAdminActionRow, ProAdminChoice, ProAdminField, ProAdminFieldKind,
    configuration::{Candidate, Capture, SourceFamily, SourceFile, SourceIdentity, UserSource},
};
use buffa::EnumValue;
use home_wire_contract::home::configurationplan::v1 as wire;
use zeroize::Zeroizing;

const MAX_BYTES: usize = 1_048_576;
#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(i32)]
enum Field {
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
            Self::Family => "Famille de source",
            Self::Identity => "Identité logique",
            Self::Entry => "Fichier d’entrée",
            Self::Path => "Chemin logique du fichier sélectionné",
            Self::Source => "Octets exacts fournis",
            Self::Files => "Fichiers du candidat",
            Self::Plan => "Aperçu du serveur",
            Self::Base => "Base de comparaison du plan",
            Self::Changes => "Changements annoncés",
            Self::Diagnostics => "Diagnostics du plan",
            Self::Captured => "Références capturées",
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct FileKey(u64);
struct File {
    key: FileKey,
    path: String,
    source: Zeroizing<String>,
}
impl File {
    fn empty(key: FileKey) -> Self {
        Self {
            key,
            path: String::new(),
            source: Zeroizing::new(String::new()),
        }
    }
}
pub(super) enum Operation {
    Build(wire::BuildPlanRequest),
    Revalidate(wire::RevalidatePlanRequest),
    Simulate(home_wire_contract::home::configurationsimulation::v1::SimulatePlanRequest),
}
pub(super) struct Editor {
    simulation: simulation::Simulation,
    captured: Capture,
    family: SourceFamily,
    identity: String,
    entry: String,
    files: Vec<File>,
    selected: FileKey,
    next_file: Option<u64>,
    plan: Option<wire::Plan>,
    stale: bool,
    notice: &'static str,
}
impl Editor {
    pub fn open(captured: Capture) -> Self {
        Self {
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
            notice: "Fournissez vos fichiers exacts. Construire ou revalider un aperçu n’active aucune configuration.",
        }
    }
    pub fn observe(&mut self, current: &Capture) {
        if self.captured.sql() != current.sql()
            || self.captured.active() != current.active()
            || self.captured.workspace() != current.workspace()
        {
            self.stale = true;
            self.notice = "Les références ont changé. Le candidat est conservé ; relisez et résolvez explicitement avant une future activation.";
        }
    }
    pub fn resolve(&mut self, current: Capture) {
        self.simulation.clear();
        self.captured = current;
        self.stale = false;
        self.plan = None;
        self.notice =
            "Références relues explicitement. Construisez un nouvel aperçu pour ces fichiers.";
    }
    pub fn notice(&self) -> &'static str {
        self.notice
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
        self.notice =
            "Candidat modifié ; l’aperçu précédent a été retiré. Aucun fichier n’a été envoyé.";
    }
    pub fn edit(&mut self, token: i32, text: String) {
        if let Some(field) = simulation::Field::admit(token) {
            self.simulation.edit(field, text);
            self.notice = "Paramètres de simulation modifiés ; le rapport précédent a été retiré. Aucun appareil n’a été interrogé.";
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
                self.files
                    .iter_mut()
                    .find(|file| file.key == self.selected)
                    .expect("selected file")
                    .path = text.to_string();
            }
            Field::Source => {
                let other_bytes: usize = self
                    .files
                    .iter()
                    .filter(|file| file.key != self.selected)
                    .map(|file| file.source.len())
                    .sum();
                if text.len() > MAX_BYTES - other_bytes {
                    self.notice = "Le candidat dépasse la limite totale de 1 Mio ; la saisie précédente est conservée.";
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
        match action {
            ProAdminAction::Simulate => {
                let request = self
                    .candidate()
                    .and_then(|candidate| self.simulation.request(&candidate));
                match request {
                    Ok(request) => {
                        self.notice = "Simulation en cours avec vos états synthétiques ; aucune commande physique.";
                        return Some(Operation::Simulate(request));
                    }
                    Err(_) => {
                        self.notice = "Simulation non envoyée : vérifiez la famille logique, les fichiers, l’horloge, les événements et les doubles JSON.";
                        return None;
                    }
                }
            }
            ProAdminAction::AddFile => {
                if self.files.len() >= 64 {
                    self.notice = "Le candidat est limité à 64 fichiers.";
                    return None;
                }
                let Some(next) = self.next_file else {
                    self.notice = "Impossible d’ouvrir un autre fichier dans ce formulaire.";
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
                    self.notice = "Complétez l’identité, l’entrée et chaque fichier ; chemins uniques, entrée présente, total maximal 1 Mio.";
                    return None;
                };
                if action == ProAdminAction::Preview {
                    self.notice = "Construction de l’aperçu ; aucune activation.";
                    return Some(Operation::Build(candidate.build_request()));
                }
                let Some(plan) = &self.plan else {
                    return None;
                };
                if plan.receipt.is_empty() {
                    return None;
                }
                self.notice = "Revalidation de l’aperçu ; aucune activation.";
                return Some(Operation::Revalidate(wire::RevalidatePlanRequest {
                    candidate: candidate.to_wire().into(),
                    receipt: plan.receipt.clone(),
                    ..Default::default()
                }));
            }
            ProAdminAction::Refresh
            | ProAdminAction::Create
            | ProAdminAction::Save
            | ProAdminAction::Remove
            | ProAdminAction::Cancel
            | ProAdminAction::More
            | ProAdminAction::Enable
            | ProAdminAction::Disable
            | ProAdminAction::Revoke
            | ProAdminAction::CloseSecret
            | ProAdminAction::Apply
            | ProAdminAction::Restore
            | ProAdminAction::Rollback
            | ProAdminAction::Resolve => (),
        }
        None
    }
    pub fn accept(&mut self, plan: wire::Plan) -> bool {
        if plan.family != self.family.to_wire() || plan.identity != self.identity {
            self.failed();
            return false;
        }
        self.notice = match plan.state {
            EnumValue::Known(wire::PlanState::Admitted) => {
                "Aperçu admis. Son reçu est diagnostique ; aucune configuration n’est activée."
            }
            EnumValue::Known(wire::PlanState::Rejected) => {
                "Candidat rejeté. Consultez les diagnostics ; aucune activation."
            }
            EnumValue::Known(wire::PlanState::Stale) => {
                "Aperçu périmé. Relisez les références et construisez un nouvel aperçu."
            }
            EnumValue::Known(wire::PlanState::Unspecified) | EnumValue::Unknown(_) => {
                self.failed();
                return false;
            }
        };
        self.plan = Some(plan);
        true
    }
    pub fn failed(&mut self) {
        self.plan = None;
        self.notice = "Aperçu indisponible ou non pris en charge. Le candidat reste privé ; aucune activation n’a été tentée.";
    }
    pub fn simulation_failed(&mut self) {
        self.simulation.clear();
        self.notice = "Simulation indisponible ou non prise en charge. L’aperçu reste distinct ; aucune activation n’a été tentée.";
    }
    pub fn accept_simulation(
        &mut self,
        response: home_wire_contract::home::configurationsimulation::v1::SimulatePlanResponse,
    ) {
        if self.simulation.accept(response) {
            self.notice = "Rapport de simulation reçu. Les intentions sont synthétiques ; aucune configuration publiée ni aucun appareil validé.";
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
            (ProAdminAction::Preview, "Construire l’aperçu", valid),
            (
                ProAdminAction::Simulate,
                "Simuler avec les doubles fournis",
                valid && self.family.simulatable(),
            ),
            (
                ProAdminAction::Revalidate,
                "Revalider l’aperçu",
                valid
                    && self
                        .plan
                        .as_ref()
                        .is_some_and(|plan| !plan.receipt.is_empty()),
            ),
            (
                ProAdminAction::AddFile,
                "Ajouter un fichier",
                available && self.files.len() < 64,
            ),
            (
                ProAdminAction::RemoveFile,
                "Retirer ce fichier",
                available && self.files.len() > 1,
            ),
            (
                ProAdminAction::NextFile,
                "Fichier suivant",
                available && self.files.len() > 1,
            ),
            (
                ProAdminAction::Resolve,
                "Relire et résoudre les références",
                available,
            ),
            (ProAdminAction::Cancel, "Fermer", true),
        ]
        .into_iter()
        .map(|(action, label, enabled)| ProAdminActionRow {
            action,
            label: label.into(),
            enabled,
            destructive: false,
        })
        .collect()
    }
    pub fn fields(&self, editable: bool) -> Vec<ProAdminField> {
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
                            "{}{} · {} octets",
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
                    "SQL {} · {}\nActif {} · {}\nWorkspace : {}\n{}",
                    self.captured.sql().revision(),
                    render::hex(self.captured.sql().source().bytes()),
                    self.captured.active().revision(),
                    render::hex(self.captured.active().manifest().bytes()),
                    self.captured.workspace().writable_hash().map_or_else(
                        || "non observé ou indisponible".into(),
                        |value| render::hex(value.bytes())
                    ),
                    if self.stale {
                        "Capture à résoudre"
                    } else {
                        "Capture initiale ou explicitement résolue"
                    }
                ),
                false,
            ),
        ];
        if let Some(plan) = &self.plan {
            let state = match plan.state {
                EnumValue::Known(wire::PlanState::Admitted) => "Admis",
                EnumValue::Known(wire::PlanState::Rejected) => "Rejeté",
                EnumValue::Known(wire::PlanState::Stale) => "Périmé",
                EnumValue::Known(wire::PlanState::Unspecified) | EnumValue::Unknown(_) => {
                    "Non pris en charge"
                }
            };
            fields.push(field(Field::Plan,format!("{} · {}\nAuteur {}\nFamille {} · identité {}\nSource {}\nSémantique {}\nGénération {} · {}\nWorkspace du plan {}\nReçu diagnostique : {}\nAucune activation",state,plan.schema_version,plan.author,self.family.label(),plan.identity,render::hex(&plan.source_sha256),plan.semantic_sha256.as_ref().map_or_else(||"absente".into(),|value|render::hex(value)),plan.generation_revision,render::hex(&plan.manifest_sha256),render::hex(&plan.observed_workspace_sha256),if plan.receipt.is_empty(){"absent"}else{"fourni par le serveur"}),false));
            let kind = match plan.base.kind {
                EnumValue::Known(wire::BaseKind::ActiveSql) => "Version SQL active",
                EnumValue::Known(wire::BaseKind::ImmutableReference) => "Référence immuable",
                EnumValue::Known(wire::BaseKind::NoActiveDocument) => "Aucun document actif",
                EnumValue::Known(wire::BaseKind::Unspecified) | EnumValue::Unknown(_) => {
                    "Non prise en charge"
                }
            };
            fields.push(field(
                Field::Base,
                format!(
                    "{}\nRévision {}\nHash {}",
                    kind,
                    plan.base
                        .revision
                        .map_or_else(|| "absente".into(), |value| value.to_string()),
                    plan.base
                        .source_sha256
                        .as_ref()
                        .map_or_else(|| "absent".into(), |value| render::hex(value))
                ),
                false,
            ));
            fields.push(field(
                Field::Changes,
                plan.changes
                    .iter()
                    .map(|change| {
                        format!(
                            "{} · {} · références +{} / -{}",
                            match change.operation {
                                EnumValue::Known(wire::ChangeOperation::Added) => "Ajout",
                                EnumValue::Known(wire::ChangeOperation::Removed) => "Retrait",
                                EnumValue::Known(wire::ChangeOperation::Replaced) => "Remplacement",
                                EnumValue::Known(wire::ChangeOperation::Unspecified)
                                | EnumValue::Unknown(_) => "Non pris en charge",
                            },
                            change.field,
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
                        EnumValue::Known(wire::DiagnosticCode::InvalidSchema) => "Schéma invalide",
                        EnumValue::Known(wire::DiagnosticCode::ExcludedReference) => {
                            "Référence exclue"
                        }
                        EnumValue::Known(wire::DiagnosticCode::UnsupportedSource) => {
                            "Source non prise en charge"
                        }
                        EnumValue::Known(wire::DiagnosticCode::Capacity) => "Capacité dépassée",
                        EnumValue::Known(wire::DiagnosticCode::BaseUnavailable) => {
                            "Base indisponible"
                        }
                        EnumValue::Known(wire::DiagnosticCode::Unspecified)
                        | EnumValue::Unknown(_) => "Diagnostic non pris en charge",
                    })
                    .collect::<Vec<_>>()
                    .join("\n"),
                false,
            ));
        }
        if self.family.simulatable() {
            fields.extend(self.simulation.fields(editable));
        }
        fields
    }
    pub fn choices(&self, editable: bool) -> Vec<ProAdminChoice> {
        SourceFamily::ALL
            .into_iter()
            .enumerate()
            .map(|(index, family)| ProAdminChoice {
                token: i32::try_from(index + 1).expect("nine families"),
                field_token: Field::Family as i32,
                label: family.label().into(),
                selected: family == self.family,
                enabled: editable,
            })
            .collect()
    }
}
fn field(field: Field, value: String, editable: bool) -> ProAdminField {
    let (kind, description) = match field {
        Field::Source => (
            ProAdminFieldKind::Multiline,
            "Octets fournis explicitement ; jamais reconstruits depuis les métadonnées",
        ),
        Field::Identity | Field::Entry | Field::Path => (
            ProAdminFieldKind::Plain,
            "Valeur fournie explicitement pour ce candidat",
        ),
        Field::Family => (
            ProAdminFieldKind::Plain,
            "Compilateur existant sélectionné pour ce candidat",
        ),
        Field::Files => (
            ProAdminFieldKind::Multiline,
            "Fichiers fournis pour ce candidat, limités à 64 et 1 Mio au total",
        ),
        Field::Plan | Field::Base | Field::Changes | Field::Diagnostics => (
            ProAdminFieldKind::Multiline,
            "Résultat diagnostique fourni par le serveur ; aucune autorisation d’activer",
        ),
        Field::Captured => (
            ProAdminFieldKind::Multiline,
            "Références SQL, manifeste actif et fichiers observés distinctes",
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
    fn editor() -> Editor {
        let capture = Capture::admit(super::super::workflow::tests::response(7, 6)).unwrap();
        let mut editor = Editor::open(capture);
        editor.choose(Field::Family as i32, 6);
        editor.edit(Field::Identity as i32, "synthetic-preview".into());
        editor.edit(Field::Entry as i32, "main.yaml".into());
        editor.edit(Field::Path as i32, "main.yaml".into());
        editor.edit(Field::Source as i32, "views: []\n".into());
        editor
    }
    fn plan() -> wire::Plan {
        wire::Plan {
            state: wire::PlanState::Admitted.into(),
            author: "synthetic-admin".into(),
            family: wire::SourceFamily::Dashboard.into(),
            identity: "synthetic-preview".into(),
            schema_version: "homeward-dashboard-1".into(),
            source_sha256: vec![1; 32],
            semantic_sha256: Some(vec![5; 32]),
            base: wire::Base {
                kind: wire::BaseKind::ActiveSql.into(),
                revision: Some(7),
                source_sha256: Some(vec![1; 32]),
                ..Default::default()
            }
            .into(),
            generation_revision: 6,
            manifest_sha256: vec![4; 32],
            observed_workspace_sha256: vec![2; 32],
            effect: wire::Effect::None.into(),
            receipt: "r".repeat(44),
            ..Default::default()
        }
    }
    #[test]
    fn an_unavailable_simulation_does_not_erase_a_distinct_diagnostic_plan() {
        let mut editor = editor();
        editor.choose(Field::Family as i32, 8);
        let mut plan = plan();
        plan.family = wire::SourceFamily::Script.into();
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
        assert_eq!(request.candidate.files[0].exact_source, b"views: []\n");
        let response = wire::BuildPlanResponse {
            plan: plan().into(),
            ..Default::default()
        };
        home_wire_contract::config_plans::admit_plan_response(
            home_wire_contract::config_plans::PlanResponse::Build(&response),
        )
        .unwrap();
        assert!(editor.accept(response.plan.into_option().unwrap()));
        let Some(Operation::Revalidate(request)) = editor.action(ProAdminAction::Revalidate) else {
            panic!("revalidate")
        };
        assert_eq!(request.receipt, "r".repeat(44));
        assert_eq!(request.candidate.files[0].exact_source, b"views: []\n");
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
