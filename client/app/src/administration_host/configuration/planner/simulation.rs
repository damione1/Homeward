//! Synthetic simulation inputs: no catalogue lookup, script engine or driver.
use crate::{ProAdminField, ProAdminFieldKind, administration::Refusal, configuration::Candidate};
use buffa::EnumValue;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{configuration_simulation_contract as contract, home::api::v2 as wire};
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
            Self::Now => "Virtual Instant (RFC3339)",
            Self::TimeZone => "Virtual Time Zone",
            Self::Limit => "Virtual Limit in Milliseconds",
            Self::Fixtures => "Synthetic States and Capabilities (JSON)",
            Self::Events => "Synthetic event types, one per line",
            Self::Report => "Simulation Result",
            Self::Intents => "Synthetic Actions",
            Self::Nodes => "Virtual Execution Trace",
            Self::Diagnostics => "Simulation Diagnostics",
            Self::Limitations => "Server-Reported Limits",
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
    fn into_wire(self) -> wire::ConfigurationSimulationSupport {
        match self {
            Self::ObserveState => wire::ConfigurationSimulationSupport::ObserveState,
            Self::ClimateSetpoint => wire::ConfigurationSimulationSupport::ClimateSetpoint,
            Self::SwitchTurnOn => wire::ConfigurationSimulationSupport::SwitchTurnOn,
            Self::LightTurnOn => wire::ConfigurationSimulationSupport::LightTurnOn,
            Self::LightTurnOff => wire::ConfigurationSimulationSupport::LightTurnOff,
            Self::LightBrightnessAbsolute => {
                wire::ConfigurationSimulationSupport::LightBrightnessAbsolute
            }
            Self::LightBrightnessRelative => {
                wire::ConfigurationSimulationSupport::LightBrightnessRelative
            }
            Self::LightTransition => wire::ConfigurationSimulationSupport::LightTransition,
            Self::SceneTurnOn => wire::ConfigurationSimulationSupport::SceneTurnOn,
            Self::SceneTransition => wire::ConfigurationSimulationSupport::SceneTransition,
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
                .map(|attribute| wire::ConfigurationSimulationAttribute {
                    name: attribute.name,
                    value: Some(match attribute.value {
                        Value::Text(value) => {
                            wire::configuration_simulation_attribute::Value::Text(value)
                        }
                        Value::Boolean(value) => {
                            wire::configuration_simulation_attribute::Value::Boolean(value)
                        }
                        Value::Integer(value) => {
                            wire::configuration_simulation_attribute::Value::Integer(value)
                        }
                        Value::Number(value) => {
                            wire::configuration_simulation_attribute::Value::Number(value)
                        }
                        Value::Null => {
                            wire::configuration_simulation_attribute::Value::NullValue(true)
                        }
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
    report: Option<wire::ConfigurationSimulationReport>,
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
    pub fn report(&self) -> Option<&wire::ConfigurationSimulationReport> {
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
    pub fn request(
        &self,
        candidate: &Candidate,
        home: &str,
    ) -> Result<wire::SimulateConfigurationPlanRequest, Refusal> {
        if !candidate.family().simulatable() {
            return Err(Refusal::Unsupported);
        }
        let fixtures: Vec<Fixture> =
            serde_json::from_str(&self.fixtures).map_err(|_| Refusal::Invalid)?;
        let now = chrono::DateTime::parse_from_rfc3339(&self.now).map_err(|_| Refusal::Invalid)?;
        let request = wire::SimulateConfigurationPlanRequest {
            name: home.into(),
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
    pub fn accept(&mut self, response: wire::SimulateConfigurationPlanResponse) -> bool {
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
                EnumValue::Known(wire::ConfigurationSimulationStatus::Completed) => "Done",
                EnumValue::Known(wire::ConfigurationSimulationStatus::Partial) => "Partial",
                EnumValue::Known(wire::ConfigurationSimulationStatus::Failed) => "Failed",
                EnumValue::Known(wire::ConfigurationSimulationStatus::Unspecified)
                | EnumValue::Unknown(_) => "Unsupported",
            };
            fields.push(field(Field::Report,format!("{} · {} virtual ms\nSimulation only; no physical device validated\n{} actions · {} steps · {} diagnostics",status,report.elapsed_millis,report.intents.len(),report.nodes.len(),report.diagnostics.len()),false));
            fields.push(field(
                Field::Intents,
                report
                    .intents
                    .iter()
                    .map(|intent| {
                        format!(
                            "{} · {} · {} · {}\n{}",
                            intent.alias,
                            parameters(intent.parameters.as_ref()),
                            match intent.outcome {
                                EnumValue::Known(wire::IntentOutcome::Recorded) =>
                                    "Recorded in the simulation",
                                EnumValue::Known(wire::IntentOutcome::Refused) =>
                                    "Synthetic rejection",
                                EnumValue::Known(wire::IntentOutcome::Unspecified)
                                | EnumValue::Unknown(_) => "Unsupported",
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
                            "{} · {} · {}\n{}\nBranches {} · iterations {}",
                            action(node.action),
                            stage(node.stage),
                            super::render::at(&node.at),
                            position(&node.position),
                            indexes(&node.branches),
                            indexes(&node.iterations)
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
fn indexes(values: &[i32]) -> String {
    if values.is_empty() {
        "not provided".into()
    } else {
        values
            .iter()
            .map(u32::to_string)
            .collect::<Vec<_>>()
            .join(" / ")
    }
}
fn power(on: bool) -> &'static str {
    if on { "On" } else { "Off" }
}
fn parameter<T: std::fmt::Display>(
    values: &mut Vec<String>,
    name: &str,
    value: Option<T>,
    unit: &str,
) {
    if let Some(value) = value {
        values.push(format!("{name} {value}{unit}"));
    }
}
fn parameters(value: Option<&wire::configuration_simulation_intent::Parameters>) -> String {
    let mut values = Vec::new();
    let kind = match value {
        Some(wire::configuration_simulation_intent::Parameters::Switch(value)) => {
            values.push(power(value.on).into());
            "Switch"
        }
        Some(wire::configuration_simulation_intent::Parameters::Light(value)) => {
            if let Some(on) = value.on {
                values.push(power(on).into());
            }
            parameter(&mut values, "Brightness", value.brightness_percent, " %");
            parameter(
                &mut values,
                "Brightness Step",
                value.brightness_step_percent,
                " %",
            );
            parameter(&mut values, "Brightness", value.brightness_level, " / 255");
            parameter(
                &mut values,
                "Color Temperature",
                value.color_temp_mireds,
                " mired",
            );
            parameter(&mut values, "Transition", value.transition_seconds, " s");
            "Light"
        }
        Some(wire::configuration_simulation_intent::Parameters::Climate(value)) => {
            parameter(&mut values, "Setpoint", value.temperature_celsius, " °C");
            if let Some(mode) = value.mode {
                values.push(
                    match mode {
                        EnumValue::Known(mode) => temperature_mode_label(mode),
                        EnumValue::Unknown(_) => "Unsupported mode",
                    }
                    .into(),
                );
            }
            parameter(&mut values, "Humidity", value.humidity_percent, " %");
            parameter(&mut values, "Fan Mode", value.fan_mode.as_deref(), "");
            parameter(&mut values, "Preset", value.preset_mode.as_deref(), "");
            "Climate"
        }
        Some(wire::configuration_simulation_intent::Parameters::Fan(value)) => {
            values.push(power(value.on).into());
            parameter(&mut values, "Speed", Some(value.percentage), " %");
            "Fan"
        }
        None => return "Parameters missing".into(),
    };
    format!(
        "{} : {}",
        kind,
        if values.is_empty() {
            "No value provided".into()
        } else {
            values.join(" · ")
        }
    )
}
fn position(value: &wire::ConfigurationSimulationPosition) -> String {
    format!(
        "{}:{}:{} · {}",
        value.file, value.line, value.column, value.path
    )
}
fn diagnostic(value: EnumValue<wire::ConfigurationSimulationDiagnosticCode>) -> &'static str {
    match value {
        EnumValue::Known(wire::ConfigurationSimulationDiagnosticCode::UnsupportedExtension) => {
            "Unsupported extension"
        }
        EnumValue::Known(wire::ConfigurationSimulationDiagnosticCode::UnsupportedConcurrency) => {
            "Unsupported concurrency"
        }
        EnumValue::Known(wire::ConfigurationSimulationDiagnosticCode::UnsupportedWait) => {
            "Unsupported wait state"
        }
        EnumValue::Known(wire::ConfigurationSimulationDiagnosticCode::InvalidSource) => {
            "Invalid source"
        }
        EnumValue::Known(wire::ConfigurationSimulationDiagnosticCode::MissingFixture) => {
            "Synthetic state missing"
        }
        EnumValue::Known(wire::ConfigurationSimulationDiagnosticCode::FixtureRefused) => {
            "Action rejected by the test double"
        }
        EnumValue::Known(wire::ConfigurationSimulationDiagnosticCode::ExecutionFailed) => {
            "Virtual execution failed"
        }
        EnumValue::Known(wire::ConfigurationSimulationDiagnosticCode::CapacityExceeded) => {
            "Capacity exceeded"
        }
        EnumValue::Known(wire::ConfigurationSimulationDiagnosticCode::Unspecified)
        | EnumValue::Unknown(_) => "Unsupported diagnostic",
    }
}
fn action(value: EnumValue<wire::ConfigurationSimulationActionKind>) -> &'static str {
    match value {
        EnumValue::Known(wire::ConfigurationSimulationActionKind::Variables) => "Variables",
        EnumValue::Known(wire::ConfigurationSimulationActionKind::Sequence) => "Sequence",
        EnumValue::Known(wire::ConfigurationSimulationActionKind::Choose) => "Choose",
        EnumValue::Known(wire::ConfigurationSimulationActionKind::If) => "Condition",
        EnumValue::Known(wire::ConfigurationSimulationActionKind::Repeat) => "Repeat",
        EnumValue::Known(wire::ConfigurationSimulationActionKind::Parallel) => "Parallel Branches",
        EnumValue::Known(wire::ConfigurationSimulationActionKind::Wait) => "Wait",
        EnumValue::Known(wire::ConfigurationSimulationActionKind::Stop) => "Stop",
        EnumValue::Known(wire::ConfigurationSimulationActionKind::Call) => "Call",
        EnumValue::Known(wire::ConfigurationSimulationActionKind::Delay) => "Delay",
        EnumValue::Known(wire::ConfigurationSimulationActionKind::WaitTemplate) => {
            "Waiting for expression"
        }
        EnumValue::Known(wire::ConfigurationSimulationActionKind::Unspecified)
        | EnumValue::Unknown(_) => "Unsupported step",
    }
}
fn stage(value: EnumValue<wire::ConfigurationSimulationNodeStage>) -> &'static str {
    match value {
        EnumValue::Known(wire::ConfigurationSimulationNodeStage::Entered) => "Started",
        EnumValue::Known(wire::ConfigurationSimulationNodeStage::Completed) => "Done",
        EnumValue::Known(wire::ConfigurationSimulationNodeStage::Stopped) => "Stopped",
        EnumValue::Known(wire::ConfigurationSimulationNodeStage::Cancelled) => "Cancelled",
        EnumValue::Known(wire::ConfigurationSimulationNodeStage::Failed) => "Failed",
        EnumValue::Known(wire::ConfigurationSimulationNodeStage::Waiting) => "Pending",
        EnumValue::Known(wire::ConfigurationSimulationNodeStage::Unspecified)
        | EnumValue::Unknown(_) => "Unsupported state",
    }
}
fn field(field: Field, value: String, editable: bool) -> ProAdminField {
    let (kind, description) = match field {
        Field::Now | Field::TimeZone | Field::Limit => (
            ProAdminFieldKind::Plain,
            "Virtual clock; no real device clock is changed",
        ),
        Field::Fixtures => (
            ProAdminFieldKind::Multiline,
            "Array of test doubles with aliases, optional state, typed attributes and declared capabilities; no real device readings",
        ),
        Field::Events => (
            ProAdminFieldKind::Multiline,
            "Events Provided Only to Simulation",
        ),
        Field::Report | Field::Intents | Field::Nodes | Field::Diagnostics | Field::Limitations => {
            (
                ProAdminFieldKind::Multiline,
                "Server report; no physical command or configuration publication",
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
    fn response() -> wire::SimulateConfigurationPlanResponse {
        wire::SimulateConfigurationPlanResponse {
            report: wire::ConfigurationSimulationReport {
                simulated: true,
                status: wire::ConfigurationSimulationStatus::Completed.into(),
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
        let request = settings.request(&candidate(), "homes/synthetic").unwrap();
        assert!(request.fixtures[0].state.is_none());
        assert_eq!(
            request.fixtures[0].supports,
            vec![EnumValue::Known(
                wire::ConfigurationSimulationSupport::SwitchTurnOn
            )]
        );
        settings.edit(
            Field::Fixtures,
            r#"[{"alias":"synthetic.switch","supports":["physical_driver"]}]"#.into(),
        );
        assert!(settings.request(&candidate(), "homes/synthetic").is_err());
        settings.edit(
            Field::Fixtures,
            r#"[{"alias":"synthetic.switch","state":"off","hidden":true}]"#.into(),
        );
        assert!(settings.request(&candidate(), "homes/synthetic").is_err());
    }
    #[test]
    fn absent_observations_and_virtual_limits_are_admitted_before_any_rpc() {
        let mut settings = Simulation::default();
        settings.edit(Field::Fixtures,r#"[{"alias":"synthetic.switch","attributes":[{"name":"present","value":{"kind":"boolean","value":true}}]}]"#.into());
        assert!(settings.request(&candidate(), "homes/synthetic").is_err());
        settings.edit(Field::Fixtures,r#"[{"alias":"synthetic.switch","state":"off","attributes":[{"name":"present","value":{"kind":"boolean","value":true}}]}]"#.into());
        assert!(settings.request(&candidate(), "homes/synthetic").is_ok());
        settings.edit(Field::Limit, "3600001".into());
        assert!(settings.request(&candidate(), "homes/synthetic").is_err());
        settings.edit(Field::Limit, "1000".into());
        settings.edit(Field::Now, "2099-02-30T00:00:00Z".into());
        assert!(settings.request(&candidate(), "homes/synthetic").is_err());
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
/// Administration-local label for v2 simulation modes; device views own their
/// separate registry vocabulary.
fn temperature_mode_label(value: wire::TemperatureMode) -> &'static str {
    match value {
        wire::TemperatureMode::Unspecified => "Unspecified",
        wire::TemperatureMode::Off => "Off",
        wire::TemperatureMode::Heat => "Heat",
        wire::TemperatureMode::Cool => "Cool",
        wire::TemperatureMode::Auto => "Auto",
        wire::TemperatureMode::Dry => "Dehumidification",
        wire::TemperatureMode::FanOnly => "Fan Only",
    }
}
