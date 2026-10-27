//! Actual private owner over the isolated Go/TLS/native-account facade.
use super::*;
use home_client_session::{SecureStore, StoreFailure};
use home_client_transport::Connection;
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};
use zeroize::Zeroizing;

#[derive(Default)]
struct MemoryStore(Mutex<Option<Zeroizing<Vec<u8>>>>);
impl SecureStore for MemoryStore {
    fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(self.0.lock().unwrap().clone())
    }
    fn save(&self, value: &[u8]) -> Result<(), StoreFailure> {
        *self.0.lock().unwrap() = Some(Zeroizing::new(value.to_vec()));
        Ok(())
    }
    fn clear(&self) -> Result<(), StoreFailure> {
        *self.0.lock().unwrap() = None;
        Ok(())
    }
}
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
    reader: Credentials,
    production_cameras: bool,
}
#[derive(Clone, Copy, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "kebab-case")]
enum Control {
    Snapshot,
    PauseImages,
    ResumeImages,
    ExpireLeases,
    RevokeRead,
    AllowRead,
    Outage,
    RestoreNetwork,
    MissingImage,
    RestoreImages,
    ChangeEvents,
}
#[derive(Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
struct Instruction {
    sequence: u64,
    action: Control,
    #[serde(skip_serializing_if = "Option::is_none")]
    entity_id: Option<String>,
}
async fn control(root: &Path, sequence: &mut u64, action: Control, entity: Option<String>) {
    *sequence += 1;
    let command = Instruction {
        sequence: *sequence,
        action,
        entity_id: entity,
    };
    let tmp = root.join("control.writing");
    std::fs::write(&tmp, serde_json::to_vec(&command).unwrap()).unwrap();
    std::fs::rename(tmp, root.join("control.json")).unwrap();
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if let Ok(raw) = std::fs::read(root.join(format!("ack-{:02}.json", *sequence))) {
                assert_eq!(
                    serde_json::from_slice::<Instruction>(&raw).unwrap(),
                    command
                );
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await
        }
    })
    .await
    .expect("camera control ack");
}
async fn stats(root: &Path, sequence: &mut u64) -> serde_json::Value {
    control(root, sequence, Control::Snapshot, None).await;
    serde_json::from_slice(
        &std::fs::read(root.join(format!("stats-{:02}.json", *sequence))).unwrap(),
    )
    .unwrap()
}
async fn until(owner: &Cameras, label: &str, predicate: impl Fn(&State) -> bool) {
    tokio::time::timeout(Duration::from_secs(12),async {loop {if predicate(&owner.0.state.lock().unwrap()){return}tokio::time::sleep(Duration::from_millis(10)).await}}).await.unwrap_or_else(|_|{let s=owner.0.state.lock().unwrap();panic!("camera owner {label}: status={:?} catalog={:?} context={} rows={} active={} requested={}",s.status,s.catalog_status,s.context,s.events.rows.len(),s.active,s.read_requested)});
}
fn generation(owner: &Cameras) -> i32 {
    owner.0.state.lock().unwrap().generation
}
fn ready(state: &State) -> bool {
    state.status == Status::Ready && state.picture.is_some() && state.events.rows.len() == 3
}
async fn qualify(owner: &Cameras, root: &Path, session: &SessionCoordinator) {
    let mut sequence = 0;
    owner.interactive(true);
    until(owner, "initial private image", ready).await;
    owner.navigate_catalog(NavigationStep::Last);
    until(owner, "catalog keyboard last exact identity", |s| {
        ready(s) && s.selected.as_ref().unwrap().entity().as_str() == "driveway"
    })
    .await;
    owner.navigate_catalog(NavigationStep::First);
    until(owner, "catalog keyboard first exact identity", |s| {
        ready(s) && s.selected.as_ref().unwrap().entity().as_str() == "_driveway"
    })
    .await;
    let previous_context = owner.0.state.lock().unwrap().context;
    owner.reload_preview(-1, generation(owner));
    assert_eq!(owner.0.state.lock().unwrap().context, previous_context);
    owner.reload_catalog(generation(owner));
    until(owner, "explicit whole catalog reload", ready).await;
    let (first, second, first_pixels, weak) = {
        let s = owner.0.state.lock().unwrap();
        let first = s.selected.clone().unwrap();
        let second = s.catalog.iter().find(|c| c.id != first).unwrap().id.clone();
        let frame = s.picture.as_ref().unwrap();
        (
            first,
            second,
            frame.pixels.rgba.to_vec(),
            Arc::downgrade(frame),
        )
    };
    owner.viewport(false);
    until(owner, "viewport privacy", |s| s.picture.is_none()).await;
    assert!(weak.upgrade().is_none());
    let before = stats(root, &mut sequence).await["image_requests"]
        .as_u64()
        .unwrap();
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(
        stats(root, &mut sequence).await["image_requests"]
            .as_u64()
            .unwrap(),
        before
    );
    owner.viewport(true);
    until(owner, "visible again", ready).await;
    let token = {
        let s = owner.0.state.lock().unwrap();
        s.events.token(0).unwrap()
    };
    owner.event(token, generation(owner));
    until(owner, "event thumbnail", |s| {
        ready(s) && matches!(s.target, Some(ImageKey::EventThumbnail(_)))
    })
    .await;
    control(root, &mut sequence, Control::PauseImages, None).await;
    let camera_token = {
        let s = owner.0.state.lock().unwrap();
        i32::try_from(s.catalog.iter().position(|c| c.id == first).unwrap() + 1).unwrap()
    };
    owner.snapshot(camera_token, generation(owner));
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if stats(root, &mut sequence).await["active_reads"] == 1 {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await
        }
    })
    .await
    .unwrap();
    owner.viewport(false);
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if stats(root, &mut sequence).await["active_reads"] == 0 {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await
        }
    })
    .await
    .unwrap();
    assert!(owner.0.state.lock().unwrap().picture.is_none());
    control(root, &mut sequence, Control::ResumeImages, None).await;
    owner.viewport(true);
    until(owner, "cancelled read joined and replaced", ready).await;
    let second_token = {
        let s = owner.0.state.lock().unwrap();
        i32::try_from(s.catalog.iter().position(|c| c.id == second).unwrap() + 1).unwrap()
    };
    owner.choose(second_token, generation(owner));
    until(owner, "second exact camera", |s| {
        ready(s) && s.selected.as_ref() == Some(&second)
    })
    .await;
    assert_ne!(
        owner
            .0
            .state
            .lock()
            .unwrap()
            .picture
            .as_ref()
            .unwrap()
            .pixels
            .rgba
            .as_ref(),
        first_pixels
    );
    control(
        root,
        &mut sequence,
        Control::MissingImage,
        Some(second.entity().as_str().into()),
    )
    .await;
    owner.reload(generation(owner));
    until(owner, "missing image", |s| {
        s.status == Status::Missing && s.picture.is_none()
    })
    .await;
    control(
        root,
        &mut sequence,
        Control::RestoreImages,
        Some(second.entity().as_str().into()),
    )
    .await;
    owner.reload(generation(owner));
    until(owner, "restored image", ready).await;
    control(root, &mut sequence, Control::ExpireLeases, None).await;
    until(owner, "lease expired", |s| {
        s.status == Status::Expired && s.picture.is_none() && s.events.rows.is_empty()
    })
    .await;
    owner.reload(generation(owner));
    until(owner, "new lease after explicit reload", ready).await;
    control(root, &mut sequence, Control::RevokeRead, None).await;
    until(owner, "object authority purged", |s| {
        s.picture.is_none() && s.events.rows.is_empty() && s.catalog.is_empty()
    })
    .await;
    control(root, &mut sequence, Control::AllowRead, None).await;
    owner.reload(generation(owner));
    until(owner, "rights restored explicitly", ready).await;
    control(root, &mut sequence, Control::Outage, None).await;
    until(owner, "offline pixels retired", |s| {
        s.status == Status::Offline && s.picture.is_none()
    })
    .await;
    control(root, &mut sequence, Control::RestoreNetwork, None).await;
    owner.reload(generation(owner));
    until(owner, "network returned explicitly", ready).await;
    owner.foreground(false);
    until(owner, "native suspension", |s| {
        s.picture.is_none() && s.events.rows.is_empty() && s.catalog.is_empty()
    })
    .await;
    owner.foreground(true);
    until(owner, "native resume", ready).await;
    session.logout().await.unwrap();
    until(owner, "session logout", |s| {
        s.picture.is_none() && s.events.rows.is_empty() && s.catalog.is_empty() && !s.active
    })
    .await;
    owner.interactive(false);
    let stats = stats(root, &mut sequence).await;
    assert_eq!(stats["active_reads"], 0);
}
#[tokio::test]
#[ignore = "explicit synthetic camera TLS fixture"]
async fn actual_camera_owner_private_pixels_viewport_and_revocation() {
    let root = PathBuf::from(std::env::var("HOMEWARD_NHF016_TLS_FIXTURE").unwrap());
    let fixture: Fixture =
        serde_json::from_slice(&std::fs::read(root.join("ready.json")).unwrap()).unwrap();
    assert!(matches!(fixture.mode, Mode::SyntheticOnly));
    assert!(!fixture.production_cameras);
    let uri = url::Url::parse(&fixture.origin).unwrap();
    assert_eq!(uri.scheme(), "https");
    assert!(
        uri.host_str()
            .unwrap()
            .parse::<std::net::IpAddr>()
            .unwrap()
            .is_loopback()
    );
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes()).unwrap();
    let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    session
        .login(
            fixture.reader.login_name,
            Zeroizing::new(fixture.reader.password),
        )
        .await
        .unwrap();
    let owner = Cameras::detached();
    let cancel = CancellationToken::new();
    let (run, run_session, api, token) = (
        owner.clone(),
        session.clone(),
        connection.cameras(),
        cancel.clone(),
    );
    let task = tokio::spawn(async move {
        run.run(run_session, api, token).await;
    });
    qualify(&owner, &root, &session).await;
    cancel.cancel();
    tokio::time::timeout(Duration::from_secs(5), task)
        .await
        .unwrap()
        .unwrap();
    println!("NHF016_CAMERA_OWNER_TLS_PASS");
}
