use buffa::EnumValue;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{
    companion_contract::*,
    home::{auth::v1 as auth, companion::v1 as wire},
};

fn device() -> wire::DeviceDescription {
    let features = [
        wire::Feature::BatteryLevel,
        wire::Feature::BatteryState,
        wire::Feature::Charging,
        wire::Feature::Connectivity,
        wire::Feature::Activity,
        wire::Feature::Focus,
        wire::Feature::DoNotDisturb,
        wire::Feature::Kiosk,
        wire::Feature::ScreenSaver,
        wire::Feature::Brightness,
        wire::Feature::Volume,
        wire::Feature::Location,
        wire::Feature::CameraMotion,
        wire::Feature::Notifications,
    ];
    wire::DeviceDescription {
        platform: wire::Platform::MacOs.into(),
        model: "Synthetic desktop model".into(),
        os_version: "synthetic-1".into(),
        capabilities: features
            .into_iter()
            .enumerate()
            .map(|(index, feature)| {
                let (availability, source, permission) = if index < 3 {
                    (
                        wire::Availability::Available,
                        wire::Source::MacOsPowerSources,
                        wire::Permission::NotRequired,
                    )
                } else if index == 3 {
                    (
                        wire::Availability::Partial,
                        wire::Source::MacOsNetworkConfiguration,
                        wire::Permission::NotRequired,
                    )
                } else if index == 4 {
                    (
                        wire::Availability::Partial,
                        wire::Source::NativeLifecycle,
                        wire::Permission::NotRequired,
                    )
                } else {
                    (
                        wire::Availability::Unsupported,
                        wire::Source::None,
                        wire::Permission::Unsupported,
                    )
                };
                wire::Capability {
                    feature: feature.into(),
                    availability: availability.into(),
                    source: source.into(),
                    permission: permission.into(),
                    ..Default::default()
                }
            })
            .collect(),
        ..Default::default()
    }
}
fn consent() -> wire::Consent {
    wire::Consent {
        battery: true,
        connectivity: true,
        activity: true,
        cadence: wire::Cadence::Minute1.into(),
        ..Default::default()
    }
}
fn snapshot() -> wire::Snapshot {
    wire::Snapshot {
        captured_at: Timestamp {
            seconds: 1_800_000_000,
            ..Default::default()
        }
        .into(),
        battery: wire::BatteryObservation {
            observation: Some(wire::battery_observation::Observation::NoData(
                wire::NoData::NoHardware.into(),
            )),
            ..Default::default()
        }
        .into(),
        connectivity: wire::ConnectivityObservation {
            observation: Some(wire::connectivity_observation::Observation::NoData(
                wire::NoData::PermissionDenied.into(),
            )),
            ..Default::default()
        }
        .into(),
        activity: wire::ActivityObservation {
            observation: Some(wire::activity_observation::Observation::NoData(
                wire::NoData::Stale.into(),
            )),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn installation() -> wire::Installation {
    wire::Installation {
        id: wire::InstallationId {
            value: "a".repeat(32),
            ..Default::default()
        }
        .into(),
        device_id: wire::DeviceId {
            value: "b".repeat(32),
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
        device: device().into(),
        consent: consent().into(),
        revision: 2,
        state: wire::InstallationState::Active.into(),
        expires_at: Timestamp {
            seconds: 1_800_001_800,
            ..Default::default()
        }
        .into(),
        registration_request_id: "c".repeat(32),
        last_sequence: 1,
        latest: snapshot().into(),
        accepted_at: Timestamp {
            seconds: 1_800_000_001,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn context() -> wire::Context {
    wire::Context {
        deployment: wire::Deployment::SyntheticOnly.into(),
        epoch: "d".repeat(32),
        ..Default::default()
    }
}

#[test]
fn whole_capability_matrix_is_bounded_and_platform_qualification_is_not_invented() {
    let mut d = device();
    assert!(valid_device(&d));
    d.capabilities[0].feature = wire::Feature::Connectivity.into();
    assert!(!valid_device(&d));
    d = device();
    d.capabilities[0].source = wire::Source::NativeLifecycle.into();
    assert!(!valid_device(&d));
    d = device();
    d.capabilities[0].feature = EnumValue::Unknown(999);
    assert!(!valid_device(&d));
    d = device();
    d.model = "x".repeat(129);
    assert!(!valid_device(&d));
    d = device();
    d.platform = wire::Platform::Android.into();
    assert!(!valid_device(&d));
}
#[test]
fn no_hardware_denied_and_stale_survive_without_zero_or_off() {
    let s = snapshot();
    assert!(valid_snapshot(&s));
    assert!(snapshot_matches_consent(&s, &consent()));
    let mut c = consent();
    c.battery = false;
    assert!(!snapshot_matches_consent(&s, &c));
    let mut s = s;
    s.battery.get_or_insert_default().observation = Some(
        wire::battery_observation::Observation::Value(Box::new(wire::Battery {
            percent: 101,
            state: wire::PowerState::Battery.into(),
            ..Default::default()
        })),
    );
    assert!(!valid_snapshot(&s));
    let mut s = snapshot();
    s.connectivity.get_or_insert_default().observation = Some(
        wire::connectivity_observation::Observation::NoData(wire::NoData::NoHardware.into()),
    );
    assert!(!valid_snapshot(&s));
}
#[test]
fn own_installation_is_bound_to_native_account_session_and_device() {
    let record = installation();
    assert!(valid_installation(&record));
    let account = home_wire_contract::auth_contract::AccountId::parse("synthetic-account").unwrap();
    let session = home_wire_contract::auth_contract::SessionId::parse("synthetic-session").unwrap();
    assert!(bind_native(&record, &account, &session).is_ok());
    let other = home_wire_contract::auth_contract::SessionId::parse("other-session").unwrap();
    assert_eq!(
        bind_native(&record, &account, &other),
        Err(Invalid::ForeignIdentity)
    );
    let mut swapped = record.clone();
    swapped.device_id.get_or_insert_default().value = "e".repeat(32);
    assert_eq!(
        bind_existing(&context(), &swapped, &context(), &record),
        Err(Invalid::ForeignIdentity)
    );
    let mut restarted = context();
    restarted.epoch = "f".repeat(32);
    assert_eq!(
        bind_existing(&restarted, &record, &context(), &record),
        Err(Invalid::StaleContext)
    );
}
#[test]
fn withdrawn_consent_or_revocation_cannot_keep_private_values() {
    let mut record = installation();
    record.consent.get_or_insert_default().battery = false;
    assert!(!valid_installation(&record));
    record = installation();
    record.state = wire::InstallationState::Revoked.into();
    assert!(!valid_installation(&record));
    record.consent = wire::Consent {
        cadence: wire::Cadence::Minute1.into(),
        ..Default::default()
    }
    .into();
    record.latest = Default::default();
    record.accepted_at = Default::default();
    assert!(valid_installation(&record));
    record = installation();
    record
        .latest
        .get_or_insert_default()
        .battery
        .get_or_insert_default()
        .observation = Some(wire::battery_observation::Observation::Value(Box::new(
        wire::Battery {
            percent: 50,
            state: EnumValue::Unknown(99),
            ..Default::default()
        },
    )));
    assert!(!valid_installation(&record));
}
#[test]
fn requests_and_responses_reject_defaults_missing_arms_and_overflow() {
    let mut request = wire::PublishTelemetryRequest {
        expected: context().into(),
        id: installation().id,
        expected_revision: 2,
        sequence: 1,
        snapshot: snapshot().into(),
        ..Default::default()
    };
    assert!(admit_request(Request::Publish(&request)).is_ok());
    request.sequence = u64::MAX;
    assert!(admit_request(Request::Publish(&request)).is_err());
    assert!(admit_request(Request::Get(&wire::GetInstallationRequest::default())).is_err());
    let mut response = wire::GetInstallationResponse {
        context: context().into(),
        installation: installation().into(),
        ..Default::default()
    };
    assert!(admit_response(Response::Get(&response)).is_ok());
    response.context.get_or_insert_default().deployment = EnumValue::Unknown(99);
    assert!(admit_response(Response::Get(&response)).is_err());
    assert!(RequestId::parse("opaque freeform ID").is_err());
    assert_eq!(cadence_seconds(wire::Cadence::default().into()), None);
}

#[test]
fn first_mutation_requires_a_previously_admitted_synthetic_context() {
    let mut registration = wire::RegisterInstallationRequest {
        request_id: "a".repeat(32),
        device: device().into(),
        ..Default::default()
    };
    assert!(admit_request(Request::Register(&registration)).is_err());
    registration.expected = context().into();
    assert!(admit_request(Request::Register(&registration)).is_ok());
    assert!(
        admit_response(Response::Describe(
            &wire::DescribeCompanionResponse::default()
        ))
        .is_err()
    );
    assert!(
        admit_response(Response::Describe(&wire::DescribeCompanionResponse {
            context: context().into(),
            ..Default::default()
        }))
        .is_ok()
    );
}

#[test]
fn android_native_sources_cannot_impersonate_macos_collectors() {
    let mut android = device();
    android.platform = wire::Platform::Android.into();
    for c in &mut android.capabilities {
        match c.feature {
            EnumValue::Known(
                wire::Feature::BatteryLevel | wire::Feature::BatteryState | wire::Feature::Charging,
            ) => c.source = wire::Source::AndroidBatteryManager.into(),
            EnumValue::Known(wire::Feature::Connectivity) => {
                c.source = wire::Source::AndroidConnectivityManager.into()
            }
            EnumValue::Known(wire::Feature::Activity | wire::Feature::Focus) => (),
            EnumValue::Known(
                wire::Feature::DoNotDisturb
                | wire::Feature::Kiosk
                | wire::Feature::ScreenSaver
                | wire::Feature::Brightness
                | wire::Feature::Volume
                | wire::Feature::Location
                | wire::Feature::CameraMotion
                | wire::Feature::Notifications,
            ) => {
                c.availability = wire::Availability::PendingQualification.into();
                c.permission = wire::Permission::NotRequested.into();
            }
            EnumValue::Known(wire::Feature::Unspecified) | EnumValue::Unknown(_) => {
                panic!("fixture must use known features")
            }
        }
    }
    assert!(valid_device(&android));
    android.capabilities[0].source = wire::Source::MacOsPowerSources.into();
    assert!(!valid_device(&android));
    let mut macos = device();
    macos.capabilities[0].source = wire::Source::AndroidBatteryManager.into();
    assert!(!valid_device(&macos));
}
