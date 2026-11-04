use home_client_model::api::{CapabilityName, DefinitionEtag, EntityName};
use home_client_model::api_observations::ActionAccess;
use home_wire_contract::{api_observations::parse_action_access, home::api::v2 as wire};
use std::collections::BTreeSet;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WriteAccess {
    Unknown,
    Denied,
    Allowed,
}

/// The exact resource slot supplies the current authorization fact. Intrinsic
/// supports_set, entity readability and observed values never grant a command.
/// Definition/trait codecs remain responsible for admitting the intended value.
pub fn write_access(entry: &wire::EntityViewEntry, target: &CapabilityName) -> WriteAccess {
    let Some(entity) = entry.entity.as_option() else {
        return WriteAccess::Unknown;
    };
    let Ok(parent) = EntityName::try_from(entity.name.as_str()) else {
        return WriteAccess::Unknown;
    };
    if !entry.__buffa_unknown_fields.is_empty()
        || !entity.__buffa_unknown_fields.is_empty()
        || target.entity() != parent
        || entry.capabilities.len() > 64
    {
        return WriteAccess::Unknown;
    }
    let mut seen = BTreeSet::new();
    let mut result = WriteAccess::Unknown;
    for capability in &entry.capabilities {
        let Ok(name) = CapabilityName::try_from(capability.name.as_str()) else {
            return WriteAccess::Unknown;
        };
        if name.entity() != parent
            || !seen.insert(name.clone())
            || !capability.__buffa_unknown_fields.is_empty()
            || capability.r#trait.is_none()
            || DefinitionEtag::try_from(capability.etag.as_str()).is_err()
        {
            return WriteAccess::Unknown;
        }
        let Ok(access) = parse_action_access(capability.action_access) else {
            return WriteAccess::Unknown;
        };
        if name == *target {
            result = match access {
                ActionAccess::Unspecified => WriteAccess::Unknown,
                ActionAccess::Denied => WriteAccess::Denied,
                ActionAccess::Allowed => WriteAccess::Allowed,
            };
        }
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use buffa::EnumValue;

    #[test]
    fn intrinsic_support_is_not_permission_and_ambiguous_slots_never_allow() {
        let target =
            CapabilityName::try_from("homes/synthetic/entities/relay/capabilities/power").unwrap();
        let mut entry = wire::EntityViewEntry {
            entity: wire::Entity {
                name: target.entity().as_str().to_owned(),
                ..Default::default()
            }
            .into(),
            capabilities: vec![wire::Capability {
                name: target.as_str().to_owned(),
                etag: "W/\"definition-one\"".into(),
                r#trait: Some(wire::capability::Trait::Power(Box::new(
                    wire::PowerCapability {
                        supports_set: true,
                        ..Default::default()
                    },
                ))),
                ..Default::default()
            }],
            ..Default::default()
        };
        assert_eq!(write_access(&entry, &target), WriteAccess::Unknown);
        entry.capabilities[0].action_access = wire::ActionAccess::Denied.into();
        assert_eq!(write_access(&entry, &target), WriteAccess::Denied);
        entry.capabilities[0].action_access = wire::ActionAccess::Allowed.into();
        assert_eq!(write_access(&entry, &target), WriteAccess::Allowed);
        let other =
            CapabilityName::try_from("homes/synthetic/entities/relay/capabilities/other").unwrap();
        assert_eq!(write_access(&entry, &other), WriteAccess::Unknown);
        entry.capabilities[0].action_access = EnumValue::Unknown(99);
        assert_eq!(write_access(&entry, &target), WriteAccess::Unknown);
        entry.capabilities[0].action_access = wire::ActionAccess::Allowed.into();
        entry.capabilities.push(entry.capabilities[0].clone());
        assert_eq!(write_access(&entry, &target), WriteAccess::Unknown);
        entry.capabilities.pop();
        entry.entity.as_option_mut().unwrap().name = "homes/synthetic/entities/other".into();
        assert_eq!(write_access(&entry, &target), WriteAccess::Unknown);
    }
}
