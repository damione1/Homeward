//! One session-owned settings reader. Callbacks only enqueue bounded intentions;
//! auth, admission and joins stay on the runtime, outside the Slint event loop.
mod configuration;
mod forms;
mod read;
mod receipt;
mod render;
#[cfg(test)]
mod tls_probe;
use crate::{
    HomewardWindow, NavigationStep, ProAdminAction,
    administration::{Identity, Pane, PrivateState, Section, Stage},
};
use home_client_cache::{ProjectionMailbox, Publication};
use home_client_session::{Generation, Phase, SessionCoordinator};
use home_client_transport::Connection;
use read::{Loaded, PageToken, Payload, ReadError, Selection};
use slint::ComponentHandle;
use std::sync::{Arc, Mutex};
use tokio::{sync::Notify, task::JoinSet};
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub(crate) struct Administration(Arc<Inner>);
struct Inner {
    state: Mutex<State>,
    changed: Notify,
    window: Option<slint::Weak<HomewardWindow>>,
    mailbox: ProjectionMailbox<render::Projection>,
    source: Mutex<Option<(SessionCoordinator, Generation)>>,
    receipts: receipt::Receipts,
}
struct State {
    configuration: configuration::Workflow,
    private: PrivateState<Payload, forms::Draft>,
    pending: Option<forms::Operation>,
    section: Section,
    generation: i32,
    visible: bool,
    foreground: bool,
    active: bool,
    requested: bool,
    cancel: CancellationToken,
    selection: Option<Selection>,
    selected: Option<render::Target>,
    rows: Vec<render::Row>,
    page: Option<PageToken>,
    next: Option<PageToken>,
    stage: Stage,
    probing: bool,
}
impl Default for State {
    fn default() -> Self {
        Self {
            configuration: configuration::Workflow::default(),
            private: PrivateState::default(),
            pending: None,
            section: Section::Account,
            generation: 1,
            visible: false,
            foreground: true,
            active: false,
            requested: false,
            cancel: CancellationToken::new(),
            selection: None,
            selected: None,
            rows: Vec::new(),
            page: None,
            next: None,
            stage: Stage::Loading,
            probing: false,
        }
    }
}
impl State {
    fn bump(&mut self) -> bool {
        let Some(next) = self.generation.checked_add(1) else {
            self.configuration.retire();
            self.stage = Stage::Invalid;
            self.requested = false;
            return false;
        };
        self.generation = next;
        true
    }
    fn request(&mut self) {
        self.configuration.retire();
        self.private.dismiss_draft();
        self.private.dismiss_secret();
        self.pending = None;
        self.cancel.cancel();
        self.cancel = CancellationToken::new();
        self.selected = None;
        self.next = None;
        self.rows.clear();
        self.stage = Stage::Loading;
        self.requested = self.bump();
        self.probing = true;
    }
    fn clear(&mut self) {
        self.configuration.retire();
        self.pending = None;
        self.cancel.cancel();
        let _ = self.private.retire();
        self.selection = None;
        self.selected = None;
        self.page = None;
        self.next = None;
        self.rows.clear();
        self.stage = Stage::Loading;
        self.active = false;
        self.requested = false;
        self.probing = false;
        self.section = Section::Account;
        self.bump();
    }
}
impl Administration {
    fn current(&self, state: &State, generation: i32) -> bool {
        state.active
            && state.visible
            && state.foreground
            && state.generation == generation
            && self
                .0
                .source
                .lock()
                .expect("admin source")
                .as_ref()
                .is_some_and(|(session, generation)| {
                    let phases = session.subscribe();
                    let phase = *phases.borrow();
                    phase.generation == *generation
                        && matches!(phase.phase, Phase::Ready | Phase::Refreshing)
                        && session.is_current(*generation)
                })
    }
    fn new(window: Option<slint::Weak<HomewardWindow>>) -> Self {
        let receipts = receipt::Receipts::new(window.clone());
        Self(Arc::new(Inner {
            state: Mutex::new(State::default()),
            changed: Notify::new(),
            window,
            mailbox: ProjectionMailbox::new(),
            source: Mutex::new(None),
            receipts,
        }))
    }
    #[cfg(test)]
    pub fn detached() -> Self {
        Self::new(None)
    }
    pub fn bind(window: &HomewardWindow) -> Self {
        let this = Self::new(Some(window.as_weak()));
        this.0.receipts.bind(window);
        let owner = this.clone();
        window.on_admin_visible(move |visible| owner.visible(visible));
        let owner = this.clone();
        window.on_admin_choose_section(move |token, generation| owner.section(token, generation));
        let owner = this.clone();
        window.on_admin_select(move |token, generation| owner.select(token, generation));
        let owner = this.clone();
        window.on_admin_action(move |token, generation, action| {
            owner.action(token, generation, action)
        });
        let owner = this.clone();
        window.on_admin_navigate(move |step| owner.navigate(step));
        let owner = this.clone();
        window.on_admin_edit(move |token, generation, field, value| {
            owner.edit(token, generation, field, value.to_string())
        });
        let owner = this.clone();
        window.on_admin_choose(move |token, generation, field, option| {
            owner.choose(token, generation, field, option)
        });
        let owner = this.clone();
        window.on_admin_editor_action(move |token, generation, action| {
            owner.editor_action(token, generation, action)
        });
        render::clear(window);
        this
    }
    fn publish(&self, state: &State) {
        let source = self.0.source.lock().expect("admin source").clone();
        let projection = render::Projection::new(state, source);
        if let Publication::Schedule(ticket) = self
            .0
            .mailbox
            .publish(self.0.mailbox.generation(), projection)
            && let Some(window) = &self.0.window
        {
            let _ = window.upgrade_in_event_loop(move |window| {
                ticket.deliver(|_, value| render::render(&window, value));
            });
        }
        self.0.changed.notify_waiters();
    }
    fn retired(&self, state: &State) {
        let _ = self.0.mailbox.invalidate();
        self.publish(state);
    }
    pub fn clear(&self) {
        self.0.receipts.clear();
        let mut state = self.0.state.lock().expect("admin state");
        state.clear();
        *self.0.source.lock().expect("admin source") = None;
        self.retired(&state);
    }
    pub fn foreground(&self, foreground: bool) {
        if !foreground {
            self.0.receipts.clear();
        }
        let mut state = self.0.state.lock().expect("admin state");
        if state.foreground == foreground {
            return;
        }
        state.foreground = foreground;
        state.clear();
        self.retired(&state);
    }
    fn visible(&self, visible: bool) {
        let mut state = self.0.state.lock().expect("admin state");
        if state.visible == visible {
            return;
        }
        state.visible = visible;
        state.clear();
        self.retired(&state);
    }
    fn section(&self, token: i32, generation: i32) {
        let mut state = self.0.state.lock().expect("admin state");
        if !self.current(&state, generation) {
            return;
        }
        let Some(section) = usize::try_from(token)
            .ok()
            .and_then(|token| token.checked_sub(1))
            .and_then(|index| Section::ALL.get(index))
            .copied()
        else {
            return;
        };
        if section.administrative()
            && !state
                .private
                .authority()
                .is_some_and(|authority| authority.administrator())
        {
            return;
        }
        self.0.receipts.clear();
        state.section = section;
        state.selection = None;
        state.page = None;
        state.request();
        self.retired(&state);
    }
    fn select(&self, token: i32, generation: i32) {
        let mut state = self.0.state.lock().expect("admin state");
        if !self.current(&state, generation)
            || state.stage != Stage::Ready
            || state.private.draft().is_some()
            || state.configuration.opened()
        {
            return;
        }
        let Some(target) = state
            .rows
            .iter()
            .find(|row| row.token == token)
            .map(|row| row.target.clone())
        else {
            return;
        };
        match &target {
            render::Target::Account(id) => {
                if state.section == Section::Policies {
                    state.selection = Some(Selection::Account(id.clone()));
                    state.request();
                } else {
                    state.selected = Some(target);
                }
            }
            render::Target::Integration(id) => {
                if state.section != Section::Integrations {
                    return;
                }
                state.selection = Some(Selection::Integration(id.clone()));
                state.request();
            }
            render::Target::Configuration(selection) => {
                if state.section != Section::Configuration {
                    return;
                }
                state.selection = Some(Selection::Configuration(selection.clone()));
                state.page = None;
                state.request();
            }
            render::Target::Session(_)
            | render::Target::Factor(_)
            | render::Target::Key(_)
            | render::Target::Invitation(_) => state.selected = Some(target),
            render::Target::Fact(_) => {
                if state.section != Section::Configuration {
                    return;
                }
                state.selected = Some(target);
            }
        }
        self.publish(&state);
    }
    fn action(&self, _token: i32, generation: i32, action: ProAdminAction) {
        let mut state = self.0.state.lock().expect("admin state");
        if !self.current(&state, generation) {
            return;
        }
        if state.section == Section::Configuration {
            if state.configuration.opened() {
                return;
            }
            if matches!(state.stage, Stage::Ready | Stage::Empty)
                && state.configuration.open(action)
            {
                state.bump();
                self.retired(&state);
                return;
            }
        }
        if let Some(command) = forms::available(&state)
            .into_iter()
            .find(|command| command.action() == action)
        {
            let context = state.private.context();
            let section = command.section();
            if state
                .private
                .edit(context, section, forms::Draft::new(command))
                .is_ok()
            {
                state.bump();
                self.retired(&state);
            }
            return;
        }
        match action {
            ProAdminAction::Refresh => {
                state.page = None;
                state.request();
            }
            ProAdminAction::More => {
                let Some(next) = state.next.clone() else {
                    return;
                };
                state.page = Some(next);
                state.request();
            }
            ProAdminAction::Cancel => {
                state.selected = None;
                if state.selection.take().is_some() {
                    state.page = None;
                    state.request();
                }
            }
            ProAdminAction::Create
            | ProAdminAction::Save
            | ProAdminAction::Remove
            | ProAdminAction::Enable
            | ProAdminAction::Disable
            | ProAdminAction::Revoke
            | ProAdminAction::CloseSecret
            | ProAdminAction::Preview
            | ProAdminAction::Revalidate
            | ProAdminAction::Simulate
            | ProAdminAction::Apply
            | ProAdminAction::Restore
            | ProAdminAction::Rollback
            | ProAdminAction::AddFile
            | ProAdminAction::RemoveFile
            | ProAdminAction::NextFile
            | ProAdminAction::Resolve => return,
        }
        self.retired(&state);
    }
    fn edit(&self, token: i32, generation: i32, field: i32, value: String) {
        let mut state = self.0.state.lock().expect("admin state");
        if token != 1 || !self.current(&state, generation) {
            return;
        }
        if state.section == Section::Configuration && state.configuration.opened() {
            state.configuration.edit(field, value);
            self.publish(&state);
            return;
        }
        if let Some(draft) = state.private.draft_mut() {
            draft.edit(field, value);
            self.publish(&state);
        }
    }
    fn choose(&self, token: i32, generation: i32, field: i32, option: i32) {
        let mut state = self.0.state.lock().expect("admin state");
        if token != 1 || !self.current(&state, generation) {
            return;
        }
        if state.section == Section::Configuration && state.configuration.opened() {
            state.configuration.choose(field, option);
            state.bump();
            self.retired(&state);
            return;
        }
        if let Some(draft) = state.private.draft_mut() {
            draft.choose(field, option);
            self.publish(&state);
        }
    }
    fn editor_action(&self, token: i32, generation: i32, action: ProAdminAction) {
        let mut state = self.0.state.lock().expect("admin state");
        if token != 1 || !self.current(&state, generation) {
            return;
        }
        if state.section == Section::Configuration && state.configuration.editor_action(action) {
            state.bump();
            self.retired(&state);
            return;
        }
        match action {
            ProAdminAction::Save => {
                if let Some(draft) = state.private.draft_mut()
                    && let Some(operation) = draft.submit()
                {
                    state.pending = Some(operation);
                    state.bump();
                }
            }
            ProAdminAction::Cancel => {
                self.0.receipts.clear();
                if state.private.draft().is_none() {
                    state.selection = None;
                }
                state.request();
            }
            ProAdminAction::CloseSecret => state.request(),
            ProAdminAction::Refresh
            | ProAdminAction::Create
            | ProAdminAction::Remove
            | ProAdminAction::More
            | ProAdminAction::Enable
            | ProAdminAction::Disable
            | ProAdminAction::Revoke
            | ProAdminAction::Preview
            | ProAdminAction::Revalidate
            | ProAdminAction::Simulate
            | ProAdminAction::Apply
            | ProAdminAction::Restore
            | ProAdminAction::Rollback
            | ProAdminAction::AddFile
            | ProAdminAction::RemoveFile
            | ProAdminAction::NextFile
            | ProAdminAction::Resolve => return,
        }
        self.retired(&state);
    }
    fn written(&self, generation: i32, result: forms::Completion) {
        let mut state = self.0.state.lock().expect("admin state");
        if !self.current(&state, generation) || state.cancel.is_cancelled() {
            return;
        }
        if result.state == crate::administration::MutationState::Published {
            if let Some((kind, value)) = result.secret {
                let context = state.private.context();
                if state.private.show_secret(context, kind, value).is_ok() {
                    state.private.dismiss_draft();
                    state.bump();
                } else if let Some(draft) = state.private.draft_mut() {
                    draft.complete(crate::administration::MutationState::Uncertain);
                }
            } else {
                if state.section == Section::Integrations {
                    state.selection = None;
                }
                state.request();
            }
        } else if let Some(draft) = state.private.draft_mut() {
            draft.complete(result.state);
        }
        self.retired(&state);
    }
    fn navigate(&self, step: NavigationStep) {
        let mut state = self.0.state.lock().expect("admin state");
        if !self.current(&state, state.generation)
            || state.rows.is_empty()
            || state.private.draft().is_some()
            || state.configuration.opened()
        {
            return;
        }
        let current = state
            .selected
            .as_ref()
            .and_then(|target| state.rows.iter().position(|row| &row.target == target))
            .unwrap_or(0);
        let last = state.rows.len() - 1;
        let index = match step {
            NavigationStep::First => 0,
            NavigationStep::Last => last,
            NavigationStep::Previous => current.saturating_sub(1),
            NavigationStep::Next => (current + 1).min(last),
            NavigationStep::PagePrevious => current.saturating_sub(8),
            NavigationStep::PageNext => (current + 8).min(last),
        };
        state.selected = Some(state.rows[index].target.clone());
        self.publish(&state);
    }
    fn accept(&self, generation: i32, loaded: Result<Loaded, ReadError>) {
        let mut state = self.0.state.lock().expect("admin state");
        if !self.current(&state, generation) || state.cancel.is_cancelled() {
            return;
        }
        match loaded {
            Ok(loaded) => {
                let source = self.0.source.lock().expect("admin source").clone();
                if !source.is_some_and(|(session, current)| {
                    current == loaded.session_generation && session.is_current(current)
                }) {
                    return;
                }
                let identity = loaded.authority.identity.clone();
                let Ok(context) = state.private.install(loaded.authority) else {
                    state.clear();
                    state.stage = Stage::Invalid;
                    self.retired(&state);
                    return;
                };
                state.section = loaded.section;
                match loaded.payload {
                    Ok(payload) => {
                        if let Payload::Configuration(value) = &payload {
                            state.configuration.observe(value.root.clone());
                        }
                        let (rows, next) = render::rows(&payload, &identity);
                        state.rows = rows;
                        state.next = next;
                        state.stage = if state.rows.is_empty() {
                            Stage::Empty
                        } else {
                            Stage::Ready
                        };
                        let section = state.section;
                        let stage = state.stage.clone();
                        let _ = state.private.publish(
                            context,
                            section,
                            Pane {
                                stage,
                                content: Some(payload),
                            },
                        );
                    }
                    Err(error) => {
                        state.rows.clear();
                        state.stage = render::failure(error);
                        let section = state.section;
                        let stage = state.stage.clone();
                        let _ = state.private.publish(
                            context,
                            section,
                            Pane {
                                stage,
                                content: None,
                            },
                        );
                    }
                }
            }
            Err(error) => {
                state.clear();
                state.active = true;
                state.stage = render::failure(error);
            }
        }
        self.publish(&state);
    }
    fn configuration_written(&self, result: configuration::Completion) {
        let mut state = self.0.state.lock().expect("admin state");
        if state.section != Section::Configuration
            || !self.current(&state, state.generation)
            || state.cancel.is_cancelled()
        {
            return;
        }
        if state.configuration.accept(result) {
            state.stage = Stage::Denied;
            state.rows.clear();
            state.selected = None;
            state.next = None;
            let context = state.private.context();
            let _ = state.private.publish(
                context,
                Section::Configuration,
                Pane {
                    stage: Stage::Denied,
                    content: None,
                },
            );
        }
        state.bump();
        self.retired(&state);
    }
    fn configuration_watch_current(
        &self,
        state: &State,
        permit: configuration::WatchPermit,
    ) -> bool {
        state.section == Section::Configuration
            && self.current(state, state.generation)
            && !state.cancel.is_cancelled()
            && state.configuration.watch_admits(permit)
    }
    fn configuration_watch_syncing(&self, permit: configuration::WatchPermit) -> bool {
        let mut state = self.0.state.lock().expect("admin state");
        if !self.configuration_watch_current(&state, permit) {
            return false;
        }
        if !state.configuration.watch_syncing(permit) {
            return false;
        }
        state.bump();
        self.retired(&state);
        true
    }
    fn configuration_watch_ready(&self, permit: configuration::WatchPermit) -> bool {
        let mut state = self.0.state.lock().expect("admin state");
        if !self.configuration_watch_current(&state, permit) {
            return false;
        }
        if !state.configuration.watch_ready(permit) {
            return false;
        }
        state.stage = if state.rows.is_empty() {
            Stage::Empty
        } else {
            Stage::Ready
        };
        state.bump();
        self.retired(&state);
        true
    }
    fn configuration_snapshot(
        &self,
        permit: configuration::WatchPermit,
        loaded: Loaded,
        page: Option<PageToken>,
    ) -> Result<bool, configuration::WatchStop> {
        let mut state = self.0.state.lock().expect("admin state");
        if !self.configuration_watch_current(&state, permit) {
            return Ok(false);
        }
        if !self
            .0
            .source
            .lock()
            .expect("admin source")
            .as_ref()
            .is_some_and(|(session, generation)| {
                *generation == loaded.session_generation && session.is_current(*generation)
            })
        {
            return Ok(false);
        }
        if !loaded.authority.administrator()
            || !state.private.authority().is_some_and(|previous| {
                previous.administrator() && previous.identity == loaded.authority.identity
            })
        {
            return Err(configuration::WatchStop::Denied);
        }
        let payload = loaded
            .payload
            .map_err(|_| configuration::WatchStop::Invalid)?;
        let Payload::Configuration(value) = &payload else {
            return Err(configuration::WatchStop::Invalid);
        };
        match state.configuration.snapshot_admission(&value.root) {
            Ok(true) => (),
            Ok(false) => return Err(configuration::WatchStop::Unavailable),
            Err(_) => return Err(configuration::WatchStop::Invalid),
        }
        state.configuration.observe(value.root.clone());
        let (rows, next) = render::rows(&payload, &loaded.authority.identity);
        if state
            .selected
            .as_ref()
            .is_some_and(|selected| !rows.iter().any(|row| &row.target == selected))
        {
            state.selected = None;
        }
        state.rows = rows;
        state.next = next;
        state.page = page;
        let context = state.private.context();
        let stage = state.stage.clone();
        state
            .private
            .publish(
                context,
                Section::Configuration,
                Pane {
                    stage,
                    content: Some(payload),
                },
            )
            .map_err(|_| configuration::WatchStop::Denied)?;
        state.bump();
        self.retired(&state);
        Ok(true)
    }
    fn configuration_watch_stopped(
        &self,
        permit: configuration::WatchPermit,
        stop: configuration::WatchStop,
    ) {
        let mut state = self.0.state.lock().expect("admin state");
        if !self.configuration_watch_current(&state, permit) {
            return;
        }
        if !state.configuration.watch_stopped(permit) {
            return;
        }
        match stop {
            configuration::WatchStop::Unavailable => state.stage = Stage::Offline,
            configuration::WatchStop::Denied | configuration::WatchStop::Invalid => {
                state.configuration.retire();
                let _ = state.private.retire();
                state.rows.clear();
                state.selected = None;
                state.next = None;
                state.page = None;
                state.stage = match stop {
                    configuration::WatchStop::Denied => Stage::Denied,
                    configuration::WatchStop::Invalid => Stage::Invalid,
                    configuration::WatchStop::Unavailable => Stage::Offline,
                };
            }
        }
        state.bump();
        self.retired(&state);
    }
    fn checked_authority(
        &self,
        generation: i32,
        result: Result<crate::administration::Authority, ReadError>,
    ) {
        let mut state = self.0.state.lock().expect("admin state");
        if !self.current(&state, generation) || state.cancel.is_cancelled() {
            return;
        }
        match result {
            Ok(authority) => {
                let unchanged = state.private.authority().is_some_and(|previous| {
                    previous.identity == authority.identity
                        && previous.administrator() == authority.administrator()
                });
                if !unchanged {
                    state.clear();
                    state.active = true;
                    state.request();
                    self.retired(&state);
                }
            }
            Err(error) => {
                state.clear();
                state.active = true;
                state.stage = render::failure(error);
                self.retired(&state);
            }
        }
    }
    pub async fn run(
        &self,
        session: SessionCoordinator,
        connection: Connection,
        cancel: CancellationToken,
    ) {
        let mut phases = session.subscribe();
        let mut generation = phases.borrow().generation;
        let mut jobs = JoinSet::new();
        let mut watches = JoinSet::new();
        let mut busy = false;
        let mut monitor = tokio::time::interval(std::time::Duration::from_secs(5));
        monitor.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        monitor.tick().await;
        let mut probe_requested = false;
        loop {
            self.0.receipts.revalidate();
            let changed = self.0.changed.notified();
            tokio::pin!(changed);
            changed.as_mut().enable();
            if watches.len() < 2 {
                let watch = {
                    let mut state = self.0.state.lock().expect("admin state");
                    if state.section == Section::Configuration
                        && self.current(&state, state.generation)
                    {
                        state
                            .configuration
                            .take_watch()
                            .map(|job| (job, state.selection.clone(), state.page.clone()))
                    } else {
                        None
                    }
                };
                if let Some((job, selection, page)) = watch {
                    let owner = self.clone();
                    let session = session.clone();
                    let connection = connection.clone();
                    watches.spawn(configuration::watch(
                        owner, session, connection, generation, selection, page, job,
                    ));
                }
            }
            let config_job = {
                let mut state = self.0.state.lock().expect("admin state");
                if !busy
                    && state.section == Section::Configuration
                    && self.current(&state, state.generation)
                {
                    state
                        .configuration
                        .take_pending()
                        .map(|job| (job, state.cancel.clone()))
                } else {
                    None
                }
            };
            if let Some((job, token)) = config_job {
                busy = true;
                let owner = self.clone();
                let session = session.clone();
                let connection = connection.clone();
                jobs.spawn(async move {
                    let result =
                        configuration::execute(&session, &connection, generation, job, &token)
                            .await;
                    owner.configuration_written(result);
                });
            }
            let operation = {
                let mut state = self.0.state.lock().expect("admin state");
                if !busy && self.current(&state, state.generation) {
                    state
                        .pending
                        .take()
                        .map(|operation| (state.generation, operation, state.cancel.clone()))
                } else {
                    None
                }
            };
            if let Some((ui_generation, operation, token)) = operation {
                busy = true;
                let owner = self.clone();
                let session = session.clone();
                let connection = connection.clone();
                let permit = owner.0.receipts.permit();
                jobs.spawn(async move {
                    let mut result = forms::execute(
                        &session,
                        &connection,
                        generation,
                        operation,
                        &token,
                        &permit,
                    )
                    .await;
                    if let Some(generation) = result.recovery_generation {
                        if let Some((crate::administration::SecretKind::RecoveryCodes, value)) =
                            result.secret.take()
                        {
                            owner
                                .0
                                .receipts
                                .deliver(&permit, session, generation, value);
                        }
                    } else {
                        owner.written(ui_generation, result);
                    }
                });
            }
            let next = {
                let mut state = self.0.state.lock().expect("admin state");
                let phase = *phases.borrow();
                if phase.generation != generation {
                    generation = phase.generation;
                    state.clear();
                    self.retired(&state);
                }
                let active = matches!(phase.phase, Phase::Ready | Phase::Refreshing)
                    && state.visible
                    && state.foreground;
                if !active && state.active {
                    state.clear();
                    self.retired(&state);
                }
                if active && !state.active {
                    state.active = true;
                    state.request();
                    *self.0.source.lock().expect("admin source") =
                        Some((session.clone(), generation));
                    self.publish(&state);
                }
                if active && !busy && state.requested {
                    state.requested = false;
                    Some((
                        state.generation,
                        state.section,
                        state.selection.clone(),
                        state.page.clone(),
                        state.cancel.clone(),
                    ))
                } else {
                    None
                }
            };
            if let Some((ui_generation, section, selection, page, token)) = next {
                busy = true;
                probe_requested = false;
                let owner = self.clone();
                let session = session.clone();
                let connection = connection.clone();
                jobs.spawn(async move {
                    let result = read::load(
                        &session,
                        &connection,
                        generation,
                        section,
                        selection,
                        page,
                        &token,
                    )
                    .await;
                    owner.accept(ui_generation, result);
                });
            }
            if !busy && probe_requested {
                probe_requested = false;
                let request = {
                    let state = self.0.state.lock().expect("admin state");
                    if state.active && state.probing {
                        Some((state.generation, state.cancel.clone()))
                    } else {
                        None
                    }
                };
                if let Some((ui_generation, token)) = request {
                    busy = true;
                    let owner = self.clone();
                    let session = session.clone();
                    let connection = connection.clone();
                    jobs.spawn(async move {
                        let result = read::probe(&session, &connection, generation, &token).await;
                        owner.checked_authority(ui_generation, result);
                    });
                }
            }
            tokio::select! {biased;
                ()=cancel.cancelled()=>break,
                phase=phases.changed()=>{if phase.is_err(){break;}},
                ()=&mut changed=>{},
                _=monitor.tick()=>{probe_requested=true;},
                result=watches.join_next(),if !watches.is_empty()=>{if result.is_some_and(|result|result.is_err()){let mut state=self.0.state.lock().expect("admin state");state.clear();state.active=true;state.stage=Stage::Invalid;self.retired(&state);}},
                result=jobs.join_next(),if !jobs.is_empty()=>{busy=false;if result.is_some_and(|result|result.is_err()){let mut state=self.0.state.lock().expect("admin state");state.clear();state.active=true;state.stage=Stage::Invalid;self.retired(&state);}},
            }
        }
        self.clear();
        while jobs.join_next().await.is_some() {}
        while watches.join_next().await.is_some() {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn callbacks_without_the_current_session_source_cannot_schedule_work() {
        let owner = Administration::detached();
        {
            let mut state = owner.0.state.lock().unwrap();
            state.visible = true;
            state.active = true;
            state.stage = Stage::Ready;
        }
        owner.section(5, 1);
        owner.action(0, 1, ProAdminAction::Refresh);
        owner.select(1, 1);
        owner.navigate(NavigationStep::Last);
        let state = owner.0.state.lock().unwrap();
        assert!(!state.requested);
        assert_eq!(state.section, Section::Account);
        assert_eq!(state.generation, 1);
    }
    #[test]
    fn suspension_retires_the_request_and_all_private_selection() {
        let owner = Administration::detached();
        let (token, generation) = {
            let mut state = owner.0.state.lock().unwrap();
            state.visible = true;
            state.active = true;
            state.request();
            (state.cancel.clone(), state.generation)
        };
        owner.foreground(false);
        let state = owner.0.state.lock().unwrap();
        assert!(token.is_cancelled());
        assert!(!state.active);
        assert!(state.generation > generation);
        assert!(state.rows.is_empty());
        assert!(state.private.authority().is_none());
    }
    #[test]
    fn replacing_a_read_cancels_the_previous_capture_without_reusing_generation() {
        let mut state = State::default();
        state.request();
        let token = state.cancel.clone();
        let generation = state.generation;
        state.request();
        assert!(token.is_cancelled());
        assert!(state.generation > generation);
        assert!(!state.cancel.is_cancelled());
    }
}
