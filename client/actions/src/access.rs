use buffa::EnumValue;
use home_wire_contract::home::state::v1 as wire;
use std::collections::BTreeSet;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WriteAccess {
    Unknown,
    Denied,
    Allowed,
}

/// Inspect only the exact slot's server authorization fact. Readable or writable
/// device capabilities alone never establish a user's permission to dispatch.
pub fn write_access(entity: &wire::ViewEntity, schema: &str, endpoint: &str) -> WriteAccess {
    let declared = entity
        .capabilities
        .iter()
        .map(|slot| (slot.schema_id.as_str(), slot.endpoint.as_str()))
        .collect::<BTreeSet<_>>();
    if !declared.contains(&(schema, endpoint)) || entity.action_access.len() > declared.len() {
        return WriteAccess::Unknown;
    }
    let mut seen = BTreeSet::new();
    let mut result = WriteAccess::Unknown;
    for permission in &entity.action_access {
        if !permission.__buffa_unknown_fields.is_empty()
            || permission.schema_id.is_empty()
            || permission.endpoint.is_empty()
            || !seen.insert((&permission.schema_id, &permission.endpoint))
            || !declared.contains(&(permission.schema_id.as_str(), permission.endpoint.as_str()))
        {
            return WriteAccess::Unknown;
        }
        let access = match permission.access {
            EnumValue::Known(wire::ActionAccess::Allowed) => WriteAccess::Allowed,
            EnumValue::Known(wire::ActionAccess::Denied) => WriteAccess::Denied,
            EnumValue::Known(wire::ActionAccess::Unspecified) | EnumValue::Unknown(_) => {
                WriteAccess::Unknown
            }
        };
        if permission.schema_id == schema && permission.endpoint == endpoint {
            result = access;
        }
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use home_wire_contract::home::registry::v1 as registry;
    #[test]
    fn capability_presence_is_not_permission_and_unknown_or_ambiguous_metadata_never_allows() {
        let mut entity = wire::ViewEntity {
            capabilities: vec![registry::CapabilityDescription {
                schema_id: "synthetic.switch".into(),
                endpoint: "main".into(),
                ..Default::default()
            }],
            ..Default::default()
        };
        assert_eq!(
            write_access(&entity, "synthetic.switch", "main"),
            WriteAccess::Unknown
        );
        entity.action_access.push(wire::CapabilityActionAccess {
            schema_id: "synthetic.switch".into(),
            endpoint: "main".into(),
            access: wire::ActionAccess::Denied.into(),
            ..Default::default()
        });
        assert_eq!(
            write_access(&entity, "synthetic.switch", "main"),
            WriteAccess::Denied
        );
        entity.action_access[0].access = wire::ActionAccess::Allowed.into();
        assert_eq!(
            write_access(&entity, "synthetic.switch", "main"),
            WriteAccess::Allowed
        );
        assert_eq!(
            write_access(&entity, "synthetic.switch", "other"),
            WriteAccess::Unknown
        );
        entity.action_access[0].access = EnumValue::Unknown(99);
        assert_eq!(
            write_access(&entity, "synthetic.switch", "main"),
            WriteAccess::Unknown
        );
        entity.action_access[0].access = wire::ActionAccess::Allowed.into();
        entity.action_access.push(entity.action_access[0].clone());
        assert_eq!(
            write_access(&entity, "synthetic.switch", "main"),
            WriteAccess::Unknown
        );
    }
}
