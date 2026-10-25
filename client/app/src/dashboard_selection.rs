//! Stable, session-local dashboard choice. The server owns the catalogue.
use crate::dashboard::{DashboardId, LayoutFailure};
use crate::{HomewardWindow, OptionRow, SelectState, SelectionAvailability, SelectionRef};
use home_wire_contract::home::dashboard::v1 as wire;
use slint::{ComponentHandle, ModelRc, VecModel};
use slint_kit::{DismissReason, Ownership, SelectBinding, SelectOption, SelectionIntent};
use std::sync::{Arc, Mutex};

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Choice {
    pub id: DashboardId,
    pub title: String,
}
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub(crate) struct Catalogue {
    pub choices: Vec<Choice>,
    pub selected: Option<DashboardId>,
}
impl Catalogue {
    pub fn from_wire(
        value: &wire::ListDashboardsResponse,
        requested: Option<&DashboardId>,
    ) -> Result<Self, LayoutFailure> {
        if value.dashboards.len() > 128 {
            return Err(LayoutFailure::Budget);
        }
        if !value.__buffa_unknown_fields.is_empty() {
            return Err(LayoutFailure::Unsupported);
        }
        let mut seen = std::collections::BTreeSet::new();
        let mut choices = Vec::new();
        for entry in &value.dashboards {
            let id =
                DashboardId::parse(&entry.id.as_option().ok_or(LayoutFailure::Invalid)?.value)?;
            if !seen.insert(id.clone()) {
                return Err(LayoutFailure::Invalid);
            }
            if entry.title.len() > 256 || entry.title.chars().any(char::is_control) {
                return Err(LayoutFailure::Invalid);
            }
            choices.push(Choice {
                title: if entry.title.is_empty() {
                    id.as_str().to_owned()
                } else {
                    entry.title.clone()
                },
                id,
            });
        }
        let selected = requested
            .filter(|id| choices.iter().any(|choice| choice.id == **id))
            .cloned()
            .or_else(|| choices.first().map(|choice| choice.id.clone()));
        Ok(Self { choices, selected })
    }
}
#[derive(Default)]
struct State {
    revision: u64,
    catalogue: Catalogue,
}
#[derive(Clone, Default)]
pub(crate) struct Choices(Arc<Mutex<State>>);
impl Choices {
    pub fn snapshot(&self) -> (u64, Catalogue) {
        let state = self.0.lock().expect("dashboard choice mutex");
        (state.revision, state.catalogue.clone())
    }
    pub fn hidden(&self) -> Catalogue {
        Catalogue {
            choices: Vec::new(),
            selected: self.snapshot().1.selected,
        }
    }
    pub fn accept(&self, revision: u64, catalogue: Catalogue) -> bool {
        let mut state = self.0.lock().expect("dashboard choice mutex");
        if state.revision != revision {
            return false;
        }
        state.catalogue = catalogue;
        true
    }
    pub fn select(&self, id: DashboardId) -> bool {
        let mut state = self.0.lock().expect("dashboard choice mutex");
        if state.catalogue.selected.as_ref() == Some(&id)
            || !state.catalogue.choices.iter().any(|choice| choice.id == id)
        {
            return false;
        }
        state.revision = state
            .revision
            .checked_add(1)
            .expect("dashboard choice revision exhausted");
        state.catalogue.selected = Some(id);
        true
    }
    pub fn clear(&self) {
        let mut state = self.0.lock().expect("dashboard choice mutex");
        state.revision = state
            .revision
            .checked_add(1)
            .expect("dashboard choice revision exhausted");
        state.catalogue = Catalogue::default();
    }
}

pub(crate) fn bind(window: &HomewardWindow, choices: Choices, changed: impl Fn() + 'static) {
    let weak = window.as_weak();
    let binding = SelectBinding::new(
        Vec::new(),
        None::<DashboardId>,
        Ownership::Controlled,
        move |snapshot| {
            if let Some(window) = weak.upgrade() {
                window.set_dashboard_choices(ModelRc::new(VecModel::from(
                    snapshot
                        .rows
                        .iter()
                        .map(|row| OptionRow {
                            token: row.token,
                            generation: row.generation,
                            label: row.label.as_str().into(),
                            description: row.description.as_str().into(),
                            disabled: row.disabled,
                        })
                        .collect::<Vec<_>>(),
                )));
                let selection = |token: Option<i32>| SelectionRef {
                    present: token.is_some(),
                    token: token.unwrap_or(-1),
                };
                window.set_dashboard_choice_state(SelectState {
                    selected: selection(snapshot.selected),
                    active: selection(snapshot.active),
                    active_index: snapshot.active_index,
                    open: snapshot.open,
                    generation: snapshot.generation,
                    selection_availability: match snapshot.selection_availability {
                        slint_kit::SelectionAvailability::Absent => SelectionAvailability::Absent,
                        slint_kit::SelectionAvailability::Available => {
                            SelectionAvailability::Available
                        }
                        slint_kit::SelectionAvailability::Unavailable => {
                            SelectionAvailability::Unavailable
                        }
                    },
                });
            }
        },
    );
    let (target, state) = (binding.clone(), choices.clone());
    window.on_dashboard_catalogue_changed(move || {
        let (_, catalogue) = state.snapshot();
        let options = catalogue
            .choices
            .iter()
            .map(|choice| SelectOption::new(choice.id.clone(), choice.title.clone()))
            .collect::<Vec<_>>();
        if target.options() != options {
            target.dismiss(DismissReason::Programmatic);
            target
                .reconcile(options)
                .expect("validated unique dashboard choices");
        }
        target.set_selected(catalogue.selected);
    });
    let clear = choices.clone();
    let weak = window.as_weak();
    window.on_dashboard_catalogue_cleared(move || {
        clear.clear();
        if let Some(window) = weak.upgrade() {
            window.invoke_dashboard_catalogue_changed();
        }
    });
    binding.on_intent(move |intent| match intent {
        SelectionIntent::Commit(id) => {
            if choices.select(id) {
                changed();
            }
        }
        SelectionIntent::Clear => {}
    });
    let target = binding.clone();
    window.on_dashboard_choice_open(move || target.open());
    let target = binding.clone();
    window
        .on_dashboard_choice_activate(move |token, generation| target.activate(token, generation));
    let target = binding.clone();
    window.on_dashboard_choice_step(move |step| {
        target.navigate(match step {
            crate::NavigationStep::Next => slint_kit::NavigationStep::Next,
            crate::NavigationStep::Previous => slint_kit::NavigationStep::Previous,
            crate::NavigationStep::First => slint_kit::NavigationStep::First,
            crate::NavigationStep::Last => slint_kit::NavigationStep::Last,
            crate::NavigationStep::PageNext => slint_kit::NavigationStep::PageNext,
            crate::NavigationStep::PagePrevious => slint_kit::NavigationStep::PagePrevious,
        })
    });
    let target = binding.clone();
    window.on_dashboard_choice_dismiss(move |_| target.dismiss(DismissReason::Programmatic));
    window.on_dashboard_choice_typeahead(move |text| binding.typeahead(text.as_str()));
}

#[cfg(test)]
mod tests {
    use super::*;
    fn catalogue() -> Catalogue {
        Catalogue {
            choices: vec![
                Choice {
                    id: DashboardId::parse("one").unwrap(),
                    title: "One".into(),
                },
                Choice {
                    id: DashboardId::parse("two").unwrap(),
                    title: "Two".into(),
                },
            ],
            selected: Some(DashboardId::parse("one").unwrap()),
        }
    }
    #[test]
    fn a_pending_read_cannot_override_a_new_choice_or_reintroduce_a_cleared_catalogue() {
        let choices = Choices::default();
        let (revision, _) = choices.snapshot();
        assert!(choices.accept(revision, catalogue()));
        assert!(choices.select(DashboardId::parse("two").unwrap()));
        assert!(!choices.accept(revision, catalogue()));
        assert_eq!(choices.snapshot().1.selected.unwrap().as_str(), "two");
        let revision = choices.snapshot().0;
        choices.clear();
        assert!(!choices.accept(revision, catalogue()));
        assert!(choices.snapshot().1.choices.is_empty());
    }
    #[test]
    fn deletion_selects_an_authorized_survivor_and_duplicate_ids_are_rejected() {
        let metadata = |id: &str| wire::Metadata {
            id: wire::DashboardId {
                value: id.into(),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        let mut list = wire::ListDashboardsResponse {
            dashboards: vec![metadata("one"), metadata("two")],
            ..Default::default()
        };
        let two = DashboardId::parse("two").unwrap();
        assert_eq!(
            Catalogue::from_wire(&list, Some(&two)).unwrap().selected,
            Some(two.clone())
        );
        list.dashboards.pop();
        assert_eq!(
            Catalogue::from_wire(&list, Some(&two))
                .unwrap()
                .selected
                .unwrap()
                .as_str(),
            "one"
        );
        list.dashboards.push(metadata("one"));
        assert_eq!(
            Catalogue::from_wire(&list, None),
            Err(LayoutFailure::Invalid)
        );
    }
}
