//! Protobuf codec boundary; no transport or UI.
#[allow(clippy::all)]
#[rustfmt::skip]
pub mod home {
    pub mod registry { pub mod v1 { include!("generated/home.registry.v1.rs"); } }
    pub mod actions { pub mod v1 { include!("generated/home.actions.v1.rs"); } }
    pub mod state { pub mod v1 { include!("generated/home.state.v1.rs"); } }
}

/// Pure admission example; no transport, authorization or dispatch is implemented here.
pub fn validate_command(
    c: &home::actions::v1::CommandRequest,
    epoch: &str,
    revision: u64,
) -> Result<(), &'static str> {
    if c.command_id.is_empty() {
        return Err("invalid_command_id");
    }
    if c.admission_epoch != epoch {
        return Err("stale_epoch");
    }
    if c.config_revision != revision {
        return Err("stale_configuration");
    }
    if !matches!(&c.target.reference,
        Some(home::registry::v1::entity_ref::Reference::EntityId(v) | home::registry::v1::entity_ref::Reference::HaAlias(v)) if !v.is_empty())
    {
        return Err("invalid_target");
    }
    if c.capability_schema_id != "core.switch.v1" {
        return Err("unsupported_capability");
    }
    if !matches!(
        c.arguments.arguments,
        Some(home::actions::v1::action_args::Arguments::SetSwitch(_))
    ) {
        return Err("invalid_arguments");
    }
    Ok(())
}
