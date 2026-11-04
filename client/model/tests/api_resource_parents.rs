use home_client_model::api::{
    CapabilityName, DeviceName, EntityName, EntityViewName, HomeName, OperationName,
};

#[test]
fn canonical_parents_are_typed_and_do_not_follow_names_inside_ids() {
    let home = HomeName::try_from("homes/entities").unwrap();
    let entity = EntityName::try_from("homes/entities/entities/capabilities").unwrap();
    let capability =
        CapabilityName::try_from("homes/entities/entities/capabilities/capabilities/entities")
            .unwrap();
    assert_eq!(entity.home(), home);
    assert_eq!(capability.entity(), entity);
    assert_eq!(capability.home(), home);
    assert_eq!(
        EntityViewName::try_from("homes/entities/entityViews/entityviews")
            .unwrap()
            .home(),
        home
    );
    assert_eq!(
        DeviceName::try_from("homes/entities/devices/devices")
            .unwrap()
            .home(),
        home
    );
    assert_eq!(
        OperationName::try_from("homes/entities/operations/operations")
            .unwrap()
            .home(),
        home
    );
    for raw in [
        "homes/entities/entities/a/capabilities/b/extra",
        "homes/entities/entities/a%2fb/capabilities/b",
        "homes/entities/entities/../capabilities/b",
    ] {
        assert!(CapabilityName::try_from(raw).is_err());
    }
}
