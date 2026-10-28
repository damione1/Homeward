//! Explicit isolated protocol qualification, not a native OS or owner/UI proof.
use buffa_types::google::protobuf::Timestamp;
use home_client_transport::{AccessToken, Connection, FailureKind, RpcCode};
use home_wire_contract::{
    auth_contract::{AccountId, SessionId},
    companion_contract as admit,
    home::{auth::v1 as auth, companion::v1 as wire},
};
use serde::Deserialize;
use std::{
    path::Path,
    time::{Duration, SystemTime},
};
use tokio_util::sync::CancellationToken;

const TIMEOUT: Duration = Duration::from_secs(10);
#[derive(Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Mode {
    SyntheticOnly,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Credentials {
    login_name: String,
    password: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    user: Credentials,
    admin: Credentials,
    production_configuration: bool,
    no_physical_providers: bool,
}

async fn login(
    c: &Connection,
    credential: &Credentials,
    cancel: &CancellationToken,
) -> auth::SessionTokens {
    let response = c
        .auth()
        .start_login(
            auth::StartLoginRequest {
                login_name: credential.login_name.clone(),
                password: credential.password.clone(),
                ..Default::default()
            },
            TIMEOUT,
            cancel,
        )
        .await
        .unwrap();
    match response.decision.outcome.as_ref() {
        Some(auth::login_decision::Outcome::Authenticated(tokens)) => (**tokens).clone(),
        Some(
            auth::login_decision::Outcome::Challenge(_)
            | auth::login_decision::Outcome::EnrollmentRequired(_),
        )
        | None => panic!("expected actual native authentication"),
    }
}
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
        model: "Synthetic protocol desktop".into(),
        os_version: "synthetic-1".into(),
        capabilities: features
            .into_iter()
            .map(|feature| {
                let (availability, source, permission) = match feature {
                    wire::Feature::BatteryLevel
                    | wire::Feature::BatteryState
                    | wire::Feature::Charging => (
                        wire::Availability::Available,
                        wire::Source::MacOsPowerSources,
                        wire::Permission::NotRequired,
                    ),
                    wire::Feature::Connectivity => (
                        wire::Availability::Partial,
                        wire::Source::MacOsNetworkConfiguration,
                        wire::Permission::NotRequired,
                    ),
                    wire::Feature::Activity => (
                        wire::Availability::Partial,
                        wire::Source::NativeLifecycle,
                        wire::Permission::NotRequired,
                    ),
                    wire::Feature::Focus
                    | wire::Feature::DoNotDisturb
                    | wire::Feature::Kiosk
                    | wire::Feature::ScreenSaver
                    | wire::Feature::Brightness
                    | wire::Feature::Volume
                    | wire::Feature::Location
                    | wire::Feature::CameraMotion
                    | wire::Feature::Notifications => (
                        wire::Availability::Unsupported,
                        wire::Source::None,
                        wire::Permission::Unsupported,
                    ),
                    wire::Feature::Unspecified => unreachable!("explicit capability set"),
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
        activity: false,
        cadence: wire::Cadence::Seconds30.into(),
        ..Default::default()
    }
}
fn snapshot() -> wire::Snapshot {
    let now = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap();
    wire::Snapshot {
        captured_at: Timestamp {
            seconds: now.as_secs().try_into().unwrap(),
            nanos: now.subsec_nanos().try_into().unwrap(),
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
                wire::NoData::Disabled.into(),
            )),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn get(id: wire::InstallationId) -> wire::GetInstallationRequest {
    wire::GetInstallationRequest {
        selector: Some(wire::get_installation_request::Selector::Id(Box::new(id))),
        ..Default::default()
    }
}
fn publish(
    context: &wire::Context,
    record: &wire::Installation,
    sequence: u64,
) -> wire::PublishTelemetryRequest {
    wire::PublishTelemetryRequest {
        expected: context.clone().into(),
        id: record.id.clone(),
        expected_revision: record.revision,
        sequence,
        snapshot: snapshot().into(),
        ..Default::default()
    }
}
#[derive(Clone, Copy)]
enum LostReply {
    Register,
    Consent,
    Publish,
}
fn lose(root: &Path, reply: LostReply) {
    let method = match reply {
        LostReply::Register => "RegisterInstallation",
        LostReply::Consent => "UpdateConsent",
        LostReply::Publish => "PublishTelemetry",
    };
    std::fs::write(
        root.join("lose-next-reply"),
        format!("/home.companion.v1.CompanionService/{method}"),
    )
    .unwrap();
}

#[tokio::test]
#[ignore = "requires explicit fresh isolated Go/PG/TLS companion fixture"]
async fn actual_companion_tls_identity_consent_cadence_uncertainty_and_revocation() {
    let path =
        std::env::var("HOMEWARD_NHF021_TLS_FIXTURE").expect("explicit companion fixture required");
    let root = Path::new(&path);
    assert!(root.is_absolute());
    let fixture: Fixture =
        serde_json::from_slice(&std::fs::read(root.join("ready.json")).unwrap()).unwrap();
    assert!(matches!(fixture.mode, Mode::SyntheticOnly));
    assert!(!fixture.production_configuration && fixture.no_physical_providers);
    let uri: http::Uri = fixture.origin.parse().unwrap();
    assert_eq!(uri.scheme_str(), Some("https"));
    assert!(
        uri.host()
            .unwrap()
            .parse::<std::net::IpAddr>()
            .unwrap()
            .is_loopback()
    );
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes()).unwrap();
    let cancel = CancellationToken::new();
    let api = connection.companion();
    let user = login(&connection, &fixture.user, &cancel).await;
    let token = AccessToken::parse(&user.access_token).unwrap();
    let described = api
        .describe_companion(
            wire::DescribeCompanionRequest::default(),
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Describe(&described)).unwrap();
    let expected = (*described.context).clone();
    let mut stale = expected.clone();
    stale.epoch = "0".repeat(32);
    assert_eq!(
        api.register_installation(
            wire::RegisterInstallationRequest {
                request_id: "0".repeat(32),
                device: device().into(),
                expected: stale.into(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::FailedPrecondition)
    );
    let registered = api
        .register_installation(
            wire::RegisterInstallationRequest {
                request_id: "1".repeat(32),
                expected: expected.clone().into(),
                device: device().into(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Register(&registered)).unwrap();
    admit::bind_native(
        &registered.installation,
        &AccountId::try_from(&*user.session.account_id).unwrap(),
        &SessionId::try_from(&*user.session.id).unwrap(),
    )
    .unwrap();
    let context = (*registered.context).clone();
    let mut record = (*registered.installation).clone();
    assert!(!record.consent.battery && !record.consent.connectivity && !record.consent.activity);
    assert!(record.latest.as_option().is_none());
    assert_ne!(record.id.value, record.device_id.value);
    assert_eq!(
        api.publish_telemetry(publish(&context, &record, 1), &token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::PermissionDenied)
    );
    let other_session = login(&connection, &fixture.user, &cancel).await;
    let other_token = AccessToken::parse(&other_session.access_token).unwrap();
    assert_ne!(other_session.session.id, user.session.id);
    assert_eq!(
        api.get_installation(get((*record.id).clone()), &other_token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::NotFound)
    );
    let admin = login(&connection, &fixture.admin, &cancel).await;
    let admin_token = AccessToken::parse(&admin.access_token).unwrap();
    assert_eq!(
        api.get_installation(get((*record.id).clone()), &admin_token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::NotFound)
    );
    let enabled = api
        .update_consent(
            wire::UpdateConsentRequest {
                expected: context.clone().into(),
                id: record.id.clone(),
                expected_revision: record.revision,
                consent: consent().into(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Consent(&enabled)).unwrap();
    assert_eq!(enabled.installation.revision, record.revision + 1);
    record = (*enabled.installation).clone();
    let first = api
        .publish_telemetry(publish(&context, &record, 1), &token, TIMEOUT, &cancel)
        .await
        .unwrap();
    admit::admit_response(admit::Response::Publish(&first)).unwrap();
    assert_eq!(first.installation.last_sequence, 1);
    record = (*first.installation).clone();
    assert!(
        matches!(record.latest.battery.observation,Some(wire::battery_observation::Observation::NoData(n)) if n==wire::NoData::NoHardware)
    );
    assert_eq!(
        api.publish_telemetry(publish(&context, &record, 2), &token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::ResourceExhausted)
    );
    assert_eq!(
        api.publish_telemetry(publish(&context, &record, 1), &token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::InvalidArgument)
    );
    let mut forbidden = publish(&context, &record, 2);
    forbidden
        .snapshot
        .get_or_insert_default()
        .activity
        .get_or_insert_default()
        .observation = Some(wire::activity_observation::Observation::Value(
        wire::AppActivity::Foreground.into(),
    ));
    assert_eq!(
        api.publish_telemetry(forbidden, &token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::InvalidArgument)
    );
    let rotated = connection
        .auth()
        .refresh_session(
            auth::RefreshSessionRequest {
                refresh_token: user.refresh_token.clone(),
                ..Default::default()
            },
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    assert_eq!(rotated.tokens.session.id, user.session.id);
    let refreshed = AccessToken::parse(&rotated.tokens.access_token).unwrap();
    assert_eq!(
        api.get_installation(get((*record.id).clone()), &token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::Unauthenticated)
    );
    let token = refreshed;
    let current = api
        .get_installation(get((*record.id).clone()), &token, TIMEOUT, &cancel)
        .await
        .unwrap();
    admit::admit_response(admit::Response::Get(&current)).unwrap();
    lose(root, LostReply::Consent);
    assert!(
        api.update_consent(
            wire::UpdateConsentRequest {
                expected: context.clone().into(),
                id: record.id.clone(),
                expected_revision: record.revision,
                consent: consent().into(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .is_err()
    );
    let resolved = api
        .get_installation(get((*record.id).clone()), &token, TIMEOUT, &cancel)
        .await
        .unwrap();
    admit::admit_response(admit::Response::Get(&resolved)).unwrap();
    assert_eq!(resolved.installation.revision, record.revision + 1);
    assert!(resolved.installation.latest.as_option().is_none());
    record = (*resolved.installation).clone();
    // A consent round trip does not reset the server's publication rate limit.
    assert_eq!(
        api.publish_telemetry(publish(&context, &record, 2), &token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::ResourceExhausted)
    );
    let revoked = api
        .revoke_installation(
            wire::RevokeInstallationRequest {
                expected: context.clone().into(),
                id: record.id.clone(),
                expected_revision: record.revision,
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Revoke(&revoked)).unwrap();
    assert_eq!(revoked.installation.state, wire::InstallationState::Revoked);
    assert_eq!(
        api.publish_telemetry(publish(&context, &record, 2), &token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::PermissionDenied)
    );
    // Revoking the companion installation does not revoke its login session.
    connection
        .auth()
        .list_sessions(
            auth::ListSessionsRequest {
                account_id: user.session.account_id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    lose(root, LostReply::Register);
    assert!(
        api.register_installation(
            wire::RegisterInstallationRequest {
                request_id: "2".repeat(32),
                expected: expected.clone().into(),
                device: device().into(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .is_err()
    );
    let resolved = api
        .get_installation(
            wire::GetInstallationRequest {
                selector: Some(
                    wire::get_installation_request::Selector::RegistrationRequestId("2".repeat(32)),
                ),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Get(&resolved)).unwrap();
    let second = (*resolved.installation).clone();
    assert_ne!(second.id, record.id);
    assert_eq!(second.registration_request_id, "2".repeat(32));
    let enabled = api
        .update_consent(
            wire::UpdateConsentRequest {
                expected: context.clone().into(),
                id: second.id.clone(),
                expected_revision: second.revision,
                consent: consent().into(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    let second = (*enabled.installation).clone();
    lose(root, LostReply::Publish);
    assert!(
        api.publish_telemetry(publish(&context, &second, 1), &token, TIMEOUT, &cancel)
            .await
            .is_err()
    );
    let resolved = api
        .get_installation(get((*second.id).clone()), &token, TIMEOUT, &cancel)
        .await
        .unwrap();
    admit::admit_response(admit::Response::Get(&resolved)).unwrap();
    assert_eq!(resolved.installation.last_sequence, 1);
    std::fs::write(
        root.join("revoke-account"),
        b"explicit isolated fixture control",
    )
    .unwrap();
    tokio::time::timeout(Duration::from_secs(10), async {
        while !root.join("account-revoked.json").is_file() {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
    assert_eq!(
        api.publish_telemetry(publish(&context, &second, 2), &token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::Unauthenticated)
    );
    assert_eq!(
        api.get_installation(get((*second.id).clone()), &token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::Unauthenticated)
    );
    println!("NHF021_COMPANION_TRANSPORT_TLS_PASS");
}
