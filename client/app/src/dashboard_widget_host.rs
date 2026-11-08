//! Session-owned resource readers for the active server-configured dashboard.
//! Each widget has its own selected source, revisions, drafts and cancellation;
//! opening the inventory page cannot change a dashboard widget's binding.
use crate::{
    HomewardWindow, cache_view,
    dashboard_widgets::{Binding, Content},
};
use home_client_model::{
    api_calendars::CalendarName, api_tasks::TaskListName, local_time::DisplayTimeZone,
};
use home_client_session::SessionCoordinator;
use home_client_transport::{CalendarTransport, TaskTransport};
use slint::ComponentHandle;
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
};
use tokio::{sync::Notify, task::JoinSet};
use tokio_util::sync::CancellationToken;

const MAX_RESOURCE_WIDGETS: usize = 32;
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct TaskTarget {
    pub token: i32,
    pub generation: i32,
    pub list: TaskListName,
    pub limit: usize,
    pub shopping_only: bool,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct AgendaTarget {
    pub token: i32,
    pub generation: i32,
    pub calendar: CalendarName,
    pub zone: DisplayTimeZone,
    pub window_seconds: u32,
    pub limit: usize,
}
#[derive(Clone)]
enum Host {
    Tasks(crate::list_host::Lists),
    Agenda(crate::calendar_host::Calendars),
}
impl Host {
    fn clear(&self) {
        match self {
            Self::Tasks(owner) => owner.clear(),
            Self::Agenda(owner) => owner.clear(),
        }
    }
}
struct Entry {
    binding: Binding,
    host: Host,
    cancel: CancellationToken,
}
struct State {
    desired: Vec<Binding>,
    entries: BTreeMap<(i32, i32), Entry>,
    foreground: bool,
}
#[derive(Clone)]
pub(crate) struct Widgets(Arc<Inner>);
struct Inner {
    window: Option<slint::Weak<HomewardWindow>>,
    state: Mutex<State>,
    changed: Notify,
}
fn interactive(window: &HomewardWindow, token: i32, writing: bool) -> bool {
    if window.get_access_stage() != crate::ProAccessStage::Connected
        || !window.get_view_loaded()
        || window.get_show_endpoint()
        || window.get_navigation_drawer().open
        || window.get_recipe_confirmation().open
        || writing && !window.get_commands_ready()
    {
        return false;
    }
    let popup = window.get_native_popup();
    if popup.open && (!popup.topmost || popup.busy) {
        return false;
    }
    if (300_000..400_000).contains(&token) != popup.open {
        return false;
    }
    if !window.get_native_popup().open
        && window.get_page() == crate::ViewPage::Room
        && window.get_room_view() != crate::RoomView::Cards
    {
        return false;
    }
    cache_view::widget_bindings(window)
        .iter()
        .any(|binding| binding.token == token)
}
impl Widgets {
    fn new(window: Option<slint::Weak<HomewardWindow>>) -> Self {
        Self(Arc::new(Inner {
            window,
            state: Mutex::new(State {
                desired: Vec::new(),
                entries: BTreeMap::new(),
                foreground: true,
            }),
            changed: Notify::new(),
        }))
    }
    #[cfg(test)]
    pub(crate) fn detached() -> Self {
        Self::new(None)
    }
    pub(crate) fn bind(window: &HomewardWindow) -> Self {
        let this = Self::new(Some(window.as_weak()));
        let owner = this.clone();
        let weak = window.as_weak();
        window.on_dashboard_widgets_changed(move || {
            if let Some(window) = weak.upgrade() {
                owner.sync(&window)
            }
        });
        let owner = this.clone();
        let weak = window.as_weak();
        window.on_dashboard_task_complete(
            move |widget, generation, item, item_generation, completed| {
                if let Some(window) = weak.upgrade() {
                    if !interactive(&window, widget, true) {
                        return;
                    }
                    owner.sync(&window);
                    owner.complete(widget, generation, item, item_generation, completed)
                }
            },
        );
        let owner = this.clone();
        let weak = window.as_weak();
        window.on_dashboard_agenda_select(move |widget, generation, item, item_generation| {
            if let Some(window) = weak.upgrade() {
                if !interactive(&window, widget, false) {
                    return;
                }
                owner.sync(&window);
                if owner.select(widget, generation, item, item_generation) {
                    window.invoke_dashboard_widget_activate(widget, generation);
                }
            }
        });
        this.sync(window);
        this
    }
    fn sync(&self, window: &HomewardWindow) {
        self.replace(cache_view::widget_bindings(window), Some(window))
    }
    fn replace(&self, bindings: Vec<Binding>, window: Option<&HomewardWindow>) {
        let desired = bindings
            .into_iter()
            .filter(|binding| {
                binding.token > 0
                    && binding.generation > 0
                    && matches!(
                        binding.config.content,
                        Content::Tasks { .. } | Content::Agenda { .. }
                    )
            })
            .collect::<Vec<_>>();
        let mut state = self.0.state.lock().expect("dashboard widget state");
        if !state.foreground || state.desired == desired {
            return;
        }
        state.entries.retain(|_, entry| {
            let keep = desired
                .iter()
                .take(MAX_RESOURCE_WIDGETS)
                .any(|binding| binding == &entry.binding);
            if !keep {
                entry.cancel.cancel();
                entry.host.clear();
            }
            keep
        });
        let pending = desired
            .iter()
            .take(MAX_RESOURCE_WIDGETS)
            .filter(|binding| {
                !state
                    .entries
                    .contains_key(&(binding.token, binding.generation))
            })
            .cloned()
            .collect::<Vec<_>>();
        let overflow = desired
            .iter()
            .skip(MAX_RESOURCE_WIDGETS)
            .cloned()
            .collect::<Vec<_>>();
        state.desired = desired;
        drop(state);
        if let Some(window) = window {
            for binding in pending {
                match binding.config.content {
                    Content::Tasks { .. } => cache_view::publish_widget_tasks(
                        window,
                        binding.token,
                        binding.generation,
                        Default::default(),
                        crate::ProTaskAccess::Offline,
                        false,
                        "Reading the configured list…".into(),
                    ),
                    Content::Agenda { .. } => cache_view::publish_widget_agenda(
                        window,
                        binding.token,
                        binding.generation,
                        Default::default(),
                        false,
                        "Reading the configured calendar…".into(),
                    ),
                    Content::ClockWeather { .. }
                    | Content::Room { .. }
                    | Content::Thermostat { .. }
                    | Content::Security { .. }
                    | Content::Observations { .. }
                    | Content::Navigation { .. } => {}
                }
            }
        }
        if let Some(window) = window {
            for binding in overflow.iter() {
                match binding.config.content {
                    Content::Tasks { .. } => cache_view::publish_widget_tasks(
                        window,
                        binding.token,
                        binding.generation,
                        Default::default(),
                        crate::ProTaskAccess::Offline,
                        false,
                        "Dashboard reader limit reached".into(),
                    ),
                    Content::Agenda { .. } => cache_view::publish_widget_agenda(
                        window,
                        binding.token,
                        binding.generation,
                        Default::default(),
                        false,
                        "Dashboard reader limit reached".into(),
                    ),
                    Content::ClockWeather { .. }
                    | Content::Room { .. }
                    | Content::Thermostat { .. }
                    | Content::Security { .. }
                    | Content::Observations { .. }
                    | Content::Navigation { .. } => {}
                }
            }
        }
        self.0.changed.notify_one();
    }
    fn complete(
        &self,
        widget: i32,
        generation: i32,
        item: i32,
        item_generation: i32,
        completed: bool,
    ) {
        let host = self
            .0
            .state
            .lock()
            .expect("dashboard widget state")
            .entries
            .get(&(widget, generation))
            .map(|entry| entry.host.clone());
        if let Some(Host::Tasks(owner)) = host {
            owner.widget_complete(item, item_generation, completed)
        }
    }
    fn select(&self, widget: i32, generation: i32, item: i32, item_generation: i32) -> bool {
        let owner = self
            .0
            .state
            .lock()
            .expect("dashboard widget state")
            .entries
            .get(&(widget, generation))
            .map(|entry| entry.host.clone());
        match owner {
            Some(Host::Agenda(owner)) => owner.widget_can_select(item, item_generation),
            Some(Host::Tasks(_)) | None => false,
        }
    }
    pub(crate) fn clear(&self) {
        let mut state = self.0.state.lock().expect("dashboard widget state");
        state.desired.clear();
        for (_, entry) in std::mem::take(&mut state.entries) {
            entry.cancel.cancel();
            entry.host.clear()
        }
        drop(state);
        self.0.changed.notify_one();
    }
    pub(crate) fn foreground(&self, foreground: bool) {
        if !foreground {
            self.clear()
        }
        self.0
            .state
            .lock()
            .expect("dashboard widget state")
            .foreground = foreground;
        self.0.changed.notify_one();
    }
    pub(crate) async fn run(
        &self,
        session: SessionCoordinator,
        lists: TaskTransport,
        calendars: CalendarTransport,
        cancel: CancellationToken,
    ) {
        let mut tasks = JoinSet::new();
        loop {
            let jobs = {
                let mut state = self.0.state.lock().expect("dashboard widget state");
                let mut jobs = Vec::new();
                if state.foreground {
                    for binding in state.desired.clone().into_iter().take(MAX_RESOURCE_WIDGETS) {
                        let key = (binding.token, binding.generation);
                        if state.entries.contains_key(&key) {
                            continue;
                        }
                        let stop = cancel.child_token();
                        let host = match &binding.config.content {
                            Content::Tasks {
                                list,
                                limit,
                                shopping_only,
                            } => Host::Tasks(crate::list_host::Lists::for_widget(
                                self.0.window.clone(),
                                TaskTarget {
                                    token: binding.token,
                                    generation: binding.generation,
                                    list: list.clone(),
                                    limit: *limit,
                                    shopping_only: *shopping_only,
                                },
                            )),
                            Content::Agenda {
                                calendar,
                                zone,
                                window_seconds,
                                limit,
                            } => Host::Agenda(crate::calendar_host::Calendars::for_widget(
                                self.0.window.clone(),
                                AgendaTarget {
                                    token: binding.token,
                                    generation: binding.generation,
                                    calendar: calendar.clone(),
                                    zone: *zone,
                                    window_seconds: *window_seconds,
                                    limit: *limit,
                                },
                            )),
                            Content::ClockWeather { .. }
                            | Content::Room { .. }
                            | Content::Thermostat { .. }
                            | Content::Security { .. }
                            | Content::Observations { .. }
                            | Content::Navigation { .. } => continue,
                        };
                        state.entries.insert(
                            key,
                            Entry {
                                binding,
                                host: host.clone(),
                                cancel: stop.clone(),
                            },
                        );
                        jobs.push((host, stop));
                    }
                }
                jobs
            };
            for (host, stop) in jobs {
                let session = session.clone();
                let lists = lists.clone();
                let calendars = calendars.clone();
                tasks.spawn(async move {
                    match host {
                        Host::Tasks(owner) => owner.run(session, lists, stop).await,
                        Host::Agenda(owner) => owner.run(session, calendars, stop).await,
                    }
                });
            }
            tokio::select! {biased;
                ()=cancel.cancelled()=>break,
                ()=self.0.changed.notified()=>{},
                _=tasks.join_next(),if !tasks.is_empty()=>{},
            }
        }
        self.clear();
        while tasks.join_next().await.is_some() {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn binding(token: i32, generation: i32, list: &str) -> Binding {
        Binding {
            token,
            generation,
            node: home_client_model::dashboard::NodeId::parse(&format!("$.widgets[{token}]"))
                .unwrap(),
            config: crate::dashboard_widgets::Widget {
                content: Content::Tasks {
                    list: home_client_model::api_tasks::TaskListName::new(
                        home_client_model::api::HomeName::try_from("homes/synthetic").unwrap(),
                        list.parse().unwrap(),
                    ),
                    limit: 2,
                    shopping_only: false,
                },
                action: None,
                action_label: String::new(),
                empty_label: String::new(),
            },
        }
    }
    #[test]
    fn a_configuration_or_route_replacement_cancels_the_old_private_owner() {
        let owner = Widgets::detached();
        let first = binding(1, 10, "configured-first");
        owner.replace(vec![first.clone()], None);
        let cancel = CancellationToken::new();
        let host = Host::Tasks(crate::list_host::Lists::for_widget(
            None,
            TaskTarget {
                token: 1,
                generation: 10,
                list: home_client_model::api_tasks::TaskListName::new(
                    home_client_model::api::HomeName::try_from("homes/synthetic").unwrap(),
                    "configured-first".parse().unwrap(),
                ),
                limit: 2,
                shopping_only: false,
            },
        ));
        owner.0.state.lock().unwrap().entries.insert(
            (1, 10),
            Entry {
                binding: first,
                host,
                cancel: cancel.clone(),
            },
        );
        owner.replace(vec![binding(1, 11, "configured-second")], None);
        assert!(cancel.is_cancelled());
        let state = owner.0.state.lock().unwrap();
        assert!(state.entries.is_empty());
        assert_eq!(state.desired[0].generation, 11);
    }
    #[test]
    fn suspension_discards_bindings_until_a_fresh_board_publication() {
        let owner = Widgets::detached();
        owner.replace(vec![binding(1, 10, "configured")], None);
        owner.foreground(false);
        owner.replace(vec![binding(1, 10, "old")], None);
        assert!(owner.0.state.lock().unwrap().desired.is_empty());
        owner.foreground(true);
        assert!(owner.0.state.lock().unwrap().desired.is_empty());
        owner.replace(vec![binding(1, 11, "fresh")], None);
        assert_eq!(owner.0.state.lock().unwrap().desired[0].generation, 11);
    }
}
