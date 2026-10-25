//! Exact server-selected cards, command admission and bounded local action
//! history. UI tokens are ephemeral; identities/permissions never come from UI.
mod capability;
mod climate;
mod helper;
mod light;
mod number;
mod panel;
pub(crate) mod presentation;
mod progress;
mod selection;
use crate::{
    IconKind, ProControlAccess, ProDeviceKind,
    cache_view::{bounded, quality_text, value_text},
    dashboard::{Layout, ProjectionRequest},
};
use capability::Control;
use home_client_actions::{ActionHandle, ActionTarget, AdmissionCheck, WriteAccess, write_access};
use home_client_cache::{CachePhase, CachedView};
use home_client_model::EntityId;
use home_wire_contract::home::{
    actions::v1 as actions, registry::v1 as registry, state::v1 as state,
};
use presentation::CardRow;
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
#[derive(Clone, Copy)]
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
    FanSpeed(state::FanSpeed),
    Helper(helper::HelperIntent),
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
            Self::Stale => "Commande non envoyée : la vue a changé. Reprenez le geste.",
            Self::ReadOnly => "Commande non envoyée : cette fonction est en lecture seule.",
            Self::Unsupported => "Commande non envoyée : cette fonction n’est pas prise en charge.",
            Self::InvalidValue => {
                "Commande non envoyée : la valeur n’est pas valide pour cet appareil."
            }
            Self::Capacity => {
                "Commande non envoyée : une opération est en cours. Réessayez dans un instant."
            }
            Self::Unavailable => "Commande non envoyée : le contrôleur est indisponible.",
        }
    }
}
#[derive(Clone, PartialEq)]
struct Slot {
    generation: CardGeneration,
    target: ActionTarget,
    capability: registry::CapabilityDescription,
    access: WriteAccess,
    control: Control,
    helper_revision: Option<helper::HelperRevision>,
}
impl Slot {
    fn same_admission(&self, other: &Self) -> bool {
        self.target == other.target
            && self.capability == other.capability
            && self.access == other.access
            && self.control == other.control
            && self.helper_revision == other.helper_revision
    }
}
#[derive(Clone, PartialEq)]
struct Context {
    revision: i32,
    layout: Arc<Layout>,
    epoch: String,
    config_revision: u64,
    authorization_revision: u64,
    grant_hash: Vec<u8>,
    slots: Vec<Slot>,
    live: bool,
}
impl Context {
    fn same_view(&self, other: &Self) -> bool {
        self.revision == other.revision
            && self.layout == other.layout
            && self.epoch == other.epoch
            && self.config_revision == other.config_revision
            && self.authorization_revision == other.authorization_revision
            && self.grant_hash == other.grant_hash
            && self.live == other.live
    }
}
struct State {
    serial: CardGeneration,
    context: Option<Context>,
    actions: VecDeque<(ActionTarget, ActionHandle)>,
    drafts: BTreeMap<(CardGeneration, i32), panel::Draft>,
    selections: BTreeMap<CardGeneration, selection::Selection>,
}
#[derive(Clone)]
pub(crate) struct DeviceStore(Arc<Mutex<State>>);
#[derive(Default)]
pub(crate) struct DeviceProjection {
    pub rows: Vec<CardRow>,
    pub ready: bool,
    pub represented: BTreeSet<EntityId>,
    pub entities: Vec<EntityId>,
}
pub(crate) struct Prepared {
    pub target: ActionTarget,
    pub request: actions::CommandRequest,
    pub guard: AdmissionCheck,
    pub layout: Arc<Layout>,
}
impl Default for DeviceStore {
    fn default() -> Self {
        Self(Arc::new(Mutex::new(State {
            serial: CardGeneration(0),
            context: None,
            actions: VecDeque::new(),
            drafts: BTreeMap::new(),
            selections: BTreeMap::new(),
        })))
    }
}
fn selected<'a>(
    view: &'a CachedView,
    layout: &Layout,
    request: &ProjectionRequest,
) -> Vec<(String, &'a state::ViewEntity)> {
    if let Some(id) = &request.detail {
        return view
            .get(id)
            .map(|entity| vec![(bounded(&entity.current_alias), entity)])
            .unwrap_or_default();
    }
    if let Some(native) = &layout.native {
        let mut seen = BTreeSet::new();
        return native
            .root(request.room.as_ref())
            .map(|root| {
                native
                    .ordered_nodes(root)
                    .into_iter()
                    .filter_map(|node| {
                        let id = node.entity.as_ref()?;
                        if !seen.insert(id.clone()) {
                            return None;
                        }
                        let entity = view.get(id)?;
                        if !request.query.is_empty()
                            && !entity.current_alias.to_lowercase().contains(&request.query)
                        {
                            return None;
                        }
                        Some((
                            if node.title.is_empty() {
                                bounded(&entity.current_alias)
                            } else {
                                node.title.clone()
                            },
                            entity,
                        ))
                    })
                    .collect()
            })
            .unwrap_or_default();
    }
    let entries = match &request.room {
        Some(room) => layout
            .room(room)
            .map(|room| {
                room.entities
                    .iter()
                    .filter_map(|id| {
                        view.get(id)
                            .map(|entity| (bounded(&entity.current_alias), entity))
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
            request.query.is_empty() || entity.current_alias.to_lowercase().contains(&request.query)
        })
        .collect()
}
impl DeviceStore {
    pub fn clear_private(&self) {
        let mut state = self.0.lock().expect("device store mutex");
        state.context = None;
        state.drafts.clear();
        state.selections.clear();
        state.serial = CardGeneration(state.serial.0.saturating_add(1));
        for (_, handle) in state.actions.drain(..) {
            handle.stop_tracking();
        }
    }
    pub fn project(
        &self,
        phase: CachePhase,
        view: Option<&CachedView>,
        request: &ProjectionRequest,
    ) -> DeviceProjection {
        let mut state = self.0.lock().expect("device store mutex");
        let layout = request
            .dashboard
            .layout()
            .filter(|layout| view.is_some_and(|view| layout.compatible(view)));
        let selection = match (view, layout) {
            (Some(view), Some(layout)) => selected(view, layout, request),
            (None, Some(_)) | (Some(_), None) | (None, None) => Vec::new(),
        };
        let mut slots = Vec::new();
        let mut cards = Vec::new();
        let mut editors = Vec::new();
        let mut helper_selections = Vec::new();
        for (title, entity) in selection {
            for capability in &entity.capabilities {
                if cards.len() == MAX_CARDS {
                    break;
                }
                let Some(id) = entity.entity_id.parse().ok() else {
                    continue;
                };
                let Some(target) =
                    ActionTarget::new(id, &capability.schema_id, &capability.endpoint)
                else {
                    continue;
                };
                let access = if layout.is_some_and(|layout| {
                    layout.native.as_ref().is_none_or(|native| {
                        native
                            .nodes
                            .values()
                            .any(|node| node.kind == crate::dashboard_layout::Kind::View)
                    })
                }) {
                    write_access(entity, target.schema(), target.endpoint())
                } else {
                    WriteAccess::Denied
                };
                let control = Control::parse(capability);
                let helper_revision = if matches!(&control, Control::Helper(_)) {
                    entity
                        .state
                        .as_option()
                        .filter(|value| value.__buffa_unknown_fields.is_empty())
                        .and_then(|value| match value.value.as_ref() {
                            Some(state::observed_state::Value::Helper(value)) => {
                                helper::HelperRevision::from_observation(value)
                            }
                            Some(
                                state::observed_state::Value::SwitchOn(_)
                                | state::observed_state::Value::Measurement(_)
                                | state::observed_state::Value::Light(_)
                                | state::observed_state::Value::Fan(_)
                                | state::observed_state::Value::Climate(_)
                                | state::observed_state::Value::Solar(_)
                                | state::observed_state::Value::SolarEvent(_),
                            )
                            | None => None,
                        })
                } else {
                    None
                };
                let mut row = card(&title, entity, &control, access);
                if let Control::Helper(helper) = &control {
                    row.command_unavailable = helper_revision.is_none() || !helper.available();
                }
                helper_selections.push(helper::observation(entity).and_then(|value|match value.value.as_ref() {
                    Some(home_wire_contract::home::helpers::v1::helper_value::Value::SelectedOption(value)) => Some(value.clone()),
                    Some(home_wire_contract::home::helpers::v1::helper_value::Value::Boolean(_) | home_wire_contract::home::helpers::v1::helper_value::Value::Number(_) | home_wire_contract::home::helpers::v1::helper_value::Value::Counter(_) | home_wire_contract::home::helpers::v1::helper_value::Value::ButtonPressedAt(_) | home_wire_contract::home::helpers::v1::helper_value::Value::Date(_) | home_wire_contract::home::helpers::v1::helper_value::Value::Time(_) | home_wire_contract::home::helpers::v1::helper_value::Value::Datetime(_) | home_wire_contract::home::helpers::v1::helper_value::Value::Text(_)) | None => None,
                }));
                editors.push(
                    panel::editors(&control)
                        .into_iter()
                        .map(|editor| (editor, editor.initial(entity, &control)))
                        .collect::<Vec<_>>(),
                );
                slots.push(Slot {
                    generation: CardGeneration(0),
                    target,
                    capability: capability.clone(),
                    access,
                    control,
                    helper_revision,
                });
                cards.push(row);
            }
            if cards.len() == MAX_CARDS {
                break;
            }
        }
        let mut context = match (view, layout) {
            (Some(view), Some(_)) => Some(Context {
                revision: request.revision,
                layout: request
                    .dashboard
                    .shared_layout()
                    .expect("compatible layout"),
                epoch: view.cursor().epoch.clone(),
                config_revision: view.cursor().config_revision,
                authorization_revision: view.cursor().authorization_revision,
                grant_hash: view.cursor().grant_sha256.clone(),
                slots,
                live: phase == CachePhase::Live,
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
        let ready = state.context.as_ref().is_some_and(|context| context.live);
        for (index, row) in cards.iter_mut().enumerate() {
            row.token = index as i32;
            let slot = state
                .context
                .as_ref()
                .expect("cards require coherent context")
                .slots[index]
                .clone();
            row.generation = slot.generation.0;
            row.values = editors[index]
                .iter()
                .enumerate()
                .map(|(index, (editor, initial))| {
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
                    .map(|slot| slot.target.entity().clone())
                    .collect()
            })
            .unwrap_or_default();
        DeviceProjection {
            entities,
            rows: cards,
            ready,
            represented,
        }
    }
    pub fn entity(&self, token: i32, generation: i32, revision: i32) -> Option<EntityId> {
        let state = self.0.lock().expect("device store mutex");
        let context = state.context.as_ref()?;
        if context.revision != revision {
            return None;
        }
        context
            .slots
            .get(usize::try_from(CardToken(token).0).ok()?)
            .filter(|slot| slot.generation == CardGeneration(generation))
            .map(|slot| slot.target.entity().clone())
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
        let draft = state
            .drafts
            .get_mut(&(CardGeneration(generation), field))
            .ok_or(Refusal::Stale)?;
        editor.edit(draft, intent, &control)
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
            if context.revision != revision {
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
    pub fn remember(&self, target: ActionTarget, handle: ActionHandle) {
        let mut state = self.0.lock().expect("device store mutex");
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
        let context = state
            .context
            .as_ref()
            .filter(|context| context.live && context.revision == revision)
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
        let request = actions::CommandRequest {
            expires_at: buffa_types::google::protobuf::Timestamp::from_unix_secs(
                i64::try_from(
                    std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .map_err(|_| Refusal::Unavailable)?
                        .as_secs(),
                )
                .map_err(|_| Refusal::Unavailable)?
                .checked_add(30)
                .ok_or(Refusal::Unavailable)?,
            )
            .into(),
            admission_epoch: context.epoch.clone(),
            config_revision: context.config_revision,
            target: registry::EntityRef {
                reference: Some(registry::entity_ref::Reference::EntityId(
                    slot.target.entity().as_str().into(),
                )),
                ..Default::default()
            }
            .into(),
            capability_schema_id: slot.target.schema().into(),
            endpoint: slot.target.endpoint().into(),
            arguments: arguments.into(),
            ..Default::default()
        };
        let target = slot.target.clone();
        let expected = slot.clone();
        let weak = Arc::downgrade(&self.0);
        let guard = Arc::new(move || {
            let Some(shared) = weak.upgrade() else {
                return false;
            };
            let state = shared.lock().expect("device store mutex");
            state.context.as_ref().is_some_and(|current| {
                current.live
                    && current.revision == revision
                    && current.slots.get(token as usize) == Some(&expected)
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
        .filter(|context| context.live && context.revision == revision)
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
mod tests;

fn card(
    title: &str,
    entity: &state::ViewEntity,
    control: &Control,
    access: WriteAccess,
) -> CardRow {
    let mut row = CardRow {
        title: bounded(title).into(),
        value: entity
            .state
            .as_option()
            .map(value_text)
            .unwrap_or_else(|| "Aucune observation".into())
            .into(),
        quality: entity
            .state
            .as_option()
            .map(quality_text)
            .unwrap_or_else(|| "Qualité absente".into())
            .into(),
        icon: IconKind::Information,
        kind: ProDeviceKind::Unsupported,
        access: match access {
            WriteAccess::Allowed => ProControlAccess::Allowed,
            WriteAccess::Denied => ProControlAccess::Denied,
            WriteAccess::Unknown => ProControlAccess::Unknown,
        },
        ..Default::default()
    };
    let observation = entity
        .state
        .as_option()
        .filter(|state| state.__buffa_unknown_fields.is_empty());
    match control {
        Control::Switch { power } => {
            row.kind = ProDeviceKind::Switch;
            row.can_power = *power;
            if let Some(state::observed_state::Value::SwitchOn(on)) =
                observation.and_then(|state| state.value.as_ref())
            {
                row.power_known = true;
                row.power_on = *on;
            }
        }
        Control::Light {
            power, brightness, ..
        } => {
            row.kind = ProDeviceKind::Light;
            row.can_power = *power;
            if let Some(bounds) = brightness {
                row.can_brightness = true;
                row.brightness_minimum = bounds.minimum() as f32;
                row.brightness_maximum = bounds.maximum() as f32;
                row.brightness_step = bounds
                    .step()
                    .unwrap_or(1_f64.min(bounds.maximum() - bounds.minimum()))
                    as f32;
            }
            if let Some(state::observed_state::Value::Light(value)) =
                observation.and_then(|state| state.value.as_ref())
                && value.__buffa_unknown_fields.is_empty()
            {
                row.power_known = value.on.is_some();
                row.power_on = value.on.unwrap_or(false);
                row.brightness_known = value.brightness_percent.is_some_and(f64::is_finite);
                row.brightness_percent = value.brightness_percent.unwrap_or(0.) as f32;
            }
        }
        Control::Binary => {
            row.kind = ProDeviceKind::Binary;
            if let Some(state::observed_state::Value::SwitchOn(on)) =
                observation.and_then(|state| state.value.as_ref())
            {
                row.value = if *on { "Actif" } else { "Inactif" }.into();
            }
        }
        Control::Measurement => row.kind = ProDeviceKind::Measurement,
        Control::Climate(_) => {
            row.kind = ProDeviceKind::Climate;
            row.value = "Climat".into();
            row.readings = panel::readings(control, entity).into();
        }
        Control::Fan(fan) => {
            row.kind = ProDeviceKind::Fan;
            row.can_power = fan.can_power();
            if let Some(state::observed_state::Value::Fan(value)) =
                observation.and_then(|value| value.value.as_ref())
                && value.__buffa_unknown_fields.is_empty()
            {
                match value.speed.as_known() {
                    Some(state::FanSpeed::Off) => {
                        row.power_known = true;
                        row.power_on = false;
                    }
                    Some(
                        state::FanSpeed::Low
                        | state::FanSpeed::Medium
                        | state::FanSpeed::MediumHigh
                        | state::FanSpeed::High,
                    ) => {
                        row.power_known = true;
                        row.power_on = true;
                    }
                    Some(state::FanSpeed::Unspecified) | None => {}
                }
            }
        }
        Control::Helper(helper) => {
            row.kind = ProDeviceKind::Helper;
            if !helper.available() {
                row.quality = format!("{} · Aucune cible autorisée", row.quality).into();
            } else if helper::observation(entity).is_none() {
                row.quality = format!("{} · Révision du helper indisponible", row.quality).into();
            }
        }
        Control::Unsupported => {
            row.quality = format!("{} · Fonction non prise en charge", row.quality).into();
        }
    }
    row.choices = panel::choice_rows(control);
    row.freshness = entity
        .state
        .as_option()
        .and_then(|state| state.received_at.as_option())
        .and_then(|at| chrono::DateTime::from_timestamp(at.seconds, at.nanos as u32))
        .map(|at| format!("Reçu le {}", at.to_rfc3339()))
        .unwrap_or_else(|| "Date non renseignée".into())
        .into();
    row
}

pub(crate) fn color_temperature_text(value: u32) -> Option<String> {
    light::Mireds::from_wire(value).map(light::Mireds::observation)
}
