//! Exact server-selected cards, command admission and bounded local action
//! history. UI tokens are ephemeral; identities/permissions never come from UI.
pub(crate) mod capability;
mod climate;
mod controls;
pub(crate) mod detail;
mod draft_retention;
mod helper;
mod light;
mod native;
mod number;
pub(crate) mod observation;
mod panel;
pub(crate) mod presentation;
pub(crate) mod progress;
mod recipes;
mod selection;
use crate::{
    ProControlAccess, ProDeviceKind,
    cache_view::bounded,
    dashboard::{Layout, ProjectionRequest},
};
use capability::Control;
use home_client_actions::capabilities::CapabilityHandle;
use home_client_actions::{AdmissionCheck, WriteAccess, write_access};
use home_client_cache::{CachePhase, CachedView};
use home_client_model::api::{CapabilityName, Command, EntityName, EntityViewName};
use home_wire_contract::home::api::v2 as wire;
use presentation::CardRow;
pub(crate) use recipes::{GestureAccess, NativeIntent, NativePrepared, RecipePresentation};
use std::{
    collections::{BTreeMap, BTreeSet, VecDeque},
    sync::{Arc, Mutex},
};

const MAX_CARDS: usize = 2048;
const MAX_ACTION_HISTORY: usize = 64;
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
struct CardToken(i32);
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
struct CardGeneration(i32);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Power {
    On,
    Off,
}
#[derive(Clone)]
pub(crate) enum DeviceIntent {
    Power(Power),
    Brightness(f64),
    ColorTemperature(light::Mireds),
    Climate(climate::ClimateIntent),
    FanSpeed(home_wire_contract::home::api::v2::FanSpeed),
    Helper(helper::HelperIntent),
    Cover(controls::CoverIntent),
    Lock(home_wire_contract::home::api::v2::LockOperation),
    Alarm(home_wire_contract::home::api::v2::AlarmOperation),
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Refusal {
    Stale,
    ReadOnly,
    Unsupported,
    InvalidValue,
    Capacity,
    Unavailable,
}
impl Refusal {
    pub fn message(self) -> &'static str {
        match self {
            Self::Stale => "Command not sent: the view changed. Repeat the gesture.",
            Self::ReadOnly => "Command not sent: this capability is read-only.",
            Self::Unsupported => "Command not sent: this capability is unsupported.",
            Self::InvalidValue => "Command not sent: the value is invalid for this device.",
            Self::Capacity => "Command not sent: an operation is in progress. Try again shortly.",
            Self::Unavailable => "Command not sent: the controller is unavailable.",
        }
    }
}
#[derive(Clone, PartialEq)]
struct Slot {
    source: ControlSource,
    generation: CardGeneration,
    target: CapabilityName,
    capability: wire::Capability,
    access: WriteAccess,
    control: Control,
    helper_revision: Option<helper::HelperRevision>,
}
impl Slot {
    fn same_admission(&self, other: &Self) -> bool {
        self.source == other.source
            && self.target == other.target
            && self.capability.etag == other.capability.etag
            && self.access == other.access
            && self.control == other.control
            && self.helper_revision == other.helper_revision
    }
}
#[derive(Clone, PartialEq, Eq)]
enum ControlSource {
    General,
    Native(crate::dashboard_layout::NodeId),
}
struct CardInput<'a> {
    source: ControlSource,
    title: String,
    entity: &'a wire::EntityViewEntry,
    capability: &'a wire::Capability,
    target: CapabilityName,
    control: Control,
}
impl<'a> CardInput<'a> {
    fn new(
        source: ControlSource,
        title: String,
        entity: &'a wire::EntityViewEntry,
        capability: &'a wire::Capability,
        control: Control,
    ) -> Option<Self> {
        let target = CapabilityName::try_from(capability.name.as_str()).ok()?;
        if target.entity().as_str() != entity.entity.name {
            return None;
        }
        Some(Self {
            source,
            title,
            entity,
            capability,
            target,
            control,
        })
    }
}
#[derive(Clone, PartialEq)]
struct Context {
    source: Option<home_client_session::SourceProof>,
    revision: i32,
    layout: Arc<Layout>,
    catalogue: Option<crate::dashboard_catalogue::CatalogueToken>,
    capture: EntityViewName,
    incarnation: home_client_cache::CaptureIncarnation,
    expires: buffa_types::google::protobuf::Timestamp,
    slots: Vec<Slot>,
    live: bool,
    native_nodes: Vec<(
        crate::dashboard_layout::NodeId,
        crate::dashboard_layout::Kind,
    )>,
}
impl Context {
    fn source_current(&self) -> bool {
        match &self.source {
            Some(source) => source.is_current(),
            // Isolated domain fixtures do not own credentials. Native captures
            // always carry provenance; production absence is fail-closed.
            None => cfg!(test),
        }
    }
    fn live(&self) -> bool {
        self.live && self.source_current()
    }
    fn same_view(&self, other: &Self) -> bool {
        self.source == other.source
            && self.revision == other.revision
            && self.layout == other.layout
            && self.catalogue == other.catalogue
            && self.capture == other.capture
            && self.incarnation == other.incarnation
            && self.expires == other.expires
            && self.live == other.live
            && self.native_nodes == other.native_nodes
    }
}
struct State {
    serial: CardGeneration,
    context: Option<Context>,
    actions: VecDeque<(CapabilityName, CapabilityHandle)>,
    drafts: BTreeMap<(CardGeneration, i32), panel::Draft>,
    draft_basis: Option<draft_retention::Basis>,
    retained_drafts: Option<draft_retention::Saved>,
    draft_retention_serial: u64,
    selections: BTreeMap<CardGeneration, selection::Selection>,
    recipe_bindings: recipes::Bindings,
    native_steps: native::Steps,
}
#[derive(Clone)]
pub(crate) struct DeviceStore(Arc<Mutex<State>>, Option<tokio::runtime::Handle>);
#[derive(Default)]
pub(crate) struct DeviceProjection {
    pub rows: Vec<CardRow>,
    pub ready: bool,
    pub represented: BTreeSet<EntityName>,
    pub entities: Vec<EntityName>,
    pub native_nodes: Vec<crate::dashboard_layout::Node>,
    pub native_devices: BTreeMap<crate::dashboard_layout::NodeId, CardRow>,
    pub recipes: BTreeMap<crate::dashboard_layout::NodeId, RecipePresentation>,
}
pub(crate) struct Prepared {
    pub target: CapabilityName,
    pub request: Command,
    pub guard: AdmissionCheck,
    pub layout: Arc<Layout>,
}
impl Default for DeviceStore {
    fn default() -> Self {
        Self(
            Arc::new(Mutex::new(State {
                serial: CardGeneration(0),
                context: None,
                actions: VecDeque::new(),
                drafts: BTreeMap::new(),
                draft_basis: None,
                retained_drafts: None,
                draft_retention_serial: 0,
                selections: BTreeMap::new(),
                recipe_bindings: recipes::Bindings::default(),
                native_steps: native::Steps::default(),
            })),
            tokio::runtime::Handle::try_current().ok(),
        )
    }
}
fn selected<'a>(
    view: &'a CachedView,
    layout: &Layout,
    request: &ProjectionRequest,
    native_nodes: &[crate::dashboard_layout::Node],
) -> Vec<(String, &'a wire::EntityViewEntry)> {
    if let Some(id) = &request.detail {
        return view
            .get(id)
            .map(|entity| vec![(bounded(&entity.entity.display_name), entity)])
            .unwrap_or_default();
    }
    if layout.native.is_some() {
        let mut seen = BTreeSet::new();
        return native_nodes
            .iter()
            .flat_map(|node| {
                node.entity
                    .iter()
                    .cloned()
                    .chain(node.recipes.iter().flat_map(|recipes| {
                        recipes.interactions.values().filter_map(|interaction| {
                            match &interaction.intention {
                                home_client_model::dashboard::Intention::Command {
                                    capability,
                                    ..
                                } => Some(capability.entity()),
                                home_client_model::dashboard::Intention::None
                                | home_client_model::dashboard::Intention::MoreInfo(_)
                                | home_client_model::dashboard::Intention::Navigate(_) => None,
                            }
                        })
                    }))
                    .chain(node.widget.iter().flat_map(|widget| widget.entities()))
                    .map(move |id| (node, id))
            })
            .filter_map(|(node, id)| {
                if !seen.insert(id.clone()) {
                    return None;
                }
                let entity = view.get(&id)?;
                if !request.query.is_empty()
                    && !entity
                        .entity
                        .display_name
                        .to_lowercase()
                        .contains(&request.query)
                {
                    return None;
                }
                Some((
                    if node.title.is_empty()
                        || (node.widget.is_some() && node.entity.as_ref() != Some(&id))
                    {
                        bounded(&entity.entity.display_name)
                    } else {
                        node.title.clone()
                    },
                    entity,
                ))
            })
            .collect();
    }
    let entries = match &request.room {
        Some(room) => layout
            .room(room)
            .map(|room| {
                room.entities
                    .iter()
                    .filter_map(|id| {
                        view.get(id)
                            .map(|entity| (bounded(&entity.entity.display_name), entity))
                    })
                    .collect()
            })
            .unwrap_or_default(),
        None => layout
            .observations
            .iter()
            .filter_map(|item| {
                view.get(&item.entity)
                    .map(|entity| (item.label.clone(), entity))
            })
            .collect::<Vec<_>>(),
    };
    entries
        .into_iter()
        .filter(|(_, entity)| {
            request.query.is_empty()
                || entity
                    .entity
                    .display_name
                    .to_lowercase()
                    .contains(&request.query)
        })
        .collect()
}
impl DeviceStore {
    pub fn clear_private(&self) {
        let mut state = self.0.lock().expect("device store mutex");
        state.context = None;
        state.recipe_bindings = recipes::Bindings::default();
        state.drafts.clear();
        state.draft_basis = None;
        state.retained_drafts = None;
        state.native_steps = native::Steps::default();
        state.selections.clear();
        state.serial = CardGeneration(state.serial.0.saturating_add(1));
        for (_, handle) in state.actions.drain(..) {
            handle.stop_tracking();
        }
    }
    #[cfg(test)]
    pub fn project(
        &self,
        phase: CachePhase,
        view: Option<&CachedView>,
        request: &ProjectionRequest,
    ) -> DeviceProjection {
        self.project_owner(phase, view, request, None)
    }
    pub fn project_owner(
        &self,
        phase: CachePhase,
        view: Option<&CachedView>,
        request: &ProjectionRequest,
        renewal: Option<&home_client_cache::PresentationSource>,
    ) -> DeviceProjection {
        let mut state = self.0.lock().expect("device store mutex");
        let saved = if state.context.is_some() {
            draft_retention::Saved::capture(&state)
        } else {
            state.retained_drafts.take()
        }
        .filter(|saved| {
            let now = draft_retention::now();
            if phase == CachePhase::Renewing {
                renewal.is_some_and(|renewal| saved.permits_renewal(renewal, request, now))
            } else {
                saved.permits(phase, view, request, now)
            }
        });
        let view = view.filter(|view| {
            view.source_proof()
                .map_or(cfg!(test), |source| source.is_current())
        });
        let layout = request
            .dashboard
            .layout()
            .filter(|layout| view.is_some_and(|view| layout.compatible(view)));
        let mut native_nodes = layout
            .and_then(|layout| {
                request
                    .native_root(layout)
                    .map(|root| crate::dashboard_visibility::project(layout, root, phase, view))
            })
            .unwrap_or_default();
        let native_controls = native::admit(&mut native_nodes, view);
        let selection = match (view, layout) {
            (Some(view), Some(layout)) => selected(view, layout, request, &native_nodes),
            (None, Some(_)) | (Some(_), None) | (None, None) => Vec::new(),
        };
        let mut slots = Vec::new();
        let mut cards = Vec::new();
        let mut editors = Vec::new();
        let mut helper_selections = Vec::new();
        let mut fresh_observations = Vec::new();
        let mut inputs = selection
            .into_iter()
            .flat_map(|(title, entity)| {
                let primary_light = detail::primary_light(entity);
                entity
                    .capabilities
                    .iter()
                    .filter(move |capability| {
                        !detail::covered_by_light(entity, capability, primary_light)
                    })
                    .filter_map(move |capability| {
                        CardInput::new(
                            ControlSource::General,
                            title.clone(),
                            entity,
                            capability,
                            Control::parse(capability),
                        )
                    })
            })
            .take(MAX_CARDS)
            .collect::<Vec<_>>();
        for (node, admitted) in native_controls {
            let Some(entity) = view.and_then(|view| view.get(&admitted.entity)) else {
                continue;
            };
            let Some(capability) = entity.capabilities.get(admitted.capability) else {
                continue;
            };
            let title = admitted.title;
            if let Some(input) = CardInput::new(
                ControlSource::Native(node),
                title,
                entity,
                capability,
                admitted.control,
            ) {
                inputs.push(input);
            }
        }
        let mut ordinary_count = 0;
        let mut native_indices = BTreeMap::new();
        for input in inputs {
            let CardInput {
                source,
                title,
                entity,
                capability,
                target,
                control,
            } = input;
            match &source {
                ControlSource::General => ordinary_count += 1,
                ControlSource::Native(node) => {
                    native_indices.insert(node.clone(), cards.len());
                }
            }
            let access = if layout.is_some_and(|layout| {
                layout.native.as_ref().is_none_or(|native| {
                    native
                        .nodes
                        .values()
                        .any(|node| node.kind == crate::dashboard_layout::Kind::View)
                })
            }) {
                write_access(entity, &target)
            } else {
                WriteAccess::Denied
            };
            let helper_revision =
                helper::observation(capability).and_then(helper::HelperRevision::from_observation);
            fresh_observations.push(observation::fresh(capability));
            let mut row = card(&title, capability, &control, access);
            if let Control::ReadOnly(kind) = &control {
                let zone = layout.map_or(crate::environment_view::ProfileZone::Missing, |layout| {
                    layout.profile_zone
                });
                let now = crate::environment_view::now();
                row.environment =
                    crate::environment_view::project(*kind, Some(capability), zone, now);
                row.freshness =
                    crate::environment_view::observed_age(Some(capability), zone, now).into();
            }
            if let Control::Helper(helper) = &control {
                row.command_unavailable = helper_revision.is_none() || !helper.available();
            }
            helper_selections.push(helper::observation(capability).and_then(|value| {
                match value.value.as_ref() {
                    Some(wire::helper_value::Value::SelectedOption(value)) => Some(value.clone()),
                    Some(
                        wire::helper_value::Value::BooleanValue(_)
                        | wire::helper_value::Value::Number(_)
                        | wire::helper_value::Value::Counter(_)
                        | wire::helper_value::Value::TriggerTime(_)
                        | wire::helper_value::Value::Date(_)
                        | wire::helper_value::Value::Time(_)
                        | wire::helper_value::Value::LocalValue(_)
                        | wire::helper_value::Value::Text(_),
                    )
                    | None => None,
                }
            }));
            editors.push(
                panel::editors(&control)
                    .into_iter()
                    .map(|editor| (editor, editor.initial(capability, &control)))
                    .collect::<Vec<_>>(),
            );
            slots.push(Slot {
                source,
                generation: CardGeneration(0),
                target,
                capability: capability.clone(),
                access,
                control,
                helper_revision,
            });
            cards.push(row);
        }
        let mut context = match (view, layout) {
            (Some(view), Some(_)) => Some(Context {
                source: view.source_proof().cloned(),
                revision: request.revision,
                catalogue: request
                    .catalogue
                    .as_ref()
                    .map(|snapshot| snapshot.token().clone()),
                layout: request
                    .dashboard
                    .shared_layout()
                    .expect("compatible layout"),
                capture: view.name().clone(),
                incarnation: view.incarnation().clone(),
                expires: view.expires().clone(),
                slots,
                live: phase == CachePhase::Live,
                native_nodes: native_nodes
                    .iter()
                    .map(|node| (node.id.clone(), node.kind))
                    .collect(),
            }),
            (None, Some(_)) | (Some(_), None) | (None, None) => None,
        };
        let same_view = match (&state.context, &context) {
            (Some(previous), Some(current)) => previous.same_view(current),
            (Some(_), None) | (None, Some(_)) | (None, None) => false,
        };
        if let Some(current) = context.as_mut() {
            for (index, slot) in current.slots.iter_mut().enumerate() {
                let previous = state
                    .context
                    .as_ref()
                    .filter(|_| same_view)
                    .and_then(|previous| previous.slots.get(index))
                    .filter(|previous| previous.same_admission(slot))
                    .map(|previous| previous.generation);
                slot.generation = match previous {
                    Some(generation) => generation,
                    None => {
                        let Some(next) = state.serial.0.checked_add(1) else {
                            state.context = None;
                            return DeviceProjection::default();
                        };
                        state.serial = CardGeneration(next);
                        state.serial
                    }
                };
            }
        }
        state.context = context;
        if state.context.is_some() {
            if let Some(saved) = saved {
                let State {
                    context, drafts, ..
                } = &mut *state;
                saved.restore(context.as_ref().expect("fresh context"), drafts);
            }
            state.retained_drafts = None;
            state.draft_basis = draft_retention::Basis::new(phase, view, request);
        } else {
            state.draft_basis = None;
            // Detached text needs its own expiry even if the UI publication is
            // superseded or never delivered. Without a scheduler, fail closed.
            state.retained_drafts = saved.filter(|_| self.1.is_some());
            let needs_timer = state
                .retained_drafts
                .as_ref()
                .is_some_and(|saved| saved.serial == 0);
            if needs_timer {
                if let Some(serial) = state.draft_retention_serial.checked_add(1) {
                    state.draft_retention_serial = serial;
                    let saved = state.retained_drafts.as_mut().expect("saved draft");
                    saved.serial = serial;
                    self.expire_drafts_at(serial, saved.deadline());
                } else {
                    state.retained_drafts = None;
                }
            }
        }
        let generations = state
            .context
            .as_ref()
            .map(|context| {
                context
                    .slots
                    .iter()
                    .map(|slot| slot.generation)
                    .collect::<BTreeSet<_>>()
            })
            .unwrap_or_default();
        state
            .drafts
            .retain(|(generation, _), _| generations.contains(generation));
        state
            .selections
            .retain(|generation, _| generations.contains(generation));
        state.native_steps.observed.clear();
        state
            .native_steps
            .pending
            .retain(|(generation, _), pending| {
                generations.contains(generation) || pending.attached()
            });
        let ready = state.context.as_ref().is_some_and(|context| context.live());
        for (index, row) in cards.iter_mut().enumerate() {
            row.token = index as i32;
            let slot = state
                .context
                .as_ref()
                .expect("cards require coherent context")
                .slots[index]
                .clone();
            row.generation = slot.generation.0;
            row.field_keys = editors[index]
                .iter()
                .enumerate()
                .filter_map(|(index, (editor, _))| match editor {
                    panel::Editor::Setpoint(_) => Some(("setpoint_celsius", index as i32)),
                    panel::Editor::Range(_) => Some(("setpoint_range_celsius", index as i32)),
                    panel::Editor::CoverPosition(_)
                    | panel::Editor::Helper
                    | panel::Editor::ColorTemperature => None,
                })
                .collect();
            row.values = editors[index]
                .iter()
                .enumerate()
                .map(|(index, (editor, initial))| {
                    native::observe_step(
                        &mut state,
                        &slot,
                        index as i32,
                        *editor,
                        initial,
                        ready && fresh_observations[row.token as usize],
                    );
                    let draft = state
                        .drafts
                        .entry((slot.generation, index as i32))
                        .or_insert_with(|| initial.clone());
                    if !draft.dirty {
                        *draft = initial.clone();
                    }
                    editor.row(index as i32, draft, &slot.control)
                })
                .collect::<Vec<_>>();
            // A retained compact-step lease remains authoritative after local
            // tracking stops or delivery becomes indeterminate.
            row.command_unavailable |= state
                .native_steps
                .pending
                .values()
                .any(|pending| pending.blocks_target(&slot.target));
            if let Control::Helper(helper) = &slot.control
                && matches!(helper.kind(), helper::Kind::Select(_))
                && !row.command_unavailable
            {
                if let std::collections::btree_map::Entry::Vacant(entry) =
                    state.selections.entry(slot.generation)
                    && let Some(selection) =
                        selection::Selection::new(helper, helper_selections[index].clone())
                {
                    entry.insert(selection);
                }
                if let Some(selection) = state.selections.get_mut(&slot.generation) {
                    selection.observe(helper_selections[index].clone());
                    if let Some((options, selected)) = selection.project() {
                        row.select_options = options;
                        row.select_state = selected;
                    } else {
                        row.command_unavailable = true;
                    }
                }
            }
            let snapshot = state
                .actions
                .iter()
                .find(|(target, _)| target == &slot.target)
                .map(|(_, handle)| handle.snapshot());
            let (phase, label, detail) = progress::presentation(snapshot.as_deref());
            row.phase = phase;
            row.action_label = label.into();
            row.action_detail = detail.into();
        }
        let recipes = recipes::project(&mut state, phase, view, &native_nodes);
        let native_devices = native_indices
            .into_iter()
            .map(|(node, index)| (node, cards[index].clone()))
            .collect();
        cards.truncate(ordinary_count);
        let represented = state
            .context
            .as_ref()
            .map(|context| {
                context
                    .slots
                    .iter()
                    .map(|slot| slot.target.entity().clone())
                    .collect()
            })
            .unwrap_or_default();
        let entities = state
            .context
            .as_ref()
            .map(|context| {
                context
                    .slots
                    .iter()
                    .take(ordinary_count)
                    .map(|slot| slot.target.entity().clone())
                    .collect()
            })
            .unwrap_or_default();
        DeviceProjection {
            recipes,
            native_nodes,
            native_devices,
            entities,
            rows: cards,
            ready,
            represented,
        }
    }
    pub fn entity(&self, token: i32, generation: i32, revision: i32) -> Option<EntityName> {
        let state = self.0.lock().expect("device store mutex");
        let context = state.context.as_ref()?;
        if !context.source_current() || context.revision != revision {
            return None;
        }
        context
            .slots
            .get(usize::try_from(CardToken(token).0).ok()?)
            .filter(|slot| slot.generation == CardGeneration(generation))
            .map(|slot| slot.target.entity().clone())
    }
    pub fn native_origin(
        &self,
        token: i32,
        generation: i32,
        revision: i32,
    ) -> Option<crate::dashboard_focus::Origin> {
        let state = self.0.lock().expect("device store mutex");
        let context = state.context.as_ref()?;
        if !context.source_current() || context.revision != revision {
            return None;
        }
        let slot = context
            .slots
            .get(usize::try_from(token).ok()?)
            .filter(|slot| slot.generation == CardGeneration(generation))?;
        if matches!(
            slot.control,
            Control::Helper(_) | Control::ReadOnly(_) | Control::Unsupported
        ) {
            return None;
        }
        let ControlSource::Native(node) = &slot.source else {
            return None;
        };
        Some(crate::dashboard_focus::Origin {
            node: node.clone(),
            layout: context.layout.clone(),
        })
    }
    pub fn native_focus(
        &self,
        node: &crate::dashboard_layout::NodeId,
        layout: &crate::dashboard::Layout,
        revision: i32,
    ) -> Option<(i32, i32)> {
        let state = self.0.lock().expect("device store mutex");
        let context = state.context.as_ref()?;
        if !context.source_current()
            || context.revision != revision
            || context.layout.as_ref() != layout
        {
            return None;
        }
        context.slots.iter().enumerate().find_map(|(token, slot)| {
            matches!(&slot.source,ControlSource::Native(id) if id == node)
                .then_some((token as i32, slot.generation.0))
        })
    }
    pub fn edit(
        &self,
        token: i32,
        generation: i32,
        revision: i32,
        field: i32,
        intent: crate::ProValueIntent,
    ) -> Result<Option<DeviceIntent>, Refusal> {
        let mut state = self.0.lock().expect("device store mutex");
        let slot = active_slot(&state, token, generation, revision)?;
        let control = slot.control.clone();
        let editor = usize::try_from(field)
            .ok()
            .and_then(|index| panel::editors(&slot.control).get(index).copied())
            .ok_or(Refusal::Unsupported)?;
        if matches!(&control,Control::Helper(helper) if matches!(helper.kind(),helper::Kind::Select(_)))
        {
            return state
                .selections
                .get_mut(&CardGeneration(generation))
                .ok_or(Refusal::Unavailable)?
                .edit(intent);
        }
        if native::is_thermostat(
            state.context.as_ref().expect("active context"),
            slot,
            editor,
        ) {
            return native::edit_step(&mut state, generation, field, editor, intent, &control);
        }
        let draft = state
            .drafts
            .get_mut(&(CardGeneration(generation), field))
            .ok_or(Refusal::Stale)?;
        editor.edit(draft, intent, &control)
    }
    /// A panel/tile step is one complete action on the admitted single setpoint.
    pub fn step(
        &self,
        token: i32,
        generation: i32,
        revision: i32,
        direction: crate::StepDirection,
    ) -> Result<Option<DeviceIntent>, Refusal> {
        let mut state = self.0.lock().expect("device store mutex");
        let slot = active_slot(&state, token, generation, revision)?;
        let control = slot.control.clone();
        let editors = panel::editors(&control);
        let mut setpoints = editors
            .iter()
            .enumerate()
            .filter(|(_, editor)| matches!(editor, panel::Editor::Setpoint(_)));
        let (field, editor) = setpoints.next().ok_or(Refusal::Unsupported)?;
        if setpoints.next().is_some() {
            return Err(Refusal::Unsupported);
        }
        let step = crate::ProValueIntent {
            event: crate::ProValueEvent::Step,
            direction,
            thumb: crate::RangeThumb::Lower,
            ..Default::default()
        };
        if native::is_thermostat(
            state.context.as_ref().expect("active context"),
            slot,
            *editor,
        ) {
            return native::edit_step(
                &mut state,
                generation,
                field as i32,
                *editor,
                step,
                &control,
            );
        }
        let draft = state
            .drafts
            .get_mut(&(CardGeneration(generation), field as i32))
            .ok_or(Refusal::Stale)?;
        editor.edit(
            draft,
            crate::ProValueIntent {
                event: crate::ProValueEvent::Start,
                ..Default::default()
            },
            &control,
        )?;
        editor.edit(draft, step, &control)?;
        editor.edit(
            draft,
            crate::ProValueIntent {
                event: crate::ProValueEvent::Commit,
                ..Default::default()
            },
            &control,
        )
    }
    pub fn abandon_native_step(&self, token: i32, generation: i32) {
        let mut state = self.0.lock().expect("device store mutex");
        native::abandon_step(&mut state, token, generation);
    }
    pub fn choose(
        &self,
        token: i32,
        generation: i32,
        revision: i32,
        choice: i32,
    ) -> Result<DeviceIntent, Refusal> {
        let state = self.0.lock().expect("device store mutex");
        let slot = active_slot(&state, token, generation, revision)?;
        usize::try_from(choice)
            .ok()
            .and_then(|index| {
                panel::choices(&slot.control)
                    .get(index)
                    .map(|choice| choice.intent.clone())
            })
            .ok_or(Refusal::Unsupported)
    }
    pub fn stop(&self, token: i32, generation: i32, revision: i32) {
        let handle = {
            let state = self.0.lock().expect("device store mutex");
            let Some(context) = state.context.as_ref() else {
                return;
            };
            if !context.source_current() || context.revision != revision {
                return;
            }
            let Some(slot) = usize::try_from(token)
                .ok()
                .and_then(|index| context.slots.get(index))
                .filter(|slot| slot.generation == CardGeneration(generation))
            else {
                return;
            };
            state
                .actions
                .iter()
                .find(|(target, _)| target == &slot.target)
                .map(|(_, handle)| handle.clone())
        };
        if let Some(handle) = handle {
            handle.stop_tracking();
        }
    }
    pub fn remember(&self, target: CapabilityName, handle: CapabilityHandle) {
        let mut state = self.0.lock().expect("device store mutex");
        native::attach_step(&mut state, &target, &handle.request_id());
        state.actions.retain(|(old, _)| old != &target);
        // At most 32 controller records can still be active. Retaining 64
        // presentation records permits eviction of a finished older record.
        if state.actions.len() == MAX_ACTION_HISTORY {
            let retired = state
                .actions
                .iter()
                .position(|(_, action)| action.is_finished());
            if let Some(index) = retired {
                state.actions.remove(index);
            } else {
                state.actions.pop_front();
            }
        }
        state.actions.push_back((target, handle));
    }
    pub fn prepare(
        &self,
        token: i32,
        generation: i32,
        revision: i32,
        intent: DeviceIntent,
    ) -> Result<Prepared, Refusal> {
        let state = self.0.lock().expect("device store mutex");
        let step_lease = native::step_lease(&state, token, generation);
        let context = state
            .context
            .as_ref()
            .filter(|context| context.live() && context.revision == revision)
            .ok_or(Refusal::Stale)?;
        let slot = usize::try_from(token)
            .ok()
            .and_then(|index| context.slots.get(index))
            .filter(|slot| slot.generation == CardGeneration(generation))
            .ok_or(Refusal::Stale)?;
        if slot.access != WriteAccess::Allowed {
            return Err(Refusal::ReadOnly);
        }
        let arguments = slot.control.arguments(intent, slot.helper_revision)?;
        let request_id = home_client_actions::new_request_id().map_err(|_| Refusal::Unavailable)?;
        let expires = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_err(|_| Refusal::Unavailable)?
            .as_secs()
            .checked_add(30)
            .ok_or(Refusal::Unavailable)?;
        let request =
            home_wire_contract::api_contract::parse_command(&wire::ExecuteCapabilityRequest {
                name: slot.target.as_str().into(),
                request_id: request_id.as_str().into(),
                etag: slot.capability.etag.clone(),
                expire_time: buffa_types::google::protobuf::Timestamp::from_unix_secs(
                    i64::try_from(expires).map_err(|_| Refusal::Unavailable)?,
                )
                .into(),
                command: Some(arguments),
                ..Default::default()
            })
            .map_err(|_| Refusal::InvalidValue)?;
        let target = slot.target.clone();
        let expected = slot.clone();
        let weak = Arc::downgrade(&self.0);
        let guard = Arc::new(move || {
            let Some(shared) = weak.upgrade() else {
                return false;
            };
            let state = shared.lock().expect("device store mutex");
            step_lease
                .as_ref()
                .is_none_or(|lease| lease.admitted(&state))
                && state.context.as_ref().is_some_and(|current| {
                    current.live()
                        && current.revision == revision
                        && current.slots.get(token as usize).is_some_and(|slot| {
                            slot.generation == expected.generation && slot.same_admission(&expected)
                        })
                })
        });
        Ok(Prepared {
            target,
            request,
            guard,
            layout: context.layout.clone(),
        })
    }
}

fn active_slot(
    state: &State,
    token: i32,
    generation: i32,
    revision: i32,
) -> Result<&Slot, Refusal> {
    let context = state
        .context
        .as_ref()
        .filter(|context| context.live() && context.revision == revision)
        .ok_or(Refusal::Stale)?;
    let slot = usize::try_from(token)
        .ok()
        .and_then(|index| context.slots.get(index))
        .filter(|slot| slot.generation == CardGeneration(generation))
        .ok_or(Refusal::Stale)?;
    if slot.access != WriteAccess::Allowed {
        return Err(Refusal::ReadOnly);
    }
    if matches!(&slot.control,Control::Helper(helper) if !helper.available() || slot.helper_revision.is_none())
    {
        return Err(Refusal::Unavailable);
    }
    Ok(slot)
}

#[cfg(test)]
mod recipe_tests;
#[cfg(test)]
mod tests;

fn card(
    title: &str,
    capability: &wire::Capability,
    control: &Control,
    access: WriteAccess,
) -> CardRow {
    use wire::capability::Trait as T;
    let mut row = CardRow {
        title: bounded(title).into(),
        value: observation::value_text(capability).into(),
        quality: observation::quality_text(capability).into(),
        kind: ProDeviceKind::Unsupported,
        access: match access {
            WriteAccess::Allowed => ProControlAccess::Allowed,
            WriteAccess::Denied => ProControlAccess::Denied,
            WriteAccess::Unknown => ProControlAccess::Unknown,
        },
        ..Default::default()
    };
    match control {
        Control::Switch { power } => {
            row.kind = ProDeviceKind::Switch;
            row.can_power = *power;
            if let Some(T::Power(value)) = capability.r#trait.as_ref()
                && observation::evidence(capability).is_some()
                && let Some(power) = value.observation.value.and_then(|value| value.as_known())
            {
                match power {
                    wire::PowerValue::On => {
                        row.power_known = true;
                        row.power_on = true;
                    }
                    wire::PowerValue::Off => row.power_known = true,
                    wire::PowerValue::Unspecified => {}
                }
            }
        }
        Control::Level { bounds } => {
            row.kind = ProDeviceKind::Light;
            brightness_bounds(&mut row, *bounds);
            if let Some(T::Level(value)) = capability.r#trait.as_ref()
                && observation::evidence(capability).is_some()
            {
                let value = value
                    .observation
                    .percentage
                    .filter(|value| value.is_finite() && (0.0..=100.0).contains(value));
                row.brightness_known = value.is_some();
                row.brightness_percent = value.unwrap_or(0.) as f32;
            }
        }
        Control::Light {
            power, brightness, ..
        } => {
            row.kind = ProDeviceKind::Light;
            row.can_power = *power;
            brightness_bounds(&mut row, *brightness);
            if let Some(T::LightControl(value)) = capability.r#trait.as_ref()
                && observation::evidence(capability).is_some()
            {
                row.power_known = value.observation.on.is_some();
                row.power_on = value.observation.on.unwrap_or(false);
                let value = value
                    .observation
                    .brightness_percent
                    .filter(|value| value.is_finite() && (0.0..=100.0).contains(value));
                row.brightness_known = value.is_some();
                row.brightness_percent = value.unwrap_or(0.) as f32;
            }
        }
        Control::Binary => row.kind = ProDeviceKind::Binary,
        Control::Cover(_) => row.kind = ProDeviceKind::Cover,
        Control::Lock(_) => row.kind = ProDeviceKind::Lock,
        Control::Alarm(_) => row.kind = ProDeviceKind::Alarm,
        Control::Measurement => row.kind = ProDeviceKind::Measurement,
        Control::Climate(_) => {
            row.kind = ProDeviceKind::Climate;
            row.readings = panel::readings(control, capability).into();
        }
        Control::Fan(fan) => {
            row.kind = ProDeviceKind::Fan;
            row.can_power = fan.can_power();
            if let Some(T::FanControl(value)) = capability.r#trait.as_ref()
                && observation::evidence(capability).is_some()
            {
                match value.observation.speed.and_then(|speed| speed.as_known()) {
                    Some(wire::FanSpeed::Off) => {
                        row.power_known = true;
                        row.power_on = false;
                    }
                    Some(
                        wire::FanSpeed::Low
                        | wire::FanSpeed::Medium
                        | wire::FanSpeed::MediumHigh
                        | wire::FanSpeed::High,
                    ) => {
                        row.power_known = true;
                        row.power_on = true;
                    }
                    Some(wire::FanSpeed::Unspecified) | None => {}
                }
            }
        }
        Control::Helper(helper) => {
            row.kind = ProDeviceKind::Helper;
            if !helper.available() {
                row.quality = format!("{} · No authorized target", row.quality).into();
            } else if helper::observation(capability).is_none() {
                row.quality = format!("{} · Helper revision unavailable", row.quality).into();
            }
        }
        Control::ReadOnly(_) => {
            row.kind = ProDeviceKind::Observation;
            row.quality = format!("{} · Read-only", row.quality).into();
        }
        Control::Unsupported => {
            row.quality = format!("{} · Unsupported capability", row.quality).into()
        }
    }
    row.detail = detail::Metadata::new(control, capability);
    row.choices = panel::choice_rows(control);
    row.cover_operations = panel::cover_rows(control);
    row.lock_operations = panel::lock_rows(control);
    row.alarm_operations = panel::alarm_rows(control);
    row.freshness = observation::observed_time(capability)
        .map(|time| format!("Observed on {}", time.to_rfc3339()))
        .unwrap_or_else(|| "Date not provided".into())
        .into();
    row
}
fn brightness_bounds(row: &mut CardRow, bounds: Option<number::Bounds>) {
    if let Some(bounds) = bounds {
        row.can_brightness = bounds.minimum() < bounds.maximum();
        row.brightness_minimum = bounds.minimum() as f32;
        row.brightness_maximum = bounds.maximum() as f32;
        row.brightness_step = bounds.step().unwrap_or(0.) as f32;
    }
}

pub(crate) fn color_temperature_text(value: u32) -> Option<String> {
    light::Mireds::from_wire(value).map(light::Mireds::observation)
}

#[cfg(test)]
mod secure_tests;

#[cfg(test)]
mod tls_probe;

#[cfg(test)]
pub(crate) mod fixture;
