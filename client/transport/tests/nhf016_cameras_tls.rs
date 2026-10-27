//! Actual Rust/Go TLS on the explicit synthetic fixture, not native rendering.
use home_client_model::cameras::{CameraId, EventKey, ImageKey};
use home_client_transport::{AccessToken, CameraTransport, Connection, FailureKind};
use home_wire_contract::{
    cameras_contract as admit,
    home::{auth::v1 as auth, cameras::v1 as wire},
};
use serde::{Deserialize, Serialize};
use std::{
    path::{Path, PathBuf},
    time::{Duration, Instant, SystemTime},
};
use tokio_util::sync::CancellationToken;

#[derive(Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Mode {
    SyntheticOnly,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Credential {
    login_name: String,
    password: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    reader: Credential,
    production_cameras: bool,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Control {
    PauseImages,
    ResumeImages,
    ExpireLeases,
    RevokeRead,
    AllowRead,
    Outage,
    RestoreNetwork,
    Snapshot,
    MissingImage,
    RestoreImages,
    ChangeEvents,
}
#[derive(Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Instruction {
    sequence: u64,
    action: Control,
    #[serde(skip_serializing_if = "Option::is_none")]
    entity_id: Option<String>,
}
async fn control(root: &Path, sequence: &mut u64, action: Control, entity: Option<&CameraId>) {
    *sequence += 1;
    let value = Instruction {
        sequence: *sequence,
        action,
        entity_id: entity.map(|id| id.entity().as_str().into()),
    };
    let temp = root.join("control.writing");
    std::fs::write(&temp, serde_json::to_vec(&value).unwrap()).unwrap();
    std::fs::rename(temp, root.join("control.json")).unwrap();
    let ack = root.join(format!("ack-{:02}.json", *sequence));
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if let Ok(raw) = std::fs::read(&ack) {
                assert_eq!(serde_json::from_slice::<Instruction>(&raw).unwrap(), value);
                break;
            };
            tokio::time::sleep(Duration::from_millis(10)).await
        }
    })
    .await
    .expect("camera control ack");
}
async fn open(
    api: &CameraTransport,
    id: &CameraId,
    token: &AccessToken,
    cancel: &CancellationToken,
) -> admit::Lease {
    let response = api
        .open_session(
            wire::OpenSessionRequest {
                entity_id: id.entity().as_str().into(),
                ..Default::default()
            },
            token,
            Duration::from_secs(5),
            cancel,
        )
        .await
        .unwrap();
    let admit::Outcome::Value(lease) =
        admit::lease(&response, id, SystemTime::now(), Instant::now()).unwrap()
    else {
        panic!("synthetic camera unavailable")
    };
    lease
}
async fn read(
    api: &CameraTransport,
    lease: &admit::Lease,
    token: &AccessToken,
    cancel: &CancellationToken,
) -> admit::Outcome<admit::Image> {
    let key = ImageKey::Snapshot(lease.reference().camera().clone());
    let response = api
        .read_image(
            admit::image_request(lease, &key).unwrap(),
            token,
            Duration::from_secs(5),
            cancel,
        )
        .await
        .unwrap();
    admit::image(&response, lease, &key, SystemTime::now(), Instant::now()).unwrap()
}
async fn stats(root: &Path, sequence: &mut u64) -> serde_json::Value {
    control(root, sequence, Control::Snapshot, None).await;
    serde_json::from_slice(
        &std::fs::read(root.join(format!("stats-{:02}.json", *sequence))).unwrap(),
    )
    .unwrap()
}
#[tokio::test]
async fn actual_camera_tls_private_sessions_images_events_and_cancellation() {
    let Ok(directory) = std::env::var("HOMEWARD_NHF016_TLS_FIXTURE") else {
        eprintln!("NHF016 TLS fixture not enabled; no integration claim");
        return;
    };
    let root = PathBuf::from(directory);
    assert!(root.is_absolute());
    let fixture: Fixture =
        serde_json::from_slice(&std::fs::read(root.join("ready.json")).unwrap()).unwrap();
    assert!(matches!(fixture.mode, Mode::SyntheticOnly));
    assert!(!fixture.production_cameras);
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
    let timeout = Duration::from_secs(5);
    let credentials = fixture.reader;
    let login = connection
        .auth()
        .start_login(
            auth::StartLoginRequest {
                login_name: credentials.login_name.clone(),
                password: credentials.password.clone(),
                ..Default::default()
            },
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let Some(auth::login_decision::Outcome::Authenticated(tokens)) =
        login.decision.outcome.as_ref()
    else {
        panic!("native login")
    };
    let token = AccessToken::parse(&tokens.access_token).unwrap();
    let api = connection.cameras();
    let response = api
        .list_cameras(
            wire::ListCamerasRequest::default(),
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let cameras = admit::catalog(&response).unwrap();
    assert_eq!(cameras.len(), 2);
    assert_eq!(cameras[0].title, cameras[1].title);
    assert_ne!(cameras[0].id, cameras[1].id);
    let first = open(&api, &cameras[0].id, &token, &cancel).await;
    let second = open(&api, &cameras[1].id, &token, &cancel).await;
    let admit::Outcome::Value(left) = read(&api, &first, &token, &cancel).await else {
        panic!("first image")
    };
    let admit::Outcome::Value(right) = read(&api, &second, &token, &cancel).await else {
        panic!("second image")
    };
    assert_ne!(left.png, right.png);
    assert_ne!(left.key, right.key);
    let mut wrong = first.wire_reference();
    wrong.entity_id = cameras[1].id.entity().as_str().into();
    let response = api
        .read_image(
            wire::ReadImageRequest {
                key: wire::ImageKey {
                    session: wrong.into(),
                    target: Some(wire::image_key::Target::Snapshot(Box::default())),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    assert!(matches!(
        response.result,
        Some(wire::read_image_response::Result::WrongCamera(_))
    ));
    let another = connection
        .auth()
        .start_login(
            auth::StartLoginRequest {
                login_name: credentials.login_name,
                password: credentials.password,
                ..Default::default()
            },
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let Some(auth::login_decision::Outcome::Authenticated(tokens)) =
        another.decision.outcome.as_ref()
    else {
        panic!("second native login")
    };
    let other_token = AccessToken::parse(&tokens.access_token).unwrap();
    assert!(matches!(
        read(&api, &first, &other_token, &cancel).await,
        admit::Outcome::Refused(admit::ReadIssue::Revoked)
    ));
    let mut cursor = Vec::new();
    let mut ids = std::collections::BTreeSet::new();
    loop {
        let response = api
            .list_events(
                wire::ListEventsRequest {
                    session: first.wire_reference().into(),
                    page_size: 1,
                    cursor,
                    ..Default::default()
                },
                &token,
                timeout,
                &cancel,
            )
            .await
            .unwrap();
        let admit::Outcome::Value(page) =
            admit::events(&response, &first, SystemTime::now(), Instant::now()).unwrap()
        else {
            panic!("event page")
        };
        for event in page.events {
            assert_eq!(event.source, cameras[0].source);
            let key = ImageKey::EventThumbnail(event.key.clone());
            let response = api
                .read_image(
                    admit::image_request(&first, &key).unwrap(),
                    &token,
                    timeout,
                    &cancel,
                )
                .await
                .unwrap();
            let outcome =
                admit::image(&response, &first, &key, SystemTime::now(), Instant::now()).unwrap();
            if event.has_image {
                let admit::Outcome::Value(image) = outcome else {
                    panic!("event thumbnail absent")
                };
                assert_eq!(image.key, key);
                assert_eq!(image.png, left.png);
            } else {
                assert!(matches!(
                    outcome,
                    admit::Outcome::Refused(admit::ReadIssue::Missing)
                ));
            }
            assert!(ids.insert(event.key));
        }
        match page.next {
            Some(next) => cursor = next.bytes().to_vec(),
            None => {
                assert_eq!(ids.len(), page.total);
                break;
            }
        }
    }
    let shared_event = ids.first().unwrap().event().clone();
    let key = ImageKey::EventThumbnail(EventKey::new(cameras[1].id.clone(), shared_event));
    let response = api
        .read_image(
            admit::image_request(&second, &key).unwrap(),
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let admit::Outcome::Value(thumbnail) =
        admit::image(&response, &second, &key, SystemTime::now(), Instant::now()).unwrap()
    else {
        panic!("second camera thumbnail")
    };
    assert_eq!(thumbnail.png, right.png);
    assert_ne!(thumbnail.png, left.png);
    let mut sequence = 0;
    let watch_cancel = cancel.child_token();
    let mut watch = api
        .watch_session(
            wire::WatchSessionRequest {
                session: first.wire_reference().into(),
                ..Default::default()
            },
            &token,
            Duration::from_secs(60),
            &watch_cancel,
        )
        .await
        .unwrap();
    let ready = watch.next().await.unwrap().unwrap();
    assert_eq!(
        admit::watch(&ready, &first, 0, 0),
        Ok(admit::WatchFrame::Ready(first.revision()))
    );
    control(
        &root,
        &mut sequence,
        Control::ChangeEvents,
        Some(&cameras[0].id),
    )
    .await;
    let changed = watch.next().await.unwrap().unwrap();
    assert_eq!(
        admit::watch(&changed, &first, 1, first.revision()),
        Ok(admit::WatchFrame::Changed(first.revision() + 1))
    );
    control(
        &root,
        &mut sequence,
        Control::MissingImage,
        Some(&cameras[0].id),
    )
    .await;
    assert!(matches!(
        read(&api, &first, &token, &cancel).await,
        admit::Outcome::Refused(admit::ReadIssue::Missing)
    ));
    control(
        &root,
        &mut sequence,
        Control::RestoreImages,
        Some(&cameras[0].id),
    )
    .await;
    control(&root, &mut sequence, Control::PauseImages, None).await;
    let pending_cancel = cancel.child_token();
    let pending_api = api.clone();
    let pending_token = token.clone();
    let pending_reference = first.clone();
    let pending_local = pending_cancel.clone();
    let pending = tokio::spawn(async move {
        let key = ImageKey::Snapshot(pending_reference.reference().camera().clone());
        pending_api
            .read_image(
                admit::image_request(&pending_reference, &key).unwrap(),
                &pending_token,
                Duration::from_secs(10),
                &pending_local,
            )
            .await
    });
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if stats(&root, &mut sequence).await["active_reads"] == 1 {
                break;
            };
            tokio::time::sleep(Duration::from_millis(20)).await
        }
    })
    .await
    .unwrap();
    pending_cancel.cancel();
    assert_eq!(
        pending.await.unwrap().err().unwrap().kind(),
        FailureKind::Cancelled
    );
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if stats(&root, &mut sequence).await["active_reads"] == 0 {
                break;
            };
            tokio::time::sleep(Duration::from_millis(20)).await
        }
    })
    .await
    .unwrap();
    control(&root, &mut sequence, Control::ResumeImages, None).await;
    assert!(matches!(
        read(&api, &first, &token, &cancel).await,
        admit::Outcome::Value(_)
    ));
    control(&root, &mut sequence, Control::ExpireLeases, None).await;
    let expired = watch.next().await.unwrap().unwrap();
    assert_eq!(
        admit::watch(&expired, &first, 2, first.revision() + 1),
        Ok(admit::WatchFrame::Expired)
    );
    assert!(matches!(
        read(&api, &first, &token, &cancel).await,
        admit::Outcome::Refused(admit::ReadIssue::Expired)
    ));
    drop(watch);
    let fresh = open(&api, &cameras[0].id, &token, &cancel).await;
    let revoked_watch_cancel = cancel.child_token();
    let mut revoked_watch = api
        .watch_session(
            wire::WatchSessionRequest {
                session: fresh.wire_reference().into(),
                ..Default::default()
            },
            &token,
            Duration::from_secs(60),
            &revoked_watch_cancel,
        )
        .await
        .unwrap();
    assert!(revoked_watch.next().await.unwrap().is_some());
    control(&root, &mut sequence, Control::RevokeRead, None).await;
    assert!(revoked_watch.next().await.is_err());
    drop(revoked_watch);
    control(&root, &mut sequence, Control::AllowRead, None).await;
    let fresh = open(&api, &cameras[0].id, &token, &cancel).await;
    assert!(matches!(
        read(&api, &fresh, &token, &cancel).await,
        admit::Outcome::Value(_)
    ));
    control(&root, &mut sequence, Control::Outage, None).await;
    assert!(
        api.list_cameras(
            wire::ListCamerasRequest::default(),
            &token,
            timeout,
            &cancel
        )
        .await
        .is_err()
    );
    control(&root, &mut sequence, Control::RestoreNetwork, None).await;
    let fresh = open(&api, &cameras[1].id, &token, &cancel).await;
    assert!(matches!(
        read(&api, &fresh, &token, &cancel).await,
        admit::Outcome::Value(_)
    ));
    api.close_session(
        wire::CloseSessionRequest {
            session: fresh.wire_reference().into(),
            ..Default::default()
        },
        &token,
        timeout,
        &cancel,
    )
    .await
    .unwrap();
    let final_stats = stats(&root, &mut sequence).await;
    assert_eq!(final_stats["active_reads"], 0);
    assert_eq!(final_stats["active_streams"], 0);
    println!("NHF016_CAMERA_TLS_PASS");
}
