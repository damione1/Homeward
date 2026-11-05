//! Window-local routes and focus. No transport or device commands.
use crate::{
    BodyFocus, CloseReason, DialogState, FocusRequest, HomewardWindow, NavigationStep, OptionRow,
    ProShellFocus, SelectState, SelectionAvailability, SelectionRef, ViewPage,
    cache_view::CacheRows,
    dashboard::{self, ProjectionRequest, Route, SharedRequest},
};
use home_client_model::dashboard::{Destination, Presentation};
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
    Lists,
    Calendars,
    Cameras,
    Settings,
}
impl Section {
    fn label(self) -> &'static str {
        match self {
            Self::Home => "Home",
            Self::Rooms => "Rooms",
            Self::Lists => "Lists",
            Self::Calendars => "Calendar",
            Self::Cameras => "Cameras",
            Self::Settings => "Settings",
        }
    }
    fn route(self) -> Route {
        match self {
            Self::Home => Route::Home,
            Self::Rooms => Route::Rooms,
            Self::Lists => Route::Lists,
            Self::Calendars => Route::Calendars,
            Self::Cameras => Route::Cameras,
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
    view: crate::dashboard_focus::SavedView,
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
    choices: Option<crate::dashboard_selection::Choices>,
}
#[derive(Clone)]
pub(crate) struct NativeNavigation {
    state: Rc<RefCell<State>>,
    window: slint::Weak<HomewardWindow>,
    refresh: Rc<dyn Fn()>,
    selection: SelectBinding<Section>,
    focus: crate::dashboard_focus::Host,
}
impl NativeNavigation {
    pub(crate) fn notification_home(&self) {
        let Some(window) = self.window.upgrade() else {
            return;
        };
        let opener = self.state.borrow().fallback_opener();
        self.state.borrow_mut().open(&window, Route::Home, opener);
        changed(&window, &self.state, &*self.refresh, false);
        take_focus(&window, &self.state, Target::Body(BodyFocus::Read));
    }
    pub fn attach_catalogue(&self, choices: crate::dashboard_selection::Choices) {
        self.state.borrow_mut().choices = Some(choices);
    }
    pub fn restore_origin(&self, prepared: &crate::device_view::NativePrepared) {
        self.focus.restore_origin(prepared);
    }
    pub fn more_info(
        &self,
        id: home_client_model::api::EntityName,
        origin: Option<crate::dashboard_focus::Origin>,
    ) -> Result<(), crate::device_view::Refusal> {
        let Some(window) = self.window.upgrade() else {
            return Err(crate::device_view::Refusal::Stale);
        };
        if origin.as_ref().is_some_and(|origin| {
            self.state
                .borrow()
                .request
                .lock()
                .expect("presentation request mutex poisoned")
                .dashboard
                .layout()
                != Some(origin.layout.as_ref())
        }) {
            return Err(crate::device_view::Refusal::Stale);
        }
        let opener = self.state.borrow().fallback_opener();
        self.state
            .borrow_mut()
            .open_from(&window, Route::Detail(id), opener, origin);
        changed(&window, &self.state, &*self.refresh, false);
        take_focus(&window, &self.state, Target::Body(BodyFocus::Back));
        Ok(())
    }
    pub fn navigate(
        &self,
        destination: Destination,
        expected: &dashboard::Layout,
        origin: Option<crate::dashboard_focus::Origin>,
    ) -> Result<(), crate::device_view::Refusal> {
        use crate::device_view::Refusal;
        let Some(window) = self.window.upgrade() else {
            return Err(Refusal::Stale);
        };
        if origin
            .as_ref()
            .is_some_and(|origin| origin.layout.as_ref() != expected)
        {
            return Err(Refusal::Stale);
        }
        let target = {
            let state = self.state.borrow();
            let request = state
                .request
                .lock()
                .expect("presentation request mutex poisoned");
            let layout = request
                .dashboard
                .layout()
                .filter(|layout| *layout == expected)
                .ok_or(Refusal::Stale)?;
            let target = if destination.dashboard.as_str() == layout.id.as_str() {
                request.dashboard.shared_layout().ok_or(Refusal::Stale)?
            } else {
                if !window.get_view_loaded() {
                    return Err(Refusal::Unavailable);
                }
                let snapshot = request
                    .catalogue
                    .as_ref()
                    .filter(|snapshot| snapshot.contains(layout))
                    .ok_or(Refusal::Stale)?;
                snapshot
                    .layout(&destination.dashboard)
                    .ok_or(Refusal::Unavailable)?
            };
            let node = target
                .native
                .as_ref()
                .and_then(|native| native.nodes.get(&destination.node))
                .ok_or(Refusal::Unavailable)?;
            let kind = match destination.presentation {
                Presentation::Page => crate::dashboard_layout::Kind::View,
                Presentation::Popup => crate::dashboard_layout::Kind::Popup,
            };
            if node.kind != kind {
                return Err(Refusal::Unsupported);
            }
            target
        };
        if self.state.borrow().route == Route::Native(destination.clone()) {
            return Ok(());
        }
        let presentation = destination.presentation;
        let opener = self.state.borrow().fallback_opener();
        {
            let mut state = self.state.borrow_mut();
            let crossing = target.id != expected.id;
            if crossing {
                let request = state
                    .request
                    .lock()
                    .expect("presentation request mutex poisoned");
                let snapshot = request.catalogue.as_ref().ok_or(Refusal::Stale)?;
                if !state
                    .choices
                    .as_ref()
                    .is_some_and(|choices| choices.select_authorized(snapshot, &target.id))
                {
                    return Err(Refusal::Stale);
                }
            }
            // Capture the source view before changing the active dashboard.
            state.open_from(&window, Route::Native(destination), opener, origin);
            if crossing {
                let mut request = state
                    .request
                    .lock()
                    .expect("presentation request mutex poisoned");
                request.dashboard = dashboard::LayoutState::Ready(target);
                request.query.clear();
                window.set_search_query("".into());
            }
        }
        window.invoke_dashboard_catalogue_changed();
        crate::cache_view::clear_native_target(&window, presentation);
        if presentation == Presentation::Page {
            let section = self.state.borrow().section();
            select_section(&self.selection, section);
        }
        changed(&window, &self.state, &*self.refresh, false);
        take_focus(
            &window,
            &self.state,
            Target::Body(match presentation {
                Presentation::Page => BodyFocus::Back,
                Presentation::Popup => BodyFocus::Popup,
            }),
        );
        Ok(())
    }
}
impl State {
    // Keep configured dashboard drill-downs in their originating shell section.
    // A source page or equipment details is not an inventory/Rooms selection.
    fn section(&self) -> Section {
        fn direct(route: &Route) -> Option<Section> {
            match route {
                Route::Home => Some(Section::Home),
                Route::Rooms | Route::Room(_) => Some(Section::Rooms),
                Route::Inventory => Some(Section::Settings),
                Route::Lists => Some(Section::Lists),
                Route::Calendars => Some(Section::Calendars),
                Route::Cameras => Some(Section::Cameras),
                Route::Settings => Some(Section::Settings),
                Route::Native(_) | Route::Detail(_) => None,
            }
        }
        direct(&self.route)
            .or_else(|| {
                self.history
                    .iter()
                    .rev()
                    .find_map(|entry| direct(&entry.route))
            })
            .unwrap_or(Section::Home)
    }
    fn restore_dashboard(
        &self,
        window: &HomewardWindow,
        view: &crate::dashboard_focus::SavedView,
    ) -> bool {
        let mut request = self
            .request
            .lock()
            .expect("presentation request mutex poisoned");
        if request.catalogue.as_ref().map(|snapshot| snapshot.token()) != view.catalogue() {
            return false;
        }
        if request.dashboard.layout() == view.layout().map(AsRef::as_ref) {
            return true;
        }
        let Some(layout) = view.layout() else {
            return false;
        };
        let Some(snapshot) = request
            .catalogue
            .as_ref()
            .filter(|snapshot| snapshot.contains(layout))
        else {
            return false;
        };
        if !self
            .choices
            .as_ref()
            .is_some_and(|choices| choices.select_authorized(snapshot, &layout.id))
        {
            return false;
        }
        request.dashboard = dashboard::LayoutState::Ready(layout.clone());
        request.query = view.query().to_owned();
        window.set_search_query(view.query().into());
        true
    }
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
            (10, Target::Body(BodyFocus::Popup), FocusScopeId(0), 8),
            (11, Target::Body(BodyFocus::Calendars), FocusScopeId(0), 9),
            (12, Target::Body(BodyFocus::Cameras), FocusScopeId(0), 10),
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
            choices: None,
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
    fn open(&mut self, window: &HomewardWindow, route: Route, opener: Target) {
        self.open_from(window, route, opener, None);
    }
    fn open_from(
        &mut self,
        window: &HomewardWindow,
        route: Route,
        opener: Target,
        origin: Option<crate::dashboard_focus::Origin>,
    ) {
        if self.history.len() == 16 {
            self.history.remove(0);
        }
        self.history.push(Entry {
            route: self.route.clone(),
            opener,
            view: crate::dashboard_focus::SavedView::capture(
                window,
                &self
                    .request
                    .lock()
                    .expect("presentation request mutex poisoned"),
                origin,
            ),
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
        let context = match &self.route {
            Route::Detail(_) => self
                .history
                .last()
                .map(|entry| &entry.route)
                .unwrap_or(&self.route),
            Route::Home
            | Route::Rooms
            | Route::Inventory
            | Route::Room(_)
            | Route::Native(_)
            | Route::Lists
            | Route::Calendars
            | Route::Cameras
            | Route::Settings => &self.route,
        };
        request.room = match context {
            Route::Room(id) => Some(id.clone()),
            Route::Home
            | Route::Rooms
            | Route::Inventory
            | Route::Native(_)
            | Route::Detail(_)
            | Route::Lists
            | Route::Calendars
            | Route::Cameras
            | Route::Settings => None,
        };
        request.native = match context {
            Route::Native(destination) => Some(destination.clone()),
            Route::Home
            | Route::Rooms
            | Route::Inventory
            | Route::Room(_)
            | Route::Detail(_)
            | Route::Lists
            | Route::Calendars
            | Route::Cameras
            | Route::Settings => None,
        };
        request.detail = match &self.route {
            Route::Detail(id) => Some(id.clone()),
            Route::Home
            | Route::Rooms
            | Route::Inventory
            | Route::Room(_)
            | Route::Native(_)
            | Route::Lists
            | Route::Calendars
            | Route::Cameras
            | Route::Settings => None,
        };
        request.revision
    }
    fn fallback_opener(&self) -> Target {
        match &self.route {
            Route::Home | Route::Rooms => Target::Shell(ProShellFocus::Workspace),
            Route::Room(_) | Route::Detail(_) => Target::Body(BodyFocus::Back),
            Route::Inventory => Target::Body(BodyFocus::Inventory),
            Route::Lists => Target::Body(BodyFocus::Lists),
            Route::Calendars => Target::Body(BodyFocus::Calendars),
            Route::Cameras => Target::Body(BodyFocus::Cameras),
            Route::Settings => Target::Body(BodyFocus::Settings),
            Route::Native(destination) => Target::Body(match destination.presentation {
                Presentation::Page => BodyFocus::Back,
                Presentation::Popup => BodyFocus::Popup,
            }),
        }
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
    let popup = match &state.route {
        Route::Native(destination) => destination.presentation == Presentation::Popup,
        Route::Home
        | Route::Rooms
        | Route::Inventory
        | Route::Room(_)
        | Route::Detail(_)
        | Route::Lists
        | Route::Calendars
        | Route::Cameras
        | Route::Settings => false,
    };
    let home = Route::Home;
    let displayed = if popup {
        state.history.iter().rev().find(|entry|
            entry.view.layout().map(AsRef::as_ref) == request.dashboard.layout()
            && !matches!(&entry.route, Route::Native(destination) if destination.presentation == Presentation::Popup)
        ).map_or(&home, |entry| &entry.route)
    } else {
        &state.route
    };
    let (page, title) = match displayed {
        Route::Home => (ViewPage::Home, "Home"),
        Route::Rooms => (ViewPage::Rooms, "My Rooms"),
        Route::Inventory => (ViewPage::Inventory, "Inventory"),
        Route::Room(id) => (
            ViewPage::Room,
            request
                .dashboard
                .layout()
                .and_then(|layout| layout.room(id))
                .map_or("Room unavailable", |room| room.label.as_str()),
        ),
        Route::Detail(_) => (ViewPage::Detail, "Detail"),
        Route::Native(destination) => (
            ViewPage::Room,
            request
                .dashboard
                .layout()
                .and_then(|layout| layout.native.as_ref())
                .and_then(|native| native.nodes.get(&destination.node))
                .map_or("View unavailable", |node| node.title.as_str()),
        ),
        Route::Lists => (ViewPage::Lists, "Lists"),
        Route::Calendars => (ViewPage::Calendars, "Calendar"),
        Route::Cameras => (ViewPage::Cameras, "Cameras"),
        Route::Settings => (ViewPage::Settings, "Settings"),
    };
    let background = state
        .history
        .iter()
        .rev()
        .find_map(|entry| match &entry.route {
            Route::Home => Some(ViewPage::Home),
            Route::Rooms => Some(ViewPage::Rooms),
            Route::Inventory => Some(ViewPage::Inventory),
            Route::Room(_) => Some(ViewPage::Room),
            Route::Native(destination) if destination.presentation == Presentation::Page => {
                Some(ViewPage::Room)
            }
            Route::Native(_)
            | Route::Detail(_)
            | Route::Lists
            | Route::Calendars
            | Route::Cameras
            | Route::Settings => None,
        })
        .unwrap_or(ViewPage::Home);
    window.set_detail_background_page(background);
    window.set_page(page);
    window.set_page_title(title.into());
    window.set_native_page(matches!(displayed, Route::Native(destination) if destination.presentation == Presentation::Page));
    window.set_native_popup_title(match &state.route {
        Route::Native(destination) if popup => request
            .dashboard
            .layout()
            .and_then(|layout| layout.native.as_ref())
            .and_then(|native| native.nodes.get(&destination.node))
            .map_or("Room unavailable", |node| node.title.as_str())
            .into(),
        Route::Home
        | Route::Rooms
        | Route::Inventory
        | Route::Room(_)
        | Route::Native(_)
        | Route::Detail(_)
        | Route::Lists
        | Route::Calendars
        | Route::Cameras
        | Route::Settings => "".into(),
    });
    let detail_popup = if let Route::Native(destination) = &state.route {
        request
            .dashboard
            .layout()
            .and_then(|layout| layout.native.as_ref())
            .and_then(|native| {
                let node = native.nodes.get(&destination.node)?;
                (node.children.len() == 1)
                    .then(|| native.nodes.get(&node.children[0]))
                    .flatten()
            })
            .and_then(|node| node.widget.as_ref())
            .is_some_and(|widget| {
                matches!(
                    widget.content,
                    crate::dashboard_widgets::Content::Thermostat { detailed: true, .. }
                )
            })
    } else {
        false
    };
    window.set_native_popup_detail(detail_popup);
    window.set_native_popup_can_back(popup && !state.history.is_empty());
    window.set_native_popup_generation(request.revision);
    window.set_native_popup(DialogState {
        open: popup,
        topmost: true,
        busy: false,
    });
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
    window.invoke_history_private_cleared();
    window.invoke_media_private_cleared();
    window.invoke_native_route_changed();
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

#[cfg(test)]
pub(crate) fn bind(window: &HomewardWindow, request: SharedRequest, refresh: impl Fn() + 'static) {
    bind_devices(window, request, refresh, || None);
}
pub(crate) fn bind_devices(
    window: &HomewardWindow,
    request: SharedRequest,
    refresh: impl Fn() + 'static,
    resolve_devices: impl Fn() -> Option<crate::device_view::DeviceStore> + 'static,
) -> NativeNavigation {
    #[cfg(target_os = "android")]
    bind_native_back(window);
    let devices: crate::dashboard_focus::Devices = Rc::new(resolve_devices);
    let focus = crate::dashboard_focus::Host::bind(window, request.clone(), devices.clone());
    let shared = Rc::new(RefCell::new(State::new(request)));
    let refresh: Rc<dyn Fn()> = Rc::new(refresh);
    let weak = window.as_weak();
    let rows_generation = std::cell::Cell::new(-1);
    let selection = SelectBinding::new(
        [
            Section::Home,
            Section::Rooms,
            Section::Lists,
            Section::Calendars,
            Section::Cameras,
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
    let (state, weak, run, selected) = (
        shared.clone(),
        window.as_weak(),
        refresh.clone(),
        selection.clone(),
    );
    window.on_quick_navigation(move |page| {
        let section = match page {
            ViewPage::Home => Section::Home,
            ViewPage::Rooms => Section::Rooms,
            ViewPage::Inventory => Section::Settings,
            ViewPage::Lists => Section::Lists,
            ViewPage::Calendars => Section::Calendars,
            ViewPage::Cameras => Section::Cameras,
            ViewPage::Settings => Section::Settings,
            ViewPage::Room | ViewPage::Detail => return,
        };
        if let Some(window) = weak.upgrade() {
            if window.get_access_stage() != crate::ProAccessStage::Connected
                || window.get_show_endpoint()
                || window.get_native_popup().open
                || window.get_navigation_drawer().open
                || window.get_recipe_confirmation().open
            {
                return;
            }
            state.borrow_mut().open(
                &window,
                if page == ViewPage::Inventory {
                    Route::Inventory
                } else {
                    section.route()
                },
                Target::Shell(ProShellFocus::Content),
            );
            select_section(&selected, section);
            changed(&window, &state, &*run, true);
        }
    });
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
            let configured_rooms = matches!(state.borrow().route, Route::Rooms)
                && window.get_rooms_layout_nodes().row_count() > 0;
            if configured_rooms {
                if !window.invoke_dashboard_focus_first() {
                    take_focus(&window, &state, Target::Shell(ProShellFocus::Workspace));
                }
                return;
            }
            let target = match &state.borrow().route {
                Route::Home => Target::Body(BodyFocus::Read),
                Route::Rooms => {
                    if window.get_view_loaded() {
                        window
                            .get_home_rooms()
                            .row_data(0)
                            .map(|room| Target::Home(room.token))
                            .unwrap_or(Target::Shell(ProShellFocus::Workspace))
                    } else {
                        Target::Body(BodyFocus::Read)
                    }
                }
                Route::Inventory => Target::Body(BodyFocus::Inventory),
                Route::Room(_) => match window.get_room_view() {
                    crate::RoomView::Cards => Target::Body(BodyFocus::Back),
                    crate::RoomView::List => Target::Body(BodyFocus::Inventory),
                },
                Route::Detail(_) => Target::Body(BodyFocus::Back),
                Route::Native(destination) => Target::Body(match destination.presentation {
                    Presentation::Page => BodyFocus::Back,
                    Presentation::Popup => BodyFocus::Popup,
                }),
                Route::Lists => Target::Body(BodyFocus::Lists),
                Route::Calendars => Target::Body(BodyFocus::Calendars),
                Route::Cameras => Target::Body(BodyFocus::Cameras),
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
            window.invoke_native_focus_cancelled();
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
            select_section(&selected, Section::Settings);
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
                .open(&window, Route::Room(id), Target::Home(token));
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
                state.borrow_mut().open(
                    &window,
                    Route::Detail(id),
                    Target::Body(BodyFocus::Inventory),
                );
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
                    .open(&window, Route::Detail(id), Target::Home(token));
                changed(&window, &state, &*run, false);
                take_focus(&window, &state, Target::Body(BodyFocus::Back));
            }
        }
    });
    let (state, weak, run) = (shared.clone(), window.as_weak(), refresh.clone());
    window.on_device_details(move |token, generation| {
        if let Some(window) = weak.upgrade()
            && let Some(id) = devices()
                .and_then(|store| store.entity(token, generation, window.get_projection_version()))
        {
            if state.borrow().route == Route::Detail(id.clone()) {
                return;
            }
            state
                .borrow_mut()
                .open(&window, Route::Detail(id), Target::Body(BodyFocus::Back));
            changed(&window, &state, &*run, false);
            take_focus(&window, &state, Target::Body(BodyFocus::Back));
        }
    });
    let (state, weak, run, selected) = (
        shared.clone(),
        window.as_weak(),
        refresh.clone(),
        selection.clone(),
    );
    let restore = focus.clone();
    window.on_go_back(move || {
        if let Some(window) = weak.upgrade() {
            let (opener, saved_view) = {
                let mut state = state.borrow_mut();
                let previous_detail = match &state.route {
                    Route::Detail(id) => Some(id.clone()),
                    Route::Home
                    | Route::Rooms
                    | Route::Inventory
                    | Route::Room(_)
                    | Route::Native(_)
                    | Route::Lists
            | Route::Calendars
            | Route::Cameras
            | Route::Settings => None,
                };
                match state.history.pop() {
                    Some(entry) => {
                        if state.restore_dashboard(&window, &entry.view) {
                        state.route = entry.route;
                        let opener = match entry.opener {
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
                        };
                        (opener, Some(entry.view))
                        } else {
                            state.history.clear();
                            state.route = Route::Home;
                            window.set_action_feedback("The original dashboard is no longer available in the current catalogue.".into());
                            (Target::Body(BodyFocus::Read), None)
                        }
                    }
                    None => {
                        state.route = Route::Home;
                        (Target::Body(BodyFocus::Read), None)
                    }
                }
            };
            window.invoke_dashboard_catalogue_changed();
            if let Route::Native(destination) = &state.borrow().route { crate::cache_view::clear_native_target(&window, destination.presentation); }
            changed(&window, &state, &*run, false);
            let section = state.borrow().section();
            select_section(&selected, section);
            let opener = if matches!(&state.borrow().route, Route::Native(destination) if destination.presentation == Presentation::Popup) { Target::Body(BodyFocus::Popup) } else { opener };
            // The source node is resolved against the next published forest.
            // Generic focus here can open Search and cancel that restoration.
            if !saved_view.as_ref().is_some_and(|view| view.has_origin()) {
                take_focus(&window, &state, opener);
            }
            if let Some(view) = saved_view {
                let revision = state.borrow().request.lock().expect("presentation request mutex poisoned").revision;
                restore.restore(view, revision);
            }
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
                .open(&window, Route::Inventory, Target::Body(BodyFocus::Settings));
            select_section(&selected, Section::Settings);
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
            state.borrow_mut().open(
                &window,
                Route::Rooms,
                Target::Shell(ProShellFocus::Workspace),
            );
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
        if window.get_recipe_confirmation().open {
            window.invoke_recipe_dismiss(
                window.get_recipe_confirmation_id(),
                CloseReason::SystemBack,
            );
            return true;
        }
        // A platform fallback can arrive after native IME focus changes, so a
        // page-local popup still gets first refusal without replaying Key.Back.
        if window.invoke_dismiss_selection_popover() {
            return true;
        }
        if window.get_page() == ViewPage::Home && window.get_dashboard_choice_state().open {
            window.invoke_dashboard_choice_dismiss(CloseReason::SystemBack);
            return true;
        }
        if matches!(
            window.get_page(),
            ViewPage::Home | ViewPage::Room | ViewPage::Detail
        ) {
            for device in window.get_device_cards().iter() {
                if device.select_state.open
                    && let Some(field) = device
                        .values
                        .iter()
                        .find(|field| field.kind == crate::ProValueKind::Select)
                {
                    window.invoke_device_edit(
                        device.token,
                        device.generation,
                        field.token,
                        crate::ProValueIntent {
                            event: crate::ProValueEvent::SelectDismiss,
                            close_reason: CloseReason::SystemBack,
                            ..Default::default()
                        },
                    );
                    return true;
                }
            }
        }
        if window.get_page() == ViewPage::Room && window.get_room_view() == crate::RoomView::List {
            window.set_room_view(crate::RoomView::Cards);
            take_focus(&window, &state, Target::Body(BodyFocus::Back));
            return true;
        }
        if state.borrow().route == Route::Home {
            return false;
        }
        window.invoke_go_back();
        true
    });
    let (state, weak) = (shared.clone(), window.as_weak());
    window.on_native_popup_dismiss(move |generation, reason| {
        let Some(window) = weak.upgrade() else { return; };
        let valid = {
            let state = state.borrow();
            let revision = state.request.lock().expect("presentation request mutex poisoned").revision;
            revision == generation
                && matches!(&state.route, Route::Native(destination) if destination.presentation == Presentation::Popup)
        };
        if valid {
            if matches!(reason, crate::CloseReason::Escape | crate::CloseReason::SystemBack) {
                window.invoke_go_back();
                return;
            }
            // One surface, one dismissal: Close leaves the whole popup trail.
            // The separate Back control below restores exactly one saved view.
            for _ in 0..64 {
                if !matches!(&state.borrow().route, Route::Native(destination) if destination.presentation == Presentation::Popup) { break; }
                window.invoke_go_back();
            }
        }
    });
    let (state, weak) = (shared.clone(), window.as_weak());
    window.on_native_popup_back(move |generation| {
        let Some(window) = weak.upgrade() else { return; };
        let valid = {
            let state = state.borrow();
            let revision = state.request.lock().expect("presentation request mutex poisoned").revision;
            revision == generation && matches!(&state.route, Route::Native(destination) if destination.presentation == Presentation::Popup)
        };
        if valid { window.invoke_go_back(); }
    });
    let (proof_state, proof_window, proof_refresh, proof_focus) = (
        shared.clone(),
        window.as_weak(),
        refresh.clone(),
        focus.clone(),
    );
    window.on_dashboard_refreshed(move || {
        let Some(window) = proof_window.upgrade() else {
            return;
        };
        {
            let mut state = proof_state.borrow_mut();
            let snapshot = state
                .request
                .lock()
                .expect("presentation request mutex poisoned")
                .catalogue
                .clone();
            if let Some(snapshot) = snapshot {
                state
                    .history
                    .retain_mut(|entry| entry.view.readmit(&snapshot));
            }
            render(&window, &state);
        }
        proof_focus.readmit();
        // No route change, revision bump, focus request or scroll reset. The
        // existing route is reprojected solely with the newly admitted proof.
        proof_refresh();
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
    let selection_for_native = selection.clone();
    let (state, weak) = (shared.clone(), window.as_weak());
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
                let catalogue = if forget_dashboard {
                    None
                } else {
                    request.catalogue.clone()
                };
                *request = ProjectionRequest {
                    revision,
                    dashboard,
                    catalogue,
                    ..Default::default()
                };
            }
            window.set_search_query("".into());
            select_section(&selection, Section::Home);
            render(&window, &state.borrow());
        }
    });
    NativeNavigation {
        state: shared,
        window: window.as_weak(),
        refresh,
        selection: selection_for_native,
        focus,
    }
}

// Android's Back fallback must reach the route owner even with no focused
// widget. Desktop window-close keeps its ordinary close semantics.
#[cfg(any(target_os = "android", test))]
pub(crate) fn bind_native_back(window: &HomewardWindow) {
    let weak = window.as_weak();
    window.window().on_close_requested(move || {
        if weak
            .upgrade()
            .is_some_and(|window| window.invoke_system_back())
        {
            slint::CloseRequestResponse::KeepWindowShown
        } else {
            slint::CloseRequestResponse::HideWindow
        }
    });
}

#[cfg(test)]
mod back_tests {
    use super::*;
    use i_slint_backend_testing::ElementQuery;
    use slint::platform::PointerEventButton;
    use std::sync::{Arc, Mutex};

    #[test]
    fn nhf035_capture_renewals_preserve_page_popup_focus_and_back_with_fresh_proofs() {
        use crate::dashboard_catalogue::Snapshot;
        use crate::dashboard_selection::{Catalogue, Choice, Choices};
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        crate::cache_view::bind(&window);
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_show_endpoint(false);
        let request = Arc::new(Mutex::new(ProjectionRequest::default()));
        let navigation = bind_devices(&window, request.clone(), || {}, || None);
        let choices = Choices::default();
        navigation.attach_catalogue(choices.clone());
        let mut layout = dashboard::joined_fixture(&dashboard::capture_fixture(vec![]));
        layout.native = Some(
            crate::dashboard_layout::NativeLayout::try_from(
                &crate::dashboard_layout::tests::extended_forest(),
            )
            .unwrap(),
        );
        let publish = |layout: dashboard::Layout, token: &str| {
            let layout = Arc::new(layout);
            let snapshot = Snapshot::fixture(token, vec![layout.clone()]);
            crate::session_host::publish_dashboard(
                &window,
                &request,
                &choices,
                0,
                Catalogue {
                    choices: vec![Choice {
                        id: layout.id.clone(),
                        title: layout.title.clone(),
                    }],
                    selected: Some(layout.id.clone()),
                    snapshot: Some(snapshot),
                },
                dashboard::LayoutState::Ready(layout),
            );
        };
        publish(layout.clone(), "first-proof");
        window.set_view_loaded(true);
        window.invoke_quick_navigation(ViewPage::Settings);
        window.invoke_shell_observed(ProShellFocus::Search, true);
        let focus = navigation.state.borrow().current;
        let revision = request.lock().unwrap().revision;
        for i in 0..3 {
            let previous = request.lock().unwrap().dashboard.shared_layout().unwrap();
            layout.capture = Some(dashboard::capture_fixture(vec![]).incarnation().clone());
            layout.binding_expires = Some((2_000_000_000 + i, 0));
            publish(layout.clone(), &format!("new-proof-{i}"));
            assert_eq!(window.get_page(), ViewPage::Settings);
            assert_eq!(navigation.state.borrow().current, focus);
            assert_eq!(request.lock().unwrap().revision, revision);
            let current = request.lock().unwrap().dashboard.shared_layout().unwrap();
            assert_ne!(previous, current, "the old authority proof is replaced");
            assert!(previous.same_document(&current));
            assert!(
                !request
                    .lock()
                    .unwrap()
                    .catalogue
                    .as_ref()
                    .unwrap()
                    .contains(&previous)
            );
        }
        let destination = Destination {
            dashboard: layout.id.clone(),
            node: crate::dashboard_layout::NodeId::parse("popup").unwrap(),
            presentation: Presentation::Popup,
        };
        navigation
            .navigate(destination.clone(), &layout, None)
            .unwrap();
        assert!(window.get_native_popup().open);
        let popup_revision = request.lock().unwrap().revision;
        layout.capture = Some(dashboard::capture_fixture(vec![]).incarnation().clone());
        layout.binding_expires = Some((2_000_000_010, 0));
        publish(layout.clone(), "popup-renewal");
        assert!(window.get_native_popup().open);
        assert_eq!(request.lock().unwrap().native, Some(destination));
        assert_eq!(request.lock().unwrap().revision, popup_revision);
        window.invoke_go_back();
        assert!(!window.get_native_popup().open);
        assert_eq!(
            window.get_page(),
            ViewPage::Settings,
            "Back uses readmitted history"
        );

        // A new document (even under the same stable dashboard identity) still
        // retires the previous route and focus rather than retaining stale UI.
        layout.etag = "changed-document".into();
        publish(layout, "changed-source");
        assert_eq!(window.get_page(), ViewPage::Home);
        assert!(navigation.state.borrow().current.is_none());
        assert!(navigation.state.borrow().history.is_empty());

        window.invoke_quick_navigation(ViewPage::Rooms);
        crate::session_host::publish_dashboard(
            &window,
            &request,
            &choices,
            0,
            choices.hidden(),
            dashboard::LayoutState::Loading,
        );
        assert_eq!(
            window.get_page(),
            ViewPage::Home,
            "invalidation still clears navigation"
        );
        assert!(!window.get_view_loaded());
        assert!(window.get_home_layout_nodes().row_count() == 0);
        assert!(request.lock().unwrap().catalogue.is_none());
    }

    fn window() -> HomewardWindow {
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        bind(
            &window,
            Arc::new(Mutex::new(ProjectionRequest::default())),
            || {},
        );
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_endpoint_ready(true);
        window.set_show_endpoint(false);
        window
            .window()
            .set_size(slint::LogicalSize::new(1100., 1600.));
        window
    }

    #[test]
    fn inventory_is_a_settings_destination_and_back_returns_to_settings() {
        let window = window();
        let labels: Vec<_> = window
            .get_navigation_rows()
            .iter()
            .map(|row| row.label.to_string())
            .collect();
        assert_eq!(
            labels,
            ["Home", "Rooms", "Lists", "Calendar", "Cameras", "Settings"]
        );
        window.invoke_quick_navigation(ViewPage::Settings);
        window.invoke_show_inventory();
        assert_eq!(window.get_page(), ViewPage::Inventory);
        let selection = window.get_navigation_state();
        let settings = window
            .get_navigation_rows()
            .iter()
            .find(|row| row.label == "Settings")
            .unwrap();
        assert_eq!(selection.selected.token, settings.token);
        window.invoke_go_back();
        assert_eq!(window.get_page(), ViewPage::Settings);
    }

    #[test]
    fn phone_room_header_keeps_back_title_and_close_inside_at_double_text() {
        // Exercise the real popup component in isolation. The ordinary helper
        // binds a Home route owner, whose focus observer correctly republishes
        // that route and would discard manually injected popup fixture state.
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_endpoint_ready(true);
        window.set_show_endpoint(false);
        window
            .window()
            .set_size(slint::LogicalSize::new(360., 740.));
        window.set_large_text(true);
        window.show().unwrap();
        window.set_native_popup_title("Kitchen".into());
        window.set_native_popup_generation(7);
        window.set_native_popup_can_back(true);
        let backs = Rc::new(std::cell::Cell::new(0));
        let out = backs.clone();
        window.on_native_popup_back(move |generation| {
            assert_eq!(generation, 7);
            out.set(out.get() + 1);
        });
        window.set_native_popup(crate::DialogState {
            open: true,
            topmost: true,
            ..Default::default()
        });
        for _ in 0..3 {
            slint::platform::update_timers_and_animations();
            i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(1));
        }
        assert!(window.get_native_popup().open);
        let popup = ElementQuery::from_root(&window)
            .match_inherits("HomewardPopup")
            .find_first()
            .expect("the native popup component is mounted");
        let control = |name: &str| {
            let name = name.to_owned();
            popup
                .query_descendants()
                .match_accessible_role(i_slint_backend_testing::AccessibleRole::Button)
                .match_predicate(move |e| e.accessible_label().as_deref() == Some(name.as_str()))
                .find_first()
                .expect("the open popup exposes its named header action")
        };
        let back = control("Back");
        let close = control("Close Kitchen");
        for button in [&back, &close] {
            let p = button.absolute_position();
            let size = button.size();
            assert!(
                p.x >= 16. && p.x + size.width <= 344.,
                "header action outside phone: {:?} {:?}",
                p,
                size
            );
            assert!(p.y >= 0. && p.y + size.height <= 740.);
            assert!(size.width >= 48. && size.height >= 48.);
        }
        let title = popup
            .query_descendants()
            .match_type_name("Text")
            .match_predicate(|e| e.accessible_label().as_deref() == Some("Kitchen"))
            .find_first()
            .expect("the popup header exposes the configured room title");
        assert!(title.size().width > 0. && title.size().height > 0.);
        assert!(title.absolute_position().x >= back.absolute_position().x + back.size().width + 7.);
        assert!(
            title.absolute_position().x + title.size().width <= close.absolute_position().x - 7.
        );
        back.mock_single_click(PointerEventButton::Left);
        assert_eq!(backs.get(), 1);
    }

    #[test]
    fn configured_rooms_content_focus_uses_the_configured_board_before_legacy_catalogue() {
        let window = window();
        let requested = Rc::new(std::cell::Cell::new(0));
        let observed = requested.clone();
        window.on_dashboard_focus_first(move || {
            observed.set(observed.get() + 1);
            true
        });
        window.set_rooms_layout_nodes(ModelRc::from([crate::ProLayoutNode::default()]));
        window.invoke_quick_navigation(ViewPage::Rooms);
        window.invoke_content_focus();
        assert_eq!(requested.get(), 1);
        window.set_rooms_layout_nodes(ModelRc::default());
        window.invoke_content_focus();
        assert_eq!(
            requested.get(),
            1,
            "legacy Rooms keeps its existing focus path"
        );
    }

    #[test]
    fn native_back_fallback_closes_list_picker_before_leaving_lists() {
        let window = window();
        window.on_lists_filter_options(crate::list_host::filter_options);
        window.on_lists_find_option(crate::list_host::find_option);
        window.set_lists_generation(7);
        window.set_lists_catalog_ready(true);
        window.set_lists_options(ModelRc::from([OptionRow {
            token: 18,
            generation: 7,
            label: "Synthetic groceries".into(),
            ..Default::default()
        }]));
        window.invoke_quick_navigation(ViewPage::Lists);
        window.show().unwrap();
        let trigger = ElementQuery::from_root(&window)
            .match_inherits("KitSelectTrigger")
            .match_predicate(|item| item.accessible_label().as_deref() == Some("Choose a list"))
            .find_first()
            .unwrap();
        trigger.mock_single_click(PointerEventButton::Left);
        slint::platform::update_timers_and_animations();
        let search_present = || {
            ElementQuery::from_root(&window)
                .match_inherits("TextInput")
                .match_predicate(|item| {
                    item.accessible_label().as_deref() == Some("Search options")
                })
                .find_first()
                .is_some()
        };
        assert!(search_present(), "the real list selection popup is mounted");
        assert!(
            window.invoke_system_back(),
            "fallback is consumed by the popup owner"
        );
        slint::platform::update_timers_and_animations();
        assert_eq!(window.get_page(), ViewPage::Lists);
        assert!(
            !search_present(),
            "Back closed the popup without replaying a key"
        );
        assert!(window.invoke_system_back());
        assert_eq!(window.get_page(), ViewPage::Home);
        window.hide().unwrap();
    }

    #[test]
    fn native_back_fallback_closes_calendar_picker_before_leaving_calendars() {
        let window = window();
        window.set_calendars(crate::CalendarPresentation {
            selected: true,
            ready: true,
            generation: 9,
            title: "Synthetic calendar".into(),
            ..Default::default()
        });
        window.invoke_quick_navigation(ViewPage::Calendars);
        window.show().unwrap();
        let button = ElementQuery::from_root(&window)
            .match_inherits("KitButton")
            .match_predicate(|item| item.accessible_label().as_deref() == Some("Open Calendar"))
            .find_first()
            .unwrap();
        button.mock_single_click(PointerEventButton::Left);
        slint::platform::update_timers_and_animations();
        let calendar_present = || {
            ElementQuery::from_root(&window)
                .match_inherits("KitCalendar")
                .find_first()
                .is_some()
        };
        assert!(
            calendar_present(),
            "the real date selection popup is mounted"
        );
        assert!(window.invoke_system_back());
        slint::platform::update_timers_and_animations();
        assert_eq!(window.get_page(), ViewPage::Calendars);
        assert!(!calendar_present());
        assert!(window.invoke_system_back());
        assert_eq!(window.get_page(), ViewPage::Home);
        window.hide().unwrap();
    }
}
