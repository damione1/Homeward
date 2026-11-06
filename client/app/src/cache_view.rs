//! Product projection: private identities/cursors stay in the Rust cache. Slint
//! keeps the inventory virtualized at 64 rows. Configured dashboard forests
//! are separately bounded by their negotiated profile.
use crate::device_view::observation;
use crate::{
    ConnectionStatus, HomewardWindow, KitGlyphs, ProObservationRow, ProRoomRow, TableRow,
    dashboard::{self, Icon, Layout, ProjectionRequest},
};
use home_client_cache::{CacheEvent, CachePhase, CachedView, ProjectionGeneration, Viewport};
use home_client_model::api::EntityName;
use home_wire_contract::home::api::v2 as wire;
use slint::{ComponentHandle, Model, ModelNotify, ModelRc, ModelTracker, SharedString};
use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
};

#[derive(Clone, PartialEq, Eq)]
struct Row {
    id: Option<EntityName>,
    index: usize,
    cells: [String; 3],
}

// The authorization inventory is independent of changing observation values.
// Equality permits retaining old pixels briefly, never retaining command authority.
#[derive(Clone, PartialEq)]
struct CapabilityAuthority {
    name: String,
    etag: String,
    access: buffa::EnumValue<wire::ActionAccess>,
    kind: Option<std::mem::Discriminant<wire::capability::Trait>>,
}
#[derive(Clone, PartialEq)]
pub(crate) struct EntityAuthority {
    name: EntityName,
    etag: String,
    capabilities: Vec<CapabilityAuthority>,
}
pub(crate) fn authority(view: &CachedView) -> Option<Vec<EntityAuthority>> {
    view.entities()
        .map(|(name, entry)| {
            if !entry.__buffa_unknown_fields.is_empty()
                || !entry.entity.__buffa_unknown_fields.is_empty()
            {
                return None;
            }
            let mut capabilities = entry
                .capabilities
                .iter()
                .map(|cap| {
                    if !cap.__buffa_unknown_fields.is_empty() || cap.etag.is_empty() {
                        return None;
                    }
                    Some(CapabilityAuthority {
                        name: cap.name.clone(),
                        etag: cap.etag.clone(),
                        access: cap.action_access,
                        kind: cap.r#trait.as_ref().map(std::mem::discriminant),
                    })
                })
                .collect::<Option<Vec<_>>>()?;
            capabilities.sort_by(|left, right| left.name.cmp(&right.name));
            Some(EntityAuthority {
                name: name.clone(),
                etag: entry.entity.etag.clone(),
                capabilities,
            })
        })
        .collect()
}

pub(crate) struct CacheProjection {
    presentation: Option<home_client_cache::PresentationSource>,
    pending_cohort: Option<Layout>,
    authority: Option<Vec<EntityAuthority>>,
    history_entity: Option<EntityName>,
    revision: i32,
    phase: CachePhase,
    authorized_total: usize,
    total: usize,
    rows: Vec<Row>,
    home: HomeProjection,
    devices: crate::device_view::DeviceProjection,
    board: crate::dashboard_presentation::Board,
    profile_zone: crate::environment_view::ProfileZone,
}
impl CacheProjection {
    pub(crate) fn history_target(&self, revision: i32) -> Option<Option<EntityName>> {
        if self.pending_cohort.is_some() {
            return Some(None);
        }
        if !self.home.captured {
            return Some(None);
        }
        (self.revision == revision).then(|| self.history_entity.clone())
    }
}

struct Observation {
    token: i32,
    label: String,
    icon: Icon,
    id: Option<EntityName>,
    value: String,
    description: String,
}
#[derive(Default)]
struct HomeProjection {
    captured: bool,
    layout: Option<Layout>,
    status: String,
    room_counts: Vec<usize>,
    observations: Vec<Observation>,
    detail: Option<[String; 4]>,
}

#[cfg(test)]
pub(crate) fn project(
    phase: CachePhase,
    view: Option<&CachedView>,
    viewport: Viewport,
) -> CacheProjection {
    project_request(
        phase,
        view,
        viewport,
        &ProjectionRequest {
            dashboard: view
                .map(|view| {
                    dashboard::LayoutState::Ready(std::sync::Arc::new(dashboard::joined_fixture(
                        view,
                    )))
                })
                .unwrap_or_default(),
            ..Default::default()
        },
    )
}

#[cfg(test)]
pub(crate) fn project_request(
    phase: CachePhase,
    view: Option<&CachedView>,
    viewport: Viewport,
    request: &ProjectionRequest,
) -> CacheProjection {
    project_devices(
        phase,
        view,
        viewport,
        request,
        &crate::device_view::DeviceStore::default(),
    )
}

#[cfg(test)]
pub(crate) fn project_devices(
    phase: CachePhase,
    view: Option<&CachedView>,
    viewport: Viewport,
    request: &ProjectionRequest,
    devices: &crate::device_view::DeviceStore,
) -> CacheProjection {
    project_owner(phase, view, viewport, request, devices, None)
}

pub(crate) fn project_owner(
    mut phase: CachePhase,
    mut view: Option<&CachedView>,
    viewport: Viewport,
    request: &ProjectionRequest,
    devices: &crate::device_view::DeviceStore,
    renewal: Option<&home_client_cache::PresentationSource>,
) -> CacheProjection {
    let presentation = renewal
        .cloned()
        .or_else(|| view.and_then(home_client_cache::PresentationSource::from_view))
        .map(|p| {
            request
                .dashboard
                .layout()
                .and_then(|l| l.binding_expires)
                .map_or(p.clone(), |deadline| p.bounded(deadline))
        });
    if let Some(source) = &presentation
        && !source.source().is_current()
    {
        view = None;
        phase = if source.renewing() {
            CachePhase::Renewing
        } else {
            CachePhase::Blocked
        };
    }
    let layout = request
        .dashboard
        .layout()
        .filter(|layout| view.is_some_and(|view| layout.compatible(view)));
    let matches = |id: &EntityName, entity: &wire::EntityViewEntry| {
        request.room.as_ref().is_none_or(|room_id| {
            layout
                .and_then(|layout| layout.room(room_id))
                .is_some_and(|room| room.entities.contains(id))
        }) && (request.query.is_empty()
            || entity
                .entity
                .display_name
                .to_lowercase()
                .contains(&request.query))
    };
    let total = view.map_or(0, |view| {
        view.entities()
            .filter(|(id, entity)| matches(id, entity))
            .count()
    });
    let rows = view
        .map(|view| {
            view.entities()
                .filter(|(id, entity)| matches(id, entity))
                .enumerate()
                .skip(viewport.first())
                .take(viewport.count())
                .map(|(index, (id, entity))| {
                    let title = if entity.entity.display_name.is_empty() {
                        "Unnamed Item".into()
                    } else {
                        bounded(&entity.entity.display_name)
                    };
                    let (value, quality) = (entity_value(entity), entity_quality(entity));
                    Row {
                        id: Some(id.clone()),
                        index,
                        cells: [title, bounded(&value), bounded(&quality)],
                    }
                })
                .collect()
        })
        .unwrap_or_default();
    let room_counts = layout
        .map(|layout| {
            layout
                .rooms
                .iter()
                .map(|room| {
                    room.entities
                        .iter()
                        .filter(|id| view.is_some_and(|view| view.get(id).is_some()))
                        .count()
                })
                .collect()
        })
        .unwrap_or_default();
    let observations = layout
        .map(|layout| {
            layout
                .observations
                .iter()
                .enumerate()
                .map(|(index, item)| {
                    let entity = view.and_then(|view| view.get(&item.entity));
                    let value = entity
                        .map(entity_value)
                        .unwrap_or_else(|| "No observation".into());
                    Observation {
                        token: dashboard::OBSERVATION_TOKEN_BASE + index as i32,
                        label: item.label.clone(),
                        icon: item.icon,
                        id: entity.map(|_| item.entity.clone()),
                        value: bounded(&value),
                        description: entity
                            .map(entity_quality)
                            .unwrap_or_else(|| "No authorized observation for this item.".into()),
                    }
                })
                .collect()
        })
        .unwrap_or_default();
    let detail = request
        .detail
        .as_ref()
        .and_then(|id| view.and_then(|view| view.get(id)))
        .map(|entity| {
            let value = entity_value(entity);
            let quality = entity_quality(entity);
            let date = entity
                .capabilities
                .iter()
                .filter_map(observation::observed_time)
                .max()
                .map(|time| time.to_rfc3339())
                .unwrap_or_else(|| "Date not provided".into());
            [
                bounded(&entity.entity.display_name),
                bounded(&value),
                bounded(&quality),
                date,
            ]
        });
    // Dashboard v2 supplies the declared display profile separately. No time
    // zone is manufactured from a capture, a location or the system locale.
    let profile_zone = layout.map_or(crate::environment_view::ProfileZone::Missing, |layout| {
        layout.profile_zone
    });
    let devices = devices.project_owner(
        phase,
        view,
        request,
        presentation
            .as_ref()
            .filter(|_| phase == CachePhase::Renewing),
    );
    let board = crate::dashboard_presentation::Board::project(layout, request, &devices, view);
    let pending_cohort = request
        .dashboard
        .layout()
        .filter(|old| {
            let now = chrono::Utc::now();
            old.capture.is_some()
                && (phase == CachePhase::Live
                    && view.is_some_and(|view| old.capture.as_ref() != Some(view.incarnation()))
                    || phase == CachePhase::Renewing
                        && presentation.as_ref().is_some_and(|p| {
                            p.renewing() && old.capture.as_ref() == Some(p.incarnation())
                        }))
                && old.binding_expires.is_some_and(|expires| {
                    expires > (now.timestamp(), now.timestamp_subsec_nanos() as i32)
                })
        })
        .cloned();
    CacheProjection {
        presentation,
        pending_cohort,
        authority: view.and_then(authority),
        history_entity: if phase == CachePhase::Renewing
            || phase == CachePhase::Live && detail.is_some()
        {
            request.detail.clone()
        } else {
            None
        },
        devices,
        board,
        profile_zone,
        revision: request.revision,
        phase,
        authorized_total: view.map_or(0, CachedView::len),
        total,
        rows,
        home: HomeProjection {
            captured: view.is_some() || phase == CachePhase::Renewing,
            layout: layout.cloned(),
            status: if layout.is_some() {
                String::new()
            } else {
                request.dashboard.message().into()
            },
            room_counts,
            observations,
            detail,
        },
    }
}

fn on_off(value: bool) -> &'static str {
    if value { "On" } else { "Off" }
}
pub(crate) fn bounded(text: &str) -> String {
    match text.char_indices().nth(256) {
        Some((end, _)) => format!("{}…", &text[..end]),
        None => text.to_owned(),
    }
}
pub(crate) fn number(value: f64) -> String {
    if value.is_finite() {
        value.to_string()
    } else {
        "Unsupported value".into()
    }
}
pub(crate) fn value_text(capability: &wire::Capability) -> String {
    observation::value_text(capability)
}
pub(crate) fn quality_text(capability: &wire::Capability) -> String {
    observation::quality_text(capability)
}
fn entity_value(entity: &wire::EntityViewEntry) -> String {
    if entity.capabilities.is_empty() {
        return "No observation".into();
    }
    entity
        .capabilities
        .iter()
        .map(value_text)
        .collect::<Vec<_>>()
        .join(" · ")
}
fn entity_quality(entity: &wire::EntityViewEntry) -> String {
    if entity.capabilities.is_empty() {
        return "Quality not provided".into();
    }
    entity
        .capabilities
        .iter()
        .map(quality_text)
        .collect::<Vec<_>>()
        .join(" · ")
}

#[derive(Default)]
struct RowStore {
    presentation: Option<home_client_cache::PresentationSource>,
    authority: Option<Vec<EntityAuthority>>,
    generation: Option<ProjectionGeneration>,
    total: usize,
    rows: BTreeMap<usize, (Row, TableRow)>,
    revision: i32,
    observations: BTreeMap<i32, EntityName>,
    layout: Option<Layout>,
}
#[derive(Default)]
pub(crate) struct CacheRows {
    state: RefCell<RowStore>,
    pending_cohort: Cell<bool>,
    notify: ModelNotify,
    serial: Cell<i32>,
    home_serial: Cell<i32>,
    profile_zone: Cell<crate::environment_view::ProfileZone>,
    rooms: std::rc::Rc<slint::VecModel<ProRoomRow>>,
    observations: std::rc::Rc<slint::VecModel<ProObservationRow>>,
    devices: std::rc::Rc<slint::VecModel<crate::ProDeviceRow>>,
    details: std::rc::Rc<slint::VecModel<crate::HomewardDetailRow>>,
    home_layout_nodes: std::rc::Rc<slint::VecModel<crate::ProLayoutNode>>,
    rooms_layout_nodes: std::rc::Rc<slint::VecModel<crate::ProLayoutNode>>,
    room_layout_nodes: std::rc::Rc<slint::VecModel<crate::ProLayoutNode>>,
    popup_layout_nodes: std::rc::Rc<slint::VecModel<crate::ProLayoutNode>>,
    widget_bindings: RefCell<Vec<crate::dashboard_widgets::Binding>>,
    widget_focus_serial: Cell<i32>,
    widget_focus_mapping: RefCell<Option<(crate::FocusRequest, i32)>>,
    focused_widget_item: RefCell<
        Option<(
            crate::dashboard_layout::NodeId,
            crate::dashboard_widgets::Field,
        )>,
    >,
}
fn table_row(row: &Row, _offline: bool) -> TableRow {
    TableRow {
        token: row.index as i32,
        cells: ModelRc::from(row.cells.clone().map(SharedString::from)),
        selected: false,
        disabled: row.id.is_none(),
    }
}
impl CacheRows {
    fn apply(&self, generation: ProjectionGeneration, projection: CacheProjection) {
        self.pending_cohort.set(false);
        self.profile_zone.set(projection.profile_zone);
        let mut old = self.state.borrow_mut();
        let reset = old.generation != Some(generation) || old.total != projection.total;
        let observation_ids = projection
            .home
            .observations
            .iter()
            .filter_map(|item| item.id.clone().map(|id| (item.token, id)))
            .collect::<BTreeMap<_, _>>();
        if old.generation != Some(generation)
            || old.observations != observation_ids
            || old.layout != projection.home.layout
        {
            self.home_serial.set(
                self.home_serial
                    .get()
                    .checked_add(1)
                    .expect("home identity generation exhausted"),
            );
        }
        // A visual position can refer to another entity after removal, sorting
        // or a viewport change. Value updates alone preserve a held gesture.
        let identities_changed = old.rows.len() != projection.rows.len()
            || projection.rows.iter().any(|row| {
                old.rows
                    .get(&row.index)
                    .is_none_or(|(before, _)| before.id != row.id)
            })
            || old.observations != observation_ids
            || old.layout != projection.home.layout;
        if reset || old.revision != projection.revision || identities_changed {
            self.serial.set(
                self.serial
                    .get()
                    .checked_add(1)
                    .expect("UI identity generation exhausted"),
            );
        }
        let mut changed = Vec::new();
        let offline = projection.phase != CachePhase::Live;
        let mut rows = BTreeMap::new();
        for row in projection.rows {
            let index = row.index;
            if old
                .rows
                .get(&index)
                .is_none_or(|(previous, _)| previous != &row)
            {
                changed.push(index);
            }
            let table = table_row(&row, offline);
            rows.insert(index, (row, table));
        }
        changed.extend(
            old.rows
                .keys()
                .filter(|index| !rows.contains_key(index))
                .copied(),
        );
        *old = RowStore {
            presentation: projection.presentation,
            authority: projection.authority,
            generation: Some(generation),
            total: projection.total,
            rows,
            revision: projection.revision,
            observations: observation_ids,
            layout: projection.home.layout,
        };
        drop(old);
        if reset {
            self.notify.reset();
        } else {
            for index in changed {
                self.notify.row_changed(index);
            }
        }
    }
    fn clear(&self) {
        self.pending_cohort.set(false);
        self.home_serial.set(
            self.home_serial
                .get()
                .checked_add(1)
                .expect("home identity generation exhausted"),
        );
        self.serial.set(
            self.serial
                .get()
                .checked_add(1)
                .expect("UI identity generation exhausted"),
        );
        *self.state.borrow_mut() = RowStore::default();
        self.notify.reset();
        self.rooms.set_vec(Vec::new());
        self.observations.set_vec(Vec::new());
        self.devices.set_vec(Vec::new());
        self.details.set_vec(Vec::new());
        self.home_layout_nodes.set_vec(Vec::new());
        self.rooms_layout_nodes.set_vec(Vec::new());
        self.room_layout_nodes.set_vec(Vec::new());
        self.popup_layout_nodes.set_vec(Vec::new());
        self.widget_bindings.borrow_mut().clear();
        *self.focused_widget_item.borrow_mut() = None;
        *self.widget_focus_mapping.borrow_mut() = None;
    }
    fn source_current(&self) -> bool {
        self.state
            .borrow()
            .presentation
            .as_ref()
            .map_or(cfg!(test), |source| source.source().is_current())
    }
    pub(crate) fn resolve(&self, token: i32, generation: i32, revision: i32) -> Option<EntityName> {
        if !self.source_current()
            || self.pending_cohort.get()
            || generation != self.serial.get()
            || self.state.borrow().revision != revision
        {
            return None;
        }
        self.state
            .borrow()
            .rows
            .get(&usize::try_from(token).ok()?)
            .and_then(|(row, _)| row.id.clone())
    }
    pub(crate) fn resolve_room(
        &self,
        token: i32,
        generation: i32,
        layout: Option<&Layout>,
    ) -> Option<dashboard::RoomId> {
        if !self.source_current()
            || self.pending_cohort.get()
            || generation != self.home_serial.get()
            || self.state.borrow().layout.as_ref() != layout
        {
            return None;
        }
        let index = usize::try_from(token.checked_sub(dashboard::ROOM_TOKEN_BASE)?).ok()?;
        self.state
            .borrow()
            .layout
            .as_ref()?
            .rooms
            .get(index)
            .map(|room| room.id.clone())
    }
    pub(crate) fn resolve_observation(
        &self,
        token: i32,
        generation: i32,
        revision: i32,
    ) -> Option<EntityName> {
        if !self.source_current()
            || self.pending_cohort.get()
            || generation != self.home_serial.get()
            || self.state.borrow().revision != revision
        {
            return None;
        }
        self.state.borrow().observations.get(&token).cloned()
    }
}

fn sync<T: Clone + PartialEq + 'static>(model: &slint::VecModel<T>, rows: Vec<T>) {
    if model.row_count() != rows.len() {
        model.set_vec(rows);
    } else {
        for (index, row) in rows.into_iter().enumerate() {
            if model.row_data(index).as_ref() != Some(&row) {
                model.set_row_data(index, row);
            }
        }
    }
}
pub(crate) fn bind(window: &HomewardWindow) {
    let weak = window.as_weak();
    window.on_cohort_expired(move || {
        if let Some(window) = weak.upgrade()
            && window.get_cohort_pending()
        {
            clear_state(&window, false);
        }
    });
    let model = std::rc::Rc::new(CacheRows::default());
    window.set_home_rooms(ModelRc::from(model.rooms.clone()));
    window.set_home_observations(ModelRc::from(model.observations.clone()));
    window.set_device_cards(ModelRc::from(model.devices.clone()));
    window.set_detail_panels(ModelRc::from(model.details.clone()));
    window.set_home_layout_nodes(ModelRc::from(model.home_layout_nodes.clone()));
    window.set_rooms_layout_nodes(ModelRc::from(model.rooms_layout_nodes.clone()));
    window.set_room_layout_nodes(ModelRc::from(model.room_layout_nodes.clone()));
    window.set_popup_layout_nodes(ModelRc::from(model.popup_layout_nodes.clone()));
    window.set_cache_rows(ModelRc::from(model));
    let weak = window.as_weak();
    window.on_dashboard_focus_first(move || {
        let Some(window) = weak.upgrade() else {
            return false;
        };
        for binding in widget_bindings(&window) {
            let Some((_, _, node)) = widget_row(&window, binding.token, binding.generation) else {
                continue;
            };
            if !node.widget.can_open {
                continue;
            }
            if !matches!(
                node.widget.kind,
                crate::ProDashboardWidgetKind::Room | crate::ProDashboardWidgetKind::Observations
            ) && node.widget.action_label.is_empty()
            {
                continue;
            }
            let item = if node.widget.kind == crate::ProDashboardWidgetKind::Observations {
                let Some(item) = node.widget.observations.iter().find(|item| item.available) else {
                    continue;
                };
                Some(item)
            } else {
                None
            };
            window.invoke_native_focus_cancelled();
            let rows = window.get_cache_rows();
            let Some(rows) = rows.as_any().downcast_ref::<CacheRows>() else {
                return false;
            };
            let request = next_widget_focus_request(rows);
            *rows.widget_focus_mapping.borrow_mut() = None;
            window.set_widget_item_focus(
                item.map(|item| crate::FocusRequest {
                    present: true,
                    token: item.token,
                    generation: item.generation,
                    request,
                })
                .unwrap_or_default(),
            );
            window.set_widget_focus(crate::FocusRequest {
                present: true,
                token: binding.token,
                generation: binding.generation,
                request,
            });
            return true;
        }
        false
    });
    let weak = window.as_weak();
    window.on_dashboard_widget_activate(move |token, generation| {
        if let Some(window) = weak.upgrade() {
            activate_widget(&window, token, generation);
        }
    });
    let weak = window.as_weak();
    window.on_dashboard_observation_open(move |token, generation, item, item_generation| {
        let Some(window) = weak.upgrade() else {
            return;
        };
        let Some((_, _, node)) = widget_row(&window, token, generation) else {
            return;
        };
        if item_generation == generation
            && node
                .widget
                .observations
                .iter()
                .any(|row| row.token == item && row.generation == item_generation && row.available)
        {
            activate_widget(&window, token, generation);
        }
    });
    let weak = window.as_weak();
    window.on_dashboard_widget_focus_requested(move || {
        if let Some(window) = weak.upgrade() {
            refresh_widget_focus(&window);
        }
    });
    let weak = window.as_weak();
    window.on_dashboard_widget_focus(move |token, generation, focused| {
        if let Some(window) = weak.upgrade()
            && let Some((_, _, node)) = widget_row(&window, token, generation)
        {
            window.invoke_recipe_focus_observed(node.recipe.token, node.recipe.generation, focused);
        }
    });
    let weak = window.as_weak();
    window.on_dashboard_widget_focus_applied(move |token, generation, request| {
        if let Some(window) = weak.upgrade()
            && let Some((_, _, node)) = widget_row(&window, token, generation)
        {
            let widget_request = window.get_widget_focus();
            let original = window.get_recipe_focus();
            if widget_request.present
                && widget_request.token == token
                && widget_request.generation == generation
                && widget_request.request == request
                && original.present
                && original.token == node.recipe.token
                && original.generation == node.recipe.generation
            {
                window.invoke_recipe_focus_applied(
                    node.recipe.token,
                    node.recipe.generation,
                    original.request,
                );
            }
        }
    });
    let weak = window.as_weak();
    window.on_dashboard_widget_focus_revealed(move |token, generation, top, bottom| {
        if let Some(window) = weak.upgrade()
            && let Some((_, _, node)) = widget_row(&window, token, generation)
        {
            window.invoke_recipe_focus_revealed(
                node.recipe.token,
                node.recipe.generation,
                top,
                bottom,
            );
        }
    });
    let weak = window.as_weak();
    window.on_dashboard_observation_focus(
        move |token, generation, item, item_generation, focused| {
            let Some(window) = weak.upgrade() else {
                return;
            };
            if !focused || item_generation != generation {
                return;
            }
            let rows = window.get_cache_rows();
            let Some(rows) = rows.as_any().downcast_ref::<CacheRows>() else {
                return;
            };
            if let Some(binding) = widget_bindings(&window)
                .into_iter()
                .find(|binding| binding.token == token && binding.generation == generation)
                && let crate::dashboard_widgets::Content::Observations { items } =
                    binding.config.content
                && let Ok(index) = usize::try_from(item)
                && let Some(item) = items.get(index)
            {
                *rows.focused_widget_item.borrow_mut() = Some((binding.node, item.field.clone()));
            }
        },
    );
}
impl Model for CacheRows {
    type Data = TableRow;
    fn row_count(&self) -> usize {
        self.state.borrow().total
    }
    fn row_data(&self, index: usize) -> Option<Self::Data> {
        let state = self.state.borrow();
        if index >= state.total {
            return None;
        }
        Some(
            state
                .rows
                .get(&index)
                .map(|(_, row)| row.clone())
                .unwrap_or_else(|| {
                    table_row(
                        &Row {
                            id: None,
                            index,
                            cells: ["Loading…".into(), String::new(), String::new()],
                        },
                        true,
                    )
                }),
        )
    }
    fn model_tracker(&self) -> &dyn ModelTracker {
        &self.notify
    }
    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}

pub(crate) fn clear(window: &HomewardWindow) {
    clear_state(window, true);
}
pub(crate) fn clear_for_layout(window: &HomewardWindow) {
    clear_state(window, false);
}
fn clear_state(window: &HomewardWindow, forget_dashboard: bool) {
    window.invoke_history_private_cleared();
    window.invoke_media_private_cleared();
    window.set_media_visible(false);
    window.set_media_has_data(false);
    window.set_media_editable(false);
    window.set_media_data(Default::default());
    window.set_media_actions(Default::default());
    window.set_media_options(Default::default());
    window.set_media_values(Default::default());
    window.set_media_members(Default::default());
    window.set_media_group_title("".into());
    window.set_media_coordinator("".into());
    window.set_media_notice("".into());
    window.set_view_loaded(false);
    window.set_cohort_pending(false);
    window.set_commands_ready(false);
    window.set_connection_status(ConnectionStatus::Idle);
    window.set_has_configured_layout(false);
    window.set_action_feedback("".into());
    window.set_dashboard_title("Home".into());
    window.set_dashboard_synthetic(false);
    let model = window.get_cache_rows();
    if let Some(model) = model.as_any().downcast_ref::<CacheRows>() {
        model.profile_zone.set(Default::default());
        model.clear();
        window.set_cache_generation(model.serial.get());
        window.set_home_generation(model.home_serial.get());
    } else {
        window.set_device_cards(Default::default());
        window.set_detail_panels(Default::default());
        window.set_home_layout_nodes(Default::default());
        window.set_rooms_layout_nodes(Default::default());
        window.set_room_layout_nodes(Default::default());
        window.set_popup_layout_nodes(Default::default());
    }
    window.set_detail_title("".into());
    window.set_detail_value("".into());
    window.set_detail_quality("".into());
    window.set_detail_date("".into());
    if forget_dashboard {
        window.invoke_dashboard_catalogue_cleared();
    }
    window.invoke_private_view_cleared(forget_dashboard);
    window.set_cache_active_index(-1);
    window.set_read_status("".into());
    window.set_cache_error("".into());
    window.invoke_recipe_context_changed();
    window.invoke_dashboard_widgets_changed();
    window.set_widget_focus(Default::default());
    window.set_widget_item_focus(Default::default());
    refresh_clock(window);
}

pub(crate) fn refresh_clock(window: &HomewardWindow) {
    let rows = window.get_cache_rows();
    let zone = rows
        .as_any()
        .downcast_ref::<CacheRows>()
        .map(|model| model.profile_zone.get())
        .unwrap_or_default();
    let now = crate::environment_view::now();
    let (clock, date) = zone.clock(now);
    window.set_greeting(zone.greeting(now).into());
    window.set_local_clock(clock.into());
    window.set_local_date(date.into());
    for binding in widget_bindings(window) {
        if let crate::dashboard_widgets::Content::ClockWeather { zone, .. } = binding.config.content
            && let Some((model, index, mut row)) =
                widget_row(window, binding.token, binding.generation)
        {
            let (clock, date) = crate::dashboard_widget_reading::clock(zone, now);
            if row.widget.clock != clock.as_str() || row.widget.date != date.as_str() {
                row.widget.clock = clock.into();
                row.widget.date = date.into();
                model.set_row_data(index, row);
            }
        }
    }
}

fn retains_pixels(
    old: &RowStore,
    generation: ProjectionGeneration,
    projection: &CacheProjection,
    now: (i64, i32),
) -> bool {
    projection
        .pending_cohort
        .as_ref()
        .is_some_and(|layout| old.layout.as_ref() == Some(layout))
        && projection
            .pending_cohort
            .as_ref()
            .and_then(|layout| layout.binding_expires)
            .is_some_and(|expires| expires > now)
        && old.generation == Some(generation)
        && old.revision == projection.revision
        && if projection.phase == CachePhase::Renewing {
            projection
                .presentation
                .as_ref()
                .zip(old.presentation.as_ref())
                .is_some_and(|(new, old)| {
                    new.renewing()
                        && new.incarnation() == old.incarnation()
                        && new.source() == old.source()
                })
        } else {
            projection.authority.is_some()
                && old.authority == projection.authority
                && old.presentation.as_ref().is_none_or(|old| {
                    projection.presentation.as_ref().is_some_and(|new| {
                        old.source().same_presentation(new.source()) && new.source().is_current()
                    })
                })
        }
}

pub(crate) enum RenderOutcome {
    Skipped,
    Applied(Option<EntityName>),
    Retained {
        entity: Option<EntityName>,
        deadline: (i64, i32),
    },
    Cleared,
}

pub(crate) fn render(
    window: &HomewardWindow,
    generation: ProjectionGeneration,
    mut projection: CacheProjection,
) -> RenderOutcome {
    // A queued coherent publication must not reinstall a retired credential's
    // view. A nominal renewal may retain existing pixels, never install these
    // old rows as a newly admitted view. Ruptures also survive watch coalescing.
    if let Some(source) = &projection.presentation
        && !source.source().is_current()
    {
        if source.renewing() {
            projection.phase = CachePhase::Renewing;
            if projection.pending_cohort.is_none() {
                projection.pending_cohort = projection.home.layout.clone();
            }
        } else {
            clear_state(window, false);
            return RenderOutcome::Cleared;
        }
    }
    // This ticket has already crossed the cache owner's publication barrier.
    // Authoritative absence must clear the old private view even when a newer
    // local search has made the presentation revision obsolete.
    if !projection.home.captured && window.get_view_loaded() {
        clear_state(window, false);
        window.set_read_status(cache_status(projection.phase, 0).into());
        window.set_connection_status(connection_status(projection.phase));
        return RenderOutcome::Cleared;
    }
    if projection.revision != window.get_projection_version() {
        return RenderOutcome::Skipped;
    }
    let rows = window.get_cache_rows();
    if let Some(model) = rows.as_any().downcast_ref::<CacheRows>() {
        let now = chrono::Utc::now();
        let retain = retains_pixels(
            &model.state.borrow(),
            generation,
            &projection,
            (now.timestamp(), now.timestamp_subsec_nanos() as i32),
        );
        if retain {
            // DeviceStore already revoked its capability and recipe context
            // before this publication. Disable navigation/readers as well.
            model.pending_cohort.set(true);
            let expiry = projection
                .pending_cohort
                .as_ref()
                .and_then(|layout| layout.binding_expires)
                .expect("retention expiry");
            let remaining = expiry
                .0
                .saturating_mul(1000)
                .saturating_add(i64::from(expiry.1 / 1_000_000))
                .saturating_sub(chrono::Utc::now().timestamp_millis());
            window.set_cohort_retain_ms(i32::try_from(remaining.max(1)).unwrap_or(i32::MAX));
            window.set_cohort_pending(true);
            window.set_commands_ready(false);
            window.invoke_dashboard_widgets_changed();
            return RenderOutcome::Retained {
                entity: projection.history_entity,
                deadline: expiry,
            };
        }
    }
    if projection.phase == CachePhase::Renewing {
        clear_state(window, false);
        return RenderOutcome::Cleared;
    }
    window.set_cohort_pending(false);
    let target = projection
        .history_target(window.get_projection_version())
        .flatten();
    let revision = projection.revision;
    window.set_connection_status(connection_status(projection.phase));
    if projection.phase == CachePhase::Live && projection.home.captured {
        // A coherent admitted recovery supersedes its transient read warning.
        // Device/list command outcomes remain independently owned.
        window.set_cache_error("".into());
    }
    let home = &projection.home;
    window.set_view_loaded(home.captured);
    window.set_dashboard_status(home.status.as_str().into());
    window.set_has_configured_layout(
        home.layout
            .as_ref()
            .is_some_and(|layout| layout.native.is_some()),
    );
    window.set_dashboard_title(
        home.layout
            .as_ref()
            .map_or("Home", |layout| layout.title.as_str())
            .into(),
    );
    window.set_dashboard_synthetic(false);
    let rooms = home
        .layout
        .as_ref()
        .map(|layout| {
            layout
                .rooms
                .iter()
                .enumerate()
                .map(|(index, room)| ProRoomRow {
                    token: index as i32 + dashboard::ROOM_TOKEN_BASE,
                    generation: 0,
                    title: room.label.as_str().into(),
                    subtitle: format!("{} observed items", home.room_counts[index]).into(),
                    glyph: window.global::<KitGlyphs>().invoke_home(),
                    ..Default::default()
                })
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    if let Some([title, value, quality, date]) = &home.detail {
        window.set_detail_title(title.as_str().into());
        window.set_detail_value(value.as_str().into());
        window.set_detail_quality(quality.as_str().into());
        window.set_detail_date(date.as_str().into());
    } else {
        window.set_detail_title("Detail unavailable".into());
        window.set_detail_value("No authorized observation for this item.".into());
        window.set_detail_quality("".into());
        window.set_detail_date("".into());
    }
    let observations = home
        .observations
        .iter()
        .filter(|item| {
            item.id
                .as_ref()
                .is_none_or(|id| !projection.devices.represented.contains(id))
        })
        .map(|item| ProObservationRow {
            token: item.token,
            generation: 0,
            title: item.label.as_str().into(),
            value: item.value.as_str().into(),
            description: item.description.as_str().into(),
            glyph: match item.icon {
                Icon::Home => window.global::<KitGlyphs>().invoke_home(),
                Icon::Information => window.global::<KitGlyphs>().invoke_information(),
                Icon::Clock => window.global::<KitGlyphs>().invoke_clock(),
                Icon::Grid => window.global::<KitGlyphs>().invoke_grid(),
            },
            available: item.id.is_some(),
            active: false,
        })
        .collect::<Vec<_>>();
    let total = projection.total;
    if total == 0 {
        window.set_cache_active_index(-1);
    }
    window.set_read_status(cache_status(projection.phase, projection.authorized_total).into());
    let model = window.get_cache_rows();
    if let Some(model) = model.as_any().downcast_ref::<CacheRows>() {
        window.set_commands_ready(projection.devices.ready);
        let cards = std::mem::take(&mut projection.devices.rows);
        sync(
            &model.devices,
            cards
                .iter()
                .enumerate()
                .map(|(index, row)| {
                    row.ui(
                        model.devices.row_data(index).as_ref(),
                        &window.global::<KitGlyphs>().invoke_information(),
                    )
                })
                .collect(),
        );
        sync(
            &model.details,
            cards
                .iter()
                .enumerate()
                .map(|(index, row)| {
                    let device = model.devices.row_data(index).expect("published device");
                    crate::device_view::detail::project(
                        row,
                        device,
                        model.details.row_data(index).as_ref(),
                    )
                })
                .collect(),
        );
        let board = std::mem::take(&mut projection.board);
        let before_generation = model.serial.get();
        model.apply(generation, projection);
        if !matches!(
            board.target(),
            crate::dashboard_presentation::BoardTarget::Retain
        ) {
            let incoming = board.bindings(0);
            let previous = model.widget_bindings.borrow();
            let same = incoming.len() == previous.len()
                && incoming
                    .iter()
                    .zip(previous.iter())
                    .all(|(a, b)| a.token == b.token && a.node == b.node && a.config == b.config);
            // Visibility can remove a node without changing the source document.
            // A shifted row must not inherit another widget's pending gesture or
            // resource-reader generation merely because its index is reused.
            if !same && model.serial.get() == before_generation {
                model.serial.set(
                    before_generation
                        .checked_add(1)
                        .expect("widget identity generation exhausted"),
                );
            }
        }
        window.set_cache_generation(model.serial.get());
        window.set_home_generation(model.home_serial.get());
        let widget_generation = model.serial.get();
        let rendered = |previous: &slint::VecModel<crate::ProLayoutNode>| {
            board.rows_with_generation(
                previous,
                &window.global::<KitGlyphs>().invoke_information(),
                widget_generation,
            )
        };
        match board.target() {
            crate::dashboard_presentation::BoardTarget::Clear => {
                sync(&model.home_layout_nodes, Vec::new());
                sync(&model.rooms_layout_nodes, Vec::new());
                sync(&model.room_layout_nodes, Vec::new());
                sync(&model.popup_layout_nodes, Vec::new());
                model.widget_bindings.borrow_mut().clear();
            }
            crate::dashboard_presentation::BoardTarget::Retain => {}
            crate::dashboard_presentation::BoardTarget::Home => {
                let home = rendered(&model.home_layout_nodes);
                sync(
                    &model.rooms_layout_nodes,
                    board.room_directory_rows(home.clone()),
                );
                sync(&model.home_layout_nodes, home);
                *model.widget_bindings.borrow_mut() = board.bindings(widget_generation);
            }
            crate::dashboard_presentation::BoardTarget::Room => {
                sync(&model.room_layout_nodes, rendered(&model.room_layout_nodes));
                *model.widget_bindings.borrow_mut() = board.bindings(widget_generation);
            }
            crate::dashboard_presentation::BoardTarget::Popup => {
                sync(
                    &model.popup_layout_nodes,
                    rendered(&model.popup_layout_nodes),
                );
                *model.widget_bindings.borrow_mut() = board.bindings(widget_generation);
            }
        }
        let observations = observations
            .into_iter()
            .map(|mut row| {
                row.generation = model.home_serial.get();
                row
            })
            .collect::<Vec<_>>();
        let rooms = rooms
            .into_iter()
            .map(|mut row| {
                row.generation = model.home_serial.get();
                row
            })
            .collect();
        sync(&model.rooms, rooms);
        sync(&model.observations, observations);
    }
    window.invoke_dashboard_widgets_changed();
    refresh_widget_focus(window);
    window.invoke_recipe_context_changed();
    window.invoke_native_layout_published(revision);
    refresh_clock(window);
    RenderOutcome::Applied(target)
}

pub(crate) fn clear_native_target(
    window: &HomewardWindow,
    presentation: home_client_model::dashboard::Presentation,
) {
    let model = window.get_cache_rows();
    if let Some(model) = model.as_any().downcast_ref::<CacheRows>() {
        match presentation {
            home_client_model::dashboard::Presentation::Page => {
                model.room_layout_nodes.set_vec(Vec::new())
            }
            home_client_model::dashboard::Presentation::Popup => {
                model.popup_layout_nodes.set_vec(Vec::new())
            }
        }
    }
}

/// Only the active, coherent configured board can own resource readers.
pub(crate) fn widget_bindings(window: &HomewardWindow) -> Vec<crate::dashboard_widgets::Binding> {
    if window.get_cohort_pending()
        || !window.get_view_loaded()
        || window.get_access_stage() != crate::ProAccessStage::Connected
        || window.get_show_endpoint()
    {
        return Vec::new();
    }
    let base = if window.get_native_popup().open {
        300_000
    } else {
        match window.get_page() {
            crate::ViewPage::Home | crate::ViewPage::Rooms => 100_000,
            crate::ViewPage::Room if window.get_room_view() == crate::RoomView::Cards => 200_000,
            crate::ViewPage::Room => return Vec::new(),
            crate::ViewPage::Inventory
            | crate::ViewPage::Detail
            | crate::ViewPage::Lists
            | crate::ViewPage::Calendars
            | crate::ViewPage::Cameras
            | crate::ViewPage::Settings => return Vec::new(),
        }
    };
    let rows = window.get_cache_rows();
    let Some(rows) = rows.as_any().downcast_ref::<CacheRows>() else {
        return Vec::new();
    };
    if !rows.source_current() {
        return Vec::new();
    }
    rows.widget_bindings
        .borrow()
        .iter()
        .filter(|binding| {
            (window.get_page() != crate::ViewPage::Rooms
                || window.get_native_popup().open
                || matches!(
                    binding.config.content,
                    crate::dashboard_widgets::Content::Room { .. }
                ))
                && binding.token >= base
                && binding.token < base + 100_000
                && binding.generation == rows.serial.get()
        })
        .cloned()
        .collect()
}
fn widget_row(
    window: &HomewardWindow,
    token: i32,
    generation: i32,
) -> Option<(
    std::rc::Rc<slint::VecModel<crate::ProLayoutNode>>,
    usize,
    crate::ProLayoutNode,
)> {
    if !widget_bindings(window)
        .iter()
        .any(|binding| binding.token == token && binding.generation == generation)
    {
        return None;
    }
    let rows = window.get_cache_rows();
    let rows = rows.as_any().downcast_ref::<CacheRows>()?;
    let (model, index) = match token {
        100_000..200_000 => (rows.home_layout_nodes.clone(), token - 100_000),
        200_000..300_000 => (rows.room_layout_nodes.clone(), token - 200_000),
        300_000..400_000 => (rows.popup_layout_nodes.clone(), token - 300_000),
        _ => return None,
    };
    let index = usize::try_from(index).ok()?;
    let row = model.row_data(index)?;
    if row.token != token
        || row.generation != generation
        || row.kind != crate::ProLayoutKind::Widget
    {
        return None;
    }
    Some((model, index, row))
}
fn activate_widget(window: &HomewardWindow, token: i32, generation: i32) {
    if window.get_navigation_drawer().open || window.get_recipe_confirmation().open {
        return;
    }
    let popup = window.get_native_popup();
    if popup.open && (!popup.topmost || popup.busy) {
        return;
    }
    let Some((_, _, node)) = widget_row(window, token, generation) else {
        return;
    };
    if !node.widget.can_open {
        return;
    }
    window.invoke_recipe_gesture(
        node.recipe.token,
        node.recipe.generation,
        crate::ProGesture::Tap,
    );
}
fn next_widget_focus_request(rows: &CacheRows) -> i32 {
    let request = rows
        .widget_focus_serial
        .get()
        .checked_add(1)
        .expect("widget focus request exhausted");
    rows.widget_focus_serial.set(request);
    request
}
fn refresh_widget_focus(window: &HomewardWindow) {
    let requested = window.get_recipe_focus();
    let rows = window.get_cache_rows();
    let Some(rows) = rows.as_any().downcast_ref::<CacheRows>() else {
        return;
    };
    let mut focus = crate::FocusRequest::default();
    let mut item_focus = crate::FocusRequest::default();
    if requested.present {
        for binding in widget_bindings(window) {
            let Some((_, _, node)) = widget_row(window, binding.token, binding.generation) else {
                continue;
            };
            if node.recipe.token != requested.token
                || node.recipe.generation != requested.generation
                || !node.widget.can_open
            {
                continue;
            }
            let mapped = rows
                .widget_focus_mapping
                .borrow()
                .as_ref()
                .filter(|(original, _)| original == &requested)
                .map(|(_, serial)| *serial);
            let serial = mapped.unwrap_or_else(|| next_widget_focus_request(rows));
            *rows.widget_focus_mapping.borrow_mut() = Some((requested.clone(), serial));
            focus = crate::FocusRequest {
                present: true,
                token: binding.token,
                generation: binding.generation,
                request: serial,
            };
            if let crate::dashboard_widgets::Content::Observations { items } =
                &binding.config.content
            {
                let saved = rows.focused_widget_item.borrow();
                let selected = saved
                    .as_ref()
                    .filter(|(id, _)| id == &binding.node)
                    .and_then(|(_, field)| items.iter().position(|item| &item.field == field));
                let selected = selected
                    .filter(|index| {
                        node.widget
                            .observations
                            .row_data(*index)
                            .is_some_and(|row| row.available)
                    })
                    .or_else(|| {
                        node.widget
                            .observations
                            .iter()
                            .position(|item| item.available)
                    });
                if let Some(index) = selected
                    .and_then(|index| node.widget.observations.row_data(index))
                    .filter(|item| item.available)
                {
                    item_focus = crate::FocusRequest {
                        token: index.token,
                        generation: index.generation,
                        present: true,
                        request: serial,
                    };
                }
            }
            break;
        }
    }
    if !focus.present {
        *rows.widget_focus_mapping.borrow_mut() = None;
    }
    window.set_widget_item_focus(item_focus);
    window.set_widget_focus(focus);
}
pub(crate) fn publish_widget_tasks(
    window: &HomewardWindow,
    token: i32,
    generation: i32,
    tasks: ModelRc<crate::ProTaskRow>,
    access: crate::ProTaskAccess,
    ready: bool,
    status: String,
) {
    let Some((model, index, mut row)) = widget_row(window, token, generation) else {
        return;
    };
    if row.widget.kind != crate::ProDashboardWidgetKind::Tasks {
        return;
    }
    row.widget.tasks = tasks;
    row.widget.task_access = access;
    row.widget.data_ready = ready;
    row.widget.status = status.into();
    model.set_row_data(index, row);
}
pub(crate) fn publish_widget_agenda(
    window: &HomewardWindow,
    token: i32,
    generation: i32,
    agenda: ModelRc<crate::ProAgendaRow>,
    ready: bool,
    status: String,
) {
    let Some((model, index, mut row)) = widget_row(window, token, generation) else {
        return;
    };
    if row.widget.kind != crate::ProDashboardWidgetKind::Agenda {
        return;
    }
    row.widget.agenda = agenda;
    row.widget.data_ready = ready;
    row.widget.status = status.into();
    model.set_row_data(index, row);
}

fn connection_status(phase: CachePhase) -> ConnectionStatus {
    match phase {
        CachePhase::Idle | CachePhase::Suspended | CachePhase::Closed => ConnectionStatus::Idle,
        CachePhase::Loading | CachePhase::Renewing | CachePhase::Resyncing => {
            ConnectionStatus::Connecting
        }
        CachePhase::Live => ConnectionStatus::Live,
        CachePhase::Offline => ConnectionStatus::Offline,
        CachePhase::Blocked | CachePhase::Invalid => ConnectionStatus::Blocked,
    }
}
fn cache_status(phase: CachePhase, total: usize) -> String {
    let label = match phase {
        CachePhase::Idle => "",
        CachePhase::Loading | CachePhase::Renewing => "Loading states…",
        CachePhase::Live => {
            if total == 0 {
                "No authorized items in this view."
            } else {
                "States up to date"
            }
        }
        CachePhase::Offline => "Offline · latest received data",
        CachePhase::Resyncing => "Refreshing the authorized view…",
        CachePhase::Suspended => "Reading suspended",
        CachePhase::Blocked => "Reading unavailable · retry or sign in again",
        CachePhase::Invalid => "The server response cannot produce a coherent view.",
        CachePhase::Closed => "",
    };
    if total > 0 {
        format!(
            "{label} · {total} {}",
            if total == 1 { "entity" } else { "entities" }
        )
    } else {
        label.into()
    }
}

pub(crate) fn event_message(event: CacheEvent) -> &'static str {
    match event {
        CacheEvent::Transport(_) => "Previous incident: network reading was interrupted.",
        CacheEvent::Session(_) => {
            "Previous incident: the session could no longer continue reading."
        }
        CacheEvent::Contract(_) => {
            "Previous incident: a response did not meet the reading contract."
        }
        CacheEvent::Resync(_) => "A full view refresh was requested.",
        CacheEvent::StreamEnded => "Previous incident: the reading stream was closed.",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use buffa::EnumValue;
    use buffa::Message;
    use home_client_cache::ProjectionMailbox;

    use crate::device_view::fixture as fixtures;
    fn view(count: usize) -> CachedView {
        view_with(count, |index| {
            fixtures::entry(
                &format!("homes/synthetic/entities/private-entity-{index:04}"),
                &format!("Élément {index}"),
                Vec::new(),
            )
        })
    }
    fn view_with(count: usize, entity: impl Fn(usize) -> wire::EntityViewEntry) -> CachedView {
        fixtures::capture((0..count).map(entity).collect())
    }

    #[test]
    fn the_virtual_model_keeps_64_rows_and_no_private_identity_in_its_cells() {
        let view = view(850);
        let projection = project(
            CachePhase::Live,
            Some(&view),
            Viewport::new(128, 64).unwrap(),
        );
        assert_eq!(projection.total, 850);
        assert_eq!(projection.rows.len(), 64);
        assert_eq!(projection.rows[0].index, 128);
        assert_eq!(projection.rows[0].cells[0], "Élément 128");
        assert!(
            projection
                .rows
                .iter()
                .flat_map(|row| &row.cells)
                .all(|cell| !cell.contains("private-"))
        );
        let model = CacheRows::default();
        let generation = ProjectionMailbox::<()>::new().generation();
        model.apply(generation, projection);
        assert_eq!(model.row_count(), 850);
        assert_eq!(model.state.borrow().rows.len(), 64);
        let loaded = model.row_data(128).unwrap();
        assert_eq!(loaded.cells.row_data(0).unwrap().as_str(), "Élément 128");
        assert!(!loaded.disabled);
        let unloaded = model.row_data(500).unwrap();
        assert_eq!(unloaded.cells.row_data(0).unwrap().as_str(), "Loading…");
        assert!(unloaded.disabled);
        assert!(model.row_data(850).is_none());
        model.apply(
            generation,
            project(
                CachePhase::Offline,
                Some(&view),
                Viewport::new(700, 32).unwrap(),
            ),
        );
        assert_eq!(model.state.borrow().rows.len(), 32);
        assert!(
            !model.row_data(700).unwrap().disabled,
            "offline navigation is read-only and remains available while the cache retains the view"
        );
        model.clear();
        assert_eq!(model.row_count(), 0);
        assert!(model.state.borrow().rows.is_empty());
        assert!(model.row_data(0).is_none());
    }

    fn home_view() -> CachedView {
        let data: serde_json::Value =
            serde_json::from_str(include_str!("../tests/fixtures/overview.json")).unwrap();
        let entities = data["entities"].as_array().unwrap();
        view_with(entities.len(), |index| {
            let name = format!(
                "homes/synthetic/entities/{}",
                entities[index]["id"].as_str().unwrap()
            );
            let mut capability = fixtures::power(
                &format!("{name}/capabilities/power"),
                Some(wire::PowerValue::Off),
            );
            let Some(wire::capability::Trait::Power(value)) = capability.r#trait.as_mut() else {
                panic!("power fixture");
            };
            value.observation.as_option_mut().unwrap().observed_time =
                buffa_types::google::protobuf::Timestamp {
                    seconds: 10,
                    ..Default::default()
                }
                .into();
            fixtures::entry(
                &name,
                entities[index]["alias"].as_str().unwrap(),
                vec![capability],
            )
        })
    }

    fn widget_node(content: crate::dashboard_widgets::Content) -> crate::dashboard_layout::Node {
        let mut native = crate::dashboard_layout::NativeLayout::try_from(
            &crate::dashboard_layout::tests::extended_forest(),
        )
        .unwrap();
        let mut node = native
            .nodes
            .remove(&crate::dashboard_layout::NodeId::parse("tile").unwrap())
            .unwrap();
        node.kind = crate::dashboard_layout::Kind::Widget;
        node.title = "Titre fourni par le serveur".into();
        node.entity = None;
        node.features.clear();
        node.recipe_kind = None;
        node.recipes = None;
        node.widget = Some(crate::dashboard_widgets::Widget {
            content,
            action: None,
            action_label: String::new(),
            empty_label: String::new(),
        });
        node
    }
    #[test]
    fn configured_rooms_directory_keeps_exact_widget_owner_and_rejects_stale_activation() {
        use crate::dashboard_layout::{Kind, NativeLayout, NodeId};
        use crate::dashboard_widgets::Content;
        use home_client_model::dashboard::{Destination, Gesture, Presentation};
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        bind(&window);
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_show_endpoint(false);
        window.set_page(crate::ViewPage::Rooms);
        let destination = Destination {
            dashboard: home_client_model::api::DashboardName::try_from(
                "homes/synthetic/dashboards/exact-dashboard",
            )
            .unwrap(),
            node: NodeId::parse("exact-configured-popup").unwrap(),
            presentation: Presentation::Popup,
        };
        let mut room = widget_node(Content::Room {
            room: dashboard::RoomId::parse("exact-room-id").unwrap(),
            temperature: None,
            activity: Vec::new(),
        });
        room.id = NodeId::parse("configured-room-widget").unwrap();
        room.widget.as_mut().unwrap().action = Some(destination.clone());
        let mut root = room.clone();
        root.id = NodeId::parse("configured-home").unwrap();
        root.kind = Kind::View;
        root.widget = None;
        root.children = vec![room.id.clone()];
        let mut layout = dashboard::fixture();
        layout.rooms.clear();
        layout.native = Some(NativeLayout {
            views: vec![root.id.clone()],
            nodes: [root.clone(), room.clone()]
                .into_iter()
                .map(|node| (node.id.clone(), node))
                .collect(),
        });
        let request = ProjectionRequest {
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(layout.clone())),
            ..Default::default()
        };
        let devices = crate::device_view::DeviceProjection {
            native_nodes: vec![root, room.clone()],
            recipes: [(
                room.id.clone(),
                crate::device_view::RecipePresentation {
                    token: 71,
                    generation: 83,
                    gestures: [(Gesture::Tap, crate::device_view::GestureAccess::Local)].into(),
                    unavailable: false,
                },
            )]
            .into(),
            ..Default::default()
        };
        let observed = home_view();
        let mut projection = project_request(
            CachePhase::Live,
            Some(&observed),
            Viewport::default(),
            &request,
        );
        projection.board = crate::dashboard_presentation::Board::project(
            Some(&layout),
            &request,
            &devices,
            Some(&observed),
        );
        render(
            &window,
            ProjectionMailbox::<()>::new().generation(),
            projection,
        );
        assert_eq!(
            window.get_home_rooms().row_count(),
            0,
            "configured directory must work without the legacy overview catalogue"
        );
        let directory = window.get_rooms_layout_nodes();
        let input = directory.row_data(0).unwrap().children.row_data(0).unwrap();
        let leaf = directory.row_data(input.index as usize).unwrap();
        let original = window
            .get_home_layout_nodes()
            .row_data(input.index as usize)
            .unwrap();
        assert_eq!(leaf.token, original.token);
        assert_eq!(leaf.generation, original.generation);
        assert_eq!(
            widget_bindings(&window)[0].config.destination(),
            Some(&destination)
        );
        let actions = std::rc::Rc::new(RefCell::new(Vec::new()));
        let captured = actions.clone();
        window.on_recipe_gesture(move |token, generation, gesture| {
            captured.borrow_mut().push((token, generation, gesture))
        });
        window.invoke_dashboard_widget_activate(leaf.token, leaf.generation);
        assert_eq!(
            actions.borrow().as_slice(),
            &[(71, 83, crate::ProGesture::Tap)]
        );
        clear_state(&window, false);
        window.invoke_dashboard_widget_activate(leaf.token, leaf.generation);
        assert_eq!(
            actions.borrow().len(),
            1,
            "cleared/replaced directory must reject the previously held owner"
        );
        assert_eq!(window.get_rooms_layout_nodes().row_count(), 0);
    }
    #[test]
    fn configured_room_temperature_never_falls_back_to_another_observed_entity() {
        use crate::dashboard_widgets::{Content, Field, FieldKey};
        let climate = |index: usize| {
            let name = format!("homes/synthetic/entities/temperature-{index}");
            fixtures::entry(
                &name,
                &format!("Thermostat {index}"),
                vec![fixtures::capability(
                    &format!("{name}/capabilities/temperature"),
                    wire::capability::Trait::NumericMeasurement(Box::new(
                        wire::NumericMeasurementCapability {
                            unit: "Cel".into(),
                            role: wire::MeasurementRole::Temperature.into(),
                            observation: wire::NumericObservation {
                                value: Some(if index == 0 { 99. } else { 0. }),
                                quality: fixtures::quality().into(),
                                ..Default::default()
                            }
                            .into(),
                            ..Default::default()
                        },
                    )),
                )],
            )
        };
        let layout = dashboard::fixture();
        let node = widget_node(Content::Room {
            room: layout.rooms[0].id.clone(),
            temperature: Some(Field {
                entity: EntityName::try_from("homes/synthetic/entities/temperature-1").unwrap(),
                capability: home_client_model::api::CapabilityName::try_from(
                    "homes/synthetic/entities/temperature-1/capabilities/temperature",
                )
                .unwrap(),
                key: FieldKey::CurrentTemperature,
            }),
            activity: Vec::new(),
        });
        let full = view_with(2, climate);
        let reading =
            crate::dashboard_widget_reading::Reading::project(&node, &layout, Some(&full));
        assert_eq!(
            reading.value, "0 °C",
            "zero is an observed value, and the configured second entity wins over the first"
        );
        let restricted = view_with(1, climate);
        let absent =
            crate::dashboard_widget_reading::Reading::project(&node, &layout, Some(&restricted));
        assert_eq!(
            absent.value, "—",
            "missing configured source must not select the remaining thermostat"
        );
        let stale = view_with(2, |index| {
            let mut entity = climate(index);
            if index == 1 {
                let Some(wire::capability::Trait::NumericMeasurement(value)) =
                    entity.capabilities[0].r#trait.as_mut()
                else {
                    panic!()
                };
                value
                    .observation
                    .as_option_mut()
                    .unwrap()
                    .quality
                    .as_option_mut()
                    .unwrap()
                    .freshness = wire::Freshness::Stale.into();
            }
            entity
        });
        let unavailable =
            crate::dashboard_widget_reading::Reading::project(&node, &layout, Some(&stale));
        assert_eq!(
            unavailable.value, "—",
            "a retained stale source is not a current reading and does not select the fresh alternative"
        );
    }
    #[test]
    fn security_summaries_follow_exact_entity_order_without_capability_rows() {
        use crate::dashboard_widgets::Content;
        let layout = dashboard::fixture();
        let node = widget_node(Content::Security {
            capabilities: [
                "homes/synthetic/entities/security-1/capabilities/power",
                "homes/synthetic/entities/security-0/capabilities/power",
            ]
            .map(|name| home_client_model::api::CapabilityName::try_from(name).unwrap())
            .to_vec(),
        });
        let source = |index: usize, stale: bool| {
            let name = format!("homes/synthetic/entities/security-{index}");
            let mut capability = fixtures::power(
                &format!("{name}/capabilities/power"),
                Some(if index == 1 {
                    wire::PowerValue::On
                } else {
                    wire::PowerValue::Off
                }),
            );
            if stale {
                let Some(wire::capability::Trait::Power(value)) = capability.r#trait.as_mut()
                else {
                    panic!()
                };
                value
                    .observation
                    .as_option_mut()
                    .unwrap()
                    .quality
                    .as_option_mut()
                    .unwrap()
                    .freshness = wire::Freshness::Stale.into();
            }
            fixtures::entry(&name, &format!("Source {index}"), vec![capability])
        };
        let full = view_with(2, |index| source(index, false));
        let reading =
            crate::dashboard_widget_reading::Reading::project(&node, &layout, Some(&full));
        assert_eq!(reading.security.len(), 2);
        assert_eq!(reading.security[0].title, "Source 1");
        assert_eq!(reading.security[0].value, "On");
        assert_eq!(reading.security[1].title, "Source 0");
        assert_eq!(reading.security[1].value, "Off");
        let field = crate::dashboard_widgets::Field {
            entity: EntityName::try_from("homes/synthetic/entities/security-1").unwrap(),
            capability: home_client_model::api::CapabilityName::try_from(
                "homes/synthetic/entities/security-1/capabilities/power",
            )
            .unwrap(),
            key: crate::dashboard_widgets::FieldKey::State,
        };
        assert_eq!(field.reading(Some(&full)).as_deref(), Some("On"));
        assert_eq!(
            crate::dashboard_visibility::literal(
                &full,
                &home_client_model::dashboard::FieldBinding {
                    capability: field.capability.clone(),
                    field: home_client_model::dashboard::FieldKey::State
                }
            ),
            Some(home_client_model::dashboard::Literal::Power(
                home_client_model::api::Power::On
            )),
            "the configured state field retains its exact literal"
        );
        let light = view_with(1, |index| {
            let mut entity = source(index, false);
            entity.capabilities[0] = fixtures::capability(
                "homes/synthetic/entities/security-0/capabilities/light",
                wire::capability::Trait::LightControl(Box::new(wire::LightControlCapability {
                    observation: wire::LightControlObservation {
                        on: Some(true),
                        brightness_percent: Some(42.),
                        quality: fixtures::quality().into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                })),
            );

            entity
        });
        let field = crate::dashboard_widgets::Field {
            entity: EntityName::try_from("homes/synthetic/entities/security-0").unwrap(),
            capability: home_client_model::api::CapabilityName::try_from(
                "homes/synthetic/entities/security-0/capabilities/power",
            )
            .unwrap(),
            key: crate::dashboard_widgets::FieldKey::State,
        };
        assert_eq!(
            field.reading(Some(&light)).as_deref(),
            None,
            "an exact power capability binding must not fall back to a different light trait"
        );
        assert!(
            reading
                .security
                .iter()
                .all(|row| !row.can_power && row.values.is_empty())
        );
        let stale = view_with(2, |index| source(index, index == 1));
        let reading =
            crate::dashboard_widget_reading::Reading::project(&node, &layout, Some(&stale));
        assert_eq!(reading.security[0].title, "Source 1");
        assert_eq!(
            reading.security[0].value, "—",
            "a stale source must not borrow another entity's value"
        );
        assert_eq!(reading.security[1].value, "Off");
    }
    #[test]
    fn widget_thermostat_uses_the_canonical_field_and_preserves_observed_values_during_drafts() {
        use crate::dashboard_widgets::{Content, Field, FieldKey};
        let entity = EntityName::try_from("homes/synthetic/entities/climate-configured").unwrap();
        let node = widget_node(Content::Thermostat {
            detailed: false,
            summary_only: false,
            entity: entity.clone(),
            current: Field {
                entity: entity.clone(),
                capability: home_client_model::api::CapabilityName::try_from(
                    "homes/synthetic/entities/climate-configured/capabilities/temperature",
                )
                .unwrap(),
                key: FieldKey::CurrentTemperature,
            },
            target: Field {
                entity,
                capability: home_client_model::api::CapabilityName::try_from(
                    "homes/synthetic/entities/climate-configured/capabilities/setpoint",
                )
                .unwrap(),
                key: FieldKey::Setpoint,
            },
        });
        let reading = crate::dashboard_widget_reading::Reading {
            value: "0".into(),
            target: Some("21".into()),
            ..Default::default()
        };
        let mut row = crate::device_view::presentation::CardRow {
            kind: crate::ProDeviceKind::Climate,
            field_keys: [("setpoint_celsius", 7)].into(),
            values: vec![
                crate::ProDeviceValue {
                    token: 3,
                    kind: crate::ProValueKind::Numeric,
                    lower: "99".into(),
                    ..Default::default()
                },
                crate::ProDeviceValue {
                    token: 7,
                    kind: crate::ProValueKind::Numeric,
                    mode: crate::ProValueMode::Single,
                    lower: "21".into(),
                    can_step: true,
                    ..Default::default()
                },
            ],
            ..Default::default()
        };
        let ui = |row: &crate::device_view::presentation::CardRow| {
            reading.ui(
                &node,
                Some(row),
                None,
                (100_001, 4),
                &Default::default(),
                false,
            )
        };
        assert_eq!(
            ui(&row).field.token,
            7,
            "the first numeric field is not a selector"
        );
        assert!(ui(&row).field.can_step);
        row.values[1].lower = "22".into();
        let draft = ui(&row);
        assert_eq!(draft.thermostat.value, "0");
        assert_eq!(draft.field.lower, "21");
        assert!(!draft.field.can_step);
        row.values[1].lower = "21".into();
        row.values[1].error = "Brouillon refusé".into();
        assert!(!ui(&row).field.can_step);
        row.field_keys.clear();
        assert!(
            !ui(&row).field.can_step,
            "no other field substitutes for a missing canonical binding"
        );
    }
    #[test]
    fn an_empty_server_layout_is_valid_and_does_not_create_dashboard_widgets() {
        let mut wire = dashboard::fixture_wire();
        wire.layout = wire::DashboardLayout::default().into();
        let layout = Layout::try_from(&wire).unwrap();
        let native = layout.native.as_ref().unwrap();
        assert!(native.nodes.is_empty());
        let board = crate::dashboard_presentation::Board::project(
            Some(&layout),
            &ProjectionRequest::default(),
            &Default::default(),
            None,
        );
        assert!(board.bindings(9).is_empty());
        assert!(
            board
                .rows_with_generation(&Default::default(), &Default::default(), 9)
                .is_empty()
        );
    }
    #[test]
    fn conditional_nodes_cannot_reassign_a_held_widget_identity_when_source_indices_shift() {
        use crate::dashboard_layout::{Kind, NativeLayout, NodeId};
        use crate::dashboard_widgets::Content;
        use home_client_model::dashboard::{Comparison, Predicate, Recipes};
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        bind(&window);
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_show_endpoint(false);
        let make_clock = || {
            widget_node(Content::ClockWeather {
                zone: "UTC".parse().unwrap(),
                weather: None,
            })
        };
        let mut conditional = make_clock();
        conditional.id = NodeId::parse("conditional").unwrap();
        conditional.recipes = Some(Recipes {
            interactions: BTreeMap::new(),
            visibility: Some(Predicate::State {
                field: home_client_model::dashboard::FieldBinding {
                    capability: home_client_model::api::CapabilityName::try_from(
                        "homes/synthetic/entities/condition/capabilities/power",
                    )
                    .unwrap(),
                    field: home_client_model::dashboard::FieldKey::State,
                },
                expected: home_client_model::dashboard::Literal::Power(
                    home_client_model::api::Power::On,
                ),
                comparison: Comparison::Equal,
            }),
        });
        let mut remaining = make_clock();
        remaining.id = NodeId::parse("remaining").unwrap();
        let mut section = make_clock();
        section.id = NodeId::parse("section").unwrap();
        section.kind = Kind::Section;
        section.widget = None;
        section.children = vec![conditional.id.clone(), remaining.id.clone()];
        let mut root = section.clone();
        root.id = NodeId::parse("view").unwrap();
        root.kind = Kind::View;
        root.children = vec![section.id.clone()];
        let entry = |on| {
            fixtures::entry(
                "homes/synthetic/entities/condition",
                "Condition du serveur",
                vec![fixtures::power(
                    "homes/synthetic/entities/condition/capabilities/power",
                    Some(if on {
                        wire::PowerValue::On
                    } else {
                        wire::PowerValue::Off
                    }),
                )],
            )
        };
        let live = RefCell::new(home_client_cache::LiveView::new(fixtures::capture(vec![
            entry(false),
        ])));
        let mut layout = dashboard::joined_fixture(live.borrow().current().unwrap());
        layout.native = Some(NativeLayout {
            views: vec![root.id.clone()],
            nodes: [root, section, conditional, remaining]
                .into_iter()
                .map(|node| (node.id.clone(), node))
                .collect(),
        });
        let request = ProjectionRequest {
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(layout)),
            ..Default::default()
        };
        let observed = |on: bool, revision: u64| {
            let mut live = live.borrow_mut();
            live.apply(wire::WatchEntityViewResponse {
                resume_token: format!("checkpoint-{revision}"),
                change: Some(wire::watch_entity_view_response::Change::Upsert(Box::new(
                    entry(on),
                ))),
                ..Default::default()
            })
            .unwrap();
            live.current().unwrap().clone()
        };
        let generation = ProjectionMailbox::<()>::new().generation();
        render(
            &window,
            generation,
            project_request(
                CachePhase::Live,
                Some(&observed(true, 1)),
                Viewport::default(),
                &request,
            ),
        );
        let before = widget_bindings(&window);
        assert_eq!(before.len(), 2);
        let held = before
            .iter()
            .find(|binding| binding.node.as_str() == "remaining")
            .unwrap()
            .clone();
        render(
            &window,
            generation,
            project_request(
                CachePhase::Live,
                Some(&observed(false, 2)),
                Viewport::default(),
                &request,
            ),
        );
        let after = widget_bindings(&window);
        assert_eq!(after.len(), 1);
        assert_eq!(after[0].node.as_str(), "remaining");
        assert_ne!(
            after[0].generation, held.generation,
            "a visibility change that reuses indices must invalidate old intents and reader publications"
        );
        assert!(widget_row(&window, held.token, held.generation).is_none());
        let stable = after[0].generation;
        render(
            &window,
            generation,
            project_request(
                CachePhase::Live,
                Some(&observed(false, 3)),
                Viewport::default(),
                &request,
            ),
        );
        assert_eq!(
            widget_bindings(&window)[0].generation,
            stable,
            "an observation-only update must preserve the configured widget owner"
        );
    }
    #[test]
    fn resetting_a_focus_property_or_private_models_never_reuses_a_widget_request_serial() {
        let rows = CacheRows::default();
        let first = next_widget_focus_request(&rows);
        *rows.widget_focus_mapping.borrow_mut() = Some((
            crate::FocusRequest {
                present: true,
                request: 1,
                token: 9,
                generation: 2,
            },
            first,
        ));
        rows.clear();
        assert!(rows.widget_focus_mapping.borrow().is_none());
        assert!(
            next_widget_focus_request(&rows) > first,
            "an ack/reset must not make a later restoration look already applied"
        );
    }
    #[test]
    fn only_a_current_live_view_clears_a_transient_read_warning() {
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        bind(&window);
        let view = home_view();
        let request = ProjectionRequest::default();
        let generation = ProjectionMailbox::<()>::new().generation();
        window.set_cache_error("Read interrupted".into());
        window.set_action_feedback("Could not confirm this command".into());
        render(
            &window,
            generation,
            project_request(
                CachePhase::Offline,
                Some(&view),
                Viewport::default(),
                &request,
            ),
        );
        assert_eq!(window.get_cache_error().as_str(), "Read interrupted");
        let stale = ProjectionRequest {
            revision: 1,
            ..Default::default()
        };
        render(
            &window,
            generation,
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &stale),
        );
        assert_eq!(window.get_cache_error().as_str(), "Read interrupted");
        render(
            &window,
            generation,
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &request),
        );
        assert!(window.get_cache_error().is_empty());
        assert_eq!(
            window.get_action_feedback().as_str(),
            "Could not confirm this command"
        );
    }

    #[test]
    fn an_empty_local_search_does_not_claim_that_authorized_entities_disappeared() {
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        bind(&window);
        let view = home_view();
        let request = ProjectionRequest {
            query: "no-synthetic-alias-matches-this".into(),
            ..Default::default()
        };
        let projection =
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &request);
        assert_eq!(projection.total, 0);
        assert!(projection.rows.is_empty());
        render(
            &window,
            ProjectionMailbox::<()>::new().generation(),
            projection,
        );
        assert_eq!(window.get_cache_rows().row_count(), 0);
        assert_eq!(
            window.get_read_status().as_str(),
            format!("States up to date · {} entities", view.len())
        );
    }

    #[test]
    fn room_search_and_detail_use_only_the_authorized_view_and_keep_missing_favorites_explicit() {
        let view = home_view();
        let request = ProjectionRequest {
            revision: 7,
            room: Some(dashboard::fixture().rooms[1].id.clone()),
            native: None,
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(
                dashboard::joined_fixture(&view),
            )),
            query: "main".into(),
            detail: Some(
                EntityName::try_from("homes/synthetic/entities/private-entity-0001").unwrap(),
            ),
            catalogue: None,
        };
        let projection =
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &request);
        assert_eq!(projection.total, 1);
        assert_eq!(
            projection.rows[0].cells[0],
            "light.synthetic_workbench_main"
        );
        assert_eq!(projection.home.room_counts, [1, 1]);
        let detail = projection.home.detail.unwrap();
        assert_eq!(detail[0], "light.synthetic_workbench_main");
        assert_eq!(detail[1], "Off");
        assert_eq!(detail[3], "1970-01-01T00:00:10+00:00");
        // The removed overview-v1 favorites collection is not a v2 data source.
        // Missing readings are exercised by exact widget bindings above.
        assert!(projection.home.observations.is_empty());
    }

    #[test]
    fn replacing_an_identity_or_query_cancels_old_intentions_but_value_updates_preserve_them() {
        let view = home_view();
        let model = CacheRows::default();
        let generation = ProjectionMailbox::<()>::new().generation();
        model.apply(
            generation,
            project(CachePhase::Live, Some(&view), Viewport::default()),
        );
        let first = model.serial.get();
        assert!(model.resolve(0, first, 0).is_some());
        assert!(
            model.resolve(0, first, 1).is_none(),
            "a pending query cannot activate the previous result"
        );
        let mut updated = project(CachePhase::Live, Some(&view), Viewport::default());
        updated.rows[0].cells[1] = "On".into();
        model.apply(generation, updated);
        assert_eq!(
            model.serial.get(),
            first,
            "value-only updates retain pointer/keyboard gestures"
        );
        let mut replaced = project(CachePhase::Live, Some(&view), Viewport::default());
        let replacement =
            EntityName::try_from("homes/synthetic/entities/private-replacement").unwrap();
        replaced.rows[0].id = Some(replacement.clone());
        model.apply(generation, replaced);
        assert!(model.resolve(0, first, 0).is_none());
        assert_eq!(model.resolve(0, model.serial.get(), 0), Some(replacement));
        // Overview-v1 favorite slots were removed. Current widget/room owner
        // invalidation is asserted in the exact-binding tests above.
        model.clear();
        assert!(model.state.borrow().observations.is_empty());
        assert!(
            model
                .resolve_observation(
                    dashboard::OBSERVATION_TOKEN_BASE,
                    model.home_serial.get(),
                    0
                )
                .is_none()
        );
    }

    #[test]
    fn room_list_transfers_focus_and_two_back_key_events_return_through_cards_to_home() {
        use i_slint_backend_testing::ElementQuery;
        use slint::{
            ComponentHandle,
            platform::{Key, PointerEventButton, WindowEvent},
        };
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        let view = home_view();
        let request = std::sync::Arc::new(std::sync::Mutex::new(ProjectionRequest {
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(
                dashboard::joined_fixture(&view),
            )),
            ..Default::default()
        }));
        bind(&window);
        crate::navigation::bind(&window, request, || {});
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_show_endpoint(false);
        window
            .window()
            .set_size(slint::LogicalSize::new(1280., 800.));
        window.show().unwrap();
        render(
            &window,
            ProjectionMailbox::<()>::new().generation(),
            project(CachePhase::Live, Some(&view), Viewport::default()),
        );
        window.invoke_room_open(dashboard::ROOM_TOKEN_BASE, window.get_home_generation());
        let button = ElementQuery::from_root(&window)
            .match_inherits("KitButton")
            .match_predicate(|item| item.accessible_label().as_deref() == Some("All Items"))
            .find_first()
            .unwrap();
        let p = button.absolute_position();
        let size = button.size();
        let position = slint::LogicalPosition::new(p.x + size.width / 2., p.y + size.height / 2.);
        window.window().dispatch_event(WindowEvent::PointerPressed {
            position,
            button: PointerEventButton::Left,
        });
        window
            .window()
            .dispatch_event(WindowEvent::PointerReleased {
                position,
                button: PointerEventButton::Left,
            });
        assert_eq!(window.get_room_view(), crate::RoomView::List);
        for expected_page in [crate::ViewPage::Room, crate::ViewPage::Home] {
            window.window().dispatch_event(WindowEvent::KeyPressed {
                text: Key::Back.into(),
            });
            window.window().dispatch_event(WindowEvent::KeyReleased {
                text: Key::Back.into(),
            });
            assert_eq!(
                window.get_room_view(),
                crate::RoomView::Cards,
                "Back must reach the content handler after its old focused control is hidden"
            );
            assert_eq!(window.get_page(), expected_page);
        }
        assert_eq!(window.get_page(), crate::ViewPage::Home);
        crate::navigation::bind_native_back(&window);
        window.invoke_choose_room();
        assert_eq!(window.get_page(), crate::ViewPage::Rooms);
        window.window().dispatch_event(WindowEvent::CloseRequested);
        assert_eq!(window.get_page(), crate::ViewPage::Home);
        assert!(
            window.window().is_visible(),
            "native Back pops a route without closing the window"
        );
        window.window().dispatch_event(WindowEvent::CloseRequested);
        assert!(
            !window.window().is_visible(),
            "native Back may leave the application only at home"
        );
    }

    #[test]
    fn route_changes_reject_stale_projections_and_private_clear_drops_every_new_surface() {
        use slint::ComponentHandle;
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        let view = home_view();
        let request = std::sync::Arc::new(std::sync::Mutex::new(ProjectionRequest {
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(
                dashboard::joined_fixture(&view),
            )),
            ..Default::default()
        }));
        bind(&window);
        crate::navigation::bind(&window, request.clone(), || {});
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_show_endpoint(false);
        window.show().unwrap();
        let generation = ProjectionMailbox::<()>::new().generation();
        render(
            &window,
            generation,
            project(CachePhase::Live, Some(&view), Viewport::default()),
        );
        let persistent = window.get_home_rooms();
        // The room catalogue is an application page. Home contains only the
        // configured forest and must not synthesize these catalogue tiles.
        window.invoke_quick_navigation(crate::ViewPage::Rooms);
        assert_eq!(window.get_page(), crate::ViewPage::Rooms);
        window.invoke_room_open(i32::MIN, 1);
        assert_eq!(window.get_page(), crate::ViewPage::Rooms);
        window.invoke_room_open(dashboard::ROOM_TOKEN_BASE, 99);
        assert_eq!(window.get_page(), crate::ViewPage::Rooms);
        window.invoke_room_open(dashboard::ROOM_TOKEN_BASE, window.get_home_generation());
        assert_eq!(window.get_page(), crate::ViewPage::Room);
        assert!(window.invoke_system_back());
        assert_eq!(window.get_page(), crate::ViewPage::Rooms);
        assert_eq!(window.get_home_focus().token, dashboard::ROOM_TOKEN_BASE);
        assert_eq!(
            window.get_home_focus().generation,
            window.get_home_generation()
        );
        slint::platform::update_timers_and_animations();
        for event in [
            slint::platform::WindowEvent::KeyPressed {
                text: slint::platform::Key::Return.into(),
            },
            slint::platform::WindowEvent::KeyReleased {
                text: slint::platform::Key::Return.into(),
            },
        ] {
            window.window().dispatch_event(event);
        }
        assert_eq!(
            window.get_page(),
            crate::ViewPage::Room,
            "Back must restore a live keyboard target, not only a focus request value"
        );
        assert!(window.invoke_system_back());
        assert_eq!(window.get_page(), crate::ViewPage::Rooms);
        // Content focus selects a visible catalogue item, with the same live
        // keyboard behavior as restoring a room opener.
        window.invoke_content_focus();
        slint::platform::update_timers_and_animations();
        for event in [
            slint::platform::WindowEvent::KeyPressed {
                text: slint::platform::Key::Return.into(),
            },
            slint::platform::WindowEvent::KeyReleased {
                text: slint::platform::Key::Return.into(),
            },
        ] {
            window.window().dispatch_event(event);
        }
        assert_eq!(
            window.get_page(),
            crate::ViewPage::Room,
            "catalogue content focus must target an actual visible room"
        );
        window.invoke_go_back();
        assert!(window.invoke_system_back());
        assert_eq!(window.get_page(), crate::ViewPage::Home);
        assert!(!window.invoke_system_back());
        // The current native dashboard has no overview-v1 observation slots.
        // Resolve the authorized entity through the live inventory projection.
        window.invoke_quick_navigation(crate::ViewPage::Inventory);
        let current = request.lock().unwrap().clone();
        render(
            &window,
            generation,
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &current),
        );
        let token = {
            let model = window.get_cache_rows();
            let rows = model.as_any().downcast_ref::<CacheRows>().unwrap();
            let state = rows.state.borrow();
            let target =
                EntityName::try_from("homes/synthetic/entities/private-entity-0000").unwrap();
            i32::try_from(
                *state
                    .rows
                    .iter()
                    .find(|(_, (row, _))| row.id.as_ref() == Some(&target))
                    .unwrap()
                    .0,
            )
            .unwrap()
        };
        window.invoke_cache_activate(token, window.get_cache_generation());
        assert_eq!(window.get_page(), crate::ViewPage::Detail);
        let current = request.lock().unwrap().clone();
        render(
            &window,
            generation,
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &current),
        );
        assert_eq!(
            window.get_detail_title().as_str(),
            "sensor.synthetic_ambient"
        );
        window.set_search_query("workbench".into());
        window.invoke_query_edited("workbench".into());
        assert_eq!(window.get_page(), crate::ViewPage::Inventory);
        let navigation = window.get_navigation_state();
        assert!(navigation.selected.present && navigation.active.present);
        assert_eq!(navigation.selected.token, navigation.active.token);
        assert!(request.lock().unwrap().detail.is_none());
        window.invoke_cache_activate(0, window.get_cache_generation());
        assert_eq!(
            window.get_page(),
            crate::ViewPage::Inventory,
            "old result cannot open during reprojection"
        );
        let current = request.lock().unwrap().clone();
        let stale = project_request(CachePhase::Live, Some(&view), Viewport::default(), &current);
        let rows_before = window.get_cache_rows().row_count();
        render(
            &window,
            generation,
            project(CachePhase::Live, Some(&view), Viewport::default()),
        );
        assert_eq!(window.get_cache_rows().row_count(), rows_before);
        render(
            &window,
            generation,
            project_request(
                CachePhase::Blocked,
                None,
                Viewport::default(),
                &ProjectionRequest {
                    revision: current.revision - 1,
                    ..current.clone()
                },
            ),
        );
        assert!(window.get_read_status().starts_with("Reading unavailable"));
        assert_eq!(window.get_cache_rows().row_count(), 0);
        assert!(!window.get_view_loaded());
        assert_eq!(window.get_home_observations().row_count(), 0);
        assert_eq!(window.get_search_query().as_str(), "");
        assert_eq!(window.get_detail_title().as_str(), "");
        assert_eq!(window.get_detail_value().as_str(), "");
        assert_eq!(window.get_detail_quality().as_str(), "");
        assert_eq!(window.get_detail_date().as_str(), "");
        assert!(request.lock().unwrap().detail.is_none());
        assert_eq!(window.get_page(), crate::ViewPage::Home);
        assert!(
            std::ptr::eq(persistent.as_any(), window.get_home_rooms().as_any()),
            "updates retain the model instance while private room data is removed"
        );
        render(&window, generation, stale);
        assert_eq!(window.get_cache_rows().row_count(), 0);
        assert_eq!(window.get_home_observations().row_count(), 0);
    }

    #[test]
    fn server_layout_changes_invalidate_old_room_gestures_and_never_mix_config_stamps() {
        let view = home_view();
        let generation = ProjectionMailbox::<()>::new().generation();
        let model = CacheRows::default();
        let mut layout = dashboard::joined_fixture(&view);
        let mut request = ProjectionRequest {
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(layout.clone())),
            ..Default::default()
        };
        model.apply(
            generation,
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &request),
        );
        let before = model.home_serial.get();
        assert!(
            model
                .resolve_room(dashboard::ROOM_TOKEN_BASE, before, Some(&layout))
                .is_some()
        );
        layout.title = "Changed by server".into();
        layout.etag = "changed-dashboard-etag".into();
        layout.rooms[0].entities = dashboard::RoomEntities::new(vec![
            EntityName::try_from("homes/synthetic/entities/private-entity-0001").unwrap(),
        ])
        .unwrap();
        request.dashboard = dashboard::LayoutState::Ready(std::sync::Arc::new(layout.clone()));
        assert!(
            model
                .resolve_room(dashboard::ROOM_TOKEN_BASE, before, Some(&layout))
                .is_none(),
            "pending definition rejects old gestures before projection"
        );
        model.apply(
            generation,
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &request),
        );
        assert!(
            model
                .resolve_room(dashboard::ROOM_TOKEN_BASE, before, Some(&layout))
                .is_none()
        );
        assert!(
            model
                .resolve_room(
                    dashboard::ROOM_TOKEN_BASE,
                    model.home_serial.get(),
                    Some(&layout)
                )
                .is_some()
        );
        layout.capture = Some(home_view().incarnation().clone());
        request.dashboard = dashboard::LayoutState::Ready(std::sync::Arc::new(layout.clone()));
        let unauthorized_cohort =
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &request);
        assert!(
            unauthorized_cohort.home.layout.is_none()
                && unauthorized_cohort.home.observations.is_empty()
        );
        layout.capture = None;
        request.dashboard = dashboard::LayoutState::Ready(std::sync::Arc::new(layout));
        let incompatible =
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &request);
        assert!(incompatible.home.layout.is_none());
        assert!(incompatible.home.observations.is_empty());
        request.dashboard = dashboard::LayoutState::Unavailable;
        let absent = project_request(CachePhase::Live, Some(&view), Viewport::default(), &request);
        assert!(absent.home.layout.is_none() && absent.home.room_counts.is_empty());
        model.clear();
        assert!(model.state.borrow().layout.is_none());
        assert_eq!(model.rooms.row_count(), 0);
    }

    #[test]
    fn visible_text_preserves_false_zero_absence_and_decimal_precision() {
        use wire::capability::Trait as T;
        let capability = |value| wire::Capability {
            r#trait: Some(value),
            ..Default::default()
        };
        let off = capability(T::Power(Box::new(wire::PowerCapability {
            observation: wire::PowerObservation {
                value: Some(wire::PowerValue::Off.into()),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        })));
        assert_eq!(value_text(&off), "Off");
        let light = |on, brightness_percent, color_temperature_mireds| {
            capability(T::LightControl(Box::new(wire::LightControlCapability {
                observation: wire::LightControlObservation {
                    on,
                    brightness_percent,
                    color_temperature_mireds,
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            })))
        };
        assert_eq!(
            value_text(&light(Some(false), None, Some(334))),
            "Off · ≈ 2994.012 K · 334 mireds"
        );
        for color in [None, Some(0), Some(1001)] {
            assert_eq!(value_text(&light(None, None, color)), "No observation");
        }
        assert_eq!(
            value_text(&light(Some(false), Some(0.0), None)),
            "Off · 0 %"
        );
        assert_eq!(value_text(&light(Some(false), None, None)), "Off");
        let measurement = |value, unit: &str, decimal: Option<&str>| {
            capability(T::NumericMeasurement(Box::new(
                wire::NumericMeasurementCapability {
                    unit: unit.into(),
                    observation: wire::NumericObservation {
                        value,
                        decimal_value: decimal.map(str::to_owned),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
            )))
        };
        assert_eq!(value_text(&measurement(Some(0.0), "W", None)), "0 W");
        assert_eq!(
            value_text(&measurement(Some(0.0), "CAD", Some("0.0100"))),
            "0.0100 CAD"
        );
        assert_eq!(value_text(&measurement(Some(21.5), "°C", None)), "21.5 °C");
        assert_eq!(value_text(&measurement(None, "W", None)), "No observation");
        assert_eq!(value_text(&wire::Capability::default()), "No observation");
        for known in [off, measurement(Some(21.5), "°C", None)] {
            let mut bytes = known.encode_to_vec();
            bytes.extend_from_slice(&[0x9a, 0x06, 0x02, 0x08, 0x01]);
            let future = wire::Capability::decode_from_slice(&bytes).unwrap();
            assert_eq!(
                value_text(&future),
                "No observation",
                "future trait evidence is never admitted as a known observation"
            );
        }
    }

    #[test]
    fn qualities_are_visible_and_long_unicode_values_stay_bounded() {
        use wire::capability::Trait as T;
        let mut observation = wire::PowerObservation {
            quality: wire::ObservationQuality {
                availability: wire::Availability::Available.into(),
                freshness: wire::Freshness::Stale.into(),
                provenance: wire::Provenance::Restored.into(),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        let cap = |observation: wire::PowerObservation| wire::Capability {
            r#trait: Some(T::Power(Box::new(wire::PowerCapability {
                observation: observation.into(),
                ..Default::default()
            }))),
            ..Default::default()
        };
        assert_eq!(
            quality_text(&cap(observation.clone())),
            "Available · Stale · Restored"
        );
        observation.quality.as_option_mut().unwrap().provenance =
            wire::Provenance::Estimated.into();
        assert_eq!(
            quality_text(&cap(observation.clone())),
            "Available · Stale · Estimated"
        );
        observation.quality.as_option_mut().unwrap().availability = EnumValue::Unknown(99);
        assert_eq!(quality_text(&cap(observation)), "Quality not provided");
        let helper = |value| wire::Capability {
            r#trait: Some(T::Helper(Box::new(wire::HelperCapability {
                observation: wire::HelperObservation {
                    value: wire::HelperValue {
                        value,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }))),
            ..Default::default()
        };
        let clipped = value_text(&helper(Some(wire::helper_value::Value::Text(
            "🪟".repeat(4096),
        ))));
        assert_eq!(clipped.chars().count(), 257);
        assert!(clipped.ends_with('…'));
        assert_eq!(
            value_text(&helper(Some(
                wire::helper_value::Value::Text(String::new())
            ))),
            "Empty text"
        );
        assert_eq!(value_text(&helper(None)), "Value not provided");
    }
    #[test]
    fn native_layout_models_survive_switching_and_private_clear_without_losing_their_binding() {
        use crate::dashboard_layout::Kind;
        use crate::dashboard_layout::tests::{children, node as strict_node};
        use slint::ComponentHandle;
        use wire::dashboard_layout_node::Content as C;
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        window.set_show_endpoint(false);
        window.set_access_stage(crate::ProAccessStage::Connected);
        let node = |id: &str, kind: Kind, child_ids: &[&str], title: &str| {
            let payload = match kind {
                Kind::View => C::View(
                    wire::DashboardViewNode {
                        children: children(child_ids).into(),
                        ..Default::default()
                    }
                    .into(),
                ),
                Kind::Section => C::Section(
                    wire::DashboardSectionNode {
                        children: children(child_ids).into(),
                        ..Default::default()
                    }
                    .into(),
                ),
                Kind::Heading => C::Heading(
                    wire::DashboardHeadingNode {
                        style: wire::DashboardHeadingStyle::Default.into(),
                        ..Default::default()
                    }
                    .into(),
                ),
                Kind::Unsupported => C::Unsupported(
                    wire::DashboardUnsupportedNode {
                        diagnostic: "Unsupported source card".into(),
                        ..Default::default()
                    }
                    .into(),
                ),
                _ => panic!("fixture only declares four closed node variants"),
            };
            let mut result = strict_node(id, payload);
            result.display_name = title.into();
            result
        };
        let cache = home_view();
        let mut layout = dashboard::joined_fixture(&cache);
        let fixture: serde_json::Value =
            serde_json::from_str(include_str!("../tests/fixtures/overview.json")).unwrap();
        let mut room_node = node("room", Kind::View, &["room-section"], "Room");
        let Some(C::View(content)) = room_node.content.as_mut() else {
            panic!()
        };
        content.route = fixture["rooms"][0]["id"].as_str().unwrap().into();
        layout.native = Some(
            crate::dashboard_layout::NativeLayout::try_from(&wire::DashboardLayout {
                views: vec!["view".into(), "room".into()],
                nodes: vec![
                    node("view", Kind::View, &["section"], "Home"),
                    node("section", Kind::Section, &["heading", "unknown"], ""),
                    node("heading", Kind::Heading, &[], "Server heading"),
                    node("unknown", Kind::Unsupported, &[], "Unknown custom card"),
                    room_node,
                    node("room-section", Kind::Section, &["room-heading"], ""),
                    node("room-heading", Kind::Heading, &[], "Room heading"),
                ],
                ..Default::default()
            })
            .unwrap(),
        );
        let request = std::sync::Arc::new(std::sync::Mutex::new(ProjectionRequest {
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(layout.clone())),
            ..Default::default()
        }));
        bind(&window);
        crate::navigation::bind(&window, request.clone(), || {});
        let generation = ProjectionMailbox::<()>::new().generation();
        render(
            &window,
            generation,
            project_request(
                CachePhase::Live,
                Some(&cache),
                Viewport::default(),
                &request.lock().unwrap(),
            ),
        );
        window.show().unwrap();
        let model = window.get_home_layout_nodes();
        assert_eq!(model.row_count(), 4);
        assert_eq!(model.row_data(1).unwrap().title, "");
        assert_eq!(model.row_data(2).unwrap().title, "Server heading");
        assert_eq!(
            model.row_data(3).unwrap().kind,
            crate::ProLayoutKind::Diagnostic
        );
        request.lock().unwrap().room = Some(layout.rooms[0].id.clone());
        render(
            &window,
            generation,
            project_request(
                CachePhase::Live,
                Some(&cache),
                Viewport::default(),
                &request.lock().unwrap(),
            ),
        );
        assert_eq!(window.get_room_layout_nodes().row_count(), 3);
        assert_eq!(
            model.row_count(),
            4,
            "a room publication must preserve the hidden home content and its scroll extent"
        );
        assert_eq!(model.row_data(2).unwrap().title, "Server heading");
        request.lock().unwrap().room = None;
        clear_for_layout(&window);
        assert_eq!(window.get_home_layout_nodes().row_count(), 0);
        assert_eq!(window.get_room_layout_nodes().row_count(), 0);
        assert!(
            std::ptr::eq(model.as_any(), window.get_home_layout_nodes().as_any()),
            "selection must clear rows, not replace the persistent model"
        );
        layout.title = "Second server dashboard".into();
        request.lock().unwrap().dashboard =
            dashboard::LayoutState::Ready(std::sync::Arc::new(layout));
        render(
            &window,
            generation,
            project_request(
                CachePhase::Live,
                Some(&cache),
                Viewport::default(),
                &request.lock().unwrap(),
            ),
        );
        assert_eq!(window.get_home_layout_nodes().row_count(), 4);
        assert_eq!(window.get_dashboard_title(), "Second server dashboard");
        clear(&window);
        assert_eq!(model.row_count(), 0);
    }

    #[test]
    fn authorization_inventory_equality_does_not_depend_on_capability_order() {
        let mut entry = fixtures::entry(
            "homes/synthetic/entities/lamp",
            "Synthetic lamp",
            vec![
                fixtures::power(
                    "homes/synthetic/entities/lamp/capabilities/a",
                    Some(wire::PowerValue::Off),
                ),
                fixtures::power(
                    "homes/synthetic/entities/lamp/capabilities/b",
                    Some(wire::PowerValue::On),
                ),
            ],
        );
        let first = fixtures::capture(vec![entry.clone()]);
        entry.capabilities.reverse();
        let second = fixtures::capture(vec![entry]);
        assert!(authority(&first).is_some() && authority(&first) == authority(&second));
    }
    #[tokio::test]
    async fn renewal_revokes_resolvers_before_ui_and_rejects_delayed_ready_after_rupture() {
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        bind(&window);
        let credentials = crate::renewal_tests::Credentials::new().await;
        let view = credentials.captured(home_view()).await;
        let request = ProjectionRequest {
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(
                dashboard::joined_fixture(&view),
            )),
            ..Default::default()
        };
        let devices = crate::device_view::DeviceStore::default();
        let generation = ProjectionMailbox::<()>::new().generation();
        let project = || {
            project_devices(
                CachePhase::Live,
                Some(&view),
                Viewport::default(),
                &request,
                &devices,
            )
        };
        render(&window, generation, project());
        let queued_ready = project();
        let queued_after_rupture = project();
        let rows = window.get_cache_rows();
        let model = rows.as_any().downcast_ref::<CacheRows>().unwrap();
        let (token, entity) = model
            .state
            .borrow()
            .rows
            .iter()
            .find_map(|(token, (row, _))| row.id.clone().map(|id| (*token as i32, id)))
            .unwrap();
        let serial = model.serial.get();
        let revision = window.get_projection_version();
        assert!(model.resolve(token, serial, revision).is_some());
        model.state.borrow_mut().observations.insert(42, entity);
        let home_serial = model.home_serial.get();
        assert!(
            model
                .resolve_observation(42, home_serial, revision)
                .is_some()
        );
        let count = window.get_home_layout_nodes().row_count();
        credentials.renew().await;
        assert!(!model.pending_cohort.get(), "no UI publication has run yet");
        assert!(model.resolve(token, serial, revision).is_none());
        assert!(
            model
                .resolve_observation(42, home_serial, revision)
                .is_none()
        );
        assert!(widget_bindings(&window).is_empty());
        assert!(matches!(
            render(&window, generation, queued_ready),
            RenderOutcome::Retained { .. }
        ));
        assert_eq!(window.get_home_layout_nodes().row_count(), count);
        assert!(!window.get_commands_ready());
        credentials.session.close().await;
        assert!(matches!(
            render(&window, generation, queued_after_rupture),
            RenderOutcome::Cleared
        ));
        assert_eq!(window.get_home_layout_nodes().row_count(), 0);
        assert!(!window.get_has_configured_layout());
    }

    #[test]
    fn a_new_capture_retains_pixels_but_revokes_intents_until_its_dashboard_joins() {
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        bind(&window);
        let initial = home_view();
        let entries: Vec<_> = initial.entities().map(|(_, entry)| entry.clone()).collect();
        let first = fixtures::capture(entries.clone());
        let second = fixtures::capture(entries);
        let layout = dashboard::joined_fixture(&first);
        let mut request = ProjectionRequest {
            detail: Some(first.entities().next().unwrap().0.clone()),
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(layout.clone())),
            ..Default::default()
        };
        let store = crate::device_view::DeviceStore::default();
        let generation = ProjectionMailbox::<()>::new().generation();
        render(
            &window,
            generation,
            project_devices(
                CachePhase::Live,
                Some(&first),
                Viewport::default(),
                &request,
                &store,
            ),
        );
        let devices = window.get_device_cards();
        let panels = window.get_detail_panels();
        let before = devices.row_data(0).expect("rendered detail");
        let rows = window.get_cache_rows();
        let model = rows.as_any().downcast_ref::<CacheRows>().unwrap();
        let old_serial = model.serial.get();
        let old_count = devices.row_count();
        let pending = project_devices(
            CachePhase::Live,
            Some(&second),
            Viewport::default(),
            &request,
            &store,
        );
        assert!(
            pending.home.layout.is_none(),
            "old layout never authorizes new capture"
        );
        assert!(pending.pending_cohort.is_some());
        let expiry = layout.binding_expires.unwrap();
        assert!(retains_pixels(
            &model.state.borrow(),
            generation,
            &pending,
            (expiry.0 - 30, expiry.1)
        ));
        assert!(!retains_pixels(
            &model.state.borrow(),
            generation,
            &pending,
            expiry
        ));
        render(&window, generation, pending);
        assert!(window.get_cohort_pending());
        assert!(!window.get_commands_ready());
        assert_eq!(devices.row_count(), old_count);
        assert_eq!(devices.row_data(0).unwrap(), before);
        assert_eq!(panels, window.get_detail_panels());
        assert!(model.resolve(0, old_serial, request.revision).is_none());
        assert!(widget_bindings(&window).is_empty());
        assert!(
            store
                .prepare(
                    before.token,
                    before.generation,
                    request.revision,
                    crate::device_view::DeviceIntent::Power(crate::device_view::Power::On)
                )
                .is_err()
        );
        request.dashboard =
            dashboard::LayoutState::Ready(std::sync::Arc::new(dashboard::joined_fixture(&second)));
        render(
            &window,
            generation,
            project_devices(
                CachePhase::Live,
                Some(&second),
                Viewport::default(),
                &request,
                &store,
            ),
        );
        assert!(!window.get_cohort_pending());
        assert_eq!(devices, window.get_device_cards());
        assert_eq!(panels, window.get_detail_panels());
        assert_eq!(devices.row_count(), old_count);
        assert_ne!(devices.row_data(0).unwrap().generation, before.generation);
    }

    #[test]
    fn capture_retention_cannot_cross_authorization_changes_or_explicit_absence() {
        let first = home_view();
        let layout = dashboard::joined_fixture(&first);
        let generation = ProjectionMailbox::<()>::new().generation();
        let model = CacheRows::default();
        let mut request = ProjectionRequest {
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(layout.clone())),
            ..Default::default()
        };
        model.apply(
            generation,
            project_request(
                CachePhase::Live,
                Some(&first),
                Viewport::default(),
                &request,
            ),
        );
        let expiry = layout.binding_expires.unwrap();
        for change in 0..4 {
            let mut entries: Vec<_> = first.entities().map(|(_, entry)| entry.clone()).collect();
            match change {
                0 => {
                    entries.pop();
                }
                1 => {
                    entries[0].capabilities.clear();
                }
                2 => {
                    entries[0].capabilities[0].action_access = wire::ActionAccess::Denied.into();
                }
                3 => {
                    entries[0].capabilities[0].etag = "changed-definition".into();
                }
                _ => unreachable!(),
            }
            let next = fixtures::capture(entries);
            let projection =
                project_request(CachePhase::Live, Some(&next), Viewport::default(), &request);
            assert!(!retains_pixels(
                &model.state.borrow(),
                generation,
                &projection,
                (expiry.0 - 30, expiry.1)
            ));
        }
        let absent = project_request(CachePhase::Live, None, Viewport::default(), &request);
        assert!(absent.pending_cohort.is_none());
        let next = home_view();
        request.dashboard = dashboard::LayoutState::Unavailable;
        assert!(
            project_request(CachePhase::Live, Some(&next), Viewport::default(), &request)
                .pending_cohort
                .is_none()
        );
    }
}

/// Weak provenance of the currently displayed capture, never the capture itself.
pub(crate) fn presentation_source(
    window: &HomewardWindow,
) -> Option<home_client_cache::PresentationSource> {
    let rows = window.get_cache_rows();
    rows.as_any()
        .downcast_ref::<CacheRows>()?
        .state
        .borrow()
        .presentation
        .clone()
}
pub(crate) fn presentation_remaining(
    source: &home_client_cache::PresentationSource,
) -> Option<std::time::Duration> {
    if !source.source().presentation_continues() {
        return None;
    }
    let now = chrono::Utc::now();
    let (seconds, nanos) = source.deadline();
    let remaining = i128::from(seconds - now.timestamp()) * 1_000_000_000
        + i128::from(nanos - now.timestamp_subsec_nanos() as i32);
    (remaining > 0)
        .then(|| std::time::Duration::from_nanos(u64::try_from(remaining).unwrap_or(u64::MAX)))
}
