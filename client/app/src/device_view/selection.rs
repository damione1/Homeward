//! Thread-transferable state for a helper's native KitSelect. The free kit owns
//! navigation, filtering generations and controlled selection semantics.
use super::{
    DeviceIntent, Refusal,
    helper::{Helper, HelperIntent, Kind},
};
use crate::{
    CloseReason, NavigationStep, OptionRow, ProValueEvent, ProValueIntent, SelectState,
    SelectionAvailability, SelectionRef,
};
use slint_kit::{
    Ownership, RemovalPolicy, RowToken, SelectController, SelectOption, SelectionIntent,
};

#[derive(Clone, Debug, PartialEq, Eq, Hash)]
struct OptionKey(String);
pub(super) struct Selection {
    source: Vec<SelectOption<OptionKey>>,
    controller: SelectController<OptionKey>,
    epoch: std::time::Instant,
}
impl Selection {
    pub fn new(helper: &Helper, observed: Option<String>) -> Option<Self> {
        let Kind::Select(options) = helper.kind() else {
            return None;
        };
        let source = options
            .iter()
            .map(|value| {
                SelectOption::new(OptionKey(value.clone()), crate::cache_view::bounded(value))
            })
            .collect::<Vec<_>>();
        let mut controller = SelectController::new(
            source.clone(),
            observed.map(OptionKey),
            Ownership::Controlled,
        );
        controller.set_removal_policy(RemovalPolicy::PreserveUnavailable);
        Some(Self {
            source,
            controller,
            epoch: std::time::Instant::now(),
        })
    }
    pub fn observe(&mut self, value: Option<String>) {
        self.controller.set_selected(value.map(OptionKey));
    }
    pub fn project(&self) -> Option<(Vec<OptionRow>, SelectState)> {
        let generation = i32::try_from(self.controller.generation()).ok()?;
        let state = self.controller.state();
        let reference = |key: &Option<OptionKey>| {
            let index = key.as_ref().and_then(|key| {
                self.controller
                    .options()
                    .iter()
                    .position(|option| &option.key == key)
            });
            SelectionRef {
                present: index.is_some(),
                token: index.map(|v| v as i32).unwrap_or(0),
            }
        };
        let active = reference(&state.active);
        Some((
            self.controller
                .options()
                .iter()
                .enumerate()
                .map(|(index, option)| OptionRow {
                    token: index as i32,
                    generation,
                    label: option.label.as_str().into(),
                    description: option.description.as_str().into(),
                    disabled: option.disabled,
                })
                .collect(),
            SelectState {
                generation,
                open: state.open,
                selected: reference(&state.selected),
                active: active.clone(),
                active_index: if active.present { active.token } else { -1 },
                selection_availability: match &state.selected {
                    None => SelectionAvailability::Absent,
                    Some(key) => {
                        if self.source.iter().any(|option| &option.key == key) {
                            SelectionAvailability::Available
                        } else {
                            SelectionAvailability::Unavailable
                        }
                    }
                },
            },
        ))
    }
    pub fn edit(&mut self, intent: ProValueIntent) -> Result<Option<DeviceIntent>, Refusal> {
        match intent.event {
            ProValueEvent::SelectOpen => {
                self.controller.open();
            }
            ProValueEvent::SelectNavigate => {
                let step = match intent.navigation {
                    NavigationStep::Next => slint_kit::NavigationStep::Next,
                    NavigationStep::Previous => slint_kit::NavigationStep::Previous,
                    NavigationStep::First => slint_kit::NavigationStep::First,
                    NavigationStep::Last => slint_kit::NavigationStep::Last,
                    NavigationStep::PageNext => slint_kit::NavigationStep::PageNext,
                    NavigationStep::PagePrevious => slint_kit::NavigationStep::PagePrevious,
                };
                self.controller.navigate(step, 6);
            }
            ProValueEvent::SelectDismiss => {
                let reason = match intent.close_reason {
                    CloseReason::Escape => slint_kit::DismissReason::Escape,
                    CloseReason::OutsidePress => slint_kit::DismissReason::OutsidePress,
                    CloseReason::SystemBack => slint_kit::DismissReason::SystemBack,
                    CloseReason::TriggerToggle => slint_kit::DismissReason::TriggerToggle,
                    CloseReason::Programmatic => slint_kit::DismissReason::Programmatic,
                    CloseReason::HostClosed => slint_kit::DismissReason::HostClosed,
                };
                self.controller.dismiss(reason);
            }
            ProValueEvent::SelectActivate => {
                let token = RowToken {
                    index: usize::try_from(intent.option_token).map_err(|_| Refusal::Stale)?,
                    generation: u64::try_from(intent.option_generation)
                        .map_err(|_| Refusal::Stale)?,
                };
                return match self.controller.activate_token(token) {
                    Some(SelectionIntent::Commit(value)) => {
                        Ok(Some(DeviceIntent::Helper(HelperIntent::Select(value.0))))
                    }
                    Some(SelectionIntent::Clear) | None => Err(Refusal::Stale),
                };
            }
            ProValueEvent::SelectQuery => {
                if intent.lower.len() > 1024 {
                    return Err(Refusal::InvalidValue);
                }
                let query = intent.lower.to_lowercase();
                self.controller
                    .reconcile(
                        self.source
                            .iter()
                            .filter(|option| option.key.0.to_lowercase().contains(&query))
                            .cloned()
                            .collect(),
                    )
                    .map_err(|_| Refusal::Unsupported)?;
            }
            ProValueEvent::SelectTypeahead => {
                if intent.lower.len() > 1024 {
                    return Err(Refusal::InvalidValue);
                }
                self.controller.typeahead(
                    &intent.lower,
                    self.epoch.elapsed().as_millis().min(u128::from(u64::MAX)) as u64,
                );
            }
            ProValueEvent::Draft
            | ProValueEvent::Submit
            | ProValueEvent::Step
            | ProValueEvent::Position
            | ProValueEvent::Navigate
            | ProValueEvent::Start
            | ProValueEvent::Commit
            | ProValueEvent::Cancel => return Err(Refusal::Unsupported),
        }
        Ok(None)
    }
}
