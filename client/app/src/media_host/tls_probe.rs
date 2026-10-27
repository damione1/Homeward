//! Actual owner and shared action controller on an opt-in native Go TLS mock.
//! This is network/lifecycle evidence; native rendering is qualified separately.
use super::*;
use home_client_actions::{ActionController, CommandId, SubmissionState};
use home_client_session::{SecureStore, StoreFailure};
use home_client_transport::Connection;
use home_wire_contract::home::{actions::v1 as actions, state::v1 as observation};
use serde::{Deserialize, Serialize};
use std::io::{BufRead as _, Read as _, Write as _};
use zeroize::Zeroizing;

type Result<T> = std::result::Result<T, Box<dyn std::error::Error + Send + Sync>>;
fn admit<T, E: std::fmt::Debug>(value: std::result::Result<T, E>) -> Result<T> {
    value.map_err(|error| format!("media qualification: {error:?}").into())
}
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
struct Entities {
    minimal: String,
    group: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    entities: Entities,
    reader: Credential,
    production_media: bool,
}
#[derive(Default)]
struct MemoryStore(Mutex<Option<Zeroizing<Vec<u8>>>>);
impl SecureStore for MemoryStore {
    fn load(&self) -> std::result::Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(self.0.lock().unwrap().clone())
    }
    fn save(&self, value: &[u8]) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = Some(Zeroizing::new(value.to_vec()));
        Ok(())
    }
    fn clear(&self) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = None;
        Ok(())
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Control {
    Confirm,
    DenyArtwork,
    ExpireArtwork,
    AbsentArtwork,
    RestoreArtwork,
    SwitchCoordinator,
    LoseNextReply,
    RevokeWrite,
    AllowWrite,
    WithdrawCapabilities,
    RestoreCapabilities,
    RemoveCoordinator,
    RestoreCoordinator,
    Outage,
    RestoreNetwork,
    RevokeRead,
    AllowRead,
}
#[derive(Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
struct Instruction {
    sequence: u64,
    action: Control,
    #[serde(skip_serializing_if = "Option::is_none")]
    command_id: Option<String>,
}
async fn control(sequence: &mut u64, action: Control, command: Option<&CommandId>) -> Result<()> {
    *sequence += 1;
    let instruction = Instruction {
        sequence: *sequence,
        action,
        command_id: command.map(|id| id.as_str().into()),
    };
    println!("NHF014_CONTROL {}", serde_json::to_string(&instruction)?);
    std::io::stdout().flush()?;
    let line = tokio::task::spawn_blocking(|| {
        let mut line = String::new();
        std::io::stdin().lock().take(4097).read_line(&mut line)?;
        if line.len() > 4096 || !line.ends_with('\n') {
            return Err(std::io::Error::other(
                "bounded media acknowledgement required",
            ));
        }
        Ok(line)
    })
    .await??;
    let acknowledgement: Instruction = serde_json::from_str(&line)?;
    if acknowledgement != instruction {
        return Err("unbound media control acknowledgement".into());
    }
    Ok(())
}
async fn until(owner: &Media, label: &str, predicate: impl Fn(&State) -> bool) -> Result<()> {
    let result = tokio::time::timeout(Duration::from_secs(15), async {
        loop {
            if predicate(&owner.0.state.lock().unwrap()) {
                return;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await;
    if result.is_err() {
        let state = owner.0.state.lock().unwrap();
        return Err(format!(
            "media owner did not reach {label}: status={:?}, active={}, generation={}, data={}",
            state.status,
            state.active,
            state.generation,
            state.data.is_some()
        )
        .into());
    }
    Ok(())
}
fn generation(owner: &Media) -> i32 {
    owner.0.state.lock().unwrap().generation
}
async fn acknowledged(owner: &Media) -> Result<CommandId> {
    until(owner, "acknowledged action", |state| {
        state.progress.as_ref().is_some_and(|p| {
            p.progress
                .as_ref()
                .is_some_and(|p| p.status == actions::ActionStatus::Acknowledged)
        })
    })
    .await?;
    let state = owner.0.state.lock().unwrap();
    assert!(state.busy());
    Ok(state
        .action
        .as_ref()
        .ok_or("action handle missing")?
        .id()
        .clone())
}
async fn confirmed(owner: &Media, sequence: &mut u64, id: &CommandId) -> Result<()> {
    control(sequence, Control::Confirm, Some(id)).await?;
    until(owner, "server confirmation", |state| {
        state.progress.as_ref().is_some_and(|p| {
            p.id == *id
                && p.progress
                    .as_ref()
                    .is_some_and(|p| p.status == actions::ActionStatus::Confirmed)
        })
    })
    .await?;
    until(owner, "released action slot", |state| {
        state
            .action
            .as_ref()
            .is_some_and(|a| a.id() == id && a.is_finished())
    })
    .await
}
fn gesture(owner: &Media, generation: i32, position: f32) {
    owner.edit(
        1,
        generation,
        Field::Volume.token(),
        ProValueIntent {
            event: ProValueEvent::Start,
            thumb: RangeThumb::Lower,
            ..Default::default()
        },
    );
    owner.edit(
        1,
        generation,
        Field::Volume.token(),
        ProValueIntent {
            event: ProValueEvent::Position,
            thumb: RangeThumb::Lower,
            position,
            ..Default::default()
        },
    );
}
fn commit(owner: &Media, generation: i32) {
    owner.edit(
        1,
        generation,
        Field::Volume.token(),
        ProValueIntent {
            event: ProValueEvent::Commit,
            thumb: RangeThumb::Lower,
            ..Default::default()
        },
    );
}
async fn choose_option(owner: &Media, kind: ChoiceKind) -> Result<CommandId> {
    let (generation, token) = {
        let state = owner.0.state.lock().unwrap();
        let data = state.data.as_ref().ok_or("media data missing")?;
        let selected = match kind {
            ChoiceKind::Source => data.snapshot().source_id.as_deref(),
            ChoiceKind::Group => data.snapshot().group_id.as_deref(),
        };
        (
            state.generation,
            state
                .choices
                .iter()
                .find(|(_, choice)| choice.kind == kind && Some(choice.id.0.as_str()) != selected)
                .ok_or("unselected media option missing")?
                .0
                .0,
        )
    };
    owner.option(
        1,
        generation,
        match kind {
            ChoiceKind::Source => ProMediaOptionKind::Source,
            ChoiceKind::Group => ProMediaOptionKind::Group,
        },
        token,
    );
    acknowledged(owner).await
}
async fn qualify(
    owner: &Media,
    connection: &Connection,
    session: &SessionCoordinator,
    fixture: &Fixture,
) -> Result<()> {
    let mut sequence = 0;
    let view = ProjectionMailbox::<()>::new();
    let minimal: EntityId = admit(fixture.entities.minimal.parse())?;
    let group: EntityId = admit(fixture.entities.group.parse())?;
    owner.interactive(true);
    owner.choose(Some(minimal), view.generation());
    until(owner, "minimal player", |state| {
        state.status == Status::Ready
    })
    .await?;
    {
        let state = owner.0.state.lock().unwrap();
        let data = state.data.as_ref().unwrap();
        assert!(
            data.snapshot()
                .capabilities
                .volume_basis_points
                .as_option()
                .is_none()
        );
        assert!(data.snapshot().capabilities.sources.is_empty());
        assert_eq!(data.snapshot().capabilities.operations.len(), 2);
        assert!(matches!(state.picture, Picture::Absent));
    }
    owner.operation(1, generation(owner), ProMediaOperation::Play);
    assert_eq!(
        owner
            .0
            .state
            .lock()
            .unwrap()
            .data
            .as_ref()
            .unwrap()
            .snapshot()
            .playback,
        wire::PlaybackState::Paused
    );
    let id = acknowledged(owner).await?;
    confirmed(owner, &mut sequence, &id).await?;
    until(owner, "observed playback", |state| {
        state
            .data
            .as_ref()
            .is_some_and(|d| d.snapshot().playback == wire::PlaybackState::Playing)
    })
    .await?;
    control(&mut sequence, Control::RestoreArtwork, None).await?;
    owner.choose(Some(group.clone()), admit(view.invalidate())?);
    until(owner, "group player and bounded artwork", |state| {
        state.status == Status::Ready && matches!(state.picture, Picture::Ready(_))
    })
    .await?;
    {
        let state = owner.0.state.lock().unwrap();
        let data = state.data.as_ref().unwrap();
        assert_eq!(data.observed(Field::Volume).unwrap().canonical(), 0);
        assert_eq!(data.snapshot().members.len(), 2);
        assert!(data.snapshot().provenance.starts_with("MOCK"));
        if let Picture::Ready(image) = &state.picture {
            assert_eq!(
                (image.width(), image.height(), image.rgba().len()),
                (64, 64, 64 * 64 * 4)
            );
        }
    }
    let id = choose_option(owner, ChoiceKind::Source).await?;
    confirmed(owner, &mut sequence, &id).await?;
    until(owner, "server source observation", |state| {
        state
            .data
            .as_ref()
            .is_some_and(|d| d.snapshot().source_id.as_deref() == Some("synthetic-radio"))
    })
    .await?;
    let id = choose_option(owner, ChoiceKind::Group).await?;
    confirmed(owner, &mut sequence, &id).await?;
    until(owner, "server resolved group route", |state| {
        state.data.as_ref().is_some_and(|d| {
            d.snapshot().group_id.as_deref() == Some("synthetic-solo")
                && d.snapshot().members.len() == 1
                && d.coordinator() == &group
        })
    })
    .await?;
    control(&mut sequence, Control::DenyArtwork, None).await?;
    until(owner, "denied artwork fallback", |state| {
        matches!(state.picture, Picture::Denied) && state.data.is_some()
    })
    .await?;
    control(&mut sequence, Control::ExpireArtwork, None).await?;
    until(owner, "expired artwork fallback", |state| {
        matches!(state.picture, Picture::Expired)
    })
    .await?;
    control(&mut sequence, Control::AbsentArtwork, None).await?;
    until(owner, "absent artwork fallback", |state| {
        matches!(state.picture, Picture::Absent)
    })
    .await?;
    control(&mut sequence, Control::RestoreArtwork, None).await?;
    until(owner, "restored authorized artwork", |state| {
        matches!(state.picture, Picture::Ready(_))
    })
    .await?;
    let old = generation(owner);
    gesture(owner, old, 0.42);
    let stale = {
        let state = owner.0.state.lock().unwrap();
        let data = state.data.as_ref().unwrap();
        data.quantity_request(data.parse(Field::Volume, "42").unwrap())
    };
    let mut stale = admit(stale)?;
    control(&mut sequence, Control::SwitchCoordinator, None).await?;
    until(owner, "new topology generation", |state| {
        state.status == Status::Ready && state.generation != old
    })
    .await?;
    commit(owner, old);
    assert!(owner.0.state.lock().unwrap().pending.is_none());
    // Deliberate adversarial protocol probe on this synthetic server: the same
    // route must also be refused when it races past local cancellation.
    stale.command_id = "nhf014-stale-route-protocol-probe".into();
    let now = SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)?
        .as_secs();
    stale.expires_at = buffa_types::google::protobuf::Timestamp {
        seconds: (now + 20).try_into()?,
        ..Default::default()
    }
    .into();
    let stop = CancellationToken::new();
    let lease = admit(session.access(&stop).await)?;
    let rejected = connection
        .actions()
        .submit_action(
            actions::SubmitActionRequest {
                command: stale.into(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(2),
            &stop,
        )
        .await?;
    assert_eq!(rejected.result.status, actions::ActionStatus::Failed);
    assert_eq!(
        rejected.result.error.code,
        actions::ErrorCode::StaleConfiguration
    );
    control(&mut sequence, Control::LoseNextReply, None).await?;
    let current = generation(owner);
    gesture(owner, current, 0.42);
    commit(owner, current);
    let id = acknowledged(owner).await?;
    {
        let state = owner.0.state.lock().unwrap();
        let snapshot = state.progress.as_ref().unwrap();
        assert_eq!(snapshot.id, id);
        assert!(snapshot.durable.is_some());
        assert!(snapshot.submission == SubmissionState::Uncertain || snapshot.issue.is_some());
        assert_ne!(
            snapshot.progress.as_ref().unwrap().status,
            actions::ActionStatus::Confirmed
        );
    }
    until(owner, "observed volume after lost response", |state| {
        state
            .data
            .as_ref()
            .is_some_and(|d| d.snapshot().volume_basis_points == Some(4200))
    })
    .await?;
    confirmed(owner, &mut sequence, &id).await?;
    let old = generation(owner);
    gesture(owner, old, 0.8);
    control(&mut sequence, Control::RevokeWrite, None).await?;
    until(owner, "write permission withdrawn", |state| {
        state
            .data
            .as_ref()
            .is_some_and(|d| d.snapshot().action_access == observation::ActionAccess::Denied)
    })
    .await?;
    commit(owner, old);
    assert!(owner.0.state.lock().unwrap().pending.is_none());
    control(&mut sequence, Control::AllowWrite, None).await?;
    until(owner, "write permission restored", State::editable).await?;
    let old = generation(owner);
    gesture(owner, old, 0.9);
    control(&mut sequence, Control::WithdrawCapabilities, None).await?;
    until(owner, "capability withdrawal", |state| {
        state.generation != old
            && state
                .data
                .as_ref()
                .is_some_and(|d| d.limits(Field::Volume).is_none())
    })
    .await?;
    commit(owner, old);
    assert!(owner.0.state.lock().unwrap().pending.is_none());
    control(&mut sequence, Control::RestoreCapabilities, None).await?;
    until(owner, "capabilities restored", |state| {
        state.editable()
            && state
                .data
                .as_ref()
                .is_some_and(|d| d.limits(Field::Volume).is_some())
    })
    .await?;
    control(&mut sequence, Control::RemoveCoordinator, None).await?;
    until(owner, "missing coordinator", |state| {
        state.status == Status::Offline && !state.editable()
    })
    .await?;
    owner.operation(1, generation(owner), ProMediaOperation::Play);
    assert!(owner.0.state.lock().unwrap().pending.is_none());
    control(&mut sequence, Control::RestoreCoordinator, None).await?;
    until(
        owner,
        "coordinator restored without replay",
        State::editable,
    )
    .await?;
    control(&mut sequence, Control::Outage, None).await?;
    until(owner, "network outage", |state| {
        state.status == Status::Offline
    })
    .await?;
    owner.operation(1, generation(owner), ProMediaOperation::Play);
    assert!(owner.0.state.lock().unwrap().pending.is_none());
    control(&mut sequence, Control::RestoreNetwork, None).await?;
    until(owner, "network restored without replay", State::editable).await?;
    control(&mut sequence, Control::RevokeRead, None).await?;
    until(owner, "private media purge", |state| {
        state.data.is_none()
            && state.source.is_none()
            && state.choices.is_empty()
            && matches!(state.picture, Picture::Absent)
    })
    .await?;
    control(&mut sequence, Control::AllowRead, None).await?;
    until(owner, "current permission reread", |state| {
        state.status == Status::Ready && state.data.is_some()
    })
    .await?;
    owner.choose(None, admit(view.invalidate())?);
    assert!(owner.0.state.lock().unwrap().data.is_none());
    Ok(())
}
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "explicit isolated Go media fixture required"]
async fn real_tls_media_owner_and_single_attempt_actions() {
    let path = std::env::var("HOMEWARD_NHF014_READY").expect("explicit media fixture required");
    let mut fixture: Fixture = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
    let Mode::SyntheticOnly = fixture.mode;
    let origin = url::Url::parse(&fixture.origin).unwrap();
    assert!(
        !fixture.production_media
            && origin.scheme() == "https"
            && origin.host_str() == Some("127.0.0.1")
            && origin.port().is_some()
            && origin.username().is_empty()
            && origin.password().is_none()
            && origin.path() == "/"
            && origin.query().is_none()
            && origin.fragment().is_none()
    );
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes()).unwrap();
    let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    admit(
        session
            .login(
                std::mem::take(&mut fixture.reader.login_name),
                Zeroizing::new(std::mem::take(&mut fixture.reader.password)),
            )
            .await,
    )
    .unwrap();
    let (actions, _notices) = ActionController::start(session.clone(), connection.actions());
    let owner = Media::detached();
    let cancel = CancellationToken::new();
    let (task_owner, credential, transport, client, stop) = (
        owner.clone(),
        session.clone(),
        connection.media(),
        actions.client(),
        cancel.clone(),
    );
    let task = tokio::spawn(async move {
        task_owner.run(credential, transport, client, stop).await;
    });
    let result = qualify(&owner, &connection, &session, &fixture).await;
    cancel.cancel();
    let joined = task.await;
    actions.close().await;
    session.close().await;
    admit(joined).unwrap();
    result.unwrap();
    println!(
        "NHF014_REPORT {}",
        serde_json::json!({"pass":true,"actual_media_tls":true,"shared_action_controller":true,"lost_reply_recovered_same_id":true,"stale_route_refused":true,"revocation_purge":true,"no_reconnect_replay":true,"artwork_fallbacks":true,"owners_joined":true,"native_rendering":false,"production_media":false})
    );
}
