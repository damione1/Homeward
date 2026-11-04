//! Actual Rust/Go TLS on the explicit synthetic fixture, not native rendering.
use home_client_model::{
    api_content::MediaSourceName,
    cameras::{EventKey, ImageKey},
};
use home_client_transport::{AccessToken, Connection, Failure, FailureKind, MediaTransport};
use home_wire_contract::{
    camera_content as admit, content_contract,
    home::{api::v2 as wire, api::v2 as auth},
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
async fn control(
    root: &Path,
    sequence: &mut u64,
    action: Control,
    entity: Option<&MediaSourceName>,
) {
    *sequence += 1;
    let value = Instruction {
        sequence: *sequence,
        action,
        entity_id: entity.map(|id| id.as_str().into()),
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
fn request_id() -> String {
    static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);
    format!(
        "12345678-1234-4000-8000-{:012x}",
        NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
    )
}
async fn open(
    api: &MediaTransport,
    id: &MediaSourceName,
    token: &AccessToken,
    cancel: &CancellationToken,
) -> admit::Lease {
    let response = api
        .create_media_session(
            content_contract::create_session_request(
                id,
                home_client_model::api_content::ContentKind::Camera,
                None,
                request_id().as_str().try_into().unwrap(),
            )
            .unwrap(),
            token,
            Duration::from_secs(5),
            cancel,
        )
        .await
        .unwrap();
    admit::lease(
        &response,
        id,
        home_client_model::api_content::ContentKind::Camera,
        SystemTime::now(),
        Instant::now(),
    )
    .unwrap()
}
async fn read(
    api: &MediaTransport,
    lease: &admit::Lease,
    token: &AccessToken,
    cancel: &CancellationToken,
) -> Result<admit::Image, Failure> {
    let key = ImageKey::Snapshot(lease.source().clone());
    let response = api
        .read_media_session(
            admit::image_request(lease, &key).unwrap(),
            token,
            Duration::from_secs(5),
            cancel,
        )
        .await?;
    Ok(admit::image(&response, lease, &key, SystemTime::now(), Instant::now()).unwrap())
}
fn assert_reason<T>(result: Result<T, Failure>, expected: wire::ApiErrorReason) {
    let Err(error) = result else {
        panic!("expected canonical refusal")
    };
    assert_eq!(error.api_failure().unwrap().unwrap().reason(), expected);
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
    let api = connection.media();
    let homes = connection
        .home()
        .list_homes(
            wire::ListHomesRequest {
                page_size: 2,
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    assert_eq!(homes.homes.len(), 1);
    assert!(homes.next_page_token.is_empty());
    let home = home_client_model::api::HomeName::try_from(homes.homes[0].name.as_str()).unwrap();
    let list_request = wire::ListMediaSourcesRequest {
        parent: home.as_str().into(),
        ..Default::default()
    };
    let response = api
        .list_media_sources(list_request.clone(), &token, timeout, &cancel)
        .await
        .unwrap();
    let cameras = admit::catalog(&response, &home).unwrap();
    assert_eq!(cameras.len(), 2);
    assert_eq!(cameras[0].title, cameras[1].title);
    assert_ne!(cameras[0].id, cameras[1].id);
    let first = open(&api, &cameras[0].id, &token, &cancel).await;
    let second = open(&api, &cameras[1].id, &token, &cancel).await;
    let left = read(&api, &first, &token, &cancel).await.unwrap();
    let right = read(&api, &second, &token, &cancel).await.unwrap();
    assert_ne!(left.png, right.png);
    assert_ne!(left.key, right.key);
    // The public request cannot forge a source on a session. Swapping replies
    // between sessions is rejected at the admission boundary as well.
    let raw = api
        .read_media_session(
            admit::image_request(&first, &left.key).unwrap(),
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    assert!(admit::image(&raw, &second, &right.key, SystemTime::now(), Instant::now()).is_err());
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
    assert_reason(
        read(&api, &first, &other_token, &cancel).await,
        wire::ApiErrorReason::AccessDenied,
    );
    let mut cursor = String::new();
    let mut ids = std::collections::BTreeSet::new();
    loop {
        let response = api
            .query_media_session_events(
                wire::QueryMediaSessionEventsRequest {
                    name: first.name().as_str().into(),
                    page_size: 1,
                    page_token: cursor,
                    ..Default::default()
                },
                &token,
                timeout,
                &cancel,
            )
            .await
            .unwrap();
        let page = admit::events(&response, &first, SystemTime::now(), Instant::now()).unwrap();
        for event in page.events {
            assert_eq!(event.source, cameras[0].source);
            let key = ImageKey::EventThumbnail(event.key.clone());
            if event.has_image {
                let raw = api
                    .read_media_session(
                        admit::image_request(&first, &key).unwrap(),
                        &token,
                        timeout,
                        &cancel,
                    )
                    .await
                    .unwrap();
                let image =
                    admit::image(&raw, &first, &key, SystemTime::now(), Instant::now()).unwrap();
                assert_eq!(image.key, key);
                assert_eq!(image.png, left.png);
            }
            assert!(ids.insert(event.key));
        }
        match page.next {
            Some(next) => cursor = next.as_str().to_owned(),
            None => {
                assert_eq!(ids.len(), page.total);
                break;
            }
        }
    }
    let shared_event = ids.first().unwrap().event().clone();
    let key = ImageKey::EventThumbnail(EventKey::new(cameras[1].id.clone(), shared_event));
    let response = api
        .read_media_session(
            admit::image_request(&second, &key).unwrap(),
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let thumbnail =
        admit::image(&response, &second, &key, SystemTime::now(), Instant::now()).unwrap();
    assert_eq!(thumbnail.png, right.png);
    assert_ne!(thumbnail.png, left.png);
    let mut sequence = 0;
    let watch_cancel = cancel.child_token();
    let mut watch = api
        .watch_media_session(
            wire::WatchMediaSessionRequest {
                name: first.name().as_str().into(),
                ..Default::default()
            },
            &token,
            Duration::from_secs(60),
            &watch_cancel,
        )
        .await
        .unwrap();
    let ready = watch.next().await.unwrap().unwrap();
    let initial_etag = first.events_etag().unwrap().clone();
    assert_eq!(
        admit::watch(&ready, &first, 0, None),
        Ok(admit::WatchFrame::Ready(initial_etag.clone()))
    );
    control(
        &root,
        &mut sequence,
        Control::ChangeEvents,
        Some(&cameras[0].id),
    )
    .await;
    let changed = watch.next().await.unwrap().unwrap();
    let admit::WatchFrame::Changed(changed_etag) =
        admit::watch(&changed, &first, 1, Some(&initial_etag)).unwrap()
    else {
        panic!("changed frame")
    };
    assert_ne!(changed_etag, initial_etag);
    control(
        &root,
        &mut sequence,
        Control::MissingImage,
        Some(&cameras[0].id),
    )
    .await;
    assert_reason(
        read(&api, &first, &token, &cancel).await,
        wire::ApiErrorReason::ResourceNotFound,
    );
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
        let key = ImageKey::Snapshot(pending_reference.source().clone());
        pending_api
            .read_media_session(
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
    assert!(read(&api, &first, &token, &cancel).await.is_ok());
    control(&root, &mut sequence, Control::ExpireLeases, None).await;
    assert!(watch.next().await.is_err());
    assert_reason(
        read(&api, &first, &token, &cancel).await,
        wire::ApiErrorReason::ResourceNotFound,
    );
    drop(watch);
    let fresh = open(&api, &cameras[0].id, &token, &cancel).await;
    let revoked_watch_cancel = cancel.child_token();
    let mut revoked_watch = api
        .watch_media_session(
            wire::WatchMediaSessionRequest {
                name: fresh.name().as_str().into(),
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
    assert!(read(&api, &fresh, &token, &cancel).await.is_ok());
    control(&root, &mut sequence, Control::Outage, None).await;
    assert!(
        api.list_media_sources(list_request, &token, timeout, &cancel)
            .await
            .is_err()
    );
    control(&root, &mut sequence, Control::RestoreNetwork, None).await;
    let fresh = open(&api, &cameras[1].id, &token, &cancel).await;
    assert!(read(&api, &fresh, &token, &cancel).await.is_ok());
    api.delete_media_session(
        wire::DeleteMediaSessionRequest {
            name: fresh.name().as_str().into(),
            request_id: request_id(),
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
