//! Restore a source node through the newly published bindings, never an old UI
//! row. Focus acknowledgments and live window geometry have separate lifetimes.
use crate::{
    FocusRequest, HomewardWindow,
    dashboard::{Layout, LayoutState, ProjectionRequest, RoomId, SharedRequest},
    dashboard_layout::NodeId,
    device_view::{DeviceStore, NativePrepared},
};
use home_client_model::{api::EntityName, dashboard::Destination};
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
    detail: Option<EntityName>,
    detail_source: Option<NodeId>,
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
            detail_source: request.detail_source.clone(),
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
            && request.detail_source == self.detail_source
    }
    pub fn layout(&self) -> Option<&Arc<Layout>> {
        self.layout.as_ref()
    }
    pub fn origin(&self) -> Option<&NodeId> {
        self.origin.as_ref()
    }
    pub fn has_origin(&self) -> bool {
        self.origin.is_some()
    }
    pub fn catalogue(&self) -> Option<&crate::dashboard_catalogue::CatalogueToken> {
        self.catalogue.as_ref()
    }
    pub fn query(&self) -> &str {
        &self.query
    }
    pub fn readmit(&mut self, snapshot: &crate::dashboard_catalogue::Snapshot) -> bool {
        let Some(previous) = &self.layout else {
            return false;
        };
        let Some(current) = snapshot.layout(&previous.id) else {
            return false;
        };
        if !previous.same_document(&current) {
            return false;
        }
        self.layout = Some(current);
        self.catalogue = Some(snapshot.token().clone());
        true
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
    focused_node: Option<NodeId>,
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
            focused_node: None,
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
                    state.focused_node = None;
                    state.window.upgrade()
                };
                if let Some(window) = window {
                    window.set_recipe_focus(FocusRequest::default());
                    window.set_tile_focus(FocusRequest::default());
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
                    state.focused_node = None;
                    state.window.upgrade()
                };
                if let Some(window) = window {
                    window.set_recipe_focus(FocusRequest::default());
                    window.set_tile_focus(FocusRequest::default());
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
                    let node = (state.devices)()
                        .and_then(|store| store.recipe_node(token, generation, revision));
                    if let Some(node) = node {
                        state.focused_node = Some(node);
                        state.focused = Some(Handle {
                            token,
                            generation,
                            revision,
                        });
                    }
                }
                window.set_recipe_focus(FocusRequest::default());
                window.set_tile_focus(FocusRequest::default());
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
                if let Some(node) = (state.devices)()
                    .and_then(|store| store.recipe_node(token, generation, revision))
                {
                    state.focused_node = Some(node);
                    state.focused = Some(handle);
                }
            } else if state.focused == Some(handle) {
                // A capture rollover temporarily removes all board rows until
                // the new dashboard proof is admitted. Retain only the focus
                // identity; readmit() will resolve it against fresh bindings.
                if state
                    .window
                    .upgrade()
                    .is_some_and(|window| window.get_has_configured_layout())
                {
                    state.focused = None;
                    state.focused_node = None;
                }
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
    pub fn readmit(&self) {
        let mut state = self.0.borrow_mut();
        let Some(window) = state.window.upgrade() else {
            return;
        };
        let request = state
            .request
            .lock()
            .expect("presentation request mutex poisoned")
            .clone();
        let Some(snapshot) = &request.catalogue else {
            return;
        };
        if let Some(pending) = &mut state.pending {
            if !pending.view.readmit(snapshot) {
                state.pending = None;
            }
        } else if let (Some(node), Some(layout)) =
            (&state.focused_node, request.dashboard.shared_layout())
        {
            state.pending = Some(Pending {
                view: SavedView::capture(
                    &window,
                    &request,
                    Some(Origin {
                        node: node.clone(),
                        layout,
                    }),
                ),
                revision: request.revision,
            });
        }
        // The route revision is intentionally stable across a proof renewal.
        // Its previous publication therefore cannot acknowledge fresh rows.
        state.published = None;
        drop(state);
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
            let tile_focus = if focus.is_none() {
                pending.view.origin.as_ref().and_then(|node| {
                    (state.devices)()?.native_focus(
                        node,
                        pending.view.layout.as_deref()?,
                        pending.revision,
                    )
                })
            } else {
                None
            };
            let view = state.pending.take().expect("checked pending focus").view;
            let tile = tile_focus.is_some();
            let request = focus
                .map(|row| (row.token, row.generation))
                .or(tile_focus)
                .map(|(token, generation)| {
                    state.serial = state
                        .serial
                        .checked_add(1)
                        .expect("native focus serial exhausted");
                    FocusRequest {
                        present: true,
                        token,
                        generation,
                        request: state.serial,
                    }
                });
            (window, view, request, tile)
        };
        let (window, view, request, tile) = resolved;
        window.invoke_restore_board_scroll(view.home_scroll, view.room_scroll, view.popup_scroll);
        if let Some(request) = request {
            if tile {
                window.set_tile_focus(request);
            } else {
                window.set_recipe_focus(request);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::dashboard_catalogue::Snapshot;
    use home_client_cache::CachePhase;
    use home_client_model::dashboard::{Intention, Presentation};

    #[test]
    fn nhf035_focus_waits_for_fresh_bindings_after_a_capture_rollover() {
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        window.set_view_loaded(true);
        window.set_has_configured_layout(true);
        let first = crate::dashboard::capture_fixture(vec![]);
        let mut layout = crate::dashboard::joined_fixture(&first);
        let mut native = crate::dashboard_layout::NativeLayout::try_from(
            &crate::dashboard_layout::tests::extended_forest(),
        )
        .unwrap();
        let node = NodeId::parse("recipe").unwrap();
        let destination = Destination {
            dashboard: layout.id.clone(),
            node: NodeId::parse("popup").unwrap(),
            presentation: Presentation::Popup,
        };
        for interaction in native
            .nodes
            .get_mut(&node)
            .unwrap()
            .recipes
            .as_mut()
            .unwrap()
            .interactions
            .values_mut()
        {
            interaction.intention = Intention::Navigate(destination.clone());
        }
        layout.native = Some(native);
        let layout = Arc::new(layout);
        let request = Arc::new(std::sync::Mutex::new(ProjectionRequest {
            dashboard: LayoutState::Ready(layout.clone()),
            catalogue: Some(Snapshot::fixture("first", vec![layout.clone()])),
            ..Default::default()
        }));
        let devices = DeviceStore::default();
        let projected = devices.project(CachePhase::Live, Some(&first), &request.lock().unwrap());
        let old = projected.recipes.get(&node).unwrap();
        let source = devices.clone();
        let host = Host::bind(
            &window,
            request.clone(),
            Rc::new(move || Some(source.clone())),
        );
        window.invoke_recipe_focus_observed(old.token, old.generation, true);
        assert_eq!(host.0.borrow().focused_node, Some(node.clone()));

        // New capture arrives before its dashboard Query response. Admission
        // removes old controls, but the local focus identity is not authority.
        let second = crate::dashboard::capture_fixture(vec![]);
        devices.project(CachePhase::Live, Some(&second), &request.lock().unwrap());
        window.set_has_configured_layout(false);
        window.invoke_recipe_focus_observed(old.token, old.generation, false);
        assert_eq!(host.0.borrow().focused_node, Some(node.clone()));
        let mut renewed = layout.as_ref().clone();
        renewed.capture = Some(second.incarnation().clone());
        renewed.binding_expires = Some((2_000_000_000, 0));
        let renewed = Arc::new(renewed);
        {
            let mut request = request.lock().unwrap();
            request.dashboard = LayoutState::Ready(renewed.clone());
            request.catalogue = Some(Snapshot::fixture("renewed", vec![renewed]));
        }
        host.readmit();
        host.apply();
        assert!(
            host.0.borrow().pending.is_some(),
            "old publication cannot consume pending focus"
        );
        assert!(!window.get_recipe_focus().present);
        let projected = devices.project(CachePhase::Live, Some(&second), &request.lock().unwrap());
        let current = projected.recipes.get(&node).unwrap();
        assert_ne!(current.generation, old.generation);
        assert!(devices.recipe_node(old.token, old.generation, 0).is_none());
        window.set_has_configured_layout(true);
        window.invoke_native_layout_published(0);
        host.apply();
        let restored = window.get_recipe_focus();
        assert!(restored.present);
        assert_eq!(
            (restored.token, restored.generation),
            (current.token, current.generation)
        );
        window.invoke_native_route_changed();
        assert!(host.0.borrow().focused_node.is_none());
        assert!(!window.get_recipe_focus().present);
    }
}
