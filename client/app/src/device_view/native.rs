//! Native card controls are admitted by source node and exact capability.
//! Presentation filtering and command construction use the same restricted Control.
use super::{capability::Control, helper::Kind};
use crate::dashboard_layout::{FeatureKind, Kind as NodeKind, Node, NodeId, RecipeKind};
use home_client_actions::ActionTarget;
use home_client_cache::CachedView;
use home_client_model::EntityId;
use home_wire_contract::home::helpers::v1::HelperOperation;
use std::collections::{BTreeMap, BTreeSet};

// A summary step is a complete intent, unlike the drafts in ordinary cards.
// Its reservation survives uncertain delivery and is released only by exact
// observation reconciliation or a definitive non-send/refusal.
type StepKey = (super::CardGeneration, i32);
#[derive(Default)]
pub(super) struct Steps {
    pub observed: BTreeMap<StepKey, String>,
    pub pending: BTreeMap<StepKey, PendingStep>,
}
pub(super) struct PendingStep {
    target: ActionTarget,
    original: String,
    proposed: String,
    action: Option<home_client_actions::CommandId>,
    life: std::sync::Arc<std::sync::atomic::AtomicU8>,
}
impl PendingStep {
    pub fn blocks_target(&self, target: &ActionTarget) -> bool {
        &self.target == target && self.life.load(std::sync::atomic::Ordering::SeqCst) != 2
    }
    pub fn attached(&self) -> bool {
        self.action.is_some()
    }
}
pub(super) struct StepLease {
    key: StepKey,
    life: std::sync::Arc<std::sync::atomic::AtomicU8>,
}
impl Drop for StepLease {
    fn drop(&mut self) {
        let _ = self.life.compare_exchange(
            0,
            2,
            std::sync::atomic::Ordering::SeqCst,
            std::sync::atomic::Ordering::SeqCst,
        );
    }
}
impl StepLease {
    pub fn admitted(&self, state: &super::State) -> bool {
        state
            .native_steps
            .pending
            .get(&self.key)
            .is_some_and(|pending| {
                std::sync::Arc::ptr_eq(&pending.life, &self.life)
                    && self.life.load(std::sync::atomic::Ordering::SeqCst) != 2
                    && state.native_steps.observed.get(&self.key) == Some(&pending.original)
            })
    }
}
pub(super) fn step_lease(state: &super::State, token: i32, generation: i32) -> Option<StepLease> {
    state
        .context
        .as_ref()?
        .slots
        .get(usize::try_from(token).ok()?)
        .filter(|slot| slot.generation == super::CardGeneration(generation))?;
    state
        .native_steps
        .pending
        .iter()
        .find_map(|(key, pending)| {
            (key.0 == super::CardGeneration(generation) && pending.action.is_none()).then(|| {
                StepLease {
                    key: *key,
                    life: pending.life.clone(),
                }
            })
        })
}
pub(super) fn is_thermostat(
    context: &super::Context,
    slot: &super::Slot,
    editor: super::panel::Editor,
) -> bool {
    use crate::dashboard_widgets::{Content, FieldKey};
    let super::ControlSource::Native(id) = &slot.source else {
        return false;
    };
    let Some(node) = context
        .layout
        .native
        .as_ref()
        .and_then(|layout| layout.nodes.get(id))
    else {
        return false;
    };
    let Some(widget) = &node.widget else {
        return false;
    };
    matches!(&widget.content, Content::Thermostat { entity, current, target, detailed: false, summary_only: false }
        if node.kind == NodeKind::Widget && node.entity.as_ref() == Some(entity)
            && entity == slot.target.entity() && &current.entity == entity && &target.entity == entity
            && current.key == FieldKey::CurrentTemperature && target.key == FieldKey::Setpoint
            && matches!(editor, super::panel::Editor::Setpoint(_)))
}
pub(super) fn observe_step(
    state: &mut super::State,
    slot: &super::Slot,
    field: i32,
    editor: super::panel::Editor,
    initial: &super::panel::Draft,
    fresh: bool,
) {
    if !state
        .context
        .as_ref()
        .is_some_and(|context| is_thermostat(context, slot, editor))
    {
        return;
    }
    let key = (slot.generation, field);
    if fresh && !initial.lower.is_empty() && initial.upper.is_empty() && initial.error.is_empty() {
        state
            .native_steps
            .observed
            .insert(key, initial.lower.clone());
    }
    let pending_key = state
        .native_steps
        .pending
        .iter()
        .find_map(|(key, pending)| (pending.target == slot.target).then_some(*key));
    if let Some(pending_key) = pending_key {
        let pending = &state.native_steps.pending[&pending_key];
        let release = pending.life.load(std::sync::atomic::Ordering::SeqCst) == 2
            || (fresh
                && !initial.lower.is_empty()
                && initial.error.is_empty()
                && initial.lower == pending.proposed)
            || pending.action.as_ref().is_some_and(|id| {
                state
                    .actions
                    .iter()
                    .any(|(_, action)| action.id() == id && definitive_failure(&action.snapshot()))
            });
        if release {
            state.native_steps.pending.remove(&pending_key);
        } else {
            // A route can retire the original slot while this exact target is
            // still unresolved. New generations inherit its locked draft.
            let mut locked = initial.clone();
            locked.lower = pending.proposed.clone();
            locked.dirty = true;
            state.drafts.insert(key, locked);
            return;
        }
    }
    state.drafts.insert(key, initial.clone());
}
fn definitive_failure(snapshot: &home_client_actions::ActionSnapshot) -> bool {
    matches!(
        super::progress::presentation(Some(snapshot)).0,
        crate::ProActionPhase::Refused
            | crate::ProActionPhase::Failed
            | crate::ProActionPhase::Cancelled
    )
}
pub(super) fn edit_step(
    state: &mut super::State,
    generation: i32,
    field: i32,
    editor: super::panel::Editor,
    intent: crate::ProValueIntent,
    control: &Control,
) -> Result<Option<super::DeviceIntent>, super::Refusal> {
    use super::Refusal;
    if intent.event != crate::ProValueEvent::Step || intent.thumb != crate::RangeThumb::Lower {
        return Err(Refusal::Unsupported);
    }
    let key = (super::CardGeneration(generation), field);
    let observed = state
        .native_steps
        .observed
        .get(&key)
        .cloned()
        .ok_or(Refusal::Unavailable)?;
    if let Some(pending) = state.native_steps.pending.get(&key) {
        if pending.life.load(std::sync::atomic::Ordering::SeqCst) != 2 {
            return Err(Refusal::Capacity);
        }
        state.native_steps.pending.remove(&key);
        reset_draft(state, key, &observed);
    }
    let target = state
        .context
        .as_ref()
        .and_then(|context| context.slots.iter().find(|slot| slot.generation == key.0))
        .ok_or(Refusal::Stale)?
        .target
        .clone();
    if state.native_steps.pending.values().any(|pending| {
        pending.target == target && pending.life.load(std::sync::atomic::Ordering::SeqCst) != 2
    }) || state
        .actions
        .iter()
        .any(|(other, action)| other == &target && !action.is_finished())
    {
        return Err(Refusal::Capacity);
    }
    if state.native_steps.pending.len() >= super::MAX_ACTION_HISTORY {
        return Err(Refusal::Capacity);
    }
    let draft = state.drafts.get(&key).ok_or(Refusal::Stale)?;
    if draft.dirty || !draft.error.is_empty() || draft.lower != observed || !draft.upper.is_empty()
    {
        return Err(Refusal::Stale);
    }
    let mut candidate = draft.clone();
    editor.edit(&mut candidate, intent, control)?;
    if candidate.lower == observed {
        return Ok(None);
    }
    let submit = crate::ProValueIntent {
        event: crate::ProValueEvent::Submit,
        lower: candidate.lower.clone().into(),
        upper: candidate.upper.clone().into(),
        ..Default::default()
    };
    let result = editor.edit(&mut candidate, submit, control)?;
    state.native_steps.pending.insert(
        key,
        PendingStep {
            target,
            original: observed,
            proposed: candidate.lower.clone(),
            action: None,
            life: std::sync::Arc::new(std::sync::atomic::AtomicU8::new(0)),
        },
    );
    state.drafts.insert(key, candidate);
    Ok(result)
}
pub(super) fn attach_step(
    state: &mut super::State,
    target: &ActionTarget,
    id: &home_client_actions::CommandId,
) {
    for pending in state
        .native_steps
        .pending
        .values_mut()
        .filter(|pending| &pending.target == target && pending.action.is_none())
    {
        pending.action = Some(id.clone());
        pending.life.store(1, std::sync::atomic::Ordering::SeqCst);
    }
}
fn reset_draft(state: &mut super::State, key: StepKey, observed: &str) {
    if let Some(draft) = state.drafts.get_mut(&key) {
        draft.lower = observed.into();
        draft.upper.clear();
        draft.error.clear();
        draft.dirty = false;
    }
}
pub(super) fn abandon_step(state: &mut super::State, token: i32, generation: i32) {
    let Some(slot) = state
        .context
        .as_ref()
        .and_then(|context| {
            usize::try_from(token)
                .ok()
                .and_then(|index| context.slots.get(index))
        })
        .filter(|slot| slot.generation == super::CardGeneration(generation))
    else {
        return;
    };
    let generation = slot.generation;
    let keys = state
        .native_steps
        .pending
        .iter()
        .filter(|(key, pending)| key.0 == generation && pending.action.is_none())
        .map(|(key, _)| *key)
        .collect::<Vec<_>>();
    for key in keys {
        state.native_steps.pending.remove(&key);
        if let Some(observed) = state.native_steps.observed.get(&key).cloned() {
            reset_draft(state, key, &observed);
        }
    }
}

pub(super) struct Admitted {
    pub entity: EntityId,
    pub capability: usize,
    pub control: Control,
    pub title: String,
}
fn features(control: &Control) -> BTreeSet<FeatureKind> {
    let mut result = BTreeSet::new();
    match control {
        Control::Switch { power } => {
            if *power {
                result.insert(FeatureKind::Toggle);
            }
        }
        Control::Light {
            power,
            brightness,
            color,
        } => {
            if *power {
                result.insert(FeatureKind::Toggle);
            }
            if brightness.is_some() {
                result.insert(FeatureKind::Brightness);
            }
            if color.is_some() {
                result.insert(FeatureKind::ColorTemperature);
            }
        }
        Control::Climate(climate) => {
            if climate.setpoint().is_some() || climate.range().is_some() {
                result.insert(FeatureKind::TargetTemperature);
            }
            if !climate.modes().is_empty() {
                result.insert(FeatureKind::ClimateModes);
            }
        }
        Control::Fan(fan) => {
            if fan.can_power() {
                result.insert(FeatureKind::Toggle);
            }
            if !fan.speeds().is_empty() {
                result.insert(FeatureKind::FanSpeed);
            }
        }
        Control::Helper(helper) => match helper.kind() {
            Kind::Number(_) | Kind::Counter { .. } => {
                if helper.operations().contains(&HelperOperation::Set) {
                    result.insert(FeatureKind::NumericInput);
                }
            }
            Kind::Boolean => {
                if helper.operations().iter().any(|operation| {
                    [HelperOperation::Set, HelperOperation::Toggle].contains(operation)
                }) {
                    result.insert(FeatureKind::Toggle);
                }
            }
            Kind::Select(_)
            | Kind::Text { .. }
            | Kind::Date
            | Kind::Time
            | Kind::DateTime
            | Kind::Button => {}
        },
        Control::Cover(cover) => {
            if !cover.operations().is_empty() {
                result.insert(FeatureKind::CoverOpenClose);
            }
            if cover.position().is_some() {
                result.insert(FeatureKind::CoverPosition);
            }
        }
        Control::Alarm(alarm) => {
            if !alarm.operations().is_empty() {
                result.insert(FeatureKind::AlarmModes);
            }
        }
        Control::Lock(_)
        | Control::Binary
        | Control::Measurement
        | Control::ReadOnly(_)
        | Control::Unsupported => {}
    }
    result
}
fn restrict(control: &Control, node: &Node) -> Option<Control> {
    if node.kind == NodeKind::Widget {
        return match (&node.widget.as_ref()?.content, control) {
            (crate::dashboard_widgets::Content::Thermostat { .. }, Control::Climate(climate))
                if climate.setpoint().is_some() =>
            {
                Some(Control::Climate(climate.restricted(true, false)))
            }
            _ => None,
        };
    }
    match node.recipe_kind {
        Some(RecipeKind::Select) => {
            let valid = match control {
                Control::Helper(helper) => match helper.kind() {
                    Kind::Select(_) => true,
                    Kind::Boolean
                    | Kind::Number(_)
                    | Kind::Counter { .. }
                    | Kind::Text { .. }
                    | Kind::Date
                    | Kind::Time
                    | Kind::DateTime
                    | Kind::Button => false,
                },
                Control::Switch { .. }
                | Control::Light { .. }
                | Control::Climate(_)
                | Control::Fan(_)
                | Control::Binary
                | Control::Measurement
                | Control::Cover(_)
                | Control::Lock(_)
                | Control::Alarm(_)
                | Control::ReadOnly(_)
                | Control::Unsupported => false,
            };
            if !valid {
                return None;
            }
        }
        Some(RecipeKind::Alarm) => {
            if !matches!(control, Control::Alarm(_)) {
                return None;
            }
        }
        Some(
            RecipeKind::Template
            | RecipeKind::BubbleButton
            | RecipeKind::BubblePopup
            | RecipeKind::Chips
            | RecipeKind::TemplateChip
            | RecipeKind::Button
            | RecipeKind::Entity
            | RecipeKind::Text,
        )
        | None => {}
    }
    let supported = features(control);
    if node
        .features
        .iter()
        .any(|feature| !supported.contains(feature))
    {
        return None;
    }
    // No explicit feature selection keeps the existing native equivalent.
    // A Select recipe still narrows operations to its selector's Set intent.
    if node.features.is_empty() {
        return Some(match control {
            Control::Helper(helper) if node.recipe_kind == Some(RecipeKind::Select) => {
                Control::Helper(helper.restricted_to(&[HelperOperation::Set]))
            }
            Control::Switch { .. }
            | Control::Light { .. }
            | Control::Climate(_)
            | Control::Fan(_)
            | Control::Helper(_)
            | Control::Binary
            | Control::Measurement
            | Control::Cover(_)
            | Control::Lock(_)
            | Control::Alarm(_)
            | Control::ReadOnly(_)
            | Control::Unsupported => control.clone(),
        });
    }
    let has = |feature| node.features.contains(&feature);
    Some(match control {
        Control::Switch { power } => Control::Switch {
            power: *power && has(FeatureKind::Toggle),
        },
        Control::Light {
            power,
            brightness,
            color,
        } => Control::Light {
            power: *power && has(FeatureKind::Toggle),
            brightness: brightness.filter(|_| has(FeatureKind::Brightness)),
            color: color.clone().filter(|_| has(FeatureKind::ColorTemperature)),
        },
        Control::Climate(climate) => Control::Climate(climate.restricted(
            has(FeatureKind::TargetTemperature),
            has(FeatureKind::ClimateModes),
        )),
        Control::Fan(fan) => {
            Control::Fan(fan.restricted(has(FeatureKind::Toggle), has(FeatureKind::FanSpeed)))
        }
        Control::Helper(helper) => {
            Control::Helper(helper.restricted_to(if has(FeatureKind::Toggle) {
                &[HelperOperation::Set, HelperOperation::Toggle]
            } else {
                &[HelperOperation::Set]
            }))
        }
        Control::Cover(cover) => Control::Cover(cover.restricted(
            has(FeatureKind::CoverOpenClose),
            has(FeatureKind::CoverPosition),
        )),
        Control::Alarm(alarm) => Control::Alarm(alarm.clone()),
        Control::Lock(_)
        | Control::Binary
        | Control::Measurement
        | Control::ReadOnly(_)
        | Control::Unsupported => {
            return None;
        }
    })
}
fn inert(node: &mut Node, reason: &str) {
    node.kind = NodeKind::Unsupported;
    node.diagnostic = reason.into();
    node.entity = None;
    node.recipes = None;
    node.recipe_kind = None;
    node.features.clear();
    node.widget = None;
}
pub(super) fn admit(nodes: &mut [Node], view: Option<&CachedView>) -> BTreeMap<NodeId, Admitted> {
    let mut result = BTreeMap::new();
    let Some(view) = view else {
        return result;
    };
    for node in nodes {
        if !node.device_card() {
            continue;
        }
        let Some(id) = node.entity.clone() else {
            continue;
        };
        let Some(entity) = view.get(&id) else {
            inert(node, "No authorized observation for this item.");
            continue;
        };
        let mut candidates =
            entity
                .capabilities
                .iter()
                .enumerate()
                .filter_map(|(index, capability)| {
                    ActionTarget::new(id.clone(), &capability.schema_id, &capability.endpoint)?;
                    let control = Control::parse(capability);
                    if matches!(control, Control::Unsupported) {
                        return None;
                    }
                    restrict(&control, node).map(|control| (index, control))
                });
        let first = candidates.next();
        let ambiguous = candidates.next().is_some();
        match first {
            Some((capability, control)) if !ambiguous => {
                result.insert(
                    node.id.clone(),
                    Admitted {
                        entity: id,
                        capability,
                        control,
                        title: if node.title.is_empty() {
                            crate::cache_view::bounded(&entity.current_alias)
                        } else {
                            node.title.clone()
                        },
                    },
                );
            }
            Some(_) => inert(
                node,
                "Several capabilities match this card; no target is chosen implicitly.",
            ),
            None => inert(
                node,
                "The configured family or features do not match an admitted native capability.",
            ),
        }
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::dashboard::LayoutState;
    use crate::dashboard_layout::NativeLayout;
    use crate::device_view::{
        DeviceIntent, DeviceStore, Power, Refusal,
        tests::{light, request, view},
    };
    use home_client_cache::CachePhase;
    use std::sync::Arc;
    fn fixture() -> (
        crate::dashboard::ProjectionRequest,
        home_wire_contract::home::state::v1::ViewEntity,
    ) {
        let mut request = request();
        let entity = light(&request);
        let mut layout = request.dashboard.layout().unwrap().clone();
        let mut native =
            NativeLayout::try_from(&crate::dashboard_layout::tests::extended_forest()).unwrap();
        for node in native.nodes.values_mut() {
            node.recipes = None;
            if node.entity.is_some() {
                node.entity = Some(entity.entity_id.parse().unwrap());
            }
        }
        native
            .nodes
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap()
            .features = vec![FeatureKind::Brightness];
        let mut toggle = native.nodes[&NodeId::parse("tile").unwrap()].clone();
        toggle.id = NodeId::parse("toggle-only").unwrap();
        toggle.source.path = "view.toggle-only".into();
        toggle.features = vec![FeatureKind::Toggle];
        native
            .nodes
            .get_mut(&NodeId::parse("view").unwrap())
            .unwrap()
            .children
            .push(toggle.id.clone());
        native.nodes.insert(toggle.id.clone(), toggle);
        layout.native = Some(native);
        request.room = None;
        request.dashboard = LayoutState::Ready(Arc::new(layout));
        (request, entity)
    }
    fn thermostat_fixture() -> (
        crate::dashboard::ProjectionRequest,
        home_wire_contract::home::state::v1::ViewEntity,
    ) {
        use crate::dashboard_widgets::{Content, Field, FieldKey, Widget};
        use home_wire_contract::home::state::v1 as state;
        let mut entity = crate::device_view::tests::climate_entity(&request());
        entity.state.as_option_mut().unwrap().quality = state::Quality {
            availability: state::Availability::Available.into(),
            freshness: state::Freshness::Fresh.into(),
            provenance: state::Provenance::Observed.into(),
            ..Default::default()
        }
        .into();
        let (mut request, _) = fixture();
        let mut layout = request.dashboard.layout().unwrap().clone();
        let native = layout.native.as_mut().unwrap();
        native.version = 3;
        let id: EntityId = entity.entity_id.parse().unwrap();
        let widget = native
            .nodes
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap();
        widget.kind = NodeKind::Widget;
        widget.features.clear();
        widget.widget = Some(Widget {
            content: Content::Thermostat {
                detailed: false,
                summary_only: false,
                entity: id.clone(),
                current: Field {
                    entity: id.clone(),
                    key: FieldKey::CurrentTemperature,
                },
                target: Field {
                    entity: id,
                    key: FieldKey::Setpoint,
                },
            },
            action: None,
            action_label: String::new(),
            empty_label: String::new(),
        });
        let ordinary = native
            .nodes
            .get_mut(&NodeId::parse("toggle-only").unwrap())
            .unwrap();
        ordinary.features = vec![FeatureKind::TargetTemperature];
        request.dashboard = LayoutState::Ready(Arc::new(layout));
        (request, entity)
    }
    fn step() -> crate::ProValueIntent {
        crate::ProValueIntent {
            event: crate::ProValueEvent::Step,
            thumb: crate::RangeThumb::Lower,
            direction: crate::StepDirection::Increment,
            lower: "999".into(),
            ..Default::default()
        }
    }
    #[test]
    fn thermostat_step_submits_exactly_and_preserves_observations_and_form_drafts() {
        use home_wire_contract::home::actions::v1 as actions;
        let (request, entity) = thermostat_fixture();
        let store = DeviceStore::default();
        let capture = view(&request, vec![entity]);
        let projected = store.project(CachePhase::Live, Some(&capture), &request);
        let row = &projected.native_devices[&NodeId::parse("tile").unwrap()];
        let ordinary = &projected.native_devices[&NodeId::parse("toggle-only").unwrap()];
        assert!(
            store
                .edit(ordinary.token, ordinary.generation, 0, 0, step())
                .unwrap()
                .is_none(),
            "ordinary controls keep their draft/Apply behavior"
        );
        let intent = store
            .edit(row.token, row.generation, 0, 0, step())
            .unwrap()
            .unwrap();
        let prepared = store.prepare(row.token, row.generation, 0, intent).unwrap();
        assert!((prepared.guard)());
        assert_eq!(prepared.request.capability_schema_id, "core.climate.v1");
        assert_eq!(prepared.request.endpoint, "lamp");
        assert_eq!(
            prepared.target.entity().as_str(),
            capture.get(prepared.target.entity()).unwrap().entity_id
        );
        assert!(
            matches!(prepared.request.arguments.arguments.as_ref(), Some(actions::action_args::Arguments::SetClimate(value)) if value.setpoint_celsius == Some(20.5))
        );
        assert!(matches!(
            store.edit(row.token, row.generation, 0, 0, step()),
            Err(Refusal::Capacity)
        ));
        let updated = store.project(CachePhase::Live, Some(&capture), &request);
        let updated = &updated.native_devices[&NodeId::parse("tile").unwrap()];
        assert!(updated.readings.contains("20 °C"));
        assert_eq!(updated.values[0].lower, "20.5");
        assert!(
            store.0.lock().unwrap().drafts[&(super::super::CardGeneration(row.generation), 0)]
                .dirty
        );
        drop(prepared);
        let released = store.project(CachePhase::Live, Some(&capture), &request);
        assert_eq!(
            released.native_devices[&NodeId::parse("tile").unwrap()].values[0].lower,
            "20"
        );
        assert!(
            store
                .edit(row.token, row.generation, 0, 0, step())
                .unwrap()
                .is_some(),
            "a definite non-send permits a new explicit gesture"
        );
        store.abandon_native_step(row.token, row.generation);
    }
    #[test]
    fn thermostat_step_does_not_release_uncertain_timeout_or_stopped_tracking() {
        use home_client_actions::{ActionSnapshot, LiveState, SubmissionState};
        use home_wire_contract::home::actions::v1 as wire;
        let mut snapshot = ActionSnapshot {
            id: "synthetic-step".parse().unwrap(),
            submission: SubmissionState::Uncertain,
            live: LiveState::NotRequested,
            response: None,
            failure: None,
            durable: None,
            progress: None,
            issue: None,
        };
        assert!(!definitive_failure(&snapshot));
        for live in [
            LiveState::Unavailable,
            LiveState::Interrupted,
            LiveState::Stopped,
        ] {
            snapshot.live = live;
            assert!(!definitive_failure(&snapshot));
        }
        snapshot.live = LiveState::Available;
        for status in [
            wire::ActionStatus::Indeterminate,
            wire::ActionStatus::Acknowledged,
            wire::ActionStatus::Confirmed,
        ] {
            snapshot.progress = Some(wire::ActionProgress {
                status: status.into(),
                ..Default::default()
            });
            assert!(!definitive_failure(&snapshot));
        }
        for status in [wire::ActionStatus::Failed, wire::ActionStatus::Cancelled] {
            snapshot.progress = Some(wire::ActionProgress {
                status: status.into(),
                ..Default::default()
            });
            assert!(definitive_failure(&snapshot));
        }
        snapshot.progress = None;
        snapshot.submission = SubmissionState::NotSent;
        assert!(definitive_failure(&snapshot));
    }
    #[test]
    fn thermostat_step_requires_fresh_exact_generation_and_reconciles_only_observed_readback() {
        use home_wire_contract::home::state::v1 as state;
        let (request, mut entity) = thermostat_fixture();
        let store = DeviceStore::default();
        let projected = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        let row = &projected.native_devices[&NodeId::parse("tile").unwrap()];
        assert!(matches!(
            store.edit(row.token, row.generation + 1, 0, 0, step()),
            Err(Refusal::Stale)
        ));
        assert!(matches!(
            store.edit(row.token, row.generation, 0, 1, step()),
            Err(Refusal::Unsupported)
        ));
        let intent = store
            .edit(row.token, row.generation, 0, 0, step())
            .unwrap()
            .unwrap();
        let prepared = store.prepare(row.token, row.generation, 0, intent).unwrap();
        {
            let mut state = store.0.lock().unwrap();
            attach_step(
                &mut state,
                &prepared.target,
                &"synthetic-step".parse().unwrap(),
            );
        }
        drop(prepared);
        store.abandon_native_step(row.token, row.generation);
        store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        assert!(
            store.0.lock().unwrap().drafts[&(super::super::CardGeneration(row.generation), 0)]
                .dirty,
            "accepted or ambiguous delivery cannot be reset by dropping the guard or cancelling preparation"
        );
        assert!(matches!(
            store.edit(row.token, row.generation, 0, 0, step()),
            Err(Refusal::Capacity)
        ));
        let Some(state::observed_state::Value::Climate(value)) =
            entity.state.as_option_mut().unwrap().value.as_mut()
        else {
            panic!()
        };
        value.setpoint = Some(state::climate_value::Setpoint::SetpointCelsius(20.5));
        let reconciled = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        assert_eq!(
            reconciled.native_devices[&NodeId::parse("tile").unwrap()].values[0].lower,
            "20.5"
        );
        assert!(
            !store.0.lock().unwrap().drafts[&(super::super::CardGeneration(row.generation), 0)]
                .dirty
        );
        entity
            .state
            .as_option_mut()
            .unwrap()
            .quality
            .as_option_mut()
            .unwrap()
            .freshness = state::Freshness::Stale.into();
        store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        assert!(matches!(
            store.edit(row.token, row.generation, 0, 0, step()),
            Err(Refusal::Unavailable)
        ));
        assert!(store.0.lock().unwrap().native_steps.pending.is_empty());
    }
    #[test]
    fn thermostat_unresolved_target_survives_route_generation_retirement() {
        use home_wire_contract::home::state::v1 as state;
        let (mut request, mut entity) = thermostat_fixture();
        let store = DeviceStore::default();
        let first = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        let first = &first.native_devices[&NodeId::parse("tile").unwrap()];
        let intent = store
            .edit(first.token, first.generation, 0, 0, step())
            .unwrap()
            .unwrap();
        let prepared = store
            .prepare(first.token, first.generation, 0, intent)
            .unwrap();
        attach_step(
            &mut store.0.lock().unwrap(),
            &prepared.target,
            &"synthetic-uncertain-step".parse().unwrap(),
        );
        drop(prepared);
        // Leaving the view retires every original card generation.
        request.room = Some(crate::dashboard::fixture().rooms[0].id.clone());
        request.revision += 1;
        store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        request.room = None;
        request.revision += 1;
        let returned = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        let returned = &returned.native_devices[&NodeId::parse("tile").unwrap()];
        assert_ne!(first.generation, returned.generation);
        assert_eq!(returned.values[0].lower, "20.5");
        assert!(matches!(
            store.edit(
                returned.token,
                returned.generation,
                request.revision,
                0,
                step()
            ),
            Err(Refusal::Capacity)
        ));
        store.abandon_native_step(returned.token, returned.generation);
        assert_eq!(store.0.lock().unwrap().native_steps.pending.len(), 1);
        let Some(state::observed_state::Value::Climate(value)) =
            entity.state.as_option_mut().unwrap().value.as_mut()
        else {
            panic!()
        };
        value.setpoint = Some(state::climate_value::Setpoint::SetpointCelsius(20.5));
        store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        assert!(store.0.lock().unwrap().native_steps.pending.is_empty());
        assert!(
            store
                .edit(
                    returned.token,
                    returned.generation,
                    request.revision,
                    0,
                    step()
                )
                .unwrap()
                .is_some()
        );
        store.clear_private();
        assert!(store.0.lock().unwrap().native_steps.pending.is_empty());
        assert!(store.0.lock().unwrap().native_steps.observed.is_empty());
    }
    #[test]
    fn duplicate_entity_cards_admit_only_their_own_features_and_keep_distinct_bindings() {
        let (mut request, mut entity) = fixture();
        let store = DeviceStore::default();
        let projection = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        let brightness = projection.native_devices[&NodeId::parse("tile").unwrap()].clone();
        let toggle = projection.native_devices[&NodeId::parse("toggle-only").unwrap()].clone();
        assert_ne!(brightness.token, toggle.token);
        assert_ne!(brightness.generation, toggle.generation);
        assert!(brightness.can_brightness && !brightness.can_power);
        assert!(toggle.can_power && !toggle.can_brightness);
        assert!(
            store
                .prepare(
                    brightness.token,
                    brightness.generation,
                    0,
                    DeviceIntent::Power(Power::On)
                )
                .is_err()
        );
        assert!(
            store
                .prepare(
                    toggle.token,
                    toggle.generation,
                    0,
                    DeviceIntent::Brightness(50.)
                )
                .is_err()
        );
        let prepared = store
            .prepare(
                brightness.token,
                brightness.generation,
                0,
                DeviceIntent::Brightness(50.),
            )
            .unwrap();
        assert!(
            store
                .prepare(
                    toggle.token,
                    toggle.generation,
                    0,
                    DeviceIntent::Power(Power::On)
                )
                .is_ok()
        );
        entity.state.as_option_mut().unwrap().revision += 1;
        store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        assert!(
            (prepared.guard)(),
            "an observation does not change the feature admission"
        );
        let mut layout = request.dashboard.layout().unwrap().clone();
        layout.document_revision += 1;
        layout.document_hash = [8; 32];
        layout
            .native
            .as_mut()
            .unwrap()
            .nodes
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap()
            .features = vec![FeatureKind::Toggle];
        request.dashboard = LayoutState::Ready(Arc::new(layout));
        let changed = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        assert!(
            !(prepared.guard)(),
            "a feature change withdraws the held command"
        );
        let changed = &changed.native_devices[&NodeId::parse("tile").unwrap()];
        assert!(changed.can_power && !changed.can_brightness);
        assert!(matches!(
            store.prepare(
                brightness.token,
                brightness.generation,
                0,
                DeviceIntent::Power(Power::On)
            ),
            Err(Refusal::Stale)
        ));
        assert!(
            store
                .prepare(
                    changed.token,
                    changed.generation,
                    0,
                    DeviceIntent::Brightness(50.)
                )
                .is_err()
        );
        assert!(store.0.lock().unwrap().actions.is_empty());
    }
    #[test]
    fn a_capability_removed_during_a_gesture_inerts_only_its_incompatible_card() {
        use home_wire_contract::home::registry::v1::capability_description::Constraints;
        let (request, mut entity) = fixture();
        let store = DeviceStore::default();
        let projection = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        let brightness = &projection.native_devices[&NodeId::parse("tile").unwrap()];
        let prepared = store
            .prepare(
                brightness.token,
                brightness.generation,
                0,
                DeviceIntent::Brightness(50.),
            )
            .unwrap();
        let Some(Constraints::Light(constraints)) = entity.capabilities[0].constraints.as_mut()
        else {
            panic!()
        };
        constraints.brightness_percent = Default::default();
        let changed = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        assert!(!(prepared.guard)());
        assert!(
            !changed
                .native_devices
                .contains_key(&NodeId::parse("tile").unwrap())
        );
        let diagnostic = changed
            .native_nodes
            .iter()
            .find(|node| node.id.as_str() == "tile")
            .unwrap();
        assert_eq!(diagnostic.kind, NodeKind::Unsupported);
        assert!(diagnostic.recipes.is_none());
        assert!(diagnostic.entity.is_none());
        assert_eq!(diagnostic.source.file, "synthetic.yaml");
        assert!(!diagnostic.diagnostic.is_empty());
        let toggle = &changed.native_devices[&NodeId::parse("toggle-only").unwrap()];
        assert!(
            store
                .prepare(
                    toggle.token,
                    toggle.generation,
                    0,
                    DeviceIntent::Power(Power::Off)
                )
                .is_ok()
        );
    }
    #[test]
    fn select_recipes_refuse_foreign_families_and_ambiguous_capabilities_never_choose_an_endpoint()
    {
        let (mut request, mut entity) = fixture();
        let mut layout = request.dashboard.layout().unwrap().clone();
        let tile = layout
            .native
            .as_mut()
            .unwrap()
            .nodes
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap();
        tile.kind = NodeKind::Recipe;
        tile.recipe_kind = Some(RecipeKind::Select);
        tile.features.clear();
        request.dashboard = LayoutState::Ready(Arc::new(layout));
        let store = DeviceStore::default();
        let projection = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        assert!(
            !projection
                .native_devices
                .contains_key(&NodeId::parse("tile").unwrap())
        );
        let mut duplicate = entity.capabilities[0].clone();
        duplicate.endpoint = "other-endpoint".into();
        entity.capabilities.push(duplicate);
        let projection = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        assert!(projection.native_devices.is_empty());
        assert_eq!(
            projection
                .native_nodes
                .iter()
                .filter(|node| node.device_card())
                .count(),
            0
        );
        assert!(
            projection
                .native_nodes
                .iter()
                .find(|node| node.id.as_str() == "toggle-only")
                .unwrap()
                .diagnostic
                .contains("Several capabilities")
        );
    }

    #[test]
    fn climate_and_fan_feature_selections_narrow_the_actual_argument_builder() {
        use crate::device_view::{
            climate::{ClimateIntent, Fan},
            number::NumericInput,
            tests::climate_entity,
        };
        use home_wire_contract::home::{registry::v1 as registry, state::v1 as state};
        let (request, _) = fixture();
        let mut node = request
            .dashboard
            .layout()
            .unwrap()
            .native
            .as_ref()
            .unwrap()
            .nodes[&NodeId::parse("tile").unwrap()]
            .clone();
        let entity = climate_entity(&crate::device_view::tests::request());
        let full = Control::parse(&entity.capabilities[0]);
        let Control::Climate(climate) = &full else {
            panic!()
        };
        let mode = climate.modes()[0];
        node.features = vec![FeatureKind::ClimateModes];
        let modes = restrict(&full, &node).unwrap();
        assert!(
            modes
                .arguments(DeviceIntent::Climate(ClimateIntent::Mode(mode)), None)
                .is_ok()
        );
        assert!(
            modes
                .arguments(
                    DeviceIntent::Climate(ClimateIntent::Setpoint(NumericInput::Text("20".into()))),
                    None
                )
                .is_err()
        );
        node.features = vec![FeatureKind::TargetTemperature];
        let temperature = restrict(&full, &node).unwrap();
        assert!(
            temperature
                .arguments(DeviceIntent::Climate(ClimateIntent::Mode(mode)), None)
                .is_err()
        );
        assert!(
            temperature
                .arguments(
                    DeviceIntent::Climate(ClimateIntent::Setpoint(NumericInput::Text("20".into()))),
                    None
                )
                .is_ok()
        );
        let full = Control::Fan(
            Fan::parse(&registry::FanConstraints {
                set_on: true,
                speed_options: vec!["Low".into(), "High".into()],
                ..Default::default()
            })
            .unwrap(),
        );
        node.features = vec![FeatureKind::FanSpeed];
        let speed = restrict(&full, &node).unwrap();
        assert!(
            speed
                .arguments(DeviceIntent::FanSpeed(state::FanSpeed::High), None)
                .is_ok()
        );
        assert!(
            speed
                .arguments(DeviceIntent::Power(Power::On), None)
                .is_err()
        );
        node.features = vec![FeatureKind::Toggle];
        let power = restrict(&full, &node).unwrap();
        assert!(
            power
                .arguments(DeviceIntent::FanSpeed(state::FanSpeed::High), None)
                .is_err()
        );
        assert!(
            power
                .arguments(DeviceIntent::Power(Power::On), None)
                .is_ok()
        );
        node.features = vec![FeatureKind::FanSpeed, FeatureKind::Brightness];
        assert!(
            restrict(&full, &node).is_none(),
            "an incompatible feature makes the whole selection inert"
        );
    }
    #[test]
    fn select_and_numeric_helper_cards_keep_their_typed_family_and_cas() {
        use crate::device_view::tests::helper_entity;
        use home_wire_contract::home::{actions::v1::action_args::Arguments, helpers::v1 as h};
        for kind in [h::HelperKind::Select, h::HelperKind::Number] {
            let (mut request, _) = fixture();
            let entity = helper_entity(&crate::device_view::tests::request(), kind);
            let mut layout = request.dashboard.layout().unwrap().clone();
            let tile = layout
                .native
                .as_mut()
                .unwrap()
                .nodes
                .get_mut(&NodeId::parse("tile").unwrap())
                .unwrap();
            match kind {
                h::HelperKind::Select => {
                    tile.kind = NodeKind::Recipe;
                    tile.recipe_kind = Some(RecipeKind::Select);
                    tile.features.clear();
                }
                h::HelperKind::Number => {
                    tile.features = vec![FeatureKind::NumericInput];
                }
                h::HelperKind::Unspecified
                | h::HelperKind::Boolean
                | h::HelperKind::Counter
                | h::HelperKind::Text
                | h::HelperKind::Date
                | h::HelperKind::Time
                | h::HelperKind::Datetime
                | h::HelperKind::ButtonPress => unreachable!(),
            }
            request.dashboard = LayoutState::Ready(Arc::new(layout));
            let store = DeviceStore::default();
            let projection = store.project(
                CachePhase::Live,
                Some(&view(&request, vec![entity.clone()])),
                &request,
            );
            let row = &projection.native_devices[&NodeId::parse("tile").unwrap()];
            assert_eq!(row.values.len(), 1);
            assert!(row.choices.is_empty());
            assert!(
                store
                    .prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On))
                    .is_err()
            );
            let intent = match kind {
                h::HelperKind::Select => {
                    crate::device_view::helper::HelperIntent::Select("custom B".into())
                }
                h::HelperKind::Number => crate::device_view::helper::HelperIntent::Number(
                    crate::device_view::number::NumericInput::Text("2".into()),
                ),
                h::HelperKind::Unspecified
                | h::HelperKind::Boolean
                | h::HelperKind::Counter
                | h::HelperKind::Text
                | h::HelperKind::Date
                | h::HelperKind::Time
                | h::HelperKind::Datetime
                | h::HelperKind::ButtonPress => unreachable!(),
            };
            let prepared = store
                .prepare(row.token, row.generation, 0, DeviceIntent::Helper(intent))
                .unwrap();
            let Some(Arguments::Helper(arguments)) =
                &prepared.request.arguments.as_option().unwrap().arguments
            else {
                panic!()
            };
            assert_eq!(arguments.expected_helper_revision, Some(7));
        }
    }

    #[test]
    fn color_only_controls_keep_canonical_units_and_the_configured_header_command_is_independent() {
        use crate::device_view::{GestureAccess, NativeIntent};
        use crate::{ProValueEvent, ProValueIntent};
        use home_client_model::dashboard::{CommandKind, Gesture, Intention, Interaction, Recipes};
        use home_wire_contract::home::{
            actions::v1::action_args::Arguments, registry::v1 as registry,
        };
        let (mut request, mut entity) = fixture();
        let Some(registry::capability_description::Constraints::Light(capability)) =
            entity.capabilities[0].constraints.as_mut()
        else {
            panic!()
        };
        capability.color_temp_mireds = registry::NumberBounds {
            minimum: 153.,
            maximum: 500.,
            step: Some(1.),
            ..Default::default()
        }
        .into();
        let mut layout = request.dashboard.layout().unwrap().clone();
        let node = layout
            .native
            .as_mut()
            .unwrap()
            .nodes
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap();
        node.features = vec![FeatureKind::ColorTemperature];
        node.recipes = Some(Recipes {
            interactions: BTreeMap::from([(
                Gesture::Tap,
                Interaction {
                    intention: Intention::Command {
                        entity: entity.entity_id.parse().unwrap(),
                        kind: CommandKind::TurnOff,
                    },
                    confirmation: None,
                    source: node.source.clone(),
                },
            )]),
            visibility: None,
        });
        request.dashboard = LayoutState::Ready(Arc::new(layout));
        let store = DeviceStore::default();
        let projection = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        let row = &projection.native_devices[&NodeId::parse("tile").unwrap()];
        assert!(!row.can_power && !row.can_brightness);
        assert_eq!(row.values.len(), 1);
        assert!(
            store
                .prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On))
                .is_err()
        );
        let intent = store
            .edit(
                row.token,
                row.generation,
                0,
                0,
                ProValueIntent {
                    event: ProValueEvent::Submit,
                    lower: "4000".into(),
                    ..Default::default()
                },
            )
            .unwrap()
            .unwrap();
        let command = store.prepare(row.token, row.generation, 0, intent).unwrap();
        let Some(Arguments::SetLight(arguments)) =
            &command.request.arguments.as_option().unwrap().arguments
        else {
            panic!()
        };
        assert_eq!(arguments.color_temp_mireds, Some(250));
        assert!(arguments.on.is_none() && arguments.brightness_percent.is_none());
        let header = &projection.recipes[&NodeId::parse("tile").unwrap()];
        assert_eq!(header.access(Gesture::Tap), GestureAccess::Command);
        let command = store
            .prepare_recipe(header.token, header.generation, 0, Gesture::Tap)
            .unwrap();
        let NativeIntent::Command(command) = command.intent else {
            panic!()
        };
        let Some(Arguments::SetLight(arguments)) =
            &command.request.arguments.as_option().unwrap().arguments
        else {
            panic!()
        };
        assert_eq!(arguments.on, Some(false));
        assert!(arguments.color_temp_mireds.is_none());
        assert!(store.0.lock().unwrap().actions.is_empty());
    }
}
