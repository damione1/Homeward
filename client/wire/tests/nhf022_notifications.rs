use buffa::EnumValue;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{
    auth_contract::{AccountId, SessionId},
    home::{
        auth::v1 as auth, companion::v1 as companion, notifications::v1 as wire,
        registry::v1 as registry,
    },
    notifications_contract::*,
};
fn stamp(seconds: i64) -> Timestamp {
    Timestamp {
        seconds,
        ..Default::default()
    }
}
fn context() -> companion::Context {
    companion::Context {
        deployment: companion::Deployment::SyntheticOnly.into(),
        epoch: "a".repeat(32),
        ..Default::default()
    }
}
fn permission() -> wire::PermissionSnapshot {
    wire::PermissionSnapshot {
        permission: wire::Permission::Authorized.into(),
        alerts: wire::Setting::Enabled.into(),
        notification_center: wire::Setting::Enabled.into(),
        source: wire::PermissionSource::MacOsUserNotifications.into(),
        ..Default::default()
    }
}
fn destination() -> wire::Destination {
    wire::Destination {
        id: wire::DestinationId {
            value: "b".repeat(32),
            ..Default::default()
        }
        .into(),
        installation_id: companion::InstallationId {
            value: "c".repeat(32),
            ..Default::default()
        }
        .into(),
        device_id: companion::DeviceId {
            value: "d".repeat(32),
            ..Default::default()
        }
        .into(),
        account_id: auth::AccountId {
            value: "synthetic-account".into(),
            ..Default::default()
        }
        .into(),
        session_id: auth::SessionId {
            value: "synthetic-session".into(),
            ..Default::default()
        }
        .into(),
        platform: companion::Platform::MacOs.into(),
        state: wire::DestinationState::Active.into(),
        revision: 1,
        native_delivery: false,
        permission: permission().into(),
        expires_at: stamp(1800).into(),
        last_request_id: "e".repeat(32),
        registration_request_id: "e".repeat(32),
        ..Default::default()
    }
}
fn delivery() -> wire::Delivery {
    wire::Delivery {
        id: wire::DeliveryId {
            value: "f".repeat(32),
            ..Default::default()
        }
        .into(),
        destination_id: destination().id,
        revision: 1,
        title: "Message synthétique".into(),
        body: "Contenu privé\nde test".into(),
        created_at: stamp(1000).into(),
        expires_at: stamp(1600).into(),
        target: wire::Target {
            resource: Some(wire::target::Resource::Home(Box::default())),
            ..Default::default()
        }
        .into(),
        transport_acceptance: wire::TransportAcceptance::AcceptedForegroundLan.into(),
        transport_reason: wire::TransportReason::None.into(),
        reception: wire::Reception::NotReceived.into(),
        presentation: wire::Presentation::NotAttempted.into(),
        read_state: wire::ReadState::Unread.into(),
        ..Default::default()
    }
}
fn installation() -> companion::Installation {
    let destination = destination();
    let features = [
        companion::Feature::BatteryLevel,
        companion::Feature::BatteryState,
        companion::Feature::Charging,
        companion::Feature::Connectivity,
        companion::Feature::Activity,
        companion::Feature::Focus,
        companion::Feature::DoNotDisturb,
        companion::Feature::Kiosk,
        companion::Feature::ScreenSaver,
        companion::Feature::Brightness,
        companion::Feature::Volume,
        companion::Feature::Location,
        companion::Feature::CameraMotion,
        companion::Feature::Notifications,
    ];
    let capabilities = features
        .into_iter()
        .map(|feature| {
            let (availability, source, permission) = match feature {
                companion::Feature::BatteryLevel
                | companion::Feature::BatteryState
                | companion::Feature::Charging => (
                    companion::Availability::Available,
                    companion::Source::MacOsPowerSources,
                    companion::Permission::NotRequired,
                ),
                companion::Feature::Connectivity => (
                    companion::Availability::Partial,
                    companion::Source::MacOsNetworkConfiguration,
                    companion::Permission::NotRequired,
                ),
                companion::Feature::Activity => (
                    companion::Availability::Partial,
                    companion::Source::NativeLifecycle,
                    companion::Permission::NotRequired,
                ),
                companion::Feature::Focus
                | companion::Feature::DoNotDisturb
                | companion::Feature::Kiosk
                | companion::Feature::ScreenSaver
                | companion::Feature::Brightness
                | companion::Feature::Volume
                | companion::Feature::Location
                | companion::Feature::CameraMotion
                | companion::Feature::Notifications => (
                    companion::Availability::Unsupported,
                    companion::Source::None,
                    companion::Permission::Unsupported,
                ),
                companion::Feature::Unspecified => panic!("fixture has no unspecified feature"),
            };
            companion::Capability {
                feature: feature.into(),
                availability: availability.into(),
                source: source.into(),
                permission: permission.into(),
                ..Default::default()
            }
        })
        .collect();
    companion::Installation {
        id: destination.installation_id,
        device_id: destination.device_id,
        account_id: destination.account_id,
        session_id: destination.session_id,
        device: companion::DeviceDescription {
            platform: companion::Platform::MacOs.into(),
            model: "Synthetic desktop".into(),
            os_version: "synthetic-1".into(),
            capabilities,
            ..Default::default()
        }
        .into(),
        consent: companion::Consent {
            cadence: companion::Cadence::Minute1.into(),
            ..Default::default()
        }
        .into(),
        revision: 1,
        state: companion::InstallationState::Active.into(),
        expires_at: stamp(1800).into(),
        registration_request_id: "e".repeat(32),
        ..Default::default()
    }
}
#[test]
fn registration_requires_discovery_nonce_and_honest_permission_source() {
    let mut request = wire::RegisterDestinationRequest {
        installation_id: destination().installation_id,
        request_id: "a".repeat(32),
        permission: permission().into(),
        ..Default::default()
    };
    assert!(admit_request(Request::Register(&request)).is_err());
    request.expected_context = context().into();
    assert!(admit_request(Request::Register(&request)).is_ok());
    request.request_id.clear();
    assert!(admit_request(Request::Register(&request)).is_err());
    let mut permission = permission();
    permission.source = wire::PermissionSource::AndroidPendingQualification.into();
    assert!(!valid_permission(&permission));
    permission.permission = wire::Permission::NotRequested.into();
    permission.alerts = wire::Setting::Unsupported.into();
    permission.notification_center = wire::Setting::Unsupported.into();
    assert!(valid_permission(&permission));
    assert!(!can_present(&permission));
    let mut update = wire::UpdateDestinationRequest {
        expected_context: context().into(),
        id: destination().id,
        expected_revision: 1,
        request_id: "a".repeat(32),
        native_delivery: true,
        permission: permission.into(),
        ..Default::default()
    };
    assert!(admit_request(Request::Update(&update)).is_err());
    update.native_delivery = false;
    assert!(admit_request(Request::Update(&update)).is_ok());
}
#[test]
fn destination_binds_actual_native_subject_installation_and_epoch() {
    let original = destination();
    let installation = installation();
    let account = AccountId::parse("synthetic-account").unwrap();
    let session = SessionId::parse("synthetic-session").unwrap();
    assert_eq!(
        bind_native(&original, &installation, &account, &session),
        Ok(())
    );
    assert_eq!(
        bind_native(
            &original,
            &installation,
            &account,
            &SessionId::parse("other-session").unwrap()
        ),
        Err(Invalid::ForeignIdentity)
    );
    let mut foreign = original.clone();
    foreign.installation_id.get_or_insert_default().value = "0".repeat(32);
    assert_eq!(
        bind_native(&foreign, &installation, &account, &session),
        Err(Invalid::ForeignIdentity)
    );
    let mut revoked = installation.clone();
    revoked.state = companion::InstallationState::Revoked.into();
    assert_eq!(
        bind_native(&original, &revoked, &account, &session),
        Err(Invalid::ForeignIdentity)
    );
    let mut changed = context();
    changed.epoch = "0".repeat(32);
    assert_eq!(
        bind_existing(&changed, &original, &context(), &original),
        Err(Invalid::StaleContext)
    );
    foreign = original.clone();
    foreign.platform = companion::Platform::Android.into();
    assert!(!valid_destination(&foreign));
}
#[test]
fn malformed_payload_unknown_states_and_fractional_lifetime_are_rejected() {
    assert!(valid_delivery(&delivery()));
    let mut value = delivery();
    value.title = "x".repeat(129);
    assert!(!valid_delivery(&value));
    value = delivery();
    value.body = "x".repeat(1025);
    assert!(!valid_delivery(&value));
    value = delivery();
    value.body = "bad\u{1b}text".into();
    assert!(!valid_delivery(&value));
    value = delivery();
    value.presentation = EnumValue::Unknown(99);
    assert!(!valid_delivery(&value));
    value = delivery();
    value.expires_at = stamp(4600).into();
    assert!(valid_delivery(&value));
    value.expires_at.get_or_insert_default().nanos = 1;
    assert!(!valid_delivery(&value));
    value = delivery();
    value.target = Default::default();
    assert!(!valid_delivery(&value));
    value = delivery();
    value.id.get_or_insert_default().value = "https://untrusted.example/".into();
    assert!(!valid_delivery(&value));
    assert!(
        admit_response(Response::Describe(
            &wire::DescribeNotificationsResponse::default()
        ))
        .is_err()
    );
}
#[test]
fn inbox_cannot_mix_destinations_duplicate_ids_or_overflow_a_bounded_capture() {
    let mut response = wire::ReadInboxResponse {
        context: context().into(),
        destination: destination().into(),
        revision: 1,
        entries: vec![delivery()],
        ..Default::default()
    };
    assert!(admit_response(Response::Inbox(&response)).is_ok());
    response.entries.push(delivery());
    assert!(admit_response(Response::Inbox(&response)).is_err());
    response.entries.pop();
    response.entries[0]
        .destination_id
        .get_or_insert_default()
        .value = "0".repeat(32);
    assert!(admit_response(Response::Inbox(&response)).is_err());
    response.entries = (0..65)
        .map(|i| {
            let mut d = delivery();
            d.id.get_or_insert_default().value = format!("{i:032x}");
            d
        })
        .collect();
    assert!(admit_response(Response::Inbox(&response)).is_err());
}
#[test]
fn provider_failure_permission_withdrawal_and_expiry_never_become_native_eligibility() {
    let mut destination = destination();
    destination.native_delivery = true;
    let mut value = delivery();
    value.reception = wire::Reception::Received.into();
    assert!(eligible_for_native(&value, &destination, &stamp(1000)));
    assert!(!eligible_for_native(&value, &destination, &stamp(999)));
    assert!(!eligible_for_native(&value, &destination, &stamp(1600)));
    value.transport_acceptance = wire::TransportAcceptance::ProviderRejected.into();
    value.transport_reason = wire::TransportReason::ProviderFailure.into();
    assert!(valid_delivery(&value));
    assert!(!eligible_for_native(&value, &destination, &stamp(1000)));
    value.presentation = wire::Presentation::PresentInOsCenter.into();
    assert!(!valid_delivery(&value));
    value = delivery();
    value.reception = wire::Reception::Received.into();
    value.presentation = wire::Presentation::OutcomeUnknown.into();
    assert!(valid_delivery(&value));
    assert!(!eligible_for_native(&value, &destination, &stamp(1000)));
    destination.native_delivery = false;
    value.presentation = wire::Presentation::NotAttempted.into();
    assert!(!eligible_for_native(&value, &destination, &stamp(1000)));
}
#[test]
fn immutable_delivery_and_monotonic_receipts_cannot_regress_or_swap_a_target() {
    let prior = delivery();
    let mut next = prior.clone();
    next.revision = 2;
    next.reception = wire::Reception::Received.into();
    assert_eq!(bind_delivery(&next, &destination(), Some(&prior)), Ok(()));
    let received = next.clone();
    next.revision = 3;
    next.presentation = wire::Presentation::OsAccepted.into();
    assert_eq!(
        bind_delivery(&next, &destination(), Some(&received)),
        Ok(())
    );
    let accepted = next.clone();
    next.revision = 4;
    next.presentation = wire::Presentation::NotAttempted.into();
    assert_eq!(
        bind_delivery(&next, &destination(), Some(&accepted)),
        Err(Invalid::Regression)
    );
    next = received.clone();
    next.revision = 3;
    next.target.get_or_insert_default().resource = Some(wire::target::Resource::Entity(Box::new(
        registry::EntityId {
            value: "different-entity".into(),
            ..Default::default()
        },
    )));
    assert_eq!(
        bind_delivery(&next, &destination(), Some(&received)),
        Err(Invalid::Regression)
    );
    next = received.clone();
    next.reception = wire::Reception::NotReceived.into();
    assert_eq!(
        bind_delivery(&next, &destination(), Some(&received)),
        Err(Invalid::Regression)
    );
    let mut changed = destination();
    changed.native_delivery = true;
    assert_eq!(
        bind_existing(&context(), &changed, &context(), &destination()),
        Err(Invalid::Regression)
    );
}
#[test]
fn open_grant_is_fresh_short_lived_and_bound_to_the_same_original_resource() {
    let mut delivery = delivery();
    delivery.reception = wire::Reception::Received.into();
    let mut response = wire::ResolveOpenResponse {
        context: context().into(),
        destination_id: destination().id,
        delivery_id: delivery.id.clone(),
        authorized_target: delivery.target.clone(),
        valid_until: stamp(1005).into(),
        ..Default::default()
    };
    assert_eq!(
        bind_open(
            &response,
            &context(),
            &destination(),
            &delivery,
            &stamp(1000)
        ),
        Ok(())
    );
    assert_eq!(
        bind_open(
            &response,
            &context(),
            &destination(),
            &delivery,
            &stamp(1005)
        ),
        Err(Invalid::Expired)
    );
    response.valid_until = stamp(1006).into();
    assert_eq!(
        bind_open(
            &response,
            &context(),
            &destination(),
            &delivery,
            &stamp(1000)
        ),
        Err(Invalid::Expired)
    );
    response.valid_until = stamp(1005).into();
    response.authorized_target.get_or_insert_default().resource = Some(
        wire::target::Resource::Entity(Box::new(registry::EntityId {
            value: "different-entity".into(),
            ..Default::default()
        })),
    );
    assert_eq!(
        bind_open(
            &response,
            &context(),
            &destination(),
            &delivery,
            &stamp(1000)
        ),
        Err(Invalid::ForeignIdentity)
    );
}

#[test]
fn opening_cannot_outlive_the_installation_destination_or_fractional_grant_bound() {
    let mut delivery = delivery();
    delivery.reception = wire::Reception::Received.into();
    let mut destination = destination();
    destination.expires_at = stamp(1003).into();
    let mut response = wire::ResolveOpenResponse {
        context: context().into(),
        destination_id: destination.id.clone(),
        delivery_id: delivery.id.clone(),
        authorized_target: delivery.target.clone(),
        valid_until: stamp(1005).into(),
        ..Default::default()
    };
    assert_eq!(
        bind_open(&response, &context(), &destination, &delivery, &stamp(1000)),
        Err(Invalid::Expired)
    );
    response.valid_until = stamp(1003).into();
    assert_eq!(
        bind_open(&response, &context(), &destination, &delivery, &stamp(1000)),
        Ok(())
    );
    destination.expires_at = stamp(1800).into();
    response.valid_until = stamp(1005).into();
    response.valid_until.get_or_insert_default().nanos = 1;
    assert_eq!(
        bind_open(&response, &context(), &destination, &delivery, &stamp(1000)),
        Err(Invalid::Expired)
    );
    response.valid_until = stamp(1001).into();
    assert_eq!(
        bind_open(&response, &context(), &destination, &delivery, &stamp(999)),
        Err(Invalid::Expired)
    );
}

#[test]
fn uncertain_registration_has_a_read_only_nonce_selector_and_immutable_identity() {
    assert!(admit_request(Request::Get(&wire::GetDestinationRequest::default())).is_err());
    let mut lookup = wire::GetDestinationRequest {
        selector: Some(
            wire::get_destination_request::Selector::RegistrationRequestId("a".repeat(32)),
        ),
        ..Default::default()
    };
    assert!(admit_request(Request::Get(&lookup)).is_ok());
    lookup.selector = Some(
        wire::get_destination_request::Selector::RegistrationRequestId(
            "unbounded freeform attempt".into(),
        ),
    );
    assert!(admit_request(Request::Get(&lookup)).is_err());
    let prior = destination();
    let mut changed = prior.clone();
    changed.revision += 1;
    changed.registration_request_id = "0".repeat(32);
    assert_eq!(
        bind_existing(&context(), &changed, &context(), &prior),
        Err(Invalid::ForeignIdentity)
    );
}

#[test]
fn measured_android_permission_is_admitted_only_for_an_android_destination() {
    let mut value = destination();
    value.permission.get_or_insert_default().source =
        wire::PermissionSource::AndroidNotificationManager.into();
    assert!(valid_permission(&value.permission));
    assert!(can_present(&value.permission));
    assert!(
        !valid_destination(&value),
        "Android evidence cannot impersonate macOS"
    );
    value.platform = companion::Platform::Android.into();
    assert!(valid_destination(&value));
    value.permission.get_or_insert_default().permission = wire::Permission::Provisional.into();
    assert!(
        !valid_permission(&value.permission),
        "Android cannot report a macOS provisional grant"
    );
    value.permission.get_or_insert_default().permission = wire::Permission::Ephemeral.into();
    assert!(!valid_permission(&value.permission));
    value.permission.get_or_insert_default().permission = wire::Permission::Authorized.into();
    value.permission.get_or_insert_default().notification_center = wire::Setting::Unknown.into();
    value.permission.get_or_insert_default().alerts = wire::Setting::Unknown.into();
    assert!(valid_destination(&value));
    assert!(
        !can_present(&value.permission),
        "Missing channel is not observed delivery permission"
    );
    value.native_delivery = true;
    assert!(!valid_destination(&value));
    value.permission = permission().into();
    assert!(
        !valid_destination(&value),
        "macOS evidence cannot impersonate Android"
    );
}
