//! Explicit real TLS qualification of the same owner used by the native UI.
//! The Go fixture owns a fresh database and contains no device Host or driver.
use super::*;
use crate::list_state::DraftPhase;
use home_client_actions::lists::{ListWriteIntent, ListWriteOutcome, PreparedListMutation};
use home_client_model::lists::{ItemKey, ListId};
use home_client_session::{SecureStore, StoreFailure};
use home_client_transport::Connection;
use home_wire_contract::home::lists::v1 as wire;
use serde::{Deserialize, Serialize};
use std::io::Write as _;
use zeroize::Zeroizing;

type Result<T> = std::result::Result<T, Box<dyn std::error::Error + Send + Sync>>;
fn admit<T, E: std::fmt::Debug>(value: std::result::Result<T, E>) -> Result<T> {
    value.map_err(|error| format!("list qualification admission: {error:?}").into())
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
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    seed_list_id: String,
    seed_item_count: usize,
    admin: Credential,
    reader: Credential,
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
#[derive(Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Control {
    Outage,
    RestoreNetwork,
    RevokeReader,
    AllowReader,
    Snapshot,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Stats {
    mutation_requests: u64,
    active_streams: u64,
    action_rpc_requests: u64,
    device_drivers: u64,
    seeded_items: u64,
}
#[derive(Default)]
struct Controls(u64);
impl Controls {
    async fn send(&mut self, action: Control) -> Result<Option<Stats>> {
        self.0 = self.0.checked_add(1).ok_or("fixture control exhausted")?;
        println!(
            "NHF011_CONTROL {}",
            serde_json::json!({"sequence":self.0,"action":action})
        );
        std::io::stdout().flush()?;
        let line = tokio::task::spawn_blocking(|| {
            let mut line = String::new();
            std::io::stdin().read_line(&mut line).map(|_| line)
        })
        .await??;
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Ack {
            sequence: u64,
            action: Control,
            stats: Option<Stats>,
        }
        let ack: Ack = serde_json::from_str(&line)?;
        if ack.sequence != self.0
            || ack.action != action
            || ack.stats.is_some() != (action == Control::Snapshot)
        {
            return Err("unbound list control acknowledgement".into());
        }
        Ok(ack.stats)
    }
    async fn stats(&mut self) -> Result<Stats> {
        self.send(Control::Snapshot)
            .await?
            .ok_or_else(|| "missing list counters".into())
    }
}

async fn until(owner: &Lists, label: &str, predicate: impl Fn(&State) -> bool) -> Result<()> {
    let wait = tokio::time::timeout(Duration::from_secs(25), async {
        loop {
            if predicate(&owner.0.state.lock().unwrap()) {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await;
    if wait.is_err() {
        let state = owner.0.state.lock().unwrap();
        return Err(format!("timed out waiting for {label}: active={}, blocked={}, catalog={}, items={}, busy={}, notice={}, read={}",
            state.active,state.blocked,state.catalog.is_some(),state.items.is_some(),state.busy(),state.notice,state.read_notice).into());
    }
    Ok(())
}
fn online(state: &State) -> bool {
    state.active
        && !state.busy()
        && state.catalog_phase == Freshness::Online
        && (state.selected_list.is_none() || state.item_phase == Freshness::Online)
}
fn catalog(state: &State) -> &[Arc<wire::ListSnapshot>] {
    match state.catalog.as_deref().map(|snapshot| &snapshot.rows) {
        Some(CapturedRows::Catalog(rows)) => rows,
        Some(CapturedRows::Items { .. }) | None => &[],
    }
}
fn rows(state: &State) -> &[Arc<wire::ItemSnapshot>] {
    match state.items.as_deref().map(|snapshot| &snapshot.rows) {
        Some(CapturedRows::Items { rows, .. }) => rows,
        Some(CapturedRows::Catalog(_)) | None => &[],
    }
}
async fn choose_list(owner: &Lists, id: &ListId) -> Result<()> {
    until(owner, "online catalog before selection", online).await?;
    {
        let mut state = owner.0.state.lock().unwrap();
        let index = catalog(&state)
            .iter()
            .position(|row| row.id.value == id.as_str())
            .ok_or("list not in authorized catalog")?;
        let (token, generation) = (state.catalog_tokens[index], state.generation);
        admit(state.select_list(token, generation))?;
    }
    owner.0.changed.notify_one();
    until(owner, "selected list capture", |state| {
        online(state) && state.selected_list.as_ref() == Some(id)
    })
    .await
}
fn choose_item(owner: &Lists, index: usize) -> Result<(ItemKey, i32)> {
    let mut state = owner.0.state.lock().unwrap();
    let token = *state.item_tokens.get(index).ok_or("missing item token")?;
    let generation = state.generation;
    admit(state.select_item(token, generation))?;
    Ok((
        state
            .selected_item
            .clone()
            .ok_or("missing selected identity")?,
        token,
    ))
}
async fn write(owner: &Lists, intent: Write, confirmed: bool) -> Result<()> {
    until(owner, "write admission", online).await?;
    let (generation, context) = {
        let state = owner.0.state.lock().unwrap();
        (state.generation, state.context)
    };
    owner.write(generation, intent);
    until(owner, "write receipt and new capture", |state| {
        online(state)
            && state.context != context
            && if confirmed {
                state.notice.starts_with("Modification confirmée")
            } else {
                state.notice.starts_with("Modification refusée")
            }
    })
    .await
}
async fn login(connection: &Connection, credential: Credential) -> Result<SessionCoordinator> {
    let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    admit(
        session
            .login(credential.login_name, Zeroizing::new(credential.password))
            .await,
    )?;
    Ok(session)
}
fn start(
    owner: &Lists,
    session: &SessionCoordinator,
    connection: &Connection,
    cancel: &CancellationToken,
) -> tokio::task::JoinHandle<()> {
    let (owner, session, transport, cancel) = (
        owner.clone(),
        session.clone(),
        connection.lists(),
        cancel.child_token(),
    );
    tokio::spawn(async move { owner.run(session, transport, cancel).await })
}
async fn run(fixture: Fixture) -> Result<()> {
    let Mode::SyntheticOnly = fixture.mode;
    let origin = url::Url::parse(&fixture.origin)?;
    if origin.scheme() != "https"
        || origin.host_str() != Some("127.0.0.1")
        || origin.port().is_none()
        || !origin.username().is_empty()
        || origin.password().is_some()
        || origin.path() != "/"
        || origin.query().is_some()
        || origin.fragment().is_some()
    {
        return Err("explicit loopback TLS fixture required".into());
    }
    assert_eq!(fixture.seed_item_count, 10000);
    let seed: ListId = admit(fixture.seed_list_id.parse())?;
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes())?;
    let admin = login(&connection, fixture.admin).await?;
    let reader = login(&connection, fixture.reader).await?;
    let cancel = CancellationToken::new();
    let owner = Lists::detached();
    let reader_owner = Lists::detached();
    let admin_task = start(&owner, &admin, &connection, &cancel);
    let reader_task = start(&reader_owner, &reader, &connection, &cancel);
    let mut controls = Controls::default();
    choose_list(&owner, &seed).await?;
    choose_list(&reader_owner, &seed).await?;
    for current in [&owner, &reader_owner] {
        let state = current.0.state.lock().unwrap();
        assert_eq!(catalog(&state).len(), 1);
        assert_eq!(state.catalog.as_ref().unwrap().count, 1);
        assert_eq!(rows(&state).len(), 10000);
        assert_eq!(state.items.as_ref().unwrap().count, 10000);
        assert_eq!(rows(&state)[9999].uid.value, "synthetic-10000");
    }
    write(&owner, Write::Create("Qualification TLS".into()), true).await?;
    let created: ListId = {
        let state = owner.0.state.lock().unwrap();
        admit(
            catalog(&state)
                .iter()
                .find(|row| row.name == "Qualification TLS")
                .ok_or("created list absent")?
                .id
                .value
                .parse(),
        )?
    };
    choose_list(&owner, &created).await?;
    write(&owner, Write::Rename("Qualification renommée".into()), true).await?;
    assert!(
        catalog(&owner.0.state.lock().unwrap())
            .iter()
            .any(|row| row.id.value == created.as_str() && row.name == "Qualification renommée")
    );
    write(&owner, Write::Add("Premier élément".into()), true).await?;
    write(&owner, Write::Add("Deuxième élément".into()), true).await?;
    let (first, first_token) = choose_item(&owner, 0)?;
    {
        let mut state = owner.0.state.lock().unwrap();
        let generation = state.generation;
        admit(state.edit(first_token, generation, "Édition explicite".into()))?;
    }
    write(&owner, Write::Save, true).await?;
    write(&owner, Write::Complete(true), true).await?;
    assert_eq!(
        rows(&owner.0.state.lock().unwrap())[0].value.completed,
        Some(true)
    );
    write(&owner, Write::Complete(false), true).await?;
    let lease = admit(admin.access(&cancel).await)?;
    let mut request = wire::GetItemRequest::default();
    request.list_id.get_or_insert_default().value = created.as_str().into();
    request.item_uid.get_or_insert_default().value = first.uid().as_str().into();
    let fetched = admit(
        connection
            .lists()
            .get_item(request, lease.token(), Duration::from_secs(10), &cancel)
            .await,
    )?;
    assert_eq!(fetched.item.value.completed, Some(false));
    assert_eq!(fetched.item.value.summary, "Édition explicite");
    write(&owner, Write::MoveDown, true).await?;
    {
        let state = owner.0.state.lock().unwrap();
        assert_eq!(rows(&state)[1].uid.value, first.uid().as_str());
        assert_eq!(state.item_tokens[1], first_token);
        assert_eq!(state.selected_item.as_ref(), Some(&first));
    }
    write(&owner, Write::MoveFirst, true).await?;
    assert_eq!(
        rows(&owner.0.state.lock().unwrap())[0].uid.value,
        first.uid().as_str()
    );
    // Two writes are prepared against the very same revisions. The external
    // writer wins; the stale attempt must be a real server CAS conflict.
    let (external, stale) = {
        let mut state = owner.0.state.lock().unwrap();
        let generation = state.generation;
        admit(state.edit(first_token, generation, "Mon brouillon conservé".into()))?;
        let (row, target) = state.selected_target().ok_or("selected target missing")?;
        let mut payload = (*row.value).clone();
        payload.summary = "Autre client".into();
        let external = admit(PreparedListMutation::new(ListWriteIntent::Edit {
            target: target.clone(),
            payload: payload.clone(),
        }))?;
        payload.summary = "Écriture périmée".into();
        (
            external,
            admit(PreparedListMutation::new(ListWriteIntent::Edit {
                target,
                payload,
            }))?,
        )
    };
    let api = connection.lists();
    assert!(matches!(
        external
            .submit(&api, &admin, &cancel, Arc::new(|| true))
            .await,
        ListWriteOutcome::Committed(_)
    ));
    assert!(matches!(
        stale.submit(&api, &admin, &cancel, Arc::new(|| true)).await,
        ListWriteOutcome::Conflict
    ));
    until(&owner, "Watch conflict with retained draft", |state| {
        online(state)
            && state.drafts.get(&first).is_some_and(|draft| {
                draft.phase == DraftPhase::Conflict && draft.text == "Mon brouillon conservé"
            })
            && rows(state)[0].value.summary == "Autre client"
    })
    .await?;
    let before_resolution = controls.stats().await?.mutation_requests;
    let generation = owner.0.state.lock().unwrap().generation;
    owner.write(generation, Write::Save);
    {
        let mut state = owner.0.state.lock().unwrap();
        let generation = state.generation;
        assert!(!state.busy());
        admit(state.resolve(generation))?;
        assert_eq!(state.drafts[&first].text, "Mon brouillon conservé");
    }
    assert_eq!(
        controls.stats().await?.mutation_requests,
        before_resolution,
        "conflict/save refusal and resolution must send nothing"
    );
    write(&owner, Write::Save, true).await?;
    assert_eq!(
        rows(&owner.0.state.lock().unwrap())[0].value.summary,
        "Mon brouillon conservé"
    );
    // READ never implies WRITE. Denial is observed through the actual owner.
    write(&reader_owner, Write::Add("Interdit".into()), false).await?;
    assert_eq!(rows(&reader_owner.0.state.lock().unwrap()).len(), 10000);
    assert_eq!(catalog(&reader_owner.0.state.lock().unwrap()).len(), 1);
    let lease = admit(reader.access(&cancel).await)?;
    let mut request = wire::GetListRequest::default();
    request.list_id.get_or_insert_default().value = created.as_str().into();
    let denied = connection
        .lists()
        .get_list(request, lease.token(), Duration::from_secs(10), &cancel)
        .await
        .err()
        .ok_or("foreign list leaked")?;
    assert_eq!(
        denied.kind(),
        home_client_transport::FailureKind::Rpc(home_client_transport::RpcCode::PermissionDenied)
    );
    let before_offline = controls.stats().await?.mutation_requests;
    {
        let mut state = owner.0.state.lock().unwrap();
        let generation = state.generation;
        admit(state.edit(first_token, generation, "Brouillon hors ligne".into()))?;
    }
    controls.send(Control::Outage).await?;
    for current in [&owner, &reader_owner] {
        until(current, "dated offline cache", |state| {
            state.catalog_phase == Freshness::Offline
                && state.item_phase == Freshness::Offline
                && state.catalog.is_some()
                && state.items.is_some()
        })
        .await?;
    }
    let offline_date = owner
        .0
        .state
        .lock()
        .unwrap()
        .items
        .as_ref()
        .unwrap()
        .fetched_at;
    let generation = owner.0.state.lock().unwrap().generation;
    owner.write(generation, Write::Save);
    assert!(!owner.0.state.lock().unwrap().busy());
    assert_eq!(
        controls.stats().await?.mutation_requests,
        before_offline,
        "offline must not send or enqueue"
    );
    controls.send(Control::RestoreNetwork).await?;
    until(&owner, "new capture after reconnect", |state| {
        online(state) && state.items.as_ref().unwrap().fetched_at > offline_date
    })
    .await?;
    until(&reader_owner, "reader reconnect", online).await?;
    assert_eq!(
        owner.0.state.lock().unwrap().drafts[&first].text,
        "Brouillon hors ligne"
    );
    assert_eq!(
        rows(&owner.0.state.lock().unwrap())[0].value.summary,
        "Mon brouillon conservé"
    );
    assert_eq!(
        controls.stats().await?.mutation_requests,
        before_offline,
        "reconnect must never replay a mutation"
    );
    // Withdraw READ while a draft and selected item exist. Old authorized
    // content and drafts must disappear before any manual reread can succeed.
    let (_, reader_token) = choose_item(&reader_owner, 0)?;
    {
        let mut state = reader_owner.0.state.lock().unwrap();
        let generation = state.generation;
        admit(state.edit(reader_token, generation, "Brouillon privé retiré".into()))?;
    }
    controls.send(Control::RevokeReader).await?;
    until(&reader_owner, "read revocation purge", |state| {
        state.items.is_none()
            && state.selected_list.is_none()
            && state.selected_item.is_none()
            && state.drafts.is_empty()
            && catalog(state).is_empty()
    })
    .await?;
    controls.send(Control::AllowReader).await?;
    let generation = reader_owner.0.state.lock().unwrap().generation;
    reader_owner.refresh(generation, false);
    until(&reader_owner, "explicit authorized reread", |state| {
        online(state) && catalog(state).len() == 1
    })
    .await?;
    assert!(reader_owner.0.state.lock().unwrap().drafts.is_empty());
    choose_list(&reader_owner, &seed).await?;
    assert_eq!(rows(&reader_owner.0.state.lock().unwrap()).len(), 10000);
    // Explicit discard before deletion; deletion must withdraw the old token.
    let generation = owner.0.state.lock().unwrap().generation;
    owner.refresh(generation, true);
    until(&owner, "discard draft reread", |state| {
        online(state) && !state.drafts.contains_key(&first)
    })
    .await?;
    write(&owner, Write::DeleteItem, true).await?;
    {
        let state = owner.0.state.lock().unwrap();
        assert_eq!(rows(&state).len(), 1);
        assert!(state.selected_item.is_none());
        assert!(!state.item_tokens.contains(&first_token));
        assert!(!state.drafts.contains_key(&first));
    }
    write(&owner, Write::DeleteList, true).await?;
    assert!(
        !catalog(&owner.0.state.lock().unwrap())
            .iter()
            .any(|row| row.id.value == created.as_str())
    );
    let mutations = controls.stats().await?.mutation_requests;
    assert_eq!(
        mutations, 15,
        "every intended write has one and only one network attempt"
    );
    cancel.cancel();
    tokio::time::timeout(Duration::from_secs(10), async {
        admin_task.await?;
        reader_task.await?;
        Ok::<(), tokio::task::JoinError>(())
    })
    .await??;
    admit(admin.logout().await)?;
    admit(reader.logout().await)?;
    admin.close().await;
    reader.close().await;
    let mut final_stats = controls.stats().await?;
    for _ in 0..20 {
        if final_stats.active_streams == 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
        final_stats = controls.stats().await?;
    }
    assert_eq!(final_stats.active_streams, 0);
    assert_eq!(final_stats.mutation_requests, 15);
    assert_eq!(final_stats.action_rpc_requests, 0);
    assert_eq!(final_stats.device_drivers, 0);
    assert_eq!(final_stats.seeded_items, 10000);
    println!(
        "NHF011_REPORT {}",
        serde_json::json!({"pass":true,"seeded_items":10000,"mutation_requests":mutations,"cas_conflict":true,"explicit_resolution":true,"offline_no_replay":true,"read_write_independent":true,"read_revocation_purge":true,"owners_joined":true,"active_streams":0,"action_rpc_requests":0,"device_drivers":0,"controls":controls.0})
    );
    Ok(())
}
#[test]
#[ignore = "explicit isolated Go ListService TLS fixture only"]
fn real_tls_lists_owner_and_cas() -> Result<()> {
    let path = std::env::var("HOMEWARD_NHF011_READY")?;
    let fixture = serde_json::from_slice(&std::fs::read(path)?)?;
    tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()?
        .block_on(run(fixture))
}
