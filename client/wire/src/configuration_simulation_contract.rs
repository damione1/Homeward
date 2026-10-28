//! Bounded simulation representation. It grants no device capability and does
//! not implement any compiler, template evaluator or physical driver.
use crate::{
    config_plans,
    configuration_contract::{self, ConfigurationAdmissionError as Error},
    home::{
        configurationplan::v1 as plan, configurationsimulation::v1 as wire,
        registry::v1 as registry,
    },
};
use buffa::{EnumValue, Message};
use std::collections::BTreeSet;

macro_rules! known {
    ($function:ident,$ty:path,$($variant:ident),+ $(,)?) => {
        fn $function(value:EnumValue<$ty>) -> bool {
            use $ty as V;
            match value {
                EnumValue::Known($(V::$variant)|+) => true,
                EnumValue::Known(V::Unspecified) | EnumValue::Unknown(_) => false,
            }
        }
    };
}
known!(
    support,
    wire::Support,
    ObserveState,
    ClimateSetpoint,
    SwitchTurnOn,
    LightTurnOn,
    LightTurnOff,
    LightBrightnessAbsolute,
    LightBrightnessRelative,
    LightTransition,
    SceneTurnOn,
    SceneTransition
);
known!(
    diagnostic,
    wire::DiagnosticCode,
    UnsupportedExtension,
    UnsupportedConcurrency,
    UnsupportedWait,
    InvalidSource,
    MissingFixture,
    FixtureRefused,
    ExecutionFailed,
    CapacityExceeded
);
known!(
    action,
    wire::ActionKind,
    Variables,
    Sequence,
    Choose,
    If,
    Repeat,
    Parallel,
    Wait,
    Stop,
    Call,
    Delay,
    WaitTemplate
);
known!(
    stage,
    wire::NodeStage,
    Entered,
    Completed,
    Stopped,
    Cancelled,
    Failed,
    Waiting
);
known!(
    climate_mode,
    registry::ClimateMode,
    Off,
    Heat,
    Cool,
    Auto,
    Dry,
    FanOnly
);
known!(outcome, wire::IntentOutcome, Recorded, Refused);

fn attribute(value: &wire::Attribute) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && config_plans::label(&value.name, 128)
        && match &value.value {
            Some(wire::attribute::Value::Text(value)) => value.len() <= 4096,
            Some(wire::attribute::Value::Boolean(_) | wire::attribute::Value::Integer(_)) => true,
            Some(wire::attribute::Value::Number(value)) => value.is_finite(),
            Some(wire::attribute::Value::NullValue(value)) => *value,
            None => false,
        }
}
pub fn admit_simulation_request(value: &wire::SimulatePlanRequest) -> Result<(), Error> {
    let family = match value.candidate.family {
        EnumValue::Known(
            plan::SourceFamily::Automation | plan::SourceFamily::Script | plan::SourceFamily::Scene,
        ) => true,
        EnumValue::Known(
            plan::SourceFamily::Operational
            | plan::SourceFamily::DeclarativeV1
            | plan::SourceFamily::DeclarativeV2
            | plan::SourceFamily::Authorization
            | plan::SourceFamily::Ownership
            | plan::SourceFamily::Dashboard
            | plan::SourceFamily::Unspecified,
        )
        | EnumValue::Unknown(_) => false,
    };
    if !value.__buffa_unknown_fields.is_empty()
        || !family
        || !value
            .candidate
            .as_option()
            .is_some_and(config_plans::candidate)
        || !value
            .now
            .as_option()
            .is_some_and(configuration_contract::timestamp)
        || !(-2_208_988_800..4_133_980_800).contains(&value.now.seconds)
        || !config_plans::label(&value.time_zone, 256)
        || !(1..=3_600_000).contains(&value.virtual_limit_millis)
        || value.fixtures.len() > 128
        || value.event_types.len() > 128
    {
        return Err(Error::Invalid);
    }
    let mut aliases = BTreeSet::new();
    let mut bytes = 0usize;
    for fixture in &value.fixtures {
        if !fixture.__buffa_unknown_fields.is_empty()
            || !config_plans::label(&fixture.alias, 255)
            || !aliases.insert(fixture.alias.as_str())
            || fixture.supports.len() > 10
            || fixture.attributes.len() > 32
            || fixture.state.is_none() && !fixture.attributes.is_empty()
            || fixture
                .state
                .as_ref()
                .is_some_and(|value| value.len() > 4096)
        {
            return Err(Error::Invalid);
        }
        if fixture
            .supports
            .iter()
            .enumerate()
            .any(|(index, value)| !support(*value) || fixture.supports[..index].contains(value))
        {
            return Err(Error::Invalid);
        }
        let mut names = BTreeSet::new();
        if fixture
            .attributes
            .iter()
            .any(|value| !attribute(value) || !names.insert(value.name.as_str()))
        {
            return Err(Error::Invalid);
        }
        bytes += fixture.encoded_len() as usize;
        if bytes > 65_536 {
            return Err(Error::Invalid);
        }
    }
    let mut events = BTreeSet::new();
    if value
        .event_types
        .iter()
        .any(|value| !config_plans::label(value, 128) || !events.insert(value.as_str()))
        || value.encoded_len() > 2 * 1024 * 1024
    {
        return Err(Error::Invalid);
    }
    Ok(())
}
fn position(value: &wire::Position) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && if value.file.is_empty() {
            value.path.is_empty() && value.line == 0 && value.column == 0
        } else {
            config_plans::label(&value.file, 4096)
                && value.path.len() <= 4096
                && value.line > 0
                && value.column > 0
        }
}
fn finite(value: Option<f64>, minimum: f64, maximum: f64) -> bool {
    value.is_none_or(|value| value.is_finite() && (minimum..=maximum).contains(&value))
}
fn intent(value: &wire::Intent) -> bool {
    if !value.__buffa_unknown_fields.is_empty()
        || !config_plans::label(&value.alias, 255)
        || !value.position.as_option().is_some_and(position)
        || value.position.file.is_empty()
        || !value
            .at
            .as_option()
            .is_some_and(configuration_contract::timestamp)
        || !outcome(value.outcome)
    {
        return false;
    }
    match &value.parameters {
        Some(wire::intent::Parameters::Switch(value)) => value.__buffa_unknown_fields.is_empty(),
        Some(wire::intent::Parameters::Fan(value)) => {
            value.__buffa_unknown_fields.is_empty() && value.percentage <= 100
        }
        Some(wire::intent::Parameters::Light(value)) => {
            let brightness = usize::from(value.brightness_percent.is_some())
                + usize::from(value.brightness_step_percent.is_some())
                + usize::from(value.brightness_255.is_some());
            value.__buffa_unknown_fields.is_empty()
                && brightness <= 1
                && (value.on.is_some() || brightness > 0 || value.color_temp_mireds.is_some())
                && finite(value.brightness_percent, 0., 100.)
                && finite(value.brightness_step_percent, -100., 100.)
                && finite(value.transition_seconds, 0., 86400.)
                && value.brightness_255.is_none_or(|value| value <= 255)
                && value.color_temp_mireds.is_none_or(|value| value > 0)
        }
        Some(wire::intent::Parameters::Climate(value)) => {
            value.__buffa_unknown_fields.is_empty()
                && (value.temperature_celsius.is_some()
                    || value.mode.is_some()
                    || value.humidity_percent.is_some()
                    || value.fan_mode.is_some()
                    || value.preset_mode.is_some())
                && value.mode.is_none_or(climate_mode)
                && finite(value.temperature_celsius, -273.15, 1000.)
                && finite(value.humidity_percent, 0., 100.)
                && value
                    .fan_mode
                    .as_ref()
                    .is_none_or(|value| config_plans::label(value, 128))
                && value
                    .preset_mode
                    .as_ref()
                    .is_none_or(|value| config_plans::label(value, 128))
        }
        None => value.outcome == wire::IntentOutcome::Refused,
    }
}
pub fn admit_simulation_response(value: &wire::SimulatePlanResponse) -> Result<(), Error> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(Error::Invalid);
    }
    let report = value.report.as_option().ok_or(Error::Invalid)?;
    let coherent = match report.status {
        EnumValue::Known(wire::Status::Completed) => report.diagnostics.is_empty(),
        EnumValue::Known(wire::Status::Partial | wire::Status::Failed) => {
            !report.diagnostics.is_empty()
        }
        EnumValue::Known(wire::Status::Unspecified) | EnumValue::Unknown(_) => false,
    };
    if !report.__buffa_unknown_fields.is_empty()
        || !report.simulated
        || report.hardware_validated
        || !coherent
        || report.elapsed_millis > 3_600_000
        || report.intents.len() > 256
        || report.nodes.len() > 1024
        || report.diagnostics.len() > 512
        || !(1..=8).contains(&report.limitations.len())
        || report
            .limitations
            .iter()
            .any(|value| !config_plans::label(value, 1024))
        || report.intents.iter().any(|value| !intent(value))
        || report.diagnostics.iter().any(|value| {
            !value.__buffa_unknown_fields.is_empty()
                || !diagnostic(value.code)
                || !value.position.as_option().is_some_and(position)
        })
        || report.nodes.iter().any(|value| {
            !value.__buffa_unknown_fields.is_empty()
                || !value.position.as_option().is_some_and(position)
                || value.position.file.is_empty()
                || !action(value.action)
                || !stage(value.stage)
                || !value
                    .at
                    .as_option()
                    .is_some_and(configuration_contract::timestamp)
                || value.branches.len() > 8
                || value.iterations.len() > 8
        })
        || value.encoded_len() > 512 * 1024
    {
        return Err(Error::Invalid);
    }
    Ok(())
}
