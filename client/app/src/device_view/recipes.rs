//! Native recipe bindings share the device store's admission context. UI row
//! positions carry no entity, destination, permission or command authority.
use super::{
    CardGeneration, CardToken, Context, DeviceIntent, DeviceStore, Power, Prepared, Refusal, State,
    capability::Control, helper,
};
use crate::dashboard::Layout;
use crate::dashboard_layout::{Kind, Node, NodeId};
use home_client_actions::{AdmissionCheck, WriteAccess};
use home_client_cache::{CachePhase, CachedView};
use home_client_model::{
    EntityId,
    dashboard::{CommandKind, Destination, Gesture, Intention, Interaction, Presentation, Source},
};
use home_wire_contract::home::helpers::v1::HelperOperation;
use std::{
    collections::BTreeMap,
    sync::Arc,
    time::{Duration, Instant},
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum GestureAccess {
    Absent,
    Inactive,
    Local,
    Command,
}

#[derive(Clone, Debug)]
pub(crate) struct RecipePresentation {
    pub token: i32,
    pub generation: i32,
    pub gestures: BTreeMap<Gesture, GestureAccess>,
    pub unavailable: bool,
}
impl RecipePresentation {
    pub fn access(&self, gesture: Gesture) -> GestureAccess {
        self.gestures
            .get(&gesture)
            .copied()
            .unwrap_or(GestureAccess::Absent)
    }
}

pub(crate) enum NativeIntent {
    MoreInfo(EntityId),
    Navigate(Destination),
    Command(Box<Prepared>),
}
pub(crate) struct NativePrepared {
    pub intent: NativeIntent,
    pub confirmation: Option<String>,
    pub title: String,
    pub description: String,
    pub guard: AdmissionCheck,
    pub layout: Arc<Layout>,
    pub origin: NodeId,
    expires: Instant,
}
impl NativePrepared {
    pub fn is_current(&self) -> bool {
        Instant::now() < self.expires && (self.guard)()
    }
}

#[derive(Clone, PartialEq, Eq)]
enum BoundIntent {
    None,
    Unavailable(Refusal),
    MoreInfo(EntityId),
    Navigate(Destination),
    Command {
        token: CardToken,
        generation: CardGeneration,
        kind: CommandKind,
    },
}
#[derive(Clone, PartialEq, Eq)]
struct BoundInteraction {
    intent: BoundIntent,
    confirmation: Option<String>,
    source: Source,
}
impl BoundInteraction {
    fn access(&self) -> GestureAccess {
        match &self.intent {
            BoundIntent::None | BoundIntent::Unavailable(_) => GestureAccess::Inactive,
            BoundIntent::MoreInfo(_) | BoundIntent::Navigate(_) => GestureAccess::Local,
            BoundIntent::Command { .. } => GestureAccess::Command,
        }
    }
}
#[derive(Clone, PartialEq, Eq)]
struct Binding {
    node: NodeId,
    generation: CardGeneration,
    interactions: BTreeMap<Gesture, BoundInteraction>,
}
struct Snapshot {
    fence: Context,
    bindings: Vec<Binding>,
    power: BTreeMap<CardToken, bool>,
    titles: Vec<String>,
    labels: BTreeMap<EntityId, String>,
}
#[derive(Default)]
pub(super) struct Bindings {
    snapshot: Option<Snapshot>,
}

impl Control {
    fn recipe_intent(
        &self,
        kind: CommandKind,
        power: Option<bool>,
    ) -> Result<DeviceIntent, Refusal> {
        match kind {
            CommandKind::Toggle => match self {
                Self::Switch { .. } | Self::Light { .. } | Self::Fan(_) => power
                    .map(|on| DeviceIntent::Power(if on { Power::Off } else { Power::On }))
                    .ok_or(Refusal::Unavailable),
                Self::Helper(helper) => {
                    if matches!(helper.kind(), helper::Kind::Boolean) {
                        Ok(DeviceIntent::Helper(helper::HelperIntent::Operation(
                            HelperOperation::Toggle,
                        )))
                    } else {
                        Err(Refusal::Unsupported)
                    }
                }
                Self::Climate(_)
                | Self::Cover(_)
                | Self::Lock(_)
                | Self::Alarm(_)
                | Self::Binary
                | Self::Measurement
                | Self::ReadOnly(_)
                | Self::Unsupported => Err(Refusal::Unsupported),
            },
            CommandKind::TurnOn | CommandKind::TurnOff => {
                let on = kind == CommandKind::TurnOn;
                match self {
                    Self::Switch { .. } | Self::Light { .. } | Self::Fan(_) => {
                        Ok(DeviceIntent::Power(if on { Power::On } else { Power::Off }))
                    }
                    Self::Helper(helper) => {
                        if matches!(helper.kind(), helper::Kind::Boolean) {
                            Ok(DeviceIntent::Helper(helper::HelperIntent::Boolean(on)))
                        } else {
                            Err(Refusal::Unsupported)
                        }
                    }
                    Self::Climate(_)
                    | Self::Cover(_)
                    | Self::Lock(_)
                    | Self::Alarm(_)
                    | Self::Binary
                    | Self::Measurement
                    | Self::ReadOnly(_)
                    | Self::Unsupported => Err(Refusal::Unsupported),
                }
            }
            CommandKind::Activate => match self {
                Self::Helper(helper) => {
                    if matches!(helper.kind(), helper::Kind::Button) {
                        Ok(DeviceIntent::Helper(helper::HelperIntent::Operation(
                            HelperOperation::Press,
                        )))
                    } else {
                        Err(Refusal::Unsupported)
                    }
                }
                Self::Switch { .. }
                | Self::Light { .. }
                | Self::Climate(_)
                | Self::Fan(_)
                | Self::Binary
                | Self::Measurement
                | Self::Cover(_)
                | Self::Lock(_)
                | Self::Alarm(_)
                | Self::ReadOnly(_)
                | Self::Unsupported => Err(Refusal::Unsupported),
            },
            CommandKind::CoverOpen => Ok(DeviceIntent::Cover(
                super::controls::CoverIntent::Operation(
                    home_wire_contract::home::controls::v1::CoverOperation::Open,
                ),
            )),
            CommandKind::CoverClose => Ok(DeviceIntent::Cover(
                super::controls::CoverIntent::Operation(
                    home_wire_contract::home::controls::v1::CoverOperation::Close,
                ),
            )),
            CommandKind::CoverStop => Ok(DeviceIntent::Cover(
                super::controls::CoverIntent::Operation(
                    home_wire_contract::home::controls::v1::CoverOperation::Stop,
                ),
            )),
            CommandKind::Lock => Ok(DeviceIntent::Lock(
                home_wire_contract::home::controls::v1::LockOperation::Lock,
            )),
            CommandKind::Unlock => Ok(DeviceIntent::Lock(
                home_wire_contract::home::controls::v1::LockOperation::Unlock,
            )),
            CommandKind::AlarmArmHome => Ok(DeviceIntent::Alarm(
                home_wire_contract::home::controls::v1::AlarmOperation::ArmHome,
            )),
            CommandKind::AlarmArmAway => Ok(DeviceIntent::Alarm(
                home_wire_contract::home::controls::v1::AlarmOperation::ArmAway,
            )),
            CommandKind::AlarmDisarm => Ok(DeviceIntent::Alarm(
                home_wire_contract::home::controls::v1::AlarmOperation::Disarm,
            )),
        }
    }
}

fn command(
    context: &Context,
    entity: &EntityId,
    kind: CommandKind,
    power: &BTreeMap<CardToken, bool>,
) -> BoundIntent {
    let mut candidates = context.slots.iter().enumerate().filter(|(index, slot)| {
        slot.source == super::ControlSource::General
            && slot.target.entity() == entity
            && slot
                .control
                .recipe_intent(kind, power.get(&CardToken(*index as i32)).copied())
                .and_then(|intent| slot.control.arguments(intent, slot.helper_revision))
                .is_ok()
    });
    let Some((index, slot)) = candidates.next() else {
        return BoundIntent::Unavailable(Refusal::Unsupported);
    };
    if candidates.next().is_some() {
        return BoundIntent::Unavailable(Refusal::Unsupported);
    }
    if !context.live {
        return BoundIntent::Unavailable(Refusal::Stale);
    }
    if slot.access != WriteAccess::Allowed {
        return BoundIntent::Unavailable(Refusal::ReadOnly);
    }
    BoundIntent::Command {
        token: CardToken(index as i32),
        generation: slot.generation,
        kind,
    }
}

fn bind(
    context: &Context,
    recipe: &Interaction,
    view: &CachedView,
    phase: CachePhase,
    power: &BTreeMap<CardToken, bool>,
) -> BoundInteraction {
    let intent = match &recipe.intention {
        Intention::None => BoundIntent::None,
        Intention::MoreInfo(entity) => {
            if view
                .get(entity)
                .is_some_and(|entity| entity.__buffa_unknown_fields.is_empty())
            {
                BoundIntent::MoreInfo(entity.clone())
            } else {
                BoundIntent::Unavailable(Refusal::Unavailable)
            }
        }
        Intention::Navigate(destination) => {
            if destination.dashboard.as_str() != context.layout.id.as_str() {
                // The host revalidates an authorized catalogue selection and
                // its target layout before entering another dashboard.
                BoundIntent::Navigate(destination.clone())
            } else if let Some(native) = context.layout.native.as_ref() {
                let expected = match destination.presentation {
                    Presentation::Page => Kind::View,
                    Presentation::Popup => Kind::Popup,
                };
                if native
                    .nodes
                    .get(&destination.node)
                    .is_some_and(|node| node.kind == expected)
                    && crate::dashboard_visibility::project(
                        &context.layout,
                        &destination.node,
                        phase,
                        Some(view),
                    )
                    .first()
                    .is_some_and(|node| node.kind == expected)
                {
                    BoundIntent::Navigate(destination.clone())
                } else {
                    BoundIntent::Unavailable(Refusal::Unavailable)
                }
            } else {
                BoundIntent::Unavailable(Refusal::Unsupported)
            }
        }
        Intention::Command { entity, kind } => command(context, entity, *kind, power),
    };
    BoundInteraction {
        intent,
        confirmation: recipe.confirmation.clone(),
        source: recipe.source.clone(),
    }
}

pub(super) fn project(
    state: &mut State,
    phase: CachePhase,
    view: Option<&CachedView>,
    nodes: &[Node],
) -> BTreeMap<NodeId, RecipePresentation> {
    let Some((context, view)) = state
        .context
        .as_ref()
        .zip(view)
        .filter(|_| matches!(phase, CachePhase::Live | CachePhase::Offline))
    else {
        state.recipe_bindings.snapshot = None;
        return BTreeMap::new();
    };
    let context = context.clone();
    let power = context
        .slots
        .iter()
        .enumerate()
        .filter_map(|(index, slot)| {
            Some((
                CardToken(index as i32),
                crate::dashboard_visibility::power(view.get(slot.target.entity())?)?,
            ))
        })
        .collect::<BTreeMap<_, _>>();
    let previous = state
        .recipe_bindings
        .snapshot
        .as_ref()
        .filter(|previous| previous.fence.same_view(&context));
    let mut bindings = Vec::new();
    let mut titles = Vec::new();
    for node in nodes {
        let Some(recipes) = node
            .recipes
            .as_ref()
            .filter(|recipes| !recipes.interactions.is_empty())
        else {
            continue;
        };
        let interactions = recipes
            .interactions
            .iter()
            .map(|(gesture, recipe)| (*gesture, bind(&context, recipe, view, phase, &power)))
            .collect();
        let mut binding = Binding {
            node: node.id.clone(),
            generation: CardGeneration(0),
            interactions,
        };
        binding.generation = match previous
            .and_then(|previous| previous.bindings.get(bindings.len()))
            .filter(|previous| {
                previous.node == binding.node && previous.interactions == binding.interactions
            }) {
            Some(previous) => previous.generation,
            None => {
                let Some(next) = state.serial.0.checked_add(1) else {
                    state.recipe_bindings.snapshot = None;
                    return BTreeMap::new();
                };
                state.serial = CardGeneration(next);
                state.serial
            }
        };
        bindings.push(binding);
        titles.push(node.title.clone());
    }
    let presentation = bindings
        .iter()
        .enumerate()
        .map(|(index, binding)| {
            (
                binding.node.clone(),
                RecipePresentation {
                    token: index as i32,
                    generation: binding.generation.0,
                    gestures: binding
                        .interactions
                        .iter()
                        .map(|(gesture, interaction)| (*gesture, interaction.access()))
                        .collect(),
                    unavailable: binding.interactions.values().any(|interaction| {
                        matches!(interaction.intent, BoundIntent::Unavailable(_))
                    }),
                },
            )
        })
        .collect();
    let labels = view
        .entities()
        .map(|(id, entity)| {
            (
                id.clone(),
                crate::cache_view::bounded(&entity.current_alias),
            )
        })
        .collect();
    state.recipe_bindings.snapshot = Some(Snapshot {
        fence: context,
        bindings,
        power,
        titles,
        labels,
    });
    presentation
}

impl DeviceStore {
    pub fn recipe_node(&self, token: i32, generation: i32, revision: i32) -> Option<NodeId> {
        let state = self.0.lock().expect("device store mutex");
        let snapshot = state.recipe_bindings.snapshot.as_ref()?;
        if snapshot.fence.revision != revision
            || !state.context.as_ref()?.same_view(&snapshot.fence)
        {
            return None;
        }
        snapshot
            .bindings
            .get(usize::try_from(token).ok()?)
            .filter(|binding| binding.generation == CardGeneration(generation))
            .map(|binding| binding.node.clone())
    }

    pub fn recipe_focus(
        &self,
        node: &NodeId,
        layout: &Layout,
        revision: i32,
    ) -> Option<RecipePresentation> {
        let state = self.0.lock().expect("device store mutex");
        let snapshot = state.recipe_bindings.snapshot.as_ref()?;
        if snapshot.fence.revision != revision
            || snapshot.fence.layout.as_ref() != layout
            || !state.context.as_ref()?.same_view(&snapshot.fence)
        {
            return None;
        }
        let (index, binding) = snapshot
            .bindings
            .iter()
            .enumerate()
            .find(|(_, binding)| &binding.node == node)?;
        let gestures = binding
            .interactions
            .iter()
            .map(|(gesture, interaction)| (*gesture, interaction.access()))
            .collect::<BTreeMap<_, _>>();
        if !gestures
            .values()
            .any(|access| matches!(access, GestureAccess::Local | GestureAccess::Command))
        {
            return None;
        }
        Some(RecipePresentation {
            token: index as i32,
            generation: binding.generation.0,
            gestures,
            unavailable: binding
                .interactions
                .values()
                .any(|interaction| matches!(interaction.intent, BoundIntent::Unavailable(_))),
        })
    }

    pub fn prepare_recipe(
        &self,
        token: i32,
        generation: i32,
        revision: i32,
        gesture: Gesture,
    ) -> Result<NativePrepared, Refusal> {
        let state = self.0.lock().expect("device store mutex");
        let snapshot = state
            .recipe_bindings
            .snapshot
            .as_ref()
            .ok_or(Refusal::Stale)?;
        let index = usize::try_from(token).map_err(|_| Refusal::Stale)?;
        if snapshot.fence.revision != revision
            || !state
                .context
                .as_ref()
                .is_some_and(|current| current.same_view(&snapshot.fence))
        {
            return Err(Refusal::Stale);
        }
        let binding = snapshot
            .bindings
            .get(index)
            .filter(|binding| binding.generation == CardGeneration(generation))
            .ok_or(Refusal::Stale)?;
        let interaction = binding
            .interactions
            .get(&gesture)
            .cloned()
            .ok_or(Refusal::Unsupported)?;
        let expected = binding.clone();
        let weak = Arc::downgrade(&self.0);
        let mut guard: AdmissionCheck = Arc::new(move || {
            let Some(shared) = weak.upgrade() else {
                return false;
            };
            let state = shared.lock().expect("device store mutex");
            state
                .recipe_bindings
                .snapshot
                .as_ref()
                .is_some_and(|snapshot| {
                    snapshot.fence.revision == revision
                        && state
                            .context
                            .as_ref()
                            .is_some_and(|current| current.same_view(&snapshot.fence))
                        && snapshot.bindings.get(index) == Some(&expected)
                })
        });
        let layout = snapshot.fence.layout.clone();
        let origin = binding.node.clone();
        let title = snapshot.titles[index].clone();
        let command = match &interaction.intent {
            BoundIntent::Command {
                token,
                generation,
                kind,
            } => {
                let slot = snapshot
                    .fence
                    .slots
                    .get(token.0 as usize)
                    .filter(|slot| slot.generation == *generation)
                    .ok_or(Refusal::Stale)?;
                Some((
                    *token,
                    *generation,
                    slot.control
                        .recipe_intent(*kind, snapshot.power.get(token).copied())?,
                ))
            }
            BoundIntent::None
            | BoundIntent::Unavailable(_)
            | BoundIntent::MoreInfo(_)
            | BoundIntent::Navigate(_) => None,
        };
        let description = match &interaction.intent {
            BoundIntent::None | BoundIntent::Unavailable(_) => String::new(),
            BoundIntent::MoreInfo(entity) => format!(
                "Afficher les détails · {}",
                snapshot
                    .labels
                    .get(entity)
                    .map_or("Élément", String::as_str)
            ),
            BoundIntent::Navigate(destination) => format!(
                "Ouvrir · {}",
                layout
                    .native
                    .as_ref()
                    .and_then(|native| native.nodes.get(&destination.node))
                    .map_or("Dashboard", |node| node.title.as_str())
            ),
            BoundIntent::Command { token, kind, .. } => {
                let power = command.as_ref().and_then(|(_, _, intent)| match intent {
                    DeviceIntent::Power(power) => Some(*power),
                    DeviceIntent::Brightness(_)
                    | DeviceIntent::ColorTemperature(_)
                    | DeviceIntent::Climate(_)
                    | DeviceIntent::FanSpeed(_)
                    | DeviceIntent::Helper(_)
                    | DeviceIntent::Cover(_)
                    | DeviceIntent::Lock(_)
                    | DeviceIntent::Alarm(_) => None,
                });
                let verb = match kind {
                    CommandKind::Toggle => match power {
                        Some(Power::On) => "Allumer",
                        Some(Power::Off) => "Éteindre",
                        None => "Basculer",
                    },
                    CommandKind::TurnOn => "Allumer",
                    CommandKind::TurnOff => "Éteindre",
                    CommandKind::Activate => "Activer",
                    CommandKind::CoverOpen => crate::control_text::cover_operation(
                        home_wire_contract::home::controls::v1::CoverOperation::Open,
                    )
                    .expect("known operation"),
                    CommandKind::CoverClose => crate::control_text::cover_operation(
                        home_wire_contract::home::controls::v1::CoverOperation::Close,
                    )
                    .expect("known operation"),
                    CommandKind::CoverStop => crate::control_text::cover_operation(
                        home_wire_contract::home::controls::v1::CoverOperation::Stop,
                    )
                    .expect("known operation"),
                    CommandKind::Lock => crate::control_text::lock_operation(
                        home_wire_contract::home::controls::v1::LockOperation::Lock,
                    )
                    .expect("known operation"),
                    CommandKind::Unlock => crate::control_text::lock_operation(
                        home_wire_contract::home::controls::v1::LockOperation::Unlock,
                    )
                    .expect("known operation"),
                    CommandKind::AlarmArmHome => crate::control_text::alarm_operation(
                        home_wire_contract::home::controls::v1::AlarmOperation::ArmHome,
                    )
                    .expect("known operation"),
                    CommandKind::AlarmArmAway => crate::control_text::alarm_operation(
                        home_wire_contract::home::controls::v1::AlarmOperation::ArmAway,
                    )
                    .expect("known operation"),
                    CommandKind::AlarmDisarm => crate::control_text::alarm_operation(
                        home_wire_contract::home::controls::v1::AlarmOperation::Disarm,
                    )
                    .expect("known operation"),
                };
                let entity = snapshot.fence.slots[token.0 as usize].target.entity();
                format!(
                    "{verb} · {}",
                    snapshot
                        .labels
                        .get(entity)
                        .map_or("Élément", String::as_str)
                )
            }
        };
        drop(state);
        let intent = match interaction.intent {
            BoundIntent::None => return Err(Refusal::Unsupported),
            BoundIntent::Unavailable(refusal) => return Err(refusal),
            BoundIntent::MoreInfo(entity) => NativeIntent::MoreInfo(entity),
            BoundIntent::Navigate(destination) => NativeIntent::Navigate(destination),
            BoundIntent::Command { .. } => {
                let (token, generation, intent) = command.ok_or(Refusal::Unsupported)?;
                let mut prepared = self.prepare(token.0, generation.0, revision, intent)?;
                let command_guard = prepared.guard.clone();
                let native_guard = guard;
                guard = Arc::new(move || native_guard() && command_guard());
                prepared.guard = guard.clone();
                NativeIntent::Command(Box::new(prepared))
            }
        };
        if !guard() {
            return Err(Refusal::Stale);
        }
        Ok(NativePrepared {
            intent,
            confirmation: interaction.confirmation,
            title,
            description,
            guard,
            layout,
            origin,
            expires: Instant::now() + Duration::from_secs(30),
        })
    }
}
