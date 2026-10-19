//! Protobuf codec boundary; no transport or UI.
#![deny(clippy::wildcard_enum_match_arm)]
#[allow(clippy::all)]
#[rustfmt::skip]
pub mod home {
    pub mod registry { pub mod v1 { include!("generated/home.registry.v1.rs"); } }
    pub mod actions { pub mod v1 { include!("generated/home.actions.v1.rs"); } }
    pub mod state { pub mod v1 { include!("generated/home.state.v1.rs"); } }
    pub mod auth { pub mod v1 { include!("generated/home.auth.v1.rs"); } }
}

pub mod auth_contract;
pub mod authorization_contract;

/// Pure representation/generation admission example. Authorization, expiry,
/// identity resolution and dispatch remain responsibilities of later services.
pub fn validate_command(
    c: &home::actions::v1::CommandRequest,
    epoch: &str,
    revision: u64,
) -> Result<(), Box<home::actions::v1::ContractError>> {
    use home::actions::v1::{ErrorCode, action_args::Arguments};
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
    if c.capability_schema_id != "core.switch.v1" {
        return Err(fail(
            ErrorCode::UnsupportedCapability,
            "capability_schema_id",
        ));
    }
    match &c.arguments.arguments {
        Some(Arguments::SetSwitch(_)) => Ok(()),
        Some(Arguments::Extension(_))
        | Some(Arguments::SetLight(_))
        | Some(Arguments::SetFan(_))
        | Some(Arguments::SetClimate(_))
        | None => Err(fail(ErrorCode::InvalidArgument, "arguments")),
    }
}
