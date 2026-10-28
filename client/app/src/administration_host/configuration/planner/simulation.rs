//! Synthetic simulation inputs: no catalogue lookup, script engine or driver.
use crate::{ProAdminField, ProAdminFieldKind, administration::Refusal, configuration::Candidate};
use buffa::EnumValue;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{
    configuration_simulation_contract as contract, home::configurationsimulation::v1 as wire,
};
use serde::Deserialize;
use zeroize::Zeroizing;

#[derive(Clone, Copy)]
#[repr(i32)]
pub(in crate::administration_host::configuration) enum Field {
    Now = 2000,
    TimeZone = 2001,
    Limit = 2002,
    Fixtures = 2003,
    Events = 2004,
    Report = 2005,
    Intents = 2006,
    Nodes = 2007,
    Diagnostics = 2008,
    Limitations = 2009,
}
impl Field {
    pub fn admit(token: i32) -> Option<Self> {
        match token {
            2000 => Some(Self::Now),
            2001 => Some(Self::TimeZone),
            2002 => Some(Self::Limit),
            2003 => Some(Self::Fixtures),
            2004 => Some(Self::Events),
            2005 => Some(Self::Report),
            2006 => Some(Self::Intents),
            2007 => Some(Self::Nodes),
            2008 => Some(Self::Diagnostics),
            2009 => Some(Self::Limitations),
            _ => None,
        }
    }
    fn label(self) -> &'static str {
        match self {
            Self::Now => "Instant virtuel (RFC3339)",
            Self::TimeZone => "Fuseau horaire virtuel",
            Self::Limit => "Limite virtuelle en millisecondes",
            Self::Fixtures => "États et capacités synthétiques (JSON)",
            Self::Events => "Types d’événements synthétiques, un par ligne",
            Self::Report => "Résultat de simulation",
            Self::Intents => "Intentions synthétiques",
            Self::Nodes => "Trace d’exécution virtuelle",
            Self::Diagnostics => "Diagnostics de simulation",
            Self::Limitations => "Limites annoncées par le serveur",
        }
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    alias: String,
    state: Option<String>,
    #[serde(default)]
    attributes: Vec<Attribute>,
    #[serde(default)]
    supports: Vec<Support>,
    #[serde(default)]
    refuse_intent: bool,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Attribute {
    name: String,
    value: Value,
}
#[derive(Deserialize)]
#[serde(
    tag = "kind",
    content = "value",
    rename_all = "snake_case",
    deny_unknown_fields
)]
enum Value {
    Text(String),
    Boolean(bool),
    Integer(i64),
    Number(f64),
    Null,
}
#[derive(Deserialize)]
#[serde(rename_all = "snake_case")]
enum Support {
    ObserveState,
    ClimateSetpoint,
    SwitchTurnOn,
    LightTurnOn,
    LightTurnOff,
    LightBrightnessAbsolute,
    LightBrightnessRelative,
    LightTransition,
    SceneTurnOn,
    SceneTransition,
}
impl Support {
    fn into_wire(self) -> wire::Support {
        match self {
            Self::ObserveState => wire::Support::ObserveState,
            Self::ClimateSetpoint => wire::Support::ClimateSetpoint,
            Self::SwitchTurnOn => wire::Support::SwitchTurnOn,
            Self::LightTurnOn => wire::Support::LightTurnOn,
            Self::LightTurnOff => wire::Support::LightTurnOff,
            Self::LightBrightnessAbsolute => wire::Support::LightBrightnessAbsolute,
            Self::LightBrightnessRelative => wire::Support::LightBrightnessRelative,
            Self::LightTransition => wire::Support::LightTransition,
            Self::SceneTurnOn => wire::Support::SceneTurnOn,
            Self::SceneTransition => wire::Support::SceneTransition,
        }
    }
}
impl Fixture {
    fn into_wire(self) -> wire::Fixture {
        wire::Fixture {
            alias: self.alias,
            state: self.state,
            attributes: self
                .attributes
                .into_iter()
                .map(|attribute| wire::Attribute {
                    name: attribute.name,
                    value: Some(match attribute.value {
                        Value::Text(value) => wire::attribute::Value::Text(value),
                        Value::Boolean(value) => wire::attribute::Value::Boolean(value),
                        Value::Integer(value) => wire::attribute::Value::Integer(value),
                        Value::Number(value) => wire::attribute::Value::Number(value),
                        Value::Null => wire::attribute::Value::NullValue(true),
                    }),
                    ..Default::default()
                })
                .collect(),
            supports: self
                .supports
                .into_iter()
                .map(|value| value.into_wire().into())
                .collect(),
            refuse_intent: self.refuse_intent,
            ..Default::default()
        }
    }
}
pub(super) struct Simulation {
    now: String,
    time_zone: String,
    limit: String,
    fixtures: Zeroizing<String>,
    events: Zeroizing<String>,
    report: Option<wire::Report>,
}
impl Default for Simulation {
    fn default() -> Self {
        Self {
            now: chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true),
            time_zone: "UTC".into(),
            limit: "1000".into(),
            fixtures: Zeroizing::new("[]".into()),
            events: Zeroizing::new(String::new()),
            report: None,
        }
    }
}
impl Simulation {
    #[cfg(test)]
    pub fn report(&self) -> Option<&wire::Report> {
        self.report.as_ref()
    }
    pub fn clear(&mut self) {
        self.report = None;
    }
    pub fn edit(&mut self, field: Field, text: String) {
        let text = Zeroizing::new(text);
        match field {
            Field::Now => {
                if text.len() > 64 {
                    return;
                }
                self.now = text.to_string();
            }
            Field::TimeZone => {
                if text.len() > 128 {
                    return;
                }
                self.time_zone = text.to_string();
            }
            Field::Limit => {
                if text.len() > 10 {
                    return;
                }
                self.limit = text.to_string();
            }
            Field::Fixtures => {
                if text.len() > 65_536 {
                    return;
                }
                self.fixtures = text;
            }
            Field::Events => {
                if text.len() > 16_384 {
                    return;
                }
                self.events = text;
            }
            Field::Report
            | Field::Intents
            | Field::Nodes
            | Field::Diagnostics
            | Field::Limitations => return,
        }
        self.clear();
    }
    pub fn request(&self, candidate: &Candidate) -> Result<wire::SimulatePlanRequest, Refusal> {
        if !candidate.family().simulatable() {
            return Err(Refusal::Unsupported);
        }
        let fixtures: Vec<Fixture> =
            serde_json::from_str(&self.fixtures).map_err(|_| Refusal::Invalid)?;
        let now = chrono::DateTime::parse_from_rfc3339(&self.now).map_err(|_| Refusal::Invalid)?;
        let request = wire::SimulatePlanRequest {
            candidate: candidate.to_wire().into(),
            fixtures: fixtures.into_iter().map(Fixture::into_wire).collect(),
            event_types: self
                .events
                .lines()
                .filter(|value| !value.is_empty())
                .map(str::to_owned)
                .collect(),
            now: Timestamp {
                seconds: now.timestamp(),
                nanos: now.timestamp_subsec_nanos() as i32,
                ..Default::default()
            }
            .into(),
            time_zone: self.time_zone.clone(),
            virtual_limit_millis: self.limit.parse().map_err(|_| Refusal::Invalid)?,
            ..Default::default()
        };
        contract::admit_simulation_request(&request).map_err(|_| Refusal::Invalid)?;
        Ok(request)
    }
    pub fn accept(&mut self, response: wire::SimulatePlanResponse) -> bool {
        if contract::admit_simulation_response(&response).is_err() {
            self.clear();
            return false;
        }
        self.report = response.report.into_option();
        true
    }
    pub fn fields(&self, editable: bool) -> Vec<ProAdminField> {
        let mut fields = vec![
            field(Field::Now, self.now.clone(), editable),
            field(Field::TimeZone, self.time_zone.clone(), editable),
            field(Field::Limit, self.limit.clone(), editable),
            field(Field::Fixtures, self.fixtures.to_string(), editable),
            field(Field::Events, self.events.to_string(), editable),
        ];
        if let Some(report) = &self.report {
            let status = match report.status {
                EnumValue::Known(wire::Status::Completed) => "Terminée",
                EnumValue::Known(wire::Status::Partial) => "Partielle",
                EnumValue::Known(wire::Status::Failed) => "Échouée",
                EnumValue::Known(wire::Status::Unspecified) | EnumValue::Unknown(_) => {
                    "Non prise en charge"
                }
            };
            fields.push(field(Field::Report,format!("{} · {} ms virtuelles\nSimulation uniquement ; aucun appareil physique validé\n{} intentions · {} étapes · {} diagnostics",status,report.elapsed_millis,report.intents.len(),report.nodes.len(),report.diagnostics.len()),false));
            fields.push(field(
                Field::Intents,
                report
                    .intents
                    .iter()
                    .map(|intent| {
                        format!(
                            "{} · {} · {} · {}\n{}",
                            intent.alias,
                            match &intent.parameters {
                                Some(wire::intent::Parameters::Switch(_)) => "Interrupteur",
                                Some(wire::intent::Parameters::Light(_)) => "Lumière",
                                Some(wire::intent::Parameters::Climate(_)) => "Climat",
                                Some(wire::intent::Parameters::Fan(_)) => "Ventilateur",
                                None => "Paramètres absents",
                            },
                            match intent.outcome {
                                EnumValue::Known(wire::IntentOutcome::Recorded) =>
                                    "Enregistrée dans la simulation",
                                EnumValue::Known(wire::IntentOutcome::Refused) =>
                                    "Refus synthétique",
                                EnumValue::Known(wire::IntentOutcome::Unspecified)
                                | EnumValue::Unknown(_) => "Non prise en charge",
                            },
                            super::render::at(&intent.at),
                            position(&intent.position)
                        )
                    })
                    .collect::<Vec<_>>()
                    .join("\n"),
                false,
            ));
            fields.push(field(
                Field::Nodes,
                report
                    .nodes
                    .iter()
                    .map(|node| {
                        format!(
                            "{} · {} · {}\n{}",
                            action(node.action),
                            stage(node.stage),
                            super::render::at(&node.at),
                            position(&node.position)
                        )
                    })
                    .collect::<Vec<_>>()
                    .join("\n"),
                false,
            ));
            fields.push(field(
                Field::Diagnostics,
                report
                    .diagnostics
                    .iter()
                    .map(|value| {
                        format!("{} · {}", diagnostic(value.code), position(&value.position))
                    })
                    .collect::<Vec<_>>()
                    .join("\n"),
                false,
            ));
            fields.push(field(
                Field::Limitations,
                report.limitations.join("\n"),
                false,
            ));
        }
        fields
    }
}
fn position(value: &wire::Position) -> String {
    format!(
        "{}:{}:{} · {}",
        value.file, value.line, value.column, value.path
    )
}
fn diagnostic(value: EnumValue<wire::DiagnosticCode>) -> &'static str {
    match value {
        EnumValue::Known(wire::DiagnosticCode::UnsupportedExtension) => {
            "Extension non prise en charge"
        }
        EnumValue::Known(wire::DiagnosticCode::UnsupportedConcurrency) => {
            "Concurrence non prise en charge"
        }
        EnumValue::Known(wire::DiagnosticCode::UnsupportedWait) => "Attente non prise en charge",
        EnumValue::Known(wire::DiagnosticCode::InvalidSource) => "Source invalide",
        EnumValue::Known(wire::DiagnosticCode::MissingFixture) => "État synthétique manquant",
        EnumValue::Known(wire::DiagnosticCode::FixtureRefused) => "Intention refusée par le double",
        EnumValue::Known(wire::DiagnosticCode::ExecutionFailed) => "Exécution virtuelle échouée",
        EnumValue::Known(wire::DiagnosticCode::CapacityExceeded) => "Capacité dépassée",
        EnumValue::Known(wire::DiagnosticCode::Unspecified) | EnumValue::Unknown(_) => {
            "Diagnostic non pris en charge"
        }
    }
}
fn action(value: EnumValue<wire::ActionKind>) -> &'static str {
    match value {
        EnumValue::Known(wire::ActionKind::Variables) => "Variables",
        EnumValue::Known(wire::ActionKind::Sequence) => "Séquence",
        EnumValue::Known(wire::ActionKind::Choose) => "Choix",
        EnumValue::Known(wire::ActionKind::If) => "Condition",
        EnumValue::Known(wire::ActionKind::Repeat) => "Répétition",
        EnumValue::Known(wire::ActionKind::Parallel) => "Branches parallèles",
        EnumValue::Known(wire::ActionKind::Wait) => "Attente",
        EnumValue::Known(wire::ActionKind::Stop) => "Arrêt",
        EnumValue::Known(wire::ActionKind::Call) => "Appel",
        EnumValue::Known(wire::ActionKind::Delay) => "Délai",
        EnumValue::Known(wire::ActionKind::WaitTemplate) => "Attente d’expression",
        EnumValue::Known(wire::ActionKind::Unspecified) | EnumValue::Unknown(_) => {
            "Étape non prise en charge"
        }
    }
}
fn stage(value: EnumValue<wire::NodeStage>) -> &'static str {
    match value {
        EnumValue::Known(wire::NodeStage::Entered) => "Démarrée",
        EnumValue::Known(wire::NodeStage::Completed) => "Terminée",
        EnumValue::Known(wire::NodeStage::Stopped) => "Arrêtée",
        EnumValue::Known(wire::NodeStage::Cancelled) => "Annulée",
        EnumValue::Known(wire::NodeStage::Failed) => "Échouée",
        EnumValue::Known(wire::NodeStage::Waiting) => "En attente",
        EnumValue::Known(wire::NodeStage::Unspecified) | EnumValue::Unknown(_) => {
            "État non pris en charge"
        }
    }
}
fn field(field: Field, value: String, editable: bool) -> ProAdminField {
    let (kind, description) = match field {
        Field::Now | Field::TimeZone | Field::Limit => (
            ProAdminFieldKind::Plain,
            "Horloge virtuelle ; aucune horloge d’appareil réel n’est modifiée",
        ),
        Field::Fixtures => (
            ProAdminFieldKind::Multiline,
            "Tableau de doubles avec alias, état facultatif, attributs typés et capacités déclarées ; aucune lecture d’appareil réel",
        ),
        Field::Events => (
            ProAdminFieldKind::Multiline,
            "Événements fournis uniquement à la simulation",
        ),
        Field::Report | Field::Intents | Field::Nodes | Field::Diagnostics | Field::Limitations => {
            (
                ProAdminFieldKind::Multiline,
                "Rapport du serveur ; aucune commande physique ni publication de configuration",
            )
        }
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
    use crate::configuration::{SourceFamily, SourceFile, SourceIdentity, UserSource};
    fn candidate() -> Candidate {
        Candidate::supplied(
            SourceFamily::Script,
            SourceIdentity::parse("synthetic.script").unwrap(),
            "main.yaml",
            vec![
                SourceFile::supplied(
                    "main.yaml",
                    UserSource::supplied(b"sequence: []\n".to_vec()).unwrap(),
                )
                .unwrap(),
            ],
        )
        .unwrap()
    }
    fn response() -> wire::SimulatePlanResponse {
        wire::SimulatePlanResponse {
            report: wire::Report {
                simulated: true,
                status: wire::Status::Completed.into(),
                limitations: vec!["Synthetic simulation only".into()],
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }
    }
    #[test]
    fn declared_inputs_keep_unobserved_state_distinct_and_unknown_capabilities_are_rejected() {
        let mut settings = Simulation::default();
        settings.edit(
            Field::Fixtures,
            r#"[{"alias":"synthetic.switch","supports":["switch_turn_on"]}]"#.into(),
        );
        let request = settings.request(&candidate()).unwrap();
        assert!(request.fixtures[0].state.is_none());
        assert_eq!(
            request.fixtures[0].supports,
            vec![EnumValue::Known(wire::Support::SwitchTurnOn)]
        );
        settings.edit(
            Field::Fixtures,
            r#"[{"alias":"synthetic.switch","supports":["physical_driver"]}]"#.into(),
        );
        assert!(settings.request(&candidate()).is_err());
        settings.edit(
            Field::Fixtures,
            r#"[{"alias":"synthetic.switch","state":"off","hidden":true}]"#.into(),
        );
        assert!(settings.request(&candidate()).is_err());
    }
    #[test]
    fn absent_observations_and_virtual_limits_are_admitted_before_any_rpc() {
        let mut settings = Simulation::default();
        settings.edit(Field::Fixtures,r#"[{"alias":"synthetic.switch","attributes":[{"name":"present","value":{"kind":"boolean","value":true}}]}]"#.into());
        assert!(settings.request(&candidate()).is_err());
        settings.edit(Field::Fixtures,r#"[{"alias":"synthetic.switch","state":"off","attributes":[{"name":"present","value":{"kind":"boolean","value":true}}]}]"#.into());
        assert!(settings.request(&candidate()).is_ok());
        settings.edit(Field::Limit, "3600001".into());
        assert!(settings.request(&candidate()).is_err());
        settings.edit(Field::Limit, "1000".into());
        settings.edit(Field::Now, "2099-02-30T00:00:00Z".into());
        assert!(settings.request(&candidate()).is_err());
    }
    #[test]
    fn reports_cannot_claim_physical_validation_and_an_input_edit_retires_them() {
        let mut settings = Simulation::default();
        assert!(settings.accept(response()));
        assert!(settings.report.is_some());
        settings.edit(Field::TimeZone, "America/Toronto".into());
        assert!(settings.report.is_none());
        let mut invalid = response();
        invalid.report.as_option_mut().unwrap().hardware_validated = true;
        assert!(!settings.accept(invalid));
        assert!(settings.report.is_none());
    }
}
