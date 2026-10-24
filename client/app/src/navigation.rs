//! Window-local routes and focus. No transport or device commands.
use crate::{
    BodyFocus, CloseReason, DialogState, FocusRequest, HomewardWindow, NavigationStep, OptionRow,
    ProShellFocus, SelectState, SelectionAvailability, SelectionRef, ViewPage,
    cache_view::CacheRows,
    dashboard::{self, ProjectionRequest, Route, SharedRequest},
};
use slint::{ComponentHandle, Model, ModelRc, VecModel};
use slint_kit::{
    DismissReason, FocusDirection, FocusHandle, FocusId, FocusRegistry, FocusScopeId, OverlayId,
    OverlaySpec, OverlayStack, Ownership, SelectBinding, SelectOption, SelectionIntent,
};
use std::{cell::RefCell, rc::Rc};

const DRAWER: OverlayId = OverlayId(1);
const MODAL: FocusScopeId = FocusScopeId(1);
#[derive(Clone, Copy, PartialEq, Eq, Hash)]
enum Section {
    Home,
    Rooms,
    Inventory,
    Settings,
}
impl Section {
    fn label(self) -> &'static str {
        match self {
            Self::Home => "Accueil",
            Self::Rooms => "Pièces",
            Self::Inventory => "Inventaire",
            Self::Settings => "Préférences",
        }
    }
    fn route(self) -> Route {
        match self {
            Self::Home => Route::Home,
            Self::Rooms => Route::Rooms,
            Self::Inventory => Route::Inventory,
            Self::Settings => Route::Settings,
        }
    }
}
#[derive(Clone, Copy)]
enum Target {
    Shell(ProShellFocus),
    Body(BodyFocus),
    Home(i32),
}
struct Entry {
    route: Route,
    opener: Target,
}
struct State {
    route: Route,
    history: Vec<Entry>,
    request: SharedRequest,
    focus: FocusRegistry,
    handles: Vec<(Target, FocusHandle)>,
    current: Option<FocusHandle>,
    overlays: OverlayStack,
    compact: bool,
}
impl State {
    fn new(request: SharedRequest) -> Self {
        let mut focus = FocusRegistry::new();
        focus
            .register_scope(MODAL, FocusScopeId(0))
            .expect("navigation scope");
        let mut handles = Vec::new();
        for (id, target, scope, order) in [
            (
                1,
                Target::Shell(ProShellFocus::NavigationTrigger),
                FocusScopeId(0),
                0,
            ),
            (2, Target::Shell(ProShellFocus::NavigationClose), MODAL, 0),
            (3, Target::Shell(ProShellFocus::NavigationItems), MODAL, 1),
            (4, Target::Body(BodyFocus::Read), FocusScopeId(0), 1),
            (5, Target::Body(BodyFocus::Inventory), FocusScopeId(0), 2),
            (6, Target::Body(BodyFocus::Back), FocusScopeId(0), 3),
            (7, Target::Body(BodyFocus::Settings), FocusScopeId(0), 4),
            (
                8,
                Target::Shell(ProShellFocus::Workspace),
                FocusScopeId(0),
                5,
            ),
            (9, Target::Shell(ProShellFocus::Search), FocusScopeId(0), 6),
            (
                20,
                Target::Shell(ProShellFocus::Content),
                FocusScopeId(0),
                7,
            ),
        ] {
            handles.push((target, focus.register(FocusId(id), scope, order, true)));
        }
        for token in (dashboard::ROOM_TOKEN_BASE
            ..dashboard::ROOM_TOKEN_BASE + dashboard::MAX_ROOMS as i32)
            .chain(
                dashboard::OBSERVATION_TOKEN_BASE
                    ..dashboard::OBSERVATION_TOKEN_BASE + dashboard::MAX_OBSERVATIONS as i32,
            )
        {
            handles.push((
                Target::Home(token),
                focus.register(FocusId(token as u64), FocusScopeId(0), token as i64, true),
            ));
        }
        Self {
            route: Route::Home,
            history: Vec::new(),
            request,
            focus,
            handles,
            current: None,
            overlays: OverlayStack::new(),
            compact: false,
        }
    }
    fn handle(&self, target: Target) -> Option<FocusHandle> {
        self.handles
            .iter()
            .find_map(|(candidate, handle)| same_target(*candidate, target).then_some(*handle))
    }
    fn target(&self, handle: FocusHandle) -> Target {
        self.handles
            .iter()
            .find(|(_, candidate)| *candidate == handle)
            .map(|(target, _)| *target)
            .expect("registered focus handle")
    }
    fn open(&mut self, route: Route, opener: Target) {
        if self.history.len() == 16 {
            self.history.remove(0);
        }
        self.history.push(Entry {
            route: self.route.clone(),
            opener,
        });
        self.route = route;
    }
    fn request_changed(&mut self) -> i32 {
        let mut request = self
            .request
            .lock()
            .expect("presentation request mutex poisoned");
        request.revision = request
            .revision
            .checked_add(1)
            .expect("presentation revision exhausted");
        request.room = match &self.route {
            Route::Room(id) => Some(id.clone()),
            Route::Detail(_) => self.history.last().and_then(|entry| match &entry.route {
                Route::Room(id) => Some(id.clone()),
                Route::Home
                | Route::Rooms
                | Route::Inventory
                | Route::Detail(_)
                | Route::Settings => None,
            }),
            Route::Home | Route::Rooms | Route::Inventory | Route::Settings => None,
        };
        request.detail = match &self.route {
            Route::Detail(id) => Some(id.clone()),
            Route::Home | Route::Rooms | Route::Inventory | Route::Room(_) | Route::Settings => {
                None
            }
        };
        request.revision
    }
    fn close(&mut self, reason: DismissReason) -> bool {
        self.overlays.close(DRAWER, reason);
        let open = self.overlays.contains(DRAWER);
        if !open {
            self.focus.pop_trap(MODAL);
        }
        open
    }
}
fn same_target(a: Target, b: Target) -> bool {
    match (a, b) {
        (Target::Shell(a), Target::Shell(b)) => a == b,
        (Target::Body(a), Target::Body(b)) => a == b,
        (Target::Home(a), Target::Home(b)) => a == b,
        (Target::Shell(_), Target::Body(_) | Target::Home(_))
        | (Target::Body(_), Target::Shell(_) | Target::Home(_))
        | (Target::Home(_), Target::Shell(_) | Target::Body(_)) => false,
    }
}
fn render(window: &HomewardWindow, state: &State) {
    let request = state
        .request
        .lock()
        .expect("presentation request mutex poisoned");
    let (page, title) = match &state.route {
        Route::Home => (ViewPage::Home, "Accueil"),
        Route::Rooms => (ViewPage::Rooms, "Mes pièces"),
        Route::Inventory => (ViewPage::Inventory, "Inventaire"),
        Route::Room(id) => (
            ViewPage::Room,
            request
                .dashboard
                .layout()
                .and_then(|layout| layout.room(id))
                .map_or("Pièce indisponible", |room| room.label.as_str()),
        ),
        Route::Detail(_) => (ViewPage::Detail, "Détail"),
        Route::Settings => (ViewPage::Settings, "Préférences"),
    };
    window.set_page(page);
    window.set_page_title(title.into());
    window.set_projection_version(request.revision);
    window.set_navigation_drawer(DialogState {
        open: state.overlays.contains(DRAWER),
        topmost: true,
        busy: false,
    });
}
fn take_focus(window: &HomewardWindow, shared: &Rc<RefCell<State>>, target: Target) {
    let requested = {
        let mut state = shared.borrow_mut();
        state
            .handle(target)
            .and_then(|handle| state.focus.request(handle))
    };
    if let Some(request) = requested {
        match target {
            Target::Shell(target) => {
                window.set_home_focus(FocusRequest::default());
                window.invoke_take_shell_focus(target);
            }
            Target::Body(target) => {
                window.set_home_focus(FocusRequest::default());
                window.invoke_take_body_focus(target);
            }
            Target::Home(token) => window.set_home_focus(FocusRequest {
                token,
                generation: window.get_home_generation(),
                present: true,
                request: i32::try_from(request.serial).expect("bounded focus serial"),
            }),
        }
    }
}
fn reason(reason: CloseReason) -> DismissReason {
    match reason {
        CloseReason::Escape => DismissReason::Escape,
        CloseReason::OutsidePress => DismissReason::OutsidePress,
        CloseReason::SystemBack => DismissReason::SystemBack,
        CloseReason::TriggerToggle => DismissReason::TriggerToggle,
        CloseReason::Programmatic => DismissReason::Programmatic,
        CloseReason::HostClosed => DismissReason::HostClosed,
    }
}
fn changed(window: &HomewardWindow, shared: &Rc<RefCell<State>>, refresh: &dyn Fn(), reset: bool) {
    {
        let mut state = shared.borrow_mut();
        state.request_changed();
        render(window, &state);
    }
    if reset {
        window.invoke_reset_list();
    }
    refresh();
}

fn select_section(binding: &SelectBinding<Section>, section: Section) {
    // An already open controller preserves its active cursor. External route
    // changes begin a new selection session at the acknowledged section.
    binding.dismiss(DismissReason::Programmatic);
    binding.set_selected(Some(section));
    binding.open();
}

pub(crate) fn bind(window: &HomewardWindow, request: SharedRequest, refresh: impl Fn() + 'static) {
    let shared = Rc::new(RefCell::new(State::new(request)));
    let refresh: Rc<dyn Fn()> = Rc::new(refresh);
    let weak = window.as_weak();
    let rows_generation = std::cell::Cell::new(-1);
    let selection = SelectBinding::new(
        [
            Section::Home,
            Section::Rooms,
            Section::Inventory,
            Section::Settings,
        ]
        .into_iter()
        .map(|section| SelectOption::new(section, section.label()))
        .collect(),
        Some(Section::Home),
        Ownership::Local,
        move |snapshot| {
            if let Some(window) = weak.upgrade() {
                if rows_generation.replace(snapshot.generation) != snapshot.generation {
                    window.set_navigation_rows(ModelRc::new(VecModel::from(
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
                }
                let selection = |token: Option<i32>| SelectionRef {
                    present: token.is_some(),
                    token: token.unwrap_or(-1),
                };
                window.set_navigation_state(SelectState {
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
    selection.open();
    let (state, weak, refresh_copy, reopen) = (
        shared.clone(),
        window.as_weak(),
        refresh.clone(),
        selection.clone(),
    );
    selection.on_intent(move |intent| {
        if let Some(window) = weak.upgrade() {
            match intent {
                SelectionIntent::Commit(section) => {
                    {
                        let mut state = state.borrow_mut();
                        state.route = section.route();
                        state.history.clear();
                        state.close(DismissReason::Programmatic);
                    }
                    changed(&window, &state, &*refresh_copy, true);
                    // Reopen through the window callback rather than retaining a binding in its own handler.
                    window.invoke_navigation_reopen();
                }
                SelectionIntent::Clear => {}
            }
        }
    });
    window.on_navigation_reopen(move || reopen.open());
    let target = selection.clone();
    window.on_navigation_activate(move |token, generation| target.activate(token, generation));
    let target = selection.clone();
    window.on_navigation_typeahead(move |text| target.typeahead(text.as_str()));
    let target = selection.clone();
    window.on_navigation_step(move |step| {
        target.navigate(match step {
            NavigationStep::Next => slint_kit::NavigationStep::Next,
            NavigationStep::Previous => slint_kit::NavigationStep::Previous,
            NavigationStep::First => slint_kit::NavigationStep::First,
            NavigationStep::Last => slint_kit::NavigationStep::Last,
            NavigationStep::PageNext => slint_kit::NavigationStep::PageNext,
            NavigationStep::PagePrevious => slint_kit::NavigationStep::PagePrevious,
        })
    });
    let (state, weak) = (shared.clone(), window.as_weak());
    window.on_content_focus(move || {
        if let Some(window) = weak.upgrade() {
            let target = match &state.borrow().route {
                Route::Home | Route::Rooms => {
                    if window.get_view_loaded() {
                        Target::Shell(ProShellFocus::Workspace)
                    } else {
                        Target::Body(BodyFocus::Read)
                    }
                }
                Route::Inventory | Route::Room(_) => Target::Body(BodyFocus::Inventory),
                Route::Detail(_) => Target::Body(BodyFocus::Back),
                Route::Settings => Target::Body(BodyFocus::Settings),
            };
            take_focus(&window, &state, target);
        }
    });
    let (state, weak) = (shared.clone(), window.as_weak());
    window.on_navigation_open(move || {
        let Some(window) = weak.upgrade() else { return };
        {
            let mut state = state.borrow_mut();
            if state.overlays.contains(DRAWER) {
                return;
            }
            let mut spec = OverlaySpec::new(DRAWER, None, true);
            spec.restore_focus = state.handle(Target::Shell(ProShellFocus::NavigationTrigger));
            state.overlays.push(spec).expect("unique drawer");
            state.focus.push_trap(MODAL).expect("registered drawer");
        }
        window.invoke_navigation_reopen();
        render(&window, &state.borrow());
    });
    let (state, weak) = (shared.clone(), window.as_weak());
    window.on_navigation_dismiss(move |why| {
        if let Some(window) = weak.upgrade() {
            state.borrow_mut().close(reason(why));
            render(&window, &state.borrow());
        }
    });
    let (state, weak) = (shared.clone(), window.as_weak());
    window.on_shell_compact(move |compact| {
        if let Some(window) = weak.upgrade() {
            {
                let mut state = state.borrow_mut();
                state.compact = compact;
                if !compact {
                    state.close(DismissReason::Programmatic);
                }
            }
            render(&window, &state.borrow());
        }
    });
    let (state, weak) = (shared.clone(), window.as_weak());
    window.on_shell_restore(move || {
        if let Some(window) = weak.upgrade() {
            let target = if state.borrow().compact {
                ProShellFocus::NavigationTrigger
            } else {
                ProShellFocus::NavigationItems
            };
            take_focus(&window, &state, Target::Shell(target));
        }
    });
    let (state, weak) = (shared.clone(), window.as_weak());
    window.on_shell_observed(move |target, focused| {
        {
            let mut state = state.borrow_mut();
            let handle = state.handle(Target::Shell(target));
            if focused {
                state.current = handle;
            } else if state.current == handle {
                state.current = None;
            }
        }
        if focused && let Some(window) = weak.upgrade() {
            window.set_home_focus(FocusRequest::default());
        }
    });
    let (state, weak) = (shared.clone(), window.as_weak());
    window.on_home_observed(move |token, generation, focused| {
        if !weak
            .upgrade()
            .is_some_and(|window| window.get_home_generation() == generation)
        {
            return;
        }
        let mut state = state.borrow_mut();
        let handle = state.handle(Target::Home(token));
        if focused {
            state.current = handle;
        } else if state.current == handle {
            state.current = None;
        }
    });
    let (state, weak) = (shared.clone(), window.as_weak());
    window.on_modal_next(move |reverse| {
        let target = {
            let mut state = state.borrow_mut();
            let current = state.current;
            state
                .focus
                .cycle(
                    MODAL,
                    current,
                    if reverse {
                        FocusDirection::Backward
                    } else {
                        FocusDirection::Forward
                    },
                )
                .map(|request| state.target(request.target))
        };
        if let (Some(window), Some(target)) = (weak.upgrade(), target) {
            take_focus(&window, &state, target);
        }
    });
    let (state, weak, run, selected) = (
        shared.clone(),
        window.as_weak(),
        refresh.clone(),
        selection.clone(),
    );
    window.on_query_edited(move |text| {
        if let Some(window) = weak.upgrade() {
            {
                let mut state = state.borrow_mut();
                state
                    .request
                    .lock()
                    .expect("presentation request mutex poisoned")
                    .query = text.chars().take(256).collect::<String>().to_lowercase();
                state.route = Route::Inventory;
                state.history.clear();
            }
            select_section(&selected, Section::Inventory);
            changed(&window, &state, &*run, true);
        }
    });
    let (state, weak, run, selected) = (
        shared.clone(),
        window.as_weak(),
        refresh.clone(),
        selection.clone(),
    );
    window.on_room_open(move |token, generation| {
        if let Some(window) = weak.upgrade() {
            let model = window.get_cache_rows();
            let id = model
                .as_any()
                .downcast_ref::<CacheRows>()
                .and_then(|model| {
                    model.resolve_room(
                        token,
                        generation,
                        state
                            .borrow()
                            .request
                            .lock()
                            .expect("presentation request mutex poisoned")
                            .dashboard
                            .layout(),
                    )
                });
            let Some(id) = id else {
                return;
            };
            state
                .borrow_mut()
                .open(Route::Room(id), Target::Home(token));
            select_section(&selected, Section::Rooms);
            changed(&window, &state, &*run, true);
            take_focus(&window, &state, Target::Body(BodyFocus::Back));
        }
    });
    let (state, weak, run) = (shared.clone(), window.as_weak(), refresh.clone());
    window.on_cache_activate(move |token, generation| {
        if let Some(window) = weak.upgrade() {
            let model = window.get_cache_rows();
            let id = model
                .as_any()
                .downcast_ref::<CacheRows>()
                .and_then(|model| {
                    model.resolve(token, generation, window.get_projection_version())
                });
            if let Some(id) = id {
                state
                    .borrow_mut()
                    .open(Route::Detail(id), Target::Body(BodyFocus::Inventory));
                changed(&window, &state, &*run, false);
                take_focus(&window, &state, Target::Body(BodyFocus::Back));
            }
        }
    });
    let (state, weak, run) = (shared.clone(), window.as_weak(), refresh.clone());
    window.on_observation_open(move |token, generation| {
        if let Some(window) = weak.upgrade() {
            let model = window.get_cache_rows();
            let id = model
                .as_any()
                .downcast_ref::<CacheRows>()
                .and_then(|model| {
                    model.resolve_observation(token, generation, window.get_projection_version())
                });
            if let Some(id) = id {
                state
                    .borrow_mut()
                    .open(Route::Detail(id), Target::Home(token));
                changed(&window, &state, &*run, false);
                take_focus(&window, &state, Target::Body(BodyFocus::Back));
            }
        }
    });
    let (state, weak, run, selected) = (
        shared.clone(),
        window.as_weak(),
        refresh.clone(),
        selection.clone(),
    );
    window.on_go_back(move || {
        if let Some(window) = weak.upgrade() {
            let opener = {
                let mut state = state.borrow_mut();
                let previous_detail = match &state.route {
                    Route::Detail(id) => Some(id.clone()),
                    Route::Home
                    | Route::Rooms
                    | Route::Inventory
                    | Route::Room(_)
                    | Route::Settings => None,
                };
                match state.history.pop() {
                    Some(entry) => {
                        state.route = entry.route;
                        match entry.opener {
                            Target::Home(token)
                                if (dashboard::OBSERVATION_TOKEN_BASE
                                    ..dashboard::OBSERVATION_TOKEN_BASE
                                        + dashboard::MAX_OBSERVATIONS as i32)
                                    .contains(&token) =>
                            {
                                let model = window.get_cache_rows();
                                let current =
                                    model
                                        .as_any()
                                        .downcast_ref::<CacheRows>()
                                        .and_then(|model| {
                                            model.resolve_observation(
                                                token,
                                                window.get_home_generation(),
                                                window.get_projection_version(),
                                            )
                                        });
                                if current.is_some() && current == previous_detail {
                                    entry.opener
                                } else {
                                    Target::Body(BodyFocus::Read)
                                }
                            }
                            Target::Shell(_) | Target::Body(_) | Target::Home(_) => entry.opener,
                        }
                    }
                    None => {
                        state.route = Route::Home;
                        Target::Body(BodyFocus::Read)
                    }
                }
            };
            select_section(
                &selected,
                match &state.borrow().route {
                    Route::Home => Section::Home,
                    Route::Rooms | Route::Room(_) => Section::Rooms,
                    Route::Inventory | Route::Detail(_) => Section::Inventory,
                    Route::Settings => Section::Settings,
                },
            );
            changed(&window, &state, &*run, false);
            take_focus(&window, &state, opener);
        }
    });
    let (state, weak, run, selected) = (
        shared.clone(),
        window.as_weak(),
        refresh.clone(),
        selection.clone(),
    );
    window.on_show_inventory(move || {
        if let Some(window) = weak.upgrade() {
            state
                .borrow_mut()
                .open(Route::Inventory, Target::Body(BodyFocus::Read));
            select_section(&selected, Section::Inventory);
            changed(&window, &state, &*run, true);
            take_focus(&window, &state, Target::Body(BodyFocus::Inventory));
        }
    });
    let (state, weak, run, selected) = (
        shared.clone(),
        window.as_weak(),
        refresh.clone(),
        selection.clone(),
    );
    window.on_choose_room(move || {
        if let Some(window) = weak.upgrade() {
            state
                .borrow_mut()
                .open(Route::Rooms, Target::Shell(ProShellFocus::Workspace));
            select_section(&selected, Section::Rooms);
            changed(&window, &state, &*run, false);
        }
    });
    let (state, weak) = (shared.clone(), window.as_weak());
    window.on_system_back(move || {
        let Some(window) = weak.upgrade() else {
            return false;
        };
        if window.get_show_endpoint()
            || window.get_access_stage() != crate::ProAccessStage::Connected
        {
            return false;
        }
        if state.borrow().overlays.contains(DRAWER) {
            window.invoke_navigation_dismiss(CloseReason::SystemBack);
            return true;
        }
        if state.borrow().route == Route::Home {
            return false;
        }
        window.invoke_go_back();
        true
    });
    let (layout_state, layout_window, layout_refresh, layout_selection) = (
        shared.clone(),
        window.as_weak(),
        refresh.clone(),
        selection.clone(),
    );
    window.on_dashboard_changed(move || {
        if let Some(window) = layout_window.upgrade() {
            {
                let mut state = layout_state.borrow_mut();
                state.route = Route::Home;
                state.history.clear();
                state.current = None;
                state.close(DismissReason::Programmatic);
                state
                    .request
                    .lock()
                    .expect("presentation request mutex poisoned")
                    .query
                    .clear();
            }
            window.set_search_query("".into());
            window.set_home_focus(FocusRequest::default());
            select_section(&layout_selection, Section::Home);
            changed(&window, &layout_state, &*layout_refresh, true);
        }
    });
    let (state, weak) = (shared, window.as_weak());
    window.on_private_view_cleared(move |forget_dashboard| {
        if let Some(window) = weak.upgrade() {
            {
                let mut state = state.borrow_mut();
                state.history.clear();
                state.current = None;
                state.route = Route::Home;
                state.close(DismissReason::Programmatic);
                let revision = state
                    .request
                    .lock()
                    .expect("presentation request mutex poisoned")
                    .revision
                    .checked_add(1)
                    .expect("presentation revision exhausted");
                let mut request = state
                    .request
                    .lock()
                    .expect("presentation request mutex poisoned");
                let dashboard = if forget_dashboard {
                    dashboard::LayoutState::default()
                } else {
                    request.dashboard.clone()
                };
                *request = ProjectionRequest {
                    revision,
                    dashboard,
                    ..Default::default()
                };
            }
            window.set_search_query("".into());
            select_section(&selection, Section::Home);
            render(&window, &state.borrow());
        }
    });
}
