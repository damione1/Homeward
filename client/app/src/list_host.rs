//! Joined TaskService owner for either the page catalogue/selected list or one
//! exact dashboard source. Both use the same revision fences, single in-flight
//! write and coalesced visual publication.
mod projection;
mod recovery;
#[cfg(test)]
mod stall_probe;
#[cfg(test)]
mod tls_probe;
use crate::{
    HomewardWindow, NavigationStep, OptionRow, ProTaskAction,
    list_state::{Freshness, Prepared, Refusal, State, Write},
};
use home_client_cache::lists::{CapturedRows, ListReadEvent, ListReadExit, Scope, read_lists_once};
use home_client_cache::{ProjectionMailbox, Publication};
use home_client_session::{Generation, Phase, SessionCoordinator};
use home_client_transport::TaskTransport;
use slint::{ComponentHandle, Model, ModelRc, SharedString, VecModel};
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, Ordering},
};
use std::time::Duration;
use tokio::{
    sync::{Notify, mpsc},
    task::JoinSet,
};
use tokio_util::sync::CancellationToken;

// View-only catalogue search. Rows retain their exact authority-bearing keys;
// selection and write admission continue through the existing list owner.
pub(crate) fn filter_options(rows: ModelRc<OptionRow>, query: SharedString) -> ModelRc<OptionRow> {
    let query = query.trim().to_lowercase();
    if query.is_empty() {
        return rows;
    }
    ModelRc::new(VecModel::from(
        rows.iter()
            .filter(|row| row.label.to_lowercase().contains(&query))
            .collect::<Vec<_>>(),
    ))
}
pub(crate) fn find_option(rows: ModelRc<OptionRow>, prefix: SharedString) -> i32 {
    let prefix = prefix.trim().to_lowercase();
    if prefix.is_empty() {
        return -1;
    }
    rows.iter()
        .position(|row| !row.disabled && row.label.to_lowercase().starts_with(&prefix))
        .map_or(-1, |index| index as i32)
}

#[derive(Clone)]
pub(crate) struct Lists(Arc<Inner>);
struct Inner {
    state: Mutex<State>,
    mailbox: ProjectionMailbox<projection::Projection>,
    window: Option<slint::Weak<HomewardWindow>>,
    changed: Notify,
    retry: AtomicBool,
    commands: Mutex<Option<mpsc::Sender<Prepared>>>,
    widget: Option<crate::dashboard_widget_host::TaskTarget>,
    session: Mutex<Option<(SessionCoordinator, Option<Generation>)>>,
}
impl Lists {
    fn new(window: Option<slint::Weak<HomewardWindow>>) -> Self {
        Self::with_widget(window, None)
    }
    fn with_widget(
        window: Option<slint::Weak<HomewardWindow>>,
        widget: Option<crate::dashboard_widget_host::TaskTarget>,
    ) -> Self {
        Self(Arc::new(Inner {
            state: Mutex::new(State::default()),
            mailbox: ProjectionMailbox::new(),
            window,
            changed: Notify::new(),
            retry: AtomicBool::new(false),
            commands: Mutex::new(None),
            widget,
            session: Mutex::new(None),
        }))
    }
    #[cfg(test)]
    pub(crate) fn detached() -> Self {
        Self::new(None)
    }
    pub(crate) fn for_widget(
        window: Option<slint::Weak<HomewardWindow>>,
        target: crate::dashboard_widget_host::TaskTarget,
    ) -> Self {
        Self::with_widget(window, Some(target))
    }
    fn widget_source_current(&self) -> bool {
        self.0.widget.is_none()
            || self
                .0
                .session
                .lock()
                .expect("list session")
                .as_ref()
                .is_some_and(|(session, source)| {
                    source.is_some_and(|source| session.is_current(source))
                })
    }
    pub(crate) fn widget_complete(&self, token: i32, generation: i32, completed: bool) {
        if !self.widget_source_current() {
            return;
        }
        let allowed = {
            let state = self.0.state.lock().expect("list state mutex");
            self.0.widget.as_ref().is_some_and(|target| {
                generation == state.generation
                    && state.items.as_ref().is_some_and(|snapshot| {
                        let CapturedRows::Items { rows, .. } = &snapshot.rows else {
                            return false;
                        };
                        rows.iter()
                            .zip(state.item_tokens.iter())
                            .filter(|(row, _)| !target.shopping_only || !row.content().completed())
                            .take(target.limit)
                            .any(|(_, item)| *item == token)
                    })
            })
        };
        if allowed {
            self.select_and_write(token, generation, Write::Complete(completed));
        }
    }
    pub fn bind(window: &HomewardWindow) -> Self {
        window.on_lists_filter_options(filter_options);
        window.on_lists_find_option(find_option);
        let this = Self::new(Some(window.as_weak()));
        let owner = this.clone();
        window.on_lists_choose(move |token, generation| {
            owner.update(|state| state.select_list(token, generation));
            owner.0.changed.notify_one();
        });
        let owner = this.clone();
        window.on_lists_select_task(move |token, generation| {
            owner.update(|state| state.select_item(token, generation))
        });
        let owner = this.clone();
        window.on_lists_edit(move |token, generation, text| {
            owner.update(|state| state.edit(token, generation, text.into()))
        });
        let owner = this.clone();
        window.on_lists_complete(move |token, generation, completed| {
            owner.select_and_write(token, generation, Write::Complete(completed));
        });
        let owner = this.clone();
        window.on_lists_grocery_complete(move |lt, lg, token, generation, completed| {
            if owner.matches_scope(lt, lg) {
                owner.select_and_write(token, generation, Write::Complete(completed));
            }
        });
        let owner = this.clone();
        window.on_lists_grocery_select(move |lt, lg, token, generation| {
            if owner.matches_scope(lt, lg) {
                owner.update(|state| state.select_item(token, generation));
            }
        });
        let owner = this.clone();
        window.on_lists_grocery_remove(move |lt, lg, token, generation| {
            if owner.matches_scope(lt, lg) {
                owner.select_and_write(token, generation, Write::DeleteItem);
            }
        });
        let owner = this.clone();
        window.on_lists_grocery_quick_add(move |lt, lg, token, generation| {
            let completed = {
                let state = owner.0.state.lock().expect("list state mutex");
                state.matches_list_scope(lt, lg)
                    && generation == state.generation
                    && state.items.as_ref().is_some_and(|snapshot| {
                        let CapturedRows::Items { rows, .. } = &snapshot.rows else {
                            return false;
                        };
                        rows.iter()
                            .zip(state.item_tokens.iter())
                            .any(|(row, item)| *item == token && row.content().completed())
                    })
            };
            if completed {
                owner.select_and_write(token, generation, Write::Complete(false));
            }
        });
        let owner = this.clone();
        window.on_lists_grocery_add(move |lt, lg, name| {
            if owner.matches_scope(lt, lg) {
                owner.write(lg, Write::Add(name.into()));
            }
        });
        let owner = this.clone();
        window.on_lists_action(move |token, generation, action| match action {
            ProTaskAction::Save => owner.select_and_write(token, generation, Write::Save),
            ProTaskAction::Remove => owner.select_and_write(token, generation, Write::DeleteItem),
            ProTaskAction::MoveFirst => owner.select_and_write(token, generation, Write::MoveFirst),
            ProTaskAction::MoveUp => owner.select_and_write(token, generation, Write::MoveUp),
            ProTaskAction::MoveDown => owner.select_and_write(token, generation, Write::MoveDown),
            ProTaskAction::Resolve => owner.update(|state| state.resolve(generation)),
            ProTaskAction::Reload => owner.refresh(generation, true),
        });
        let owner = this.clone();
        window.on_lists_create(move |generation, name| {
            owner.write(generation, Write::Create(name.into()))
        });
        let owner = this.clone();
        window.on_lists_rename(move |generation, name| {
            owner.write(generation, Write::Rename(name.into()))
        });
        let owner = this.clone();
        window.on_lists_delete(move |generation| owner.write(generation, Write::DeleteList));
        let owner = this.clone();
        window
            .on_lists_add(move |generation, name| owner.write(generation, Write::Add(name.into())));
        let owner = this.clone();
        window.on_lists_reload(move |generation| owner.refresh(generation, false));
        let owner = this.clone();
        window.on_lists_navigate(move |step| owner.navigate(step));
        this
    }
    fn matches_scope(&self, token: i32, generation: i32) -> bool {
        self.0
            .state
            .lock()
            .expect("list state mutex")
            .matches_list_scope(token, generation)
    }
    fn publish(&self, state: &State) {
        let value = projection::Projection::new(state);
        if let Publication::Schedule(ticket) =
            self.0.mailbox.publish(self.0.mailbox.generation(), value)
        {
            let Some(window) = &self.0.window else {
                return;
            };
            let target = self.0.widget.clone();
            let source = self.0.session.lock().expect("list session").clone();
            let _ = window.upgrade_in_event_loop(move |window| {
                ticket.deliver(|_, projection| match target {
                    Some(target) => projection::render_widget(&window, projection, &target, source),
                    None => projection::render(&window, projection),
                });
            });
        }
    }
    fn update(&self, change: impl FnOnce(&mut State) -> Result<(), Refusal>) {
        let mut state = self.0.state.lock().expect("list state mutex");
        if let Err(refusal) = change(&mut state) {
            state.notice = match refusal {
                Refusal::Stale => "The context changed. Reload the list before editing.",
                Refusal::Busy => "A change is already awaiting the server’s response.",
                Refusal::Invalid => "This change is invalid.",
                Refusal::Capacity => "The draft or display limit has been reached.",
            };
        }
        self.publish(&state);
    }
    pub fn clear(&self) {
        let mut state = self.0.state.lock().expect("list state mutex");
        state.clear();
        let _ = self.0.mailbox.invalidate();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    pub fn foreground(&self, foreground: bool) {
        let mut state = self.0.state.lock().expect("list state mutex");
        if !foreground {
            state.clear();
            let _ = self.0.mailbox.invalidate();
        }
        state.foreground = foreground;
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn write(&self, generation: i32, write: Write) {
        let mut state = self.0.state.lock().expect("list state mutex");
        let prepared = match state.prepare(generation, write) {
            Ok(prepared) => prepared,
            Err(_) => {
                state.notice = "Change not sent: context unavailable, draft invalid or a write is in progress.";
                self.publish(&state);
                return;
            }
        };
        let context = prepared.context;
        let id = prepared.id;
        let sender = self.0.commands.lock().expect("list commands mutex").clone();
        if sender.is_none_or(|sender| sender.try_send(prepared).is_err()) {
            state.finish(
                context,
                id,
                home_client_actions::lists::ListWriteOutcome::NotSent(
                    home_client_actions::lists::NotSentReason::AdmissionChanged,
                ),
            );
        }
        self.publish(&state);
    }
    fn select_and_write(&self, token: i32, generation: i32, write: Write) {
        let next = {
            let mut state = self.0.state.lock().expect("list state mutex");
            if generation != state.generation {
                return;
            }
            let same = state
                .selected_item
                .as_ref()
                .and_then(|key| state.current_item(key))
                .is_some_and(|(row, _)| {
                    state
                        .item_tokens
                        .iter()
                        .position(|candidate| *candidate == token)
                        .and_then(|index| {
                            state
                                .items
                                .as_ref()
                                .and_then(|snapshot| match &snapshot.rows {
                                    CapturedRows::Items { rows, .. } => rows.get(index),
                                    CapturedRows::Catalog(_) => None,
                                })
                        })
                        .is_some_and(|candidate| candidate.key() == row.key())
                });
            if !same && state.select_item(token, generation).is_err() {
                return;
            }
            state.generation
        };
        self.write(next, write);
    }
    fn refresh(&self, generation: i32, discard_draft: bool) {
        self.update(|state| {
            if generation != state.generation || state.busy() {
                return Err(Refusal::Stale);
            }
            if discard_draft {
                state.reload(generation)?;
            }
            state.blocked = false;
            state.context = state.context.checked_add(1).ok_or(Refusal::Capacity)?;
            state.catalog_phase = Freshness::Syncing;
            if state.selected_list.is_some() {
                state.item_phase = Freshness::Syncing;
            }
            Ok(())
        });
        self.0.retry.store(true, Ordering::SeqCst);
        self.0.changed.notify_one();
    }
    fn navigate(&self, step: NavigationStep) {
        self.update(|state| {
            let Some(snapshot) = &state.items else {
                return Err(Refusal::Stale);
            };
            let CapturedRows::Items { rows, .. } = &snapshot.rows else {
                return Err(Refusal::Stale);
            };
            if rows.is_empty() {
                return Ok(());
            }
            let index = state
                .selected_item
                .as_ref()
                .and_then(|key| rows.iter().position(|row| row.key().uid() == key.uid()))
                .unwrap_or(0);
            let next = match step {
                NavigationStep::Next => (index + 1).min(rows.len() - 1),
                NavigationStep::Previous => index.saturating_sub(1),
                NavigationStep::First => 0,
                NavigationStep::Last => rows.len() - 1,
                NavigationStep::PageNext => (index + 5).min(rows.len() - 1),
                NavigationStep::PagePrevious => index.saturating_sub(5),
            };
            state.select_item(state.item_tokens[next], state.generation)
        });
    }
    pub async fn run(
        &self,
        session: SessionCoordinator,
        transport: TaskTransport,
        cancel: CancellationToken,
    ) {
        *self.0.session.lock().expect("list session") = Some((session.clone(), None));
        let (sender, mut commands) = mpsc::channel::<Prepared>(1);
        *self.0.commands.lock().expect("list commands mutex") = Some(sender);
        let mut phases = session.subscribe();
        let mut source = phases.borrow().generation;
        let mut context = None;
        let mut tasks = JoinSet::new();
        let mut attempt = cancel.child_token();
        loop {
            if self.0.retry.swap(false, Ordering::SeqCst) {
                let _ = session.allow_new_view_attempt().await;
            }
            let (active, next, selected) = {
                let mut state = self.0.state.lock().expect("list state mutex");
                if self.0.widget.is_some() && source != phases.borrow().generation {
                    source = phases.borrow().generation;
                    state.clear();
                    let _ = self.0.mailbox.invalidate();
                    self.publish(&state);
                }
                let active = matches!(phases.borrow().phase, Phase::Ready | Phase::Refreshing)
                    && state.foreground
                    && state.context != 0
                    && !state.blocked;
                if !active && state.active {
                    state.clear();
                    let _ = self.0.mailbox.invalidate();
                    self.publish(&state);
                }
                state.active = active;
                if active
                    && let Some(target) = &self.0.widget
                    && state.selected_list.is_none()
                {
                    state.selected_list = Some(target.list.native_id().clone());
                    state.item_phase = Freshness::Syncing;
                }
                (active, state.context, state.selected_list.clone())
            };
            if !active || context != Some(next) {
                attempt.cancel();
                while tasks.join_next().await.is_some() {}
                context = None;
                if active {
                    attempt = cancel.child_token();
                    context = Some(next);
                    let owner = self.clone();
                    let credential = session.clone();
                    let api = transport.clone();
                    let stop = attempt.clone();
                    if self.0.widget.is_none() {
                        tasks.spawn(async move {
                            owner
                                .read(credential, api, Scope::Catalog, next, stop)
                                .await;
                        });
                    }
                    if let Some(list) = selected {
                        let owner = self.clone();
                        let credential = session.clone();
                        let api = transport.clone();
                        let stop = attempt.clone();
                        tasks.spawn(async move {
                            owner
                                .read(credential, api, Scope::Items(list), next, stop)
                                .await;
                        });
                    }
                }
            }
            tokio::select! { biased;
                () = cancel.cancelled() => break,
                changed = phases.changed() => { if changed.is_err() { break; } }
                () = self.0.changed.notified() => {}
                command = commands.recv() => {
                    let Some(prepared) = command else { break };
                    let owner = self.clone(); let credential = session.clone(); let api = transport.clone(); let stop = attempt.clone();
                    tasks.spawn(async move {
                        let (context,id) = (prepared.context,prepared.id);
                        let admission = owner.clone();
                        let result = prepared.mutation.submit(&api,&credential,&stop,Arc::new(move || admission.0.state.lock().expect("list state mutex").admits(context,id)&&admission.widget_source_current())).await;
                        owner.update(|state| { state.finish(context,id,result); Ok(()) });
                        let generation = owner.0.state.lock().expect("list state mutex").generation;
                        owner.refresh(generation,false);
                    });
                }
                _ = tasks.join_next(), if !tasks.is_empty() => {}
            }
        }
        attempt.cancel();
        while tasks.join_next().await.is_some() {}
        *self.0.commands.lock().expect("list commands mutex") = None;
        self.clear();
        *self.0.session.lock().expect("list session") = None;
    }
    async fn read(
        &self,
        session: SessionCoordinator,
        transport: TaskTransport,
        scope: Scope,
        context: u64,
        cancel: CancellationToken,
    ) {
        loop {
            let lease = tokio::select! { biased; ()=cancel.cancelled()=>return, lease=session.access(&cancel)=>match lease {Ok(lease)=>lease,Err(_)=>return} };
            let result = read_lists_once(&transport, &lease, scope.clone(), &cancel, |event| {
                let mut state = self.0.state.lock().expect("list state mutex");
                if state.context != context
                    || !state.active
                    || !state.foreground
                    || !session.is_current(lease.generation)
                {
                    return;
                }
                let changed = match event {
                    ListReadEvent::Invalidated => {
                        match scope {
                            Scope::Catalog => state.catalog_phase = Freshness::Syncing,
                            Scope::Items(_) => {
                                state.item_phase = Freshness::Syncing;
                                if self.0.widget.is_some() {
                                    state.catalog_phase = Freshness::Syncing
                                }
                            }
                        };
                        Ok(())
                    }
                    ListReadEvent::Published(snapshot) => match &scope {
                        Scope::Catalog => state.publish_catalog(snapshot),
                        Scope::Items(_) => {
                            let result = state.publish_items(snapshot);
                            if result.is_ok() && self.0.widget.is_some() {
                                state.catalog_phase = Freshness::Online;
                                if let Some((_, source)) =
                                    self.0.session.lock().expect("list session").as_mut()
                                {
                                    *source = Some(lease.generation)
                                }
                            }
                            result
                        }
                    },
                };
                if changed.is_err() {
                    state.clear();
                    let _ = self.0.mailbox.invalidate();
                }
                self.publish(&state);
                if state.context != context {
                    self.0.changed.notify_one();
                }
            })
            .await;
            if cancel.is_cancelled() {
                return;
            }
            use recovery::Recovery;
            let recovery = match result {
                ListReadExit::Transport(failure) => {
                    let mut recovery = recovery::transport(&failure);
                    if self.0.widget.is_some()
                        && matches!(
                            failure.kind(),
                            home_client_transport::FailureKind::Rpc(
                                home_client_transport::RpcCode::NotFound
                            )
                        )
                    {
                        recovery = Recovery::Stop;
                    }
                    let _ = session.observe_failure(lease.generation, &failure).await;
                    recovery
                }
                ListReadExit::Cancelled => {
                    if matches!(
                        session.subscribe().borrow().phase,
                        Phase::Ready | Phase::Refreshing
                    ) {
                        Recovery::Reconnect
                    } else {
                        Recovery::Stop
                    }
                }
                ListReadExit::Ended | ListReadExit::Resync => Recovery::Reset,
                ListReadExit::Contract(_) => Recovery::Stop,
            };
            {
                let mut state = self.0.state.lock().expect("list state mutex");
                if state.context != context {
                    return;
                }
                match recovery {
                    Recovery::Offline => {
                        match scope {
                            Scope::Catalog => state.catalog_phase = Freshness::Offline,
                            Scope::Items(_) => state.item_phase = Freshness::Offline,
                        }
                        state.read_notice = "Connection interrupted. Dated cache is read-only; pending writes will not be replayed.";
                    }
                    Recovery::Reconnect => match scope {
                        Scope::Catalog => state.catalog_phase = Freshness::Syncing,
                        Scope::Items(_) => state.item_phase = Freshness::Syncing,
                    },
                    Recovery::Reset => {
                        state.reset_for_resync();
                        let _ = self.0.mailbox.invalidate();
                    }
                    Recovery::Stop => {
                        state.clear();
                        state.blocked = true;
                        state.read_notice =
                            "Lists must be reloaded. Previous private data was removed.";
                        let _ = self.0.mailbox.invalidate();
                    }
                }
                self.publish(&state);
            }
            if matches!(recovery, Recovery::Reset | Recovery::Stop) {
                self.0.changed.notify_one();
                return;
            }
            tokio::select! {biased; ()=cancel.cancelled()=>return, ()=tokio::time::sleep(Duration::from_secs(1))=>{}}
        }
    }
}

#[cfg(test)]
mod widget_tests {
    use super::*;
    struct EmptyStore;
    impl home_client_session::SecureStore for EmptyStore {
        fn load(
            &self,
        ) -> Result<Option<zeroize::Zeroizing<Vec<u8>>>, home_client_session::StoreFailure>
        {
            Ok(None)
        }
        fn save(&self, _: &[u8]) -> Result<(), home_client_session::StoreFailure> {
            Ok(())
        }
        fn clear(&self) -> Result<(), home_client_session::StoreFailure> {
            Ok(())
        }
    }
    fn owner(limit: usize) -> Lists {
        let owner = Lists::for_widget(
            None,
            crate::dashboard_widget_host::TaskTarget {
                token: 7,
                generation: 11,
                list: home_client_model::api_tasks::TaskListName::new(
                    home_client_model::api::HomeName::try_from("homes/synthetic").unwrap(),
                    "configured-second-list".parse().unwrap(),
                ),
                limit,
                shopping_only: false,
            },
        );
        let connection = home_client_transport::Connection::new(
            "https://127.0.0.1:9",
            include_bytes!("../tests/fixtures/trust-a.pem"),
        )
        .unwrap();
        let session = SessionCoordinator::new(connection.auth(), Arc::new(EmptyStore));
        *owner.0.session.lock().unwrap() = Some((session.clone(), Some(session.generation())));
        {
            let mut state = owner.0.state.lock().unwrap();
            state.active = true;
            state.selected_list = Some("configured-second-list".parse().unwrap());
            state
                .publish_items(crate::list_state::tests::items(
                    "configured-second-list",
                    10,
                    &[
                        ("same-uid", 3, "Observed first"),
                        ("hidden-uid", 2, "Outside preview"),
                    ],
                ))
                .unwrap();
            state.catalog_phase = Freshness::Online;
        }
        owner
    }
    #[test]
    fn widget_checkbox_uses_exact_source_visible_identity_and_consumes_one_preparation() {
        let owner = owner(1);
        let (sender, mut receiver) = mpsc::channel(1);
        *owner.0.commands.lock().unwrap() = Some(sender);
        let (generation, first, hidden) = {
            let state = owner.0.state.lock().unwrap();
            assert!(
                state.catalog.is_none(),
                "a fixed source does not select from a page catalogue"
            );
            (state.generation, state.item_tokens[0], state.item_tokens[1])
        };
        owner.widget_complete(hidden, generation, true);
        assert!(receiver.try_recv().is_err());
        owner.widget_complete(first, generation - 1, true);
        assert!(receiver.try_recv().is_err());
        owner.widget_complete(first, generation, true);
        let prepared = receiver.try_recv().expect("one prepared completion");
        {
            let state = owner.0.state.lock().unwrap();
            assert!(state.admits(prepared.context, prepared.id));
            assert_eq!(
                state.selected_target().unwrap().1.key().list().as_str(),
                "configured-second-list"
            );
            assert!(
                !state.selected_target().unwrap().0.content().completed(),
                "completion stays observed until server readback"
            );
        }
        owner.widget_complete(first, generation, true);
        assert!(
            receiver.try_recv().is_err(),
            "the stale delegate cannot repeat the write"
        );
        owner.clear();
        assert!(
            !owner
                .0
                .state
                .lock()
                .unwrap()
                .admits(prepared.context, prepared.id)
        );
        owner.widget_complete(first, generation, true);
        assert!(receiver.try_recv().is_err());
    }
    #[tokio::test]
    async fn a_credential_generation_change_refuses_old_widget_rows_before_the_reader_wakes() {
        let owner = owner(1);
        let (sender, mut receiver) = mpsc::channel(1);
        *owner.0.commands.lock().unwrap() = Some(sender);
        let (token, generation) = {
            let state = owner.0.state.lock().unwrap();
            (state.item_tokens[0], state.generation)
        };
        let session = owner.0.session.lock().unwrap().as_ref().unwrap().0.clone();
        assert!(owner.widget_source_current());
        session.logout_local().await.unwrap();
        assert!(!owner.widget_source_current());
        owner.widget_complete(token, generation, true);
        assert!(receiver.try_recv().is_err());
        assert!(!owner.0.state.lock().unwrap().busy());
    }
    #[test]
    fn independent_widgets_do_not_share_selection_or_pending_mutations() {
        let first = owner(1);
        let second = owner(2);
        let (sender, mut receiver) = mpsc::channel(1);
        *first.0.commands.lock().unwrap() = Some(sender);
        let (token, generation) = {
            let state = first.0.state.lock().unwrap();
            (state.item_tokens[0], state.generation)
        };
        first.widget_complete(token, generation, true);
        assert!(receiver.try_recv().is_ok());
        assert!(first.0.state.lock().unwrap().busy());
        assert!(!second.0.state.lock().unwrap().busy());
        first.clear();
        assert!(second.0.state.lock().unwrap().items.is_some());
    }
}

#[cfg(test)]
mod selector_search_tests {
    use super::*;
    #[test]
    fn local_catalogue_search_preserves_keys_generations_and_eligibility() {
        let rows = ModelRc::from([
            OptionRow {
                token: 71,
                generation: 9,
                label: "Groceries".into(),
                description: "Private description".into(),
                disabled: true,
            },
            OptionRow {
                token: 81,
                generation: 11,
                label: "Hardware".into(),
                description: "".into(),
                disabled: false,
            },
        ]);
        assert_eq!(filter_options(rows.clone(), "  ".into()).row_count(), 2);
        let result = filter_options(rows.clone(), "  GRO  ".into());
        assert_eq!(result.row_count(), 1);
        let row = result.row_data(0).unwrap();
        assert_eq!((row.token, row.generation), (71, 9));
        assert!(row.disabled);
        assert_eq!(row.description, "Private description");
        assert_eq!(
            filter_options(rows.clone(), "missing".into()).row_count(),
            0
        );
        assert_eq!(rows.row_count(), 2);
        assert_eq!(find_option(rows.clone(), "ha".into()), 1);
        assert_eq!(find_option(rows, "gr".into()), -1);
    }
}
