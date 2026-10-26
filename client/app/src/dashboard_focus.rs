//! Restore a source node through the newly published bindings, never an old UI
//! row. Focus acknowledgments and live window geometry have separate lifetimes.
use crate::{
    FocusRequest, HomewardWindow,
    dashboard::{Layout, LayoutState, ProjectionRequest, RoomId, SharedRequest},
    dashboard_layout::NodeId,
    device_view::{DeviceStore, NativePrepared},
};
use home_client_model::{EntityId, dashboard::Destination};
use slint::ComponentHandle;
use std::{cell::RefCell, rc::Rc, sync::Arc, time::Duration};

pub(crate) type Devices = Rc<dyn Fn() -> Option<DeviceStore>>;

#[derive(Clone)]
pub(crate) struct Origin {
    pub node: NodeId,
    pub layout: Arc<Layout>,
}
impl Origin {
    pub fn prepared(prepared: &NativePrepared) -> Self {
        Self {
            node: prepared.origin.clone(),
            layout: prepared.layout.clone(),
        }
    }
}

#[derive(Clone)]
pub(crate) struct SavedView {
    layout: Option<Arc<Layout>>,
    catalogue: Option<crate::dashboard_catalogue::CatalogueToken>,
    query: String,
    room: Option<RoomId>,
    native: Option<Destination>,
    detail: Option<EntityId>,
    origin: Option<NodeId>,
    home_scroll: f32,
    room_scroll: f32,
    popup_scroll: f32,
}
impl SavedView {
    pub fn capture(
        window: &HomewardWindow,
        request: &ProjectionRequest,
        origin: Option<Origin>,
    ) -> Self {
        Self {
            catalogue: request
                .catalogue
                .as_ref()
                .map(|snapshot| snapshot.token().clone()),
            query: request.query.clone(),
            layout: match &request.dashboard {
                LayoutState::Ready(layout) => Some(layout.clone()),
                LayoutState::Loading
                | LayoutState::Unavailable
                | LayoutState::Invalid
                | LayoutState::Unsupported(_) => None,
            },
            room: request.room.clone(),
            native: request.native.clone(),
            detail: request.detail.clone(),
            origin: origin
                .filter(|origin| request.dashboard.layout() == Some(origin.layout.as_ref()))
                .map(|origin| origin.node),
            home_scroll: window.get_home_scroll(),
            room_scroll: window.get_room_scroll(),
            popup_scroll: window.get_popup_scroll(),
        }
    }
    fn matches(&self, request: &ProjectionRequest) -> bool {
        request.dashboard.layout() == self.layout.as_deref()
            && request.catalogue.as_ref().map(|snapshot| snapshot.token())
                == self.catalogue.as_ref()
            && request.query == self.query
            && request.room == self.room
            && request.native == self.native
            && request.detail == self.detail
    }
    pub fn layout(&self) -> Option<&Arc<Layout>> {
        self.layout.as_ref()
    }
    pub fn catalogue(&self) -> Option<&crate::dashboard_catalogue::CatalogueToken> {
        self.catalogue.as_ref()
    }
    pub fn query(&self) -> &str {
        &self.query
    }
}
struct Pending {
    view: SavedView,
    revision: i32,
}
#[derive(Clone, Copy, PartialEq, Eq)]
struct Handle {
    token: i32,
    generation: i32,
    revision: i32,
}
struct State {
    request: SharedRequest,
    window: slint::Weak<HomewardWindow>,
    devices: Devices,
    pending: Option<Pending>,
    published: Option<i32>,
    focused: Option<Handle>,
    serial: i32,
    queued: bool,
}
#[derive(Clone)]
pub(crate) struct Host(Rc<RefCell<State>>);
impl Host {
    pub fn bind(window: &HomewardWindow, request: SharedRequest, devices: Devices) -> Self {
        let host = Self(Rc::new(RefCell::new(State {
            request,
            window: window.as_weak(),
            devices,
            pending: None,
            published: None,
            focused: None,
            serial: 0,
            queued: false,
        })));
        let weak = Rc::downgrade(&host.0);
        window.on_native_route_changed(move || {
            if let Some(state) = weak.upgrade() {
                let window = {
                    let mut state = state.borrow_mut();
                    state.pending = None;
                    state.published = None;
                    state.focused = None;
                    state.window.upgrade()
                };
                if let Some(window) = window {
                    window.set_recipe_focus(FocusRequest::default());
                }
            }
        });
        let weak = Rc::downgrade(&host.0);
        window.on_native_focus_cancelled(move || {
            if let Some(state) = weak.upgrade() {
                let window = {
                    let mut state = state.borrow_mut();
                    state.pending = None;
                    state.focused = None;
                    state.window.upgrade()
                };
                if let Some(window) = window {
                    window.set_recipe_focus(FocusRequest::default());
                }
            }
        });
        let weak = Rc::downgrade(&host.0);
        window.on_native_layout_published(move |revision| {
            if let Some(state) = weak.upgrade() {
                state.borrow_mut().published = Some(revision);
                Self(state).schedule();
            }
        });
        let weak = Rc::downgrade(&host.0);
        window.on_recipe_focus_applied(move |token, generation, serial| {
            let Some(state) = weak.upgrade() else {
                return;
            };
            let Some(window) = state.borrow().window.upgrade() else {
                return;
            };
            let request = window.get_recipe_focus();
            if request.present
                && (request.token, request.generation, request.request)
                    == (token, generation, serial)
            {
                {
                    let mut state = state.borrow_mut();
                    let revision = state
                        .request
                        .lock()
                        .expect("presentation request mutex poisoned")
                        .revision;
                    let current = (state.devices)()
                        .and_then(|store| store.recipe_node(token, generation, revision))
                        .is_some();
                    if current {
                        state.focused = Some(Handle {
                            token,
                            generation,
                            revision,
                        });
                    }
                }
                window.set_recipe_focus(FocusRequest::default());
            }
        });
        let weak = Rc::downgrade(&host.0);
        window.on_recipe_focus_observed(move |token, generation, focused| {
            let Some(state) = weak.upgrade() else {
                return;
            };
            let mut state = state.borrow_mut();
            let revision = state
                .request
                .lock()
                .expect("presentation request mutex poisoned")
                .revision;
            let handle = Handle {
                token,
                generation,
                revision,
            };
            if focused {
                state.pending = None;
                if (state.devices)()
                    .and_then(|store| store.recipe_node(token, generation, revision))
                    .is_some()
                {
                    state.focused = Some(handle);
                }
            } else if state.focused == Some(handle) {
                state.focused = None;
            }
        });
        let weak = Rc::downgrade(&host.0);
        window.on_recipe_focus_revealed(move |token, generation, top, bottom| {
            let Some(state) = weak.upgrade() else {
                return;
            };
            let target = {
                let state = state.borrow();
                let revision = state
                    .request
                    .lock()
                    .expect("presentation request mutex poisoned")
                    .revision;
                let valid = top.is_finite()
                    && bottom.is_finite()
                    && bottom >= top
                    && state.focused
                        == Some(Handle {
                            token,
                            generation,
                            revision,
                        })
                    && (state.devices)()
                        .and_then(|store| store.recipe_node(token, generation, revision))
                        .is_some();
                valid.then(|| state.window.upgrade()).flatten()
            };
            if let Some(window) = target {
                window.invoke_reveal_recipe(top, bottom);
            }
        });
        host
    }
    pub fn restore(&self, view: SavedView, revision: i32) {
        self.0.borrow_mut().pending = Some(Pending { view, revision });
        self.schedule();
    }
    pub fn restore_origin(&self, prepared: &NativePrepared) {
        let state = self.0.borrow();
        let Some(window) = state.window.upgrade() else {
            return;
        };
        let request = state
            .request
            .lock()
            .expect("presentation request mutex poisoned");
        if request.dashboard.layout() != Some(prepared.layout.as_ref()) {
            return;
        }
        let view = SavedView::capture(&window, &request, Some(Origin::prepared(prepared)));
        let revision = request.revision;
        drop(request);
        drop(state);
        self.restore(view, revision);
    }
    fn schedule(&self) {
        {
            let mut state = self.0.borrow_mut();
            if state.queued || state.pending.is_none() {
                return;
            }
            state.queued = true;
        }
        let weak = Rc::downgrade(&self.0);
        slint::Timer::single_shot(Duration::ZERO, move || {
            if let Some(state) = weak.upgrade() {
                state.borrow_mut().queued = false;
                Self(state).apply();
            }
        });
    }
    fn apply(&self) {
        let resolved = {
            let mut state = self.0.borrow_mut();
            let Some(pending) = state.pending.as_ref() else {
                return;
            };
            let Some(window) = state.window.upgrade() else {
                state.pending = None;
                return;
            };
            let request = state
                .request
                .lock()
                .expect("presentation request mutex poisoned")
                .clone();
            if request.revision != pending.revision
                || !pending.view.matches(&request)
                || !window.get_view_loaded()
                || window.get_search_focused()
            {
                state.pending = None;
                return;
            }
            if state.published != Some(pending.revision) {
                return;
            }
            let focus = pending
                .view
                .origin
                .as_ref()
                .and_then(|node| {
                    (state.devices)()?.recipe_focus(
                        node,
                        pending.view.layout.as_deref()?,
                        pending.revision,
                    )
                })
                .filter(|row| {
                    row.gestures.values().any(|access| match access {
                        crate::device_view::GestureAccess::Local => true,
                        crate::device_view::GestureAccess::Command => window.get_commands_ready(),
                        crate::device_view::GestureAccess::Absent
                        | crate::device_view::GestureAccess::Inactive => false,
                    })
                });
            let view = state.pending.take().expect("checked pending focus").view;
            let request = focus.map(|row| {
                state.serial = state
                    .serial
                    .checked_add(1)
                    .expect("native focus serial exhausted");
                FocusRequest {
                    present: true,
                    token: row.token,
                    generation: row.generation,
                    request: state.serial,
                }
            });
            (window, view, request)
        };
        let (window, view, request) = resolved;
        window.invoke_restore_board_scroll(view.home_scroll, view.room_scroll, view.popup_scroll);
        if let Some(request) = request {
            window.set_recipe_focus(request);
        }
    }
}
