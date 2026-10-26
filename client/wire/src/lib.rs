//! Protobuf codec boundary; no transport or UI.
#![deny(clippy::wildcard_enum_match_arm)]
#[allow(clippy::all)]
#[rustfmt::skip]
pub mod home {
    pub mod environment { pub mod v1 { include!("generated/home.environment.v1.rs"); } }
    pub mod controls { pub mod v1 { include!("generated/home.controls.v1.rs"); } }
    pub mod dashboard { pub mod v1 { include!("generated/home.dashboard.v1.rs"); } }
    pub mod configurationapply { pub mod v1 { include!("generated/home.configurationapply.v1.rs"); } }
    pub mod configurationsimulation {
        pub mod v1 {
            include!("generated/home.configurationsimulation.v1.rs");
        }
    }
    pub mod configurationplan {
        pub mod v1 {
            include!("generated/home.configurationplan.v1.rs");
        }
    }
    pub mod integrationadmin { pub mod v1 { include!("generated/home.integrationadmin.v1.rs"); } }
    pub mod configuration { pub mod v1 { include!("generated/home.configuration.v1.rs"); } }
    pub mod history { pub mod v1 { include!("generated/home.history.v1.rs"); } }
    pub mod observability { pub mod v1 { include!("generated/home.observability.v1.rs"); } }
    pub mod integrations { pub mod v1 { include!("generated/home.integrations.v1.rs"); } }
    pub mod solar { pub mod v1 { include!("generated/home.solar.v1.rs"); } }
    pub mod helpers { pub mod v1 { include!("generated/home.helpers.v1.rs"); } }
    pub mod registry { pub mod v1 { include!("generated/home.registry.v1.rs"); } }
    pub mod actions { pub mod v1 { include!("generated/home.actions.v1.rs"); } }
    pub mod state { pub mod v1 { include!("generated/home.state.v1.rs"); } }
    pub mod auth { pub mod v1 { include!("generated/home.auth.v1.rs"); } }
    pub mod lists { pub mod v1 { include!("generated/home.lists.v1.rs"); } }
}

pub mod actions_contract;
pub mod auth_contract;
pub mod authorization_contract;
pub mod config_plans;
pub mod configuration_contract;
pub mod controls_contract;
pub mod dashboard_contract;
pub mod history_contract;
pub mod integration_admin_contract;
pub mod lists_contract;
pub mod observability_contract;
pub mod solar_contract;

fn known_helper_set_value(value: &home::helpers::v1::HelperValue) -> bool {
    use home::helpers::v1::helper_value::Value;

    if value.helper_revision != 0 || !value.__buffa_unknown_fields.is_empty() {
        return false;
    }
    match &value.value {
        Some(
            Value::Boolean(_)
            | Value::Number(_)
            | Value::Counter(_)
            | Value::SelectedOption(_)
            | Value::Text(_),
        ) => true,
        Some(Value::Date(date)) => date.__buffa_unknown_fields.is_empty(),
        Some(Value::Time(time)) => time.__buffa_unknown_fields.is_empty(),
        Some(Value::Datetime(datetime)) => {
            datetime.__buffa_unknown_fields.is_empty()
                && datetime
                    .date
                    .as_option()
                    .is_some_and(|date| date.__buffa_unknown_fields.is_empty())
                && datetime
                    .time
                    .as_option()
                    .is_some_and(|time| time.__buffa_unknown_fields.is_empty())
        }
        Some(Value::ButtonPressedAt(_)) | None => false,
    }
}

/// Pure representation/generation admission example. Authorization, expiry,
/// identity resolution and dispatch remain responsibilities of later services.
pub fn validate_command(
    c: &home::actions::v1::CommandRequest,
    epoch: &str,
    revision: u64,
) -> Result<(), Box<home::actions::v1::ContractError>> {
    use buffa::EnumValue;
    use home::actions::v1::{ErrorCode, action_args::Arguments};
    use home::helpers::v1::HelperOperation;
    use home::registry::v1::entity_ref::Reference;

    let fail = |code: ErrorCode, path: &str| {
        Box::new(home::actions::v1::ContractError {
            code: code.into(),
            field_path: path.into(),
            message: "command rejected by contract".into(),
            ..Default::default()
        })
    };
    if c.command_id.is_empty() {
        return Err(fail(ErrorCode::InvalidArgument, "command_id"));
    }
    if c.admission_epoch.is_empty() || epoch.is_empty() {
        return Err(fail(ErrorCode::InvalidArgument, "admission_epoch"));
    }
    if c.admission_epoch != epoch {
        return Err(fail(ErrorCode::StaleEpoch, "admission_epoch"));
    }
    if c.config_revision != revision {
        return Err(fail(ErrorCode::StaleConfiguration, "config_revision"));
    }
    let valid_target = match &c.target.reference {
        Some(Reference::EntityId(value)) => value.parse::<home_client_model::EntityId>().is_ok(),
        Some(Reference::HaAlias(value)) => !value.is_empty(),
        None => false,
    };
    if !valid_target {
        return Err(fail(ErrorCode::InvalidArgument, "target"));
    }
    if c.capability_schema_id != "core.switch.v1" && c.capability_schema_id != "core.helper.v1" {
        return Err(fail(
            ErrorCode::UnsupportedCapability,
            "capability_schema_id",
        ));
    }
    match &c.arguments.arguments {
        Some(Arguments::SetSwitch(_)) => {
            if c.capability_schema_id == "core.switch.v1" {
                Ok(())
            } else {
                Err(fail(ErrorCode::InvalidArgument, "arguments"))
            }
        }
        Some(Arguments::Helper(action)) => {
            if c.capability_schema_id != "core.helper.v1" {
                return Err(fail(ErrorCode::InvalidArgument, "arguments"));
            }
            let precondition = action.expected_helper_revision.ok_or_else(|| {
                fail(
                    ErrorCode::InvalidArgument,
                    "arguments.helper.expected_helper_revision",
                )
            })?;
            if precondition > i64::MAX as u64 || !action.__buffa_unknown_fields.is_empty() {
                return Err(fail(ErrorCode::InvalidArgument, "arguments.helper"));
            }
            let is_set = match action.operation {
                EnumValue::Known(HelperOperation::Set) => true,
                EnumValue::Known(
                    HelperOperation::Toggle
                    | HelperOperation::Increment
                    | HelperOperation::Decrement
                    | HelperOperation::Reset
                    | HelperOperation::Press
                    | HelperOperation::Next
                    | HelperOperation::Previous,
                ) => false,
                EnumValue::Known(HelperOperation::Unspecified) | EnumValue::Unknown(_) => {
                    return Err(fail(
                        ErrorCode::InvalidArgument,
                        "arguments.helper.operation",
                    ));
                }
            };
            if is_set != action.value.as_option().is_some() {
                return Err(fail(ErrorCode::InvalidArgument, "arguments.helper.value"));
            }
            if is_set && !action.value.as_option().is_some_and(known_helper_set_value) {
                return Err(fail(ErrorCode::InvalidArgument, "arguments.helper.value"));
            }
            Ok(())
        }
        Some(Arguments::Extension(_))
        | Some(Arguments::SetLight(_))
        | Some(Arguments::SetFan(_))
        | Some(Arguments::SetClimate(_))
        | Some(Arguments::Cover(_))
        | Some(Arguments::Lock(_))
        | Some(Arguments::Alarm(_))
        | None => Err(fail(ErrorCode::InvalidArgument, "arguments")),
    }
}
