//! One joined ListService owner per profile; one catalog and one selected-list
//! Watch, a single in-flight write, and one coalesced visual notification.
mod projection;
mod recovery;
use crate::{
    HomewardWindow, NavigationStep, ProTaskAction,
    list_state::{Freshness, Prepared, Refusal, State, Write},
};
use home_client_cache::lists::{CapturedRows, ListReadEvent, ListReadExit, Scope, read_lists_once};
use home_client_cache::{ProjectionMailbox, Publication};
use home_client_session::{Phase, SessionCoordinator};
use home_client_transport::ListTransport;
use slint::ComponentHandle;
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

#[derive(Clone)]
pub(crate) struct Lists(Arc<Inner>);
struct Inner {
    state: Mutex<State>,
    mailbox: ProjectionMailbox<projection::Projection>,
    window: Option<slint::Weak<HomewardWindow>>,
    changed: Notify,
    retry: AtomicBool,
    commands: Mutex<Option<mpsc::Sender<Prepared>>>,
}
impl Lists {
    fn new(window: Option<slint::Weak<HomewardWindow>>) -> Self {
        Self(Arc::new(Inner {
            state: Mutex::new(State::default()),
            mailbox: ProjectionMailbox::new(),
            window,
            changed: Notify::new(),
            retry: AtomicBool::new(false),
            commands: Mutex::new(None),
        }))
    }
    #[cfg(test)]
    pub(crate) fn detached() -> Self {
        Self::new(None)
    }
    pub fn bind(window: &HomewardWindow) -> Self {
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
    fn publish(&self, state: &State) {
        let value = projection::Projection::new(state);
        if let Publication::Schedule(ticket) =
            self.0.mailbox.publish(self.0.mailbox.generation(), value)
        {
            let Some(window) = &self.0.window else {
                return;
            };
            let _ = window.upgrade_in_event_loop(move |window| {
                ticket.deliver(|_, projection| projection::render(&window, projection));
            });
        }
    }
    fn update(&self, change: impl FnOnce(&mut State) -> Result<(), Refusal>) {
        let mut state = self.0.state.lock().expect("list state mutex");
        if let Err(refusal) = change(&mut state) {
            state.notice = match refusal {
                Refusal::Stale => "Le contexte a changé. Relisez la liste avant de modifier.",
                Refusal::Busy => "Une modification attend déjà la réponse du serveur.",
                Refusal::Invalid => "Cette modification n’est pas valide.",
                Refusal::Capacity => "La limite de brouillons ou de présentation est atteinte.",
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
                state.notice = "Modification non envoyée : contexte indisponible, brouillon invalide ou écriture en cours.";
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
                        .is_some_and(|candidate| candidate.uid.value == row.uid.value)
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
                .and_then(|key| {
                    rows.iter()
                        .position(|row| row.uid.value == key.uid().as_str())
                })
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
        transport: ListTransport,
        cancel: CancellationToken,
    ) {
        let (sender, mut commands) = mpsc::channel::<Prepared>(1);
        *self.0.commands.lock().expect("list commands mutex") = Some(sender);
        let mut phases = session.subscribe();
        let mut context = None;
        let mut tasks = JoinSet::new();
        let mut attempt = cancel.child_token();
        loop {
            if self.0.retry.swap(false, Ordering::SeqCst) {
                let _ = session.allow_new_view_attempt().await;
            }
            let (active, next, selected) = {
                let mut state = self.0.state.lock().expect("list state mutex");
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
                    tasks.spawn(async move {
                        owner
                            .read(credential, api, Scope::Catalog, next, stop)
                            .await;
                    });
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
                        let result = prepared.mutation.submit(&api,&credential,&stop,Arc::new(move || admission.0.state.lock().expect("list state mutex").admits(context,id))).await;
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
    }
    async fn read(
        &self,
        session: SessionCoordinator,
        transport: ListTransport,
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
                            Scope::Items(_) => state.item_phase = Freshness::Syncing,
                        };
                        Ok(())
                    }
                    ListReadEvent::Published(snapshot) => match &scope {
                        Scope::Catalog => state.publish_catalog(snapshot),
                        Scope::Items(_) => state.publish_items(snapshot),
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
                    let recovery = recovery::transport(&failure);
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
                        state.read_notice = "Connexion interrompue. Cache daté en lecture seule ; aucune écriture en attente ne sera rejouée.";
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
                        state.read_notice = "Les listes doivent être relues. Les anciennes données privées ont été retirées.";
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
