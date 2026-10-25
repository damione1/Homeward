use home_client_model::EntityId;

/// Open identities stay exact and cannot be confused with labels or row indices.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct SchemaId(String);
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct Endpoint(String);
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct ActionTarget {
    entity: EntityId,
    schema: SchemaId,
    endpoint: Endpoint,
}
impl ActionTarget {
    pub fn new(entity: EntityId, schema: &str, endpoint: &str) -> Option<Self> {
        let valid = |value: &str| {
            !value.is_empty() && value.len() <= 256 && !value.chars().any(char::is_control)
        };
        if !valid(entity.as_str()) || !valid(schema) || !valid(endpoint) {
            return None;
        }
        Some(Self {
            entity,
            schema: SchemaId(schema.into()),
            endpoint: Endpoint(endpoint.into()),
        })
    }
    pub fn entity(&self) -> &EntityId {
        &self.entity
    }
    pub fn schema(&self) -> &str {
        &self.schema.0
    }
    pub fn endpoint(&self) -> &str {
        &self.endpoint.0
    }
}
