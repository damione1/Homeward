//! Actual isolated native-session TLS protocol, with injected OS receipts.
//! Not a foreground delegate, native presentation or owner/UI qualification.
use buffa_types::google::protobuf::Timestamp;
use home_client_transport::{AccessToken, Connection, FailureKind, NotificationTransport, RpcCode};
use home_wire_contract::{
    auth_contract::{AccountId, SessionId},
    companion_contract as companion_admit,
    home::{auth::v1 as auth, companion::v1 as companion, notifications::v1 as wire},
    notifications_contract as admit,
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
fn device() -> companion::DeviceDescription {
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
    companion::DeviceDescription {
        platform: companion::Platform::MacOs.into(),
        model: "Synthetic protocol desktop".into(),
        os_version: "synthetic-1".into(),
        capabilities: features
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
                    companion::Feature::Unspecified => unreachable!("explicit capability set"),
                };
                companion::Capability {
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
fn permission() -> wire::PermissionSnapshot {
    wire::PermissionSnapshot {
        permission: wire::Permission::Authorized.into(),
        alerts: wire::Setting::Enabled.into(),
        notification_center: wire::Setting::Enabled.into(),
        source: wire::PermissionSource::MacOsUserNotifications.into(),
        ..Default::default()
    }
}
fn now() -> Timestamp {
    let value = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap();
    Timestamp {
        seconds: value.as_secs().try_into().unwrap(),
        nanos: value.subsec_nanos().try_into().unwrap(),
        ..Default::default()
    }
}
fn nonce(value: u128) -> String {
    format!("{value:032x}")
}
fn get_destination(id: wire::DestinationId) -> wire::GetDestinationRequest {
    wire::GetDestinationRequest {
        selector: Some(wire::get_destination_request::Selector::Id(Box::new(id))),
        ..Default::default()
    }
}
fn update(
    context: &companion::Context,
    d: &wire::Destination,
    native: bool,
    permission: wire::PermissionSnapshot,
    n: u128,
) -> wire::UpdateDestinationRequest {
    wire::UpdateDestinationRequest {
        expected_context: context.clone().into(),
        id: d.id.clone(),
        expected_revision: d.revision,
        request_id: nonce(n),
        native_delivery: native,
        permission: permission.into(),
        ..Default::default()
    }
}
fn acknowledge(
    context: &companion::Context,
    d: &wire::Destination,
    entry: &wire::Delivery,
    kind: wire::ReceiptKind,
    n: u128,
) -> wire::AcknowledgeDeliveryRequest {
    wire::AcknowledgeDeliveryRequest {
        expected_context: context.clone().into(),
        destination_id: d.id.clone(),
        delivery_id: entry.id.clone(),
        expected_revision: entry.revision,
        request_id: nonce(n),
        kind: kind.into(),
        observed_at: now().into(),
        ..Default::default()
    }
}
#[derive(Clone, Copy)]
enum LostReply {
    Register,
    Update,
    Receipt,
}
fn lose(root: &Path, reply: LostReply) {
    let procedure = match reply {
        LostReply::Register => "RegisterDestination",
        LostReply::Update => "UpdateDestination",
        LostReply::Receipt => "AcknowledgeDelivery",
    };
    std::fs::write(
        root.join("lose-next-reply"),
        format!("/home.notifications.v1.NotificationService/{procedure}"),
    )
    .unwrap();
}
#[derive(Clone, Copy)]
enum Control {
    Nominal,
    Entity,
    Expiring,
    WithdrawTarget,
    RevokeAccount,
}
impl Control {
    fn file(self) -> &'static str {
        match self {
            Self::Nominal => "seed-nominal",
            Self::Entity => "seed-entity",
            Self::Expiring => "seed-expiring",
            Self::WithdrawTarget => "withdraw-target",
            Self::RevokeAccount => "revoke-account",
        }
    }
    fn acknowledgement(self) -> String {
        match self {
            Self::RevokeAccount => "account-revoked.json".into(),
            Self::Nominal | Self::Entity | Self::Expiring | Self::WithdrawTarget => {
                format!("{}-done.json", self.file())
            }
        }
    }
}
async fn control(root: &Path, value: Control) {
    // Each explicit fixture control must wait for its own acknowledgement.
    // A previous use of the same scenario cannot acknowledge the next one.
    match std::fs::remove_file(root.join(value.acknowledgement())) {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => panic!("cannot retire prior fixture acknowledgement: {error}"),
    }
    std::fs::write(
        root.join(value.file()),
        b"explicit isolated fixture control",
    )
    .unwrap();
    tokio::time::timeout(TIMEOUT, async {
        while !root.join(value.acknowledgement()).is_file() {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
}
struct ReceiptClient<'a> {
    api: &'a NotificationTransport,
    context: &'a companion::Context,
    token: &'a AccessToken,
    cancel: &'a CancellationToken,
}
impl ReceiptClient<'_> {
    async fn receipt(
        &self,
        d: &wire::Destination,
        entry: &wire::Delivery,
        kind: wire::ReceiptKind,
        n: u128,
    ) -> wire::Delivery {
        let request = acknowledge(self.context, d, entry, kind, n);
        admit::admit_request(admit::Request::Acknowledge(&request)).unwrap();
        let response = self
            .api
            .acknowledge_delivery(request, self.token, TIMEOUT, self.cancel)
            .await
            .unwrap();
        admit::admit_response(admit::Response::Acknowledge(&response)).unwrap();
        admit::bind_delivery(&response.delivery, d, Some(entry)).unwrap();
        (*response.delivery).clone()
    }
}

#[tokio::test]
#[ignore = "requires an explicit fresh isolated Go-race/PG/TLS notification fixture"]
async fn actual_notifications_tls_private_inbox_receipts_targets_and_revocation() {
    let path = std::env::var("HOMEWARD_NHF022_TLS_FIXTURE")
        .expect("explicit notification fixture required");
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
    let companion_api = connection.companion();
    let api = connection.notifications();
    let user = login(&connection, &fixture.user, &cancel).await;
    let token = AccessToken::parse(&user.access_token).unwrap();
    let account = AccountId::try_from(&*user.session.account_id).unwrap();
    let session = SessionId::try_from(&*user.session.id).unwrap();
    let description = companion_api
        .describe_companion(
            companion::DescribeCompanionRequest::default(),
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    companion_admit::admit_response(companion_admit::Response::Describe(&description)).unwrap();
    let registered = companion_api
        .register_installation(
            companion::RegisterInstallationRequest {
                expected: description.context.clone(),
                device: device().into(),
                request_id: nonce(1),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    companion_admit::admit_response(companion_admit::Response::Register(&registered)).unwrap();
    let installation = (*registered.installation).clone();
    let described = api
        .describe_notifications(
            wire::DescribeNotificationsRequest::default(),
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Describe(&described)).unwrap();
    let context = (*described.context).clone();
    assert_eq!(described.context, registered.context);
    let receipts = ReceiptClient {
        api: &api,
        context: &context,
        token: &token,
        cancel: &cancel,
    };
    let mut wrong = context.clone();
    wrong.epoch = nonce(0);
    let mut registration = wire::RegisterDestinationRequest {
        expected_context: wrong.into(),
        installation_id: installation.id.clone(),
        request_id: nonce(2),
        permission: permission().into(),
        ..Default::default()
    };
    assert_eq!(
        api.register_destination(registration.clone(), &token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::FailedPrecondition)
    );
    registration.expected_context = context.clone().into();
    admit::admit_request(admit::Request::Register(&registration)).unwrap();
    lose(root, LostReply::Register);
    assert!(
        api.register_destination(registration.clone(), &token, TIMEOUT, &cancel)
            .await
            .is_err()
    );
    let resolved = api
        .get_destination(
            wire::GetDestinationRequest {
                selector: Some(
                    wire::get_destination_request::Selector::RegistrationRequestId(nonce(2)),
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
    let mut destination = (*resolved.destination).clone();
    admit::bind_native(&destination, &installation, &account, &session).unwrap();
    assert!(!destination.native_delivery);
    assert_eq!(destination.last_request_id, nonce(2));
    assert_ne!(destination.id.value, destination.installation_id.value);
    assert_eq!(
        api.register_destination(registration, &token, TIMEOUT, &cancel)
            .await
            .unwrap_err()
            .kind(),
        FailureKind::Rpc(RpcCode::AlreadyExists)
    );
    let other = login(&connection, &fixture.user, &cancel).await;
    let other_token = AccessToken::parse(&other.access_token).unwrap();
    let admin = login(&connection, &fixture.admin, &cancel).await;
    let admin_token = AccessToken::parse(&admin.access_token).unwrap();
    for foreign in [&other_token, &admin_token] {
        assert_eq!(
            api.get_destination(
                get_destination((*destination.id).clone()),
                foreign,
                TIMEOUT,
                &cancel
            )
            .await
            .unwrap_err()
            .kind(),
            FailureKind::Rpc(RpcCode::NotFound)
        );
        assert_eq!(
            api.read_inbox(
                wire::ReadInboxRequest {
                    id: destination.id.clone(),
                    ..Default::default()
                },
                foreign,
                TIMEOUT,
                &cancel
            )
            .await
            .unwrap_err()
            .kind(),
            FailureKind::Rpc(RpcCode::NotFound)
        );
    }
    let initial = api
        .read_inbox(
            wire::ReadInboxRequest {
                id: destination.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Inbox(&initial)).unwrap();
    assert!(initial.entries.is_empty());
    let before = destination.clone();
    lose(root, LostReply::Update);
    assert!(
        api.update_destination(
            update(&context, &before, true, permission(), 3),
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .is_err()
    );
    let read = api
        .get_destination(
            get_destination((*destination.id).clone()),
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Get(&read)).unwrap();
    admit::bind_existing(&read.context, &read.destination, &context, &before).unwrap();
    destination = (*read.destination).clone();
    assert!(destination.native_delivery);
    assert_eq!(destination.last_request_id, nonce(3));
    assert_eq!(
        api.update_destination(
            update(&context, &before, false, permission(), 4),
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::Aborted)
    );

    control(root, Control::Nominal).await;
    let inbox = api
        .read_inbox(
            wire::ReadInboxRequest {
                id: destination.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Inbox(&inbox)).unwrap();
    assert_eq!(inbox.entries.len(), 2, "already expired message escaped");
    let initial_message = inbox
        .entries
        .iter()
        .find(|v| v.transport_acceptance == wire::TransportAcceptance::AcceptedForegroundLan)
        .unwrap()
        .clone();
    let provider_failed = inbox
        .entries
        .iter()
        .find(|v| v.transport_acceptance == wire::TransportAcceptance::ProviderRejected)
        .unwrap()
        .clone();
    assert_eq!(
        initial_message.presentation,
        wire::Presentation::NotAttempted
    );
    lose(root, LostReply::Receipt);
    assert!(
        api.acknowledge_delivery(
            acknowledge(
                &context,
                &destination,
                &initial_message,
                wire::ReceiptKind::Received,
                5
            ),
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .is_err()
    );
    let read = api
        .get_delivery(
            wire::GetDeliveryRequest {
                destination_id: destination.id.clone(),
                delivery_id: initial_message.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Delivery(&read)).unwrap();
    admit::bind_delivery(&read.delivery, &destination, Some(&initial_message)).unwrap();
    let mut message = (*read.delivery).clone();
    assert_eq!(message.reception, wire::Reception::Received);
    assert_eq!(message.last_request_id, nonce(5));
    assert_eq!(
        api.acknowledge_delivery(
            acknowledge(
                &context,
                &destination,
                &initial_message,
                wire::ReceiptKind::Received,
                6
            ),
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::Aborted)
    );
    assert!(admit::eligible_for_native(&message, &destination, &now()));
    // These receipts deliberately model a native report for the protocol.
    // They do not attest an actual OS call or presentation.
    message = receipts
        .receipt(&destination, &message, wire::ReceiptKind::OsAccepted, 7)
        .await;
    assert!(!admit::eligible_for_native(&message, &destination, &now()));
    message = receipts
        .receipt(
            &destination,
            &message,
            wire::ReceiptKind::PresentInOsCenter,
            8,
        )
        .await;
    let provider_failed = receipts
        .receipt(
            &destination,
            &provider_failed,
            wire::ReceiptKind::Received,
            9,
        )
        .await;
    assert!(!admit::eligible_for_native(
        &provider_failed,
        &destination,
        &now()
    ));
    assert_eq!(
        api.acknowledge_delivery(
            acknowledge(
                &context,
                &destination,
                &provider_failed,
                wire::ReceiptKind::OsAccepted,
                10
            ),
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::PermissionDenied)
    );
    let open = api
        .resolve_open(
            wire::ResolveOpenRequest {
                destination_id: destination.id.clone(),
                delivery_id: message.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::bind_open(&open, &context, &destination, &message, &now()).unwrap();
    message = receipts
        .receipt(&destination, &message, wire::ReceiptKind::Read, 11)
        .await;
    message = receipts
        .receipt(&destination, &message, wire::ReceiptKind::Dismissed, 12)
        .await;
    assert_eq!(message.read_state, wire::ReadState::Dismissed);
    let inbox = api
        .read_inbox(
            wire::ReadInboxRequest {
                id: destination.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Inbox(&inbox)).unwrap();
    assert_eq!(inbox.entries.len(), 1);
    assert_eq!(
        api.resolve_open(
            wire::ResolveOpenRequest {
                destination_id: destination.id.clone(),
                delivery_id: message.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::PermissionDenied)
    );

    control(root, Control::Entity).await;
    let inbox = api
        .read_inbox(
            wire::ReadInboxRequest {
                id: destination.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Inbox(&inbox)).unwrap();
    let entity = inbox
        .entries
        .iter()
        .find(|entry| {
            matches!(
                entry.target.resource.as_ref(),
                Some(wire::target::Resource::Entity(_))
            )
        })
        .unwrap()
        .clone();
    let entity = receipts
        .receipt(&destination, &entity, wire::ReceiptKind::Received, 13)
        .await;
    let open = api
        .resolve_open(
            wire::ResolveOpenRequest {
                destination_id: destination.id.clone(),
                delivery_id: entity.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::bind_open(&open, &context, &destination, &entity, &now()).unwrap();
    control(root, Control::WithdrawTarget).await;
    assert_eq!(
        api.resolve_open(
            wire::ResolveOpenRequest {
                destination_id: destination.id.clone(),
                delivery_id: entity.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::PermissionDenied)
    );
    control(root, Control::Expiring).await;
    let inbox = api
        .read_inbox(
            wire::ReadInboxRequest {
                id: destination.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    let expiring = inbox
        .entries
        .iter()
        .find(|entry| entry.expires_at.seconds - entry.created_at.seconds <= 3)
        .unwrap()
        .clone();
    tokio::time::sleep(Duration::from_millis(2300)).await;
    assert_eq!(
        api.get_delivery(
            wire::GetDeliveryRequest {
                destination_id: destination.id.clone(),
                delivery_id: expiring.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::NotFound)
    );
    assert_eq!(
        api.resolve_open(
            wire::ResolveOpenRequest {
                destination_id: destination.id.clone(),
                delivery_id: expiring.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::NotFound)
    );

    let mut denied = permission();
    denied.permission = wire::Permission::Denied.into();
    assert_eq!(
        api.update_destination(
            update(&context, &destination, true, denied.clone(), 14),
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::InvalidArgument)
    );
    let changed = api
        .update_destination(
            update(&context, &destination, false, denied, 15),
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Update(&changed)).unwrap();
    admit::bind_existing(
        &changed.context,
        &changed.destination,
        &context,
        &destination,
    )
    .unwrap();
    destination = (*changed.destination).clone();
    let changed = api
        .update_destination(
            update(&context, &destination, true, permission(), 16),
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Update(&changed)).unwrap();
    destination = (*changed.destination).clone();
    let revoked = api
        .revoke_destination(
            wire::RevokeDestinationRequest {
                expected_context: context.clone().into(),
                id: destination.id.clone(),
                expected_revision: destination.revision,
                request_id: nonce(17),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Revoke(&revoked)).unwrap();
    assert!(!revoked.destination.native_delivery);
    assert_eq!(
        api.read_inbox(
            wire::ReadInboxRequest {
                id: destination.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::PermissionDenied)
    );
    let second = api
        .register_destination(
            wire::RegisterDestinationRequest {
                expected_context: context.clone().into(),
                installation_id: installation.id.clone(),
                request_id: nonce(18),
                permission: permission().into(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Register(&second)).unwrap();
    let second = api
        .update_destination(
            update(&context, &second.destination, true, permission(), 19),
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Update(&second)).unwrap();
    control(root, Control::Nominal).await;
    let retired = companion_api
        .revoke_installation(
            companion::RevokeInstallationRequest {
                expected: context.clone().into(),
                id: installation.id.clone(),
                expected_revision: installation.revision,
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    companion_admit::admit_response(companion_admit::Response::Revoke(&retired)).unwrap();
    let linked = api
        .get_destination(
            get_destination((*second.destination.id).clone()),
            &token,
            TIMEOUT,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_response(admit::Response::Get(&linked)).unwrap();
    assert_eq!(linked.destination.state, wire::DestinationState::Revoked);
    assert!(!linked.destination.native_delivery);
    assert_eq!(
        api.read_inbox(
            wire::ReadInboxRequest {
                id: second.destination.id.clone(),
                ..Default::default()
            },
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::PermissionDenied)
    );
    control(root, Control::RevokeAccount).await;
    assert_eq!(
        api.get_destination(
            get_destination((*second.destination.id).clone()),
            &token,
            TIMEOUT,
            &cancel
        )
        .await
        .unwrap_err()
        .kind(),
        FailureKind::Rpc(RpcCode::Unauthenticated)
    );
    println!("NHF022_NOTIFICATIONS_TRANSPORT_TLS_PASS");
}
