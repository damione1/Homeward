//! Strict parsing only. Execution must revalidate the exact capability and etag.
use crate::home::api::v2 as wire;
use home_client_model::{
    api::{CapabilityName, CoverOperation, DefinitionEtag, SelectorKey},
    control_modules::*,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct InvalidControlModule;
type Result<T> = std::result::Result<T, InvalidControlModule>;
fn range(v: &wire::NumberBounds) -> Result<NativeRange> {
    if !v.__buffa_unknown_fields.is_empty()
        || !v.minimum.is_finite()
        || !v.maximum.is_finite()
        || v.minimum > v.maximum
        || v.step.is_some_and(|v| !v.is_finite() || v <= 0.)
    {
        return Err(InvalidControlModule);
    }
    Ok(NativeRange {
        minimum: v.minimum,
        maximum: v.maximum,
        step: v.step,
    })
}
fn choices(v: &wire::ControlChoices) -> Result<Vec<Choice>> {
    if !v.__buffa_unknown_fields.is_empty() || v.choices.is_empty() || v.choices.len() > 256 {
        return Err(InvalidControlModule);
    }
    let mut seen = std::collections::HashSet::new();
    v.choices
        .iter()
        .map(|v| {
            if !v.__buffa_unknown_fields.is_empty()
                || !seen.insert(v.key.as_str())
                || v.display_name.len() > 16384
            {
                return Err(InvalidControlModule);
            }
            Ok(Choice {
                key: SelectorKey::try_from(v.key.as_str()).map_err(|_| InvalidControlModule)?,
                display_name: v.display_name.clone(),
            })
        })
        .collect()
}
pub fn visual_family(v: buffa::EnumValue<wire::ControlVisualFamily>) -> Result<VisualFamily> {
    use wire::ControlVisualFamily as W;
    match v.as_known() {
        Some(W::Binary) => Ok(VisualFamily::Binary),
        Some(W::ContinuousLevel) => Ok(VisualFamily::ContinuousLevel),
        Some(W::SteppedLevel) => Ok(VisualFamily::SteppedLevel),
        Some(W::Blind) => Ok(VisualFamily::Blind),
        Some(W::Garage) => Ok(VisualFamily::Garage),
        _ => Err(InvalidControlModule),
    }
}
pub fn module(v: &wire::ControlModule) -> Result<ControlModule> {
    use wire::{ControlModuleRole as R, control_module::Definition as D};
    if !v.__buffa_unknown_fields.is_empty() {
        return Err(InvalidControlModule);
    }
    let capability =
        CapabilityName::try_from(v.capability.as_str()).map_err(|_| InvalidControlModule)?;
    let etag = DefinitionEtag::try_from(v.etag.as_str()).map_err(|_| InvalidControlModule)?;
    let allowed = match v.action_access.as_known() {
        Some(wire::ActionAccess::Allowed) => true,
        Some(wire::ActionAccess::Denied) => false,
        _ => return Err(InvalidControlModule),
    };
    let (role, definition) = match (v.role.as_known(), v.definition.as_ref()) {
        (Some(R::Power), Some(D::Power(true))) => (ModuleRole::Power, ModuleDefinition::Power),
        (Some(R::Range), Some(D::Range(v))) => {
            (ModuleRole::Range, ModuleDefinition::Range(range(v)?))
        }
        (Some(R::Steps), Some(D::Steps(v))) => {
            (ModuleRole::Steps, ModuleDefinition::Steps(choices(v)?))
        }
        (Some(R::Preset), Some(D::Preset(v))) => {
            (ModuleRole::Preset, ModuleDefinition::Preset(choices(v)?))
        }
        (Some(R::Color), Some(D::Color(v))) => {
            if !v.__buffa_unknown_fields.is_empty() || !v.rgb && !v.temperature_mireds.is_set() {
                return Err(InvalidControlModule);
            }
            let temperature_mireds = v.temperature_mireds.as_option().map(range).transpose()?;
            if temperature_mireds
                .as_ref()
                .is_some_and(|v| v.minimum < 1. || v.maximum > 1000.)
            {
                return Err(InvalidControlModule);
            }
            (
                ModuleRole::Color,
                ModuleDefinition::Color {
                    temperature_mireds,
                    rgb: v.rgb,
                },
            )
        }
        (Some(R::Motion), Some(D::Motion(v))) => {
            if !crate::api_controls_contract::cover_constraints(v) || v.operations.is_empty() {
                return Err(InvalidControlModule);
            }
            let operations = v
                .operations
                .iter()
                .map(|v| match v.as_known() {
                    Some(wire::CoverOperation::Open) => Ok(CoverOperation::Open),
                    Some(wire::CoverOperation::Close) => Ok(CoverOperation::Close),
                    Some(wire::CoverOperation::Stop) => Ok(CoverOperation::Stop),
                    _ => Err(InvalidControlModule),
                })
                .collect::<Result<Vec<_>>>()?;
            (ModuleRole::Motion, ModuleDefinition::Motion(operations))
        }
        _ => return Err(InvalidControlModule),
    };
    Ok(ControlModule {
        capability,
        etag,
        allowed,
        role,
        definition,
    })
}
// A capability's module cannot point at a different source or borrow a sibling
// etag/access. Dashboard modules are joined to their own current resource first.
pub fn capability_modules(v: &wire::Capability) -> Result<Vec<ControlModule>> {
    if v.modules.len() > 16 {
        return Err(InvalidControlModule);
    }
    let mut roles = std::collections::HashSet::new();
    v.modules
        .iter()
        .map(|m| {
            if m.capability != v.name
                || m.etag != v.etag
                || m.action_access != v.action_access
                || !roles.insert(m.role.to_i32())
            {
                return Err(InvalidControlModule);
            }
            module(m)
        })
        .collect()
}
