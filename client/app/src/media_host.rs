//! One owned media reader, coalesced presentation and the shared action client.
//! No UI callback runs network work or retains a replayable physical intention.
mod read;
mod render;
mod state;
#[cfg(test)]
mod tests;
use crate::{
    HomewardWindow, NavigationStep, ProMediaOperation, ProMediaOptionKind, ProValueEvent,
    ProValueIntent, RangeThumb, StepDirection,
    media::{Data, Field, Motion, Refusal},
};
use home_client_actions::{ActionClient, ActionSnapshot, AdmissionCheck};
use home_client_cache::{ProjectionGeneration, ProjectionMailbox, Publication};
use home_client_model::EntityId;
use home_client_session::{Phase, SessionCoordinator};
use home_client_transport::MediaTransport;
use home_wire_contract::home::media::v1 as wire;
use slint::ComponentHandle;
use state::{ChoiceKind, ChoiceToken, Pending, Picture, State, Status};
use std::{
    sync::{Arc, Mutex},
    time::{Duration, SystemTime},
};
use tokio::{
    sync::{Notify, watch},
    task::JoinSet,
};
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub(crate) struct Media(Arc<Inner>);
struct Inner {
    state: Mutex<State>,
    changed: Notify,
    mailbox: ProjectionMailbox<render::Projection>,
    window: Option<slint::Weak<HomewardWindow>>,
    session: Mutex<Option<SessionCoordinator>>,
}
impl Media {
    fn new(window: Option<slint::Weak<HomewardWindow>>) -> Self {
        Self(Arc::new(Inner {
            state: Mutex::new(State::default()),
            changed: Notify::new(),
            mailbox: ProjectionMailbox::new(),
            window,
            session: Mutex::new(None),
        }))
    }
    #[cfg(test)]
    pub fn detached() -> Self {
        Self::new(None)
    }
    pub fn bind(window: &HomewardWindow) -> Self {
        let owner = Self::new(Some(window.as_weak()));
        let this = owner.clone();
        window.on_media_operation(move |t, g, op| this.operation(t, g, op));
        let this = owner.clone();
        window.on_media_option(move |t, g, k, id| this.option(t, g, k, id));
        let this = owner.clone();
        window.on_media_edit(move |t, g, f, intent| this.edit(t, g, f, intent));
        let this = owner.clone();
        window.on_media_reload(move |g| this.reload(g));
        let this = owner.clone();
        window.on_media_expire_artwork(move |g| this.expire(g));
        let this = owner.clone();
        window.on_media_set_interactive(move |active| this.interactive(active));
        let this = owner.clone();
        let weak = window.as_weak();
        window.on_media_private_cleared(move || {
            if let Some(window) = weak.upgrade() {
                render::clear(&window);
            }
            this.clear();
        });
        render::clear(window);
        owner
    }
    fn publish(&self, state: &State) {
        let guard = state.source.and_then(|g| {
            self.0
                .session
                .lock()
                .expect("media session")
                .clone()
                .map(|session| (session, g))
        });
        let projection = render::Projection::new(state, guard);
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
    }
    pub fn clear(&self) {
        let mut state = self.0.state.lock().expect("media state");
        state.clear_private();
        state.entity = None;
        state.view = None;
        let _ = self.0.mailbox.invalidate();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    pub fn choose(&self, entity: Option<EntityId>, view: ProjectionGeneration) {
        let mut state = self.0.state.lock().expect("media state");
        if state.entity == entity && state.view == Some(view) {
            return;
        }
        state.clear_private();
        state.entity = entity;
        state.view = Some(view);
        state.read_pending = state.entity.is_some();
        let _ = self.0.mailbox.invalidate();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    pub fn foreground(&self, active: bool) {
        let mut state = self.0.state.lock().expect("media state");
        if state.foreground == active {
            return;
        }
        state.foreground = active;
        state.clear_private();
        state.read_pending = active && state.entity.is_some();
        let _ = self.0.mailbox.invalidate();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn interactive(&self, active: bool) {
        let mut state = self.0.state.lock().expect("media state");
        if state.interactive == active {
            return;
        }
        state.interactive = active;
        state.clear_private();
        state.read_pending = active && state.entity.is_some();
        let _ = self.0.mailbox.invalidate();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn reload(&self, generation: i32) {
        let mut state = self.0.state.lock().expect("media state");
        if generation != state.generation || !state.active || !state.interactive {
            return;
        }
        state.read_cancel.cancel();
        state.read_cancel = CancellationToken::new();
        state.read_pending = true;
        state.picture = Picture::Loading;
        state.status = Status::Loading;
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn expire(&self, generation: i32) {
        let mut state = self.0.state.lock().expect("media state");
        if generation != state.generation {
            return;
        }
        if let Picture::Ready(picture) = &state.picture
            && picture.expires() <= SystemTime::now()
        {
            state.picture = Picture::Expired;
            self.publish(&state);
        }
    }
    fn operation(&self, token: i32, generation: i32, op: ProMediaOperation) {
        let op = match op {
            ProMediaOperation::Play => wire::Operation::Play,
            ProMediaOperation::Pause => wire::Operation::Pause,
            ProMediaOperation::Stop => wire::Operation::Stop,
            ProMediaOperation::Previous => wire::Operation::Previous,
            ProMediaOperation::Next => wire::Operation::Next,
        };
        self.prepare(token, generation, |data| {
            data.request(wire::media_action::Command::Operation(op.into()))
        });
    }
    fn option(&self, token: i32, generation: i32, kind: ProMediaOptionKind, option: i32) {
        let kind = match kind {
            ProMediaOptionKind::Source => ChoiceKind::Source,
            ProMediaOptionKind::Group => ChoiceKind::Group,
        };
        let mut state = self.0.state.lock().expect("media state");
        if !state.admits(token, generation) {
            return;
        }
        let Some(choice) = state
            .choices
            .get(&ChoiceToken(option))
            .filter(|c| c.kind == kind)
        else {
            return;
        };
        let command = match choice.kind {
            ChoiceKind::Source => wire::media_action::Command::SourceId(choice.id.0.clone()),
            ChoiceKind::Group => wire::media_action::Command::GroupId(choice.id.0.clone()),
        };
        let result = state
            .data
            .as_ref()
            .ok_or(Refusal::Contract)
            .and_then(|data| data.request(command));
        Self::enqueue(&mut state, result);
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn prepare(
        &self,
        token: i32,
        generation: i32,
        build: impl FnOnce(
            &Data,
        )
            -> Result<home_wire_contract::home::actions::v1::CommandRequest, Refusal>,
    ) {
        let mut state = self.0.state.lock().expect("media state");
        if !state.admits(token, generation) {
            return;
        }
        let result = state
            .data
            .as_ref()
            .ok_or(Refusal::Contract)
            .and_then(|data| build(data));
        Self::enqueue(&mut state, result);
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn enqueue(
        state: &mut State,
        result: Result<home_wire_contract::home::actions::v1::CommandRequest, Refusal>,
    ) {
        match result {
            Ok(request) => {
                state.notice.clear();
                state.pending = Some(Pending {
                    generation: state.generation,
                    request,
                });
            }
            Err(error) => state.notice = render::refusal(error).into(),
        }
    }
    fn edit(&self, token: i32, generation: i32, field: i32, intent: ProValueIntent) {
        let Some(field) = Field::from_token(field) else {
            return;
        };
        match intent.thumb {
            RangeThumb::Lower => {}
            RangeThumb::Upper => return,
        }
        let mut state = self.0.state.lock().expect("media state");
        if !state.admits(token, generation) {
            return;
        }
        let Some(data) = state.data.clone() else {
            return;
        };
        match intent.event {
            ProValueEvent::Draft => {
                if intent.lower.len() > 64 {
                    return;
                }
                let result = data.parse(field, &intent.lower);
                let draft = state.draft_mut(field);
                draft.editing = true;
                draft.text = intent.lower.to_string();
                draft.value = result.ok();
                draft.error.clear();
            }
            ProValueEvent::Submit => {
                let value = data.parse(field, &intent.lower);
                match value {
                    Ok(value) => {
                        let draft = state.draft_mut(field);
                        draft.editing = false;
                        draft.value = Some(value);
                        draft.text = value.text();
                        draft.error.clear();
                        Self::enqueue(&mut state, data.quantity_request(value));
                    }
                    Err(error) => state.draft_mut(field).error = render::refusal(error).into(),
                }
            }
            ProValueEvent::Start => {
                let draft = state.draft_mut(field);
                draft.gesture = true;
                draft.editing = true;
                if draft.value.is_none() {
                    draft.value = data
                        .observed(field)
                        .or_else(|| data.at_position(field, 0.).ok());
                }
                draft.error.clear();
            }
            ProValueEvent::Position => {
                if !state.draft(field).gesture {
                    return;
                }
                match data.at_position(field, intent.position) {
                    Ok(value) => {
                        let draft = state.draft_mut(field);
                        draft.value = Some(value);
                        draft.text = value.text();
                    }
                    Err(error) => state.draft_mut(field).error = render::refusal(error).into(),
                }
            }
            ProValueEvent::Navigate => {
                if !state.draft(field).gesture {
                    return;
                }
                let motion = match intent.navigation {
                    NavigationStep::Previous => Motion::Previous,
                    NavigationStep::Next => Motion::Next,
                    NavigationStep::First => Motion::First,
                    NavigationStep::Last => Motion::Last,
                    NavigationStep::PagePrevious => Motion::PagePrevious,
                    NavigationStep::PageNext => Motion::PageNext,
                };
                if let Some(current) = state.draft(field).value
                    && let Ok(value) = data.moved(current, motion)
                {
                    let draft = state.draft_mut(field);
                    draft.value = Some(value);
                    draft.text = value.text();
                }
            }
            ProValueEvent::Step => {
                let current = state.draft(field).value.or_else(|| data.observed(field));
                let result = match current {
                    Some(value) => data.moved(
                        value,
                        match intent.direction {
                            StepDirection::Decrement => Motion::Previous,
                            StepDirection::Increment => Motion::Next,
                        },
                    ),
                    None => data.at_position(field, 0.),
                };
                match result {
                    Ok(value) => {
                        let draft = state.draft_mut(field);
                        draft.value = Some(value);
                        draft.text = value.text();
                        draft.editing = false;
                        Self::enqueue(&mut state, data.quantity_request(value));
                    }
                    Err(error) => state.draft_mut(field).error = render::refusal(error).into(),
                }
            }
            ProValueEvent::Commit => {
                if !state.draft(field).gesture {
                    return;
                }
                let value = state.draft(field).value;
                let draft = state.draft_mut(field);
                draft.gesture = false;
                draft.editing = false;
                if let Some(value) = value {
                    Self::enqueue(&mut state, data.quantity_request(value));
                }
            }
            ProValueEvent::Cancel => {
                let draft = state.draft_mut(field);
                draft.gesture = false;
                draft.editing = false;
                draft.value = data.observed(field);
                draft.text = draft
                    .value
                    .map(crate::media::Quantity::text)
                    .unwrap_or_default();
                draft.error.clear();
            }
            ProValueEvent::SelectOpen
            | ProValueEvent::SelectNavigate
            | ProValueEvent::SelectActivate
            | ProValueEvent::SelectDismiss
            | ProValueEvent::SelectQuery
            | ProValueEvent::SelectTypeahead => return,
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn admission(&self, generation: i32) -> AdmissionCheck {
        let owner = Arc::downgrade(&self.0);
        Arc::new(move || {
            let Some(owner) = owner.upgrade() else {
                return false;
            };
            let state = owner.state.lock().expect("media admission");
            state.generation == generation
                && state.authorized()
                && state.source.is_some_and(|g| {
                    owner
                        .session
                        .lock()
                        .expect("media session")
                        .as_ref()
                        .is_some_and(|s| s.is_current(g))
                })
        })
    }
    pub async fn run(
        &self,
        session: SessionCoordinator,
        transport: MediaTransport,
        actions: ActionClient,
        cancel: CancellationToken,
    ) {
        *self.0.session.lock().expect("media session") = Some(session.clone());
        let mut phases = session.subscribe();
        let mut source_generation = phases.borrow().generation;
        let mut reads = JoinSet::new();
        let mut updates: Option<watch::Receiver<Arc<ActionSnapshot>>> = None;
        let mut clock = tokio::time::interval_at(
            tokio::time::Instant::now() + Duration::from_secs(2),
            Duration::from_secs(2),
        );
        clock.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            let (pending, read, poll) = {
                let mut state = self.0.state.lock().expect("media state");
                let view = *phases.borrow();
                if view.generation != source_generation {
                    source_generation = view.generation;
                    state.clear_private();
                    state.read_pending = state.entity.is_some();
                    let _ = self.0.mailbox.invalidate();
                }
                let active = matches!(view.phase, Phase::Ready | Phase::Refreshing)
                    && state.foreground
                    && state.interactive
                    && state.generation > 0;
                if !active && state.active {
                    state.clear_private();
                    let _ = self.0.mailbox.invalidate();
                }
                state.active = active;
                if state.action.is_none() {
                    updates = None;
                }
                let pending = if active { state.pending.take() } else { None };
                let read = if active && state.read_pending && reads.is_empty() {
                    state.entity.clone().map(|entity| {
                        state.read_pending = false;
                        if state.data.is_none() {
                            state.status = Status::Loading;
                        }
                        let previous = state.artwork_reference().map(|(r, p)| (r.clone(), p));
                        (
                            entity,
                            state.generation,
                            state.read_cancel.clone(),
                            previous,
                        )
                    })
                } else {
                    None
                };
                self.publish(&state);
                (
                    pending,
                    read,
                    active && state.entity.is_some() && state.status != Status::Unsupported,
                )
            };
            if let Some(pending) = pending {
                // ActionClient checks admission synchronously, then checks the
                // same guard after awaiting session access. Never hold State here.
                let result = actions.submit(pending.request, self.admission(pending.generation));
                let mut state = self.0.state.lock().expect("media state");
                match result {
                    Ok(action) => {
                        if state.generation == pending.generation && state.authorized() {
                            updates = Some(action.subscribe());
                            state.progress = Some(action.snapshot());
                            state.action = Some(action);
                        } else {
                            action.stop_tracking();
                        }
                    }
                    Err(_) => {
                        if state.generation == pending.generation {
                            state.notice =
                                "Commande non envoyée : contexte changé ou contrôleur occupé."
                                    .into();
                        }
                    }
                }
                self.publish(&state);
            }
            if let Some((entity, generation, stop, previous)) = read {
                clock.reset();
                let (owner, session, transport) =
                    (self.clone(), session.clone(), transport.clone());
                reads.spawn(async move {
                    owner
                        .read(session, transport, entity, generation, stop, previous)
                        .await;
                });
            }
            tokio::select! {biased;
                ()=cancel.cancelled()=>break,
                changed=phases.changed()=>{if changed.is_err(){break}},
                ()=self.0.changed.notified()=>{},
                _=clock.tick(),if poll=>{self.0.state.lock().expect("media state").read_pending=true;},
                _=reads.join_next(),if !reads.is_empty()=>{},
                changed=async{match updates.as_mut(){Some(receiver)=>receiver.changed().await.is_ok(),None=>std::future::pending().await}}=>{
                    if changed{let snapshot=updates.as_ref().expect("media action receiver").borrow().clone();let mut state=self.0.state.lock().expect("media state");if state.action.as_ref().is_some_and(|action|action.id()==&snapshot.id){state.progress=Some(snapshot);self.publish(&state);}}else{updates=None;}
                }
            }
        }
        self.clear();
        while reads.join_next().await.is_some() {}
        *self.0.session.lock().expect("media session") = None;
    }
}
