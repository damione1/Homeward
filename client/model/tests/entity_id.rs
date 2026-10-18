use home_client_model::{EntityId, ParseEntityIdError};

#[test]
fn opaque_id_is_nonempty_and_preserved_without_normalization() {
    assert_eq!("".parse::<EntityId>(), Err(ParseEntityIdError::Empty));
    for value in ["synthetic-entity", "Host:opaque/É-42", " id with spaces "] {
        let id: EntityId = value.parse().unwrap();
        assert_eq!(id.as_str(), value);
        assert_eq!(id.clone(), id);
    }
}
