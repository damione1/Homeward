//! Product projection: private identities/cursors stay in the Rust cache. Slint
//! keeps the inventory virtualized at 64 rows. Configured dashboard forests
//! are separately bounded by their negotiated profile.
use crate::{
    ConnectionStatus, HomewardWindow, KitGlyphs, ProObservationRow, ProRoomRow, TableRow,
    dashboard::{self, Icon, Layout, ProjectionRequest},
};
use buffa::EnumValue;
use home_client_cache::{CacheEvent, CachePhase, CachedView, ProjectionGeneration, Viewport};
use home_client_model::EntityId;
use home_wire_contract::home::{helpers::v1 as helpers, state::v1 as state};
use slint::{ComponentHandle, Model, ModelNotify, ModelRc, ModelTracker, SharedString};
use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
};

#[derive(Clone, PartialEq, Eq)]
struct Row {
    id: Option<EntityId>,
    index: usize,
    cells: [String; 3],
}

pub(crate) struct CacheProjection {
    history_entity: Option<EntityId>,
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
    pub(crate) fn history_target(&self, revision: i32) -> Option<Option<EntityId>> {
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
    id: Option<EntityId>,
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
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(dashboard::fixture())),
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

pub(crate) fn project_devices(
    phase: CachePhase,
    view: Option<&CachedView>,
    viewport: Viewport,
    request: &ProjectionRequest,
    devices: &crate::device_view::DeviceStore,
) -> CacheProjection {
    let layout = request
        .dashboard
        .layout()
        .filter(|layout| view.is_some_and(|view| layout.compatible(view)));
    let matches = |id: &EntityId, entity: &state::ViewEntity| {
        request.room.as_ref().is_none_or(|room_id| {
            layout
                .and_then(|layout| layout.room(room_id))
                .is_some_and(|room| room.entities.contains(id))
        }) && (request.query.is_empty()
            || entity.current_alias.to_lowercase().contains(&request.query))
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
                    let title = if entity.current_alias.is_empty() {
                        "Unnamed Item".into()
                    } else {
                        bounded(&entity.current_alias)
                    };
                    let (value, quality) = match entity.state.as_option() {
                        Some(observation) => (value_text(observation), quality_text(observation)),
                        None => ("No observation".into(), "Not provided".into()),
                    };
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
                        .and_then(|entity| entity.state.as_option())
                        .map(summary_text)
                        .unwrap_or_else(|| "No observation".into());
                    Observation {
                        token: dashboard::OBSERVATION_TOKEN_BASE + index as i32,
                        label: item.label.clone(),
                        icon: item.icon,
                        id: entity.map(|_| item.entity.clone()),
                        value: bounded(&value),
                        description: entity
                            .and_then(|entity| entity.state.as_option())
                            .map(quality_text)
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
            let (value, quality, date) = match entity.state.as_option() {
                Some(observation) => (
                    value_text(observation),
                    quality_text(observation),
                    observation
                        .observed_at
                        .as_option()
                        .and_then(|at| {
                            chrono::DateTime::from_timestamp(at.seconds, at.nanos as u32)
                        })
                        .map(|at| at.to_rfc3339())
                        .unwrap_or_else(|| "Date not provided".into()),
                ),
                None => (
                    "No observation".into(),
                    "Not provided".into(),
                    "Date not provided".into(),
                ),
            };
            [
                bounded(&entity.current_alias),
                bounded(&value),
                bounded(&quality),
                date,
            ]
        });
    let profile_zone =
        crate::environment_view::ProfileZone::from_profile(view.and_then(|v| v.display_profile()));
    let devices = devices.project(phase, view, request);
    let board = crate::dashboard_presentation::Board::project(layout, request, &devices, view);
    CacheProjection {
        history_entity: if phase == CachePhase::Live && detail.is_some() {
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
            captured: view.is_some(),
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
fn helper_text(value: &helpers::HelperValue) -> String {
    use helpers::helper_value::Value;
    match value.value.as_ref() {
        Some(Value::Boolean(value)) => on_off(*value).into(),
        Some(Value::Number(value)) => number(*value),
        Some(Value::Counter(value)) => value.to_string(),
        Some(Value::SelectedOption(value) | Value::Text(value)) => {
            if value.is_empty() {
                "Empty text".into()
            } else {
                bounded(value)
            }
        }
        Some(Value::Date(date)) => format!("{:04}-{:02}-{:02}", date.year, date.month, date.day),
        Some(Value::Time(time)) => {
            format!("{:02}:{:02}:{:02}", time.hour, time.minute, time.second)
        }
        Some(Value::Datetime(value)) => match (value.date.as_option(), value.time.as_option()) {
            (Some(date), Some(time)) => format!(
                "{:04}-{:02}-{:02} {:02}:{:02}:{:02}",
                date.year, date.month, date.day, time.hour, time.minute, time.second
            ),
            (None, _) | (_, None) => "Date or time missing".into(),
        },
        Some(Value::ButtonPressedAt(_)) => "Activation observed".into(),
        None => "Value not provided".into(),
    }
}

fn summary_text(observation: &state::ObservedState) -> String {
    if !observation.__buffa_unknown_fields.is_empty() {
        return value_text(observation);
    }
    match observation.value.as_ref() {
        Some(state::observed_state::Value::Climate(climate)) => climate
            .current_temperature_celsius
            .map(|value| format!("{} °C", number(value)))
            .unwrap_or_else(|| value_text(observation)),
        Some(
            state::observed_state::Value::SwitchOn(_)
            | state::observed_state::Value::Measurement(_)
            | state::observed_state::Value::Light(_)
            | state::observed_state::Value::Fan(_)
            | state::observed_state::Value::Helper(_)
            | state::observed_state::Value::Solar(_)
            | state::observed_state::Value::SolarEvent(_)
            | state::observed_state::Value::Cover(_)
            | state::observed_state::Value::Lock(_)
            | state::observed_state::Value::Alarm(_)
            | state::observed_state::Value::Weather(_)
            | state::observed_state::Value::Presence(_)
            | state::observed_state::Value::PresenceDetails(_),
        )
        | None => value_text(observation),
    }
}

pub(crate) fn value_text(observation: &state::ObservedState) -> String {
    if !observation.__buffa_unknown_fields.is_empty() {
        return "Unsupported value".into();
    }
    use state::observed_state::Value;
    match observation.value.as_ref() {
        Some(Value::Cover(value)) => crate::control_text::cover(value),
        Some(Value::Lock(value)) => crate::control_text::lock(value).into(),
        Some(Value::Alarm(value)) => crate::control_text::alarm(value).into(),
        Some(Value::SwitchOn(value)) => on_off(*value).into(),
        Some(Value::Measurement(value)) => format!(
            "{} {}",
            value
                .decimal_value
                .as_deref()
                .map(bounded)
                .unwrap_or_else(|| number(value.value)),
            bounded(&value.unit)
        )
        .trim()
        .to_owned(),
        Some(Value::Light(value)) => {
            let mut parts = Vec::new();
            if let Some(on) = value.on {
                parts.push(on_off(on).to_owned());
            }
            if let Some(brightness) = value.brightness_percent {
                parts.push(format!("{} %", number(brightness)));
            }
            if let Some(value) = value
                .color_temp_mireds
                .and_then(crate::device_view::color_temperature_text)
            {
                parts.push(value);
            }
            if parts.is_empty() {
                "Value not provided".into()
            } else {
                parts.join(" · ")
            }
        }
        Some(Value::Fan(value)) => match value.speed {
            EnumValue::Known(state::FanSpeed::Unspecified) => "Speed not provided",
            EnumValue::Known(state::FanSpeed::Off) => "Off",
            EnumValue::Known(state::FanSpeed::Low) => "Low",
            EnumValue::Known(state::FanSpeed::Medium) => "Medium",
            EnumValue::Known(state::FanSpeed::MediumHigh) => "Medium high",
            EnumValue::Known(state::FanSpeed::High) => "High",
            EnumValue::Unknown(_) => "Unsupported speed",
        }
        .into(),
        Some(Value::Climate(value)) => {
            let mut parts = Vec::new();
            if let Some(temperature) = value.current_temperature_celsius {
                parts.push(format!("{} °C", number(temperature)));
            }
            match value.setpoint.as_ref() {
                Some(state::climate_value::Setpoint::SetpointCelsius(value)) => {
                    parts.push(format!("Setpoint {} °C", number(*value)))
                }
                Some(state::climate_value::Setpoint::SetpointRangeCelsius(range)) => {
                    if let Some(low) = range.low_celsius {
                        parts.push(format!("Minimum {} °C", number(low)));
                    }
                    if let Some(high) = range.high_celsius {
                        parts.push(format!("Maximum {} °C", number(high)));
                    }
                }
                None => {}
            }
            if let Some(humidity) = value.humidity_percent {
                parts.push(format!("{} %", number(humidity)));
            }
            if let Some(active) = value.fan_active {
                parts.push(if active { "Fan active" } else { "Fan inactive" }.into());
            }
            if parts.is_empty() {
                "Climate observation received".into()
            } else {
                parts.join(" · ")
            }
        }
        Some(Value::Weather(value)) => crate::environment_text::weather(value),
        Some(Value::Presence(value)) => crate::environment_text::presence(value),
        Some(Value::PresenceDetails(value)) => crate::environment_text::details(value),
        Some(Value::Helper(value)) => helper_text(value),
        Some(Value::Solar(value)) => {
            let mut parts = Vec::new();
            if let Some(elevation) = value.elevation_degrees {
                parts.push(format!("Elevation {}°", number(elevation)));
            }
            if let Some(azimuth) = value.azimuth_degrees {
                parts.push(format!("Azimuth {}°", number(azimuth)));
            }
            if let Some(rising) = value.rising {
                parts.push(if rising { "Rising" } else { "Falling" }.into());
            }
            if parts.is_empty() {
                "Solar observation received".into()
            } else {
                parts.join(" · ")
            }
        }
        Some(Value::SolarEvent(value)) => match value
            .occurrence
            .as_option()
            .and_then(|value| value.occurrence.as_ref())
        {
            Some(home_wire_contract::home::solar::v1::solar_occurrence::Occurrence::At(_)) => {
                "Calculated event"
            }
            Some(home_wire_contract::home::solar::v1::solar_occurrence::Occurrence::Absent(_)) => {
                "Event missing from calculation"
            }
            None => "Event not provided",
        }
        .into(),
        None => if observation.__buffa_unknown_fields.is_empty() {
            "Value not provided"
        } else {
            "Unsupported value"
        }
        .into(),
    }
}

pub(crate) fn quality_text(observation: &state::ObservedState) -> String {
    let Some(quality) = observation.quality.as_option() else {
        return "Quality missing".into();
    };
    if !quality.__buffa_unknown_fields.is_empty() {
        return "Unsupported quality".into();
    }
    let availability = match quality.availability {
        EnumValue::Known(state::Availability::Unspecified) => "Availability missing",
        EnumValue::Known(state::Availability::Available) => "Available",
        EnumValue::Known(state::Availability::Unknown) => "State unknown",
        EnumValue::Known(state::Availability::Unavailable) => "Unavailable",
        EnumValue::Unknown(_) => "Availability unrecognized",
    };
    let freshness = match quality.freshness {
        EnumValue::Known(state::Freshness::Unspecified) => "Freshness missing",
        EnumValue::Known(state::Freshness::Fresh) => "Up to date",
        EnumValue::Known(state::Freshness::Stale) => "Dated",
        EnumValue::Unknown(_) => "Freshness unrecognized",
    };
    let provenance = match quality.provenance {
        EnumValue::Known(state::Provenance::Unspecified) => "Origin not provided",
        EnumValue::Known(state::Provenance::Observed) => "Observed",
        EnumValue::Known(state::Provenance::Restored) => "Restored",
        EnumValue::Known(state::Provenance::Estimated) => "Estimated",
        EnumValue::Unknown(_) => "Unsupported origin",
    };
    format!("{availability} · {freshness} · {provenance}")
}

#[derive(Default)]
struct RowStore {
    generation: Option<ProjectionGeneration>,
    total: usize,
    rows: BTreeMap<usize, (Row, TableRow)>,
    revision: i32,
    observations: BTreeMap<i32, EntityId>,
    layout: Option<Layout>,
}
#[derive(Default)]
pub(crate) struct CacheRows {
    state: RefCell<RowStore>,
    notify: ModelNotify,
    serial: Cell<i32>,
    home_serial: Cell<i32>,
    profile_zone: Cell<crate::environment_view::ProfileZone>,
    rooms: std::rc::Rc<slint::VecModel<ProRoomRow>>,
    observations: std::rc::Rc<slint::VecModel<ProObservationRow>>,
    devices: std::rc::Rc<slint::VecModel<crate::ProDeviceRow>>,
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
        self.home_layout_nodes.set_vec(Vec::new());
        self.rooms_layout_nodes.set_vec(Vec::new());
        self.room_layout_nodes.set_vec(Vec::new());
        self.popup_layout_nodes.set_vec(Vec::new());
        self.widget_bindings.borrow_mut().clear();
        *self.focused_widget_item.borrow_mut() = None;
        *self.widget_focus_mapping.borrow_mut() = None;
    }
    pub(crate) fn resolve(&self, token: i32, generation: i32, revision: i32) -> Option<EntityId> {
        if generation != self.serial.get() || self.state.borrow().revision != revision {
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
        if generation != self.home_serial.get() || self.state.borrow().layout.as_ref() != layout {
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
    ) -> Option<EntityId> {
        if generation != self.home_serial.get() || self.state.borrow().revision != revision {
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
    let model = std::rc::Rc::new(CacheRows::default());
    window.set_home_rooms(ModelRc::from(model.rooms.clone()));
    window.set_home_observations(ModelRc::from(model.observations.clone()));
    window.set_device_cards(ModelRc::from(model.devices.clone()));
    window.set_home_layout_nodes(ModelRc::from(model.home_layout_nodes.clone()));
    window.set_rooms_layout_nodes(ModelRc::from(model.rooms_layout_nodes.clone()));
    window.set_room_layout_nodes(ModelRc::from(model.room_layout_nodes.clone()));
    window.set_popup_layout_nodes(ModelRc::from(model.popup_layout_nodes.clone()));
    crate::dashboard_presentation::bind(window);
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
    window.set_commands_ready(false);
    window.set_connection_status(ConnectionStatus::Idle);
    window.set_has_configured_layout(false);
    window.set_action_feedback("".into());
    window.set_dashboard_title("Home".into());
    window.set_dashboard_synthetic(false);
    window.set_dashboard_status("Dashboard unavailable".into());
    window.set_home_focus(crate::FocusRequest::default());
    window.invoke_native_route_changed();
    let model = window.get_cache_rows();
    if let Some(model) = model.as_any().downcast_ref::<CacheRows>() {
        model.profile_zone.set(Default::default());
        model.clear();
        window.set_cache_generation(model.serial.get());
        window.set_home_generation(model.home_serial.get());
    } else {
        window.set_device_cards(Default::default());
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

pub(crate) fn render(
    window: &HomewardWindow,
    generation: ProjectionGeneration,
    mut projection: CacheProjection,
) {
    // This ticket has already crossed the cache owner's publication barrier.
    // Authoritative absence must clear the old private view even when a newer
    // local search has made the presentation revision obsolete.
    if !projection.home.captured && window.get_view_loaded() {
        clear_state(window, false);
        window.set_read_status(cache_status(projection.phase, 0).into());
        window.set_connection_status(connection_status(projection.phase));
        return;
    }
    if projection.revision != window.get_projection_version() {
        return;
    }
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
    window.set_dashboard_synthetic(
        home.layout
            .as_ref()
            .is_some_and(|layout| layout.mode == dashboard::ServingMode::Synthetic),
    );
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
                    icon: window.global::<KitGlyphs>().invoke_home(),
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
            icon: match item.icon {
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
        sync(
            &model.devices,
            std::mem::take(&mut projection.devices.rows)
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
    if !window.get_view_loaded()
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
        crate::ProRecipeGesture::Tap,
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
        CachePhase::Loading | CachePhase::Resyncing => ConnectionStatus::Connecting,
        CachePhase::Live => ConnectionStatus::Live,
        CachePhase::Offline => ConnectionStatus::Offline,
        CachePhase::Blocked | CachePhase::Invalid => ConnectionStatus::Blocked,
    }
}
fn cache_status(phase: CachePhase, total: usize) -> String {
    let label = match phase {
        CachePhase::Idle => "",
        CachePhase::Loading => "Loading states…",
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
    use buffa::Message;
    use buffa_types::google::protobuf::Timestamp;
    use home_client_cache::{Filter, ProjectionMailbox, SnapshotCapture};

    fn view(count: usize) -> CachedView {
        view_with(count, |index| state::ViewEntity {
            entity_id: format!("private-entity-{index:04}"),
            current_alias: format!("Élément {index}"),
            ..Default::default()
        })
    }

    fn view_with(count: usize, entity: impl Fn(usize) -> state::ViewEntity) -> CachedView {
        let filter = Filter::all();
        let cursor = state::ViewCursor {
            epoch: "private-epoch-not-a-label".into(),
            config_revision: 1,
            authorization_revision: 1,
            global_sequence: 10,
            filter_sha256: filter.digest().to_vec(),
            config_sha256: vec![1; 32],
            grant_sha256: vec![2; 32],
            admission_proof: vec![3; 40],
            ..Default::default()
        };
        let page = |start: usize, end: usize| state::OpenViewResponse {
            snapshot_token: "private-snapshot-not-a-label".into(),
            cursor: cursor.clone().into(),
            expires_at: Timestamp::from_unix_secs(60).into(),
            next_page_token: if end == count {
                String::new()
            } else {
                format!("page-{end}")
            },
            entities: (start..end).map(&entity).collect(),
            ..Default::default()
        };
        let now = Timestamp::from_unix_secs(0);
        let mut capture =
            SnapshotCapture::begin(filter, 256, page(0, count.min(256)), &now).unwrap();
        for start in (256..count).step_by(256) {
            capture
                .push(page(start, (start + 256).min(count)), &now)
                .unwrap();
        }
        capture.finish(&now).unwrap()
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
        let fixture: serde_json::Value =
            serde_json::from_str(include_str!("../tests/fixtures/overview.json")).unwrap();
        let entities = fixture["entities"].as_array().unwrap();
        view_with(entities.len(), |index| state::ViewEntity {
            entity_id: entities[index]["id"].as_str().unwrap().into(),
            current_alias: entities[index]["alias"].as_str().unwrap().into(),
            state: state::ObservedState {
                entity: home_wire_contract::home::registry::v1::EntityRef {
                    reference: Some(
                        home_wire_contract::home::registry::v1::entity_ref::Reference::EntityId(
                            entities[index]["id"].as_str().unwrap().into(),
                        ),
                    ),
                    ..Default::default()
                }
                .into(),
                revision: 1,
                value: Some(state::observed_state::Value::SwitchOn(false)),
                observed_at: Timestamp::from_unix_secs(10).into(),
                ..Default::default()
            }
            .into(),
            ..Default::default()
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
            dashboard: home_client_model::dashboard::DashboardId::parse("exact-dashboard").unwrap(),
            node: NodeId::parse("exact-configured-popup").unwrap(),
            presentation: Presentation::Popup,
        };
        let mut room = widget_node(Content::Room {
            room: dashboard::RoomId::parse("exact-room-id").unwrap(),
            temperature: None,
            activity: Vec::new(),
            active_states: Vec::new(),
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
            version: 3,
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
            &[(71, 83, crate::ProRecipeGesture::Tap)]
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
            let id = format!("temperature-{index}");
            state::ViewEntity {
                entity_id: id.clone(),
                current_alias: format!("Thermostat {index}"),
                state: state::ObservedState {
                    entity: home_wire_contract::home::registry::v1::EntityRef {
                        reference: Some(
                            home_wire_contract::home::registry::v1::entity_ref::Reference::EntityId(
                                id,
                            ),
                        ),
                        ..Default::default()
                    }
                    .into(),
                    revision: 1,
                    quality: state::Quality {
                        availability: state::Availability::Available.into(),
                        freshness: state::Freshness::Fresh.into(),
                        provenance: state::Provenance::Observed.into(),
                        ..Default::default()
                    }
                    .into(),
                    value: Some(state::observed_state::Value::Climate(Box::new(
                        state::ClimateValue {
                            current_temperature_celsius: Some(if index == 0 { 99. } else { 0. }),
                            ..Default::default()
                        },
                    ))),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }
        };
        let layout = dashboard::fixture();
        let node = widget_node(Content::Room {
            room: layout.rooms[0].id.clone(),
            temperature: Some(Field {
                entity: "temperature-1".parse().unwrap(),
                key: FieldKey::CurrentTemperature,
            }),
            activity: Vec::new(),
            active_states: Vec::new(),
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
                entity
                    .state
                    .as_option_mut()
                    .unwrap()
                    .quality
                    .as_option_mut()
                    .unwrap()
                    .freshness = state::Freshness::Stale.into();
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
            entities: vec!["security-1".parse().unwrap(), "security-0".parse().unwrap()],
        });
        let source = |index: usize, stale: bool| {
            let id = format!("security-{index}");
            state::ViewEntity {
                entity_id: id.clone(),
                current_alias: format!("Source {index}"),
                state: state::ObservedState {
                    entity: home_wire_contract::home::registry::v1::EntityRef {
                        reference: Some(
                            home_wire_contract::home::registry::v1::entity_ref::Reference::EntityId(
                                id,
                            ),
                        ),
                        ..Default::default()
                    }
                    .into(),
                    revision: 1,
                    quality: state::Quality {
                        availability: state::Availability::Available.into(),
                        freshness: if stale {
                            state::Freshness::Stale
                        } else {
                            state::Freshness::Fresh
                        }
                        .into(),
                        provenance: state::Provenance::Observed.into(),
                        ..Default::default()
                    }
                    .into(),
                    value: Some(state::observed_state::Value::SwitchOn(index == 1)),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }
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
            entity: "security-1".parse().unwrap(),
            key: crate::dashboard_widgets::FieldKey::State,
        };
        assert_eq!(field.reading(Some(&full)).as_deref(), Some("on"));
        assert_eq!(
            crate::dashboard_visibility::literal(full.get(&field.entity).unwrap()),
            Some("on"),
            "the configured state field retains its exact literal"
        );
        let light = view_with(1, |index| {
            let mut entity = source(index, false);
            entity.state.as_option_mut().unwrap().value = Some(
                state::observed_state::Value::Light(Box::new(state::LightValue {
                    on: Some(true),
                    brightness_percent: Some(42.),
                    ..Default::default()
                })),
            );
            entity
        });
        let field = crate::dashboard_widgets::Field {
            entity: "security-0".parse().unwrap(),
            key: crate::dashboard_widgets::FieldKey::State,
        };
        assert_eq!(
            field.reading(Some(&light)).as_deref(),
            Some("on"),
            "a state binding must not include unselected brightness or other value properties"
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
        let entity: EntityId = "climate.configured".parse().unwrap();
        let node = widget_node(Content::Thermostat {
            detailed: false,
            summary_only: false,
            entity: entity.clone(),
            current: Field {
                entity: entity.clone(),
                key: FieldKey::CurrentTemperature,
            },
            target: Field {
                entity,
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
        wire.metadata.as_option_mut().unwrap().profile =
            home_client_model::dashboard::Profile::Widgets.name().into();
        wire.native_layout = home_wire_contract::home::dashboard::v1::NativeLayout {
            version: 3,
            ..Default::default()
        }
        .into();
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
                entity: "condition".parse().unwrap(),
                expected: "on".into(),
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
        let mut layout = dashboard::fixture();
        layout.native = Some(NativeLayout {
            version: 3,
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
            view_with(1, |_| state::ViewEntity {
                entity_id: "condition".into(),
                current_alias: "Condition du serveur".into(),
                state: state::ObservedState {
                    entity: home_wire_contract::home::registry::v1::EntityRef {
                        reference: Some(
                            home_wire_contract::home::registry::v1::entity_ref::Reference::EntityId(
                                "condition".into(),
                            ),
                        ),
                        ..Default::default()
                    }
                    .into(),
                    revision,
                    quality: state::Quality {
                        availability: state::Availability::Available.into(),
                        freshness: state::Freshness::Fresh.into(),
                        provenance: state::Provenance::Observed.into(),
                        ..Default::default()
                    }
                    .into(),
                    value: Some(state::observed_state::Value::SwitchOn(on)),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            })
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
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(dashboard::fixture())),
            query: "main".into(),
            detail: Some("private-entity-0001".parse().unwrap()),
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
        let weather = projection
            .home
            .observations
            .iter()
            .find(|item| item.token == dashboard::OBSERVATION_TOKEN_BASE + 1)
            .unwrap();
        assert!(weather.id.is_none());
        assert_eq!(weather.value, "No observation");
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
        let replacement = "private-replacement".parse::<EntityId>().unwrap();
        replaced.rows[0].id = Some(replacement.clone());
        model.apply(generation, replaced);
        assert!(model.resolve(0, first, 0).is_none());
        assert_eq!(model.resolve(0, model.serial.get(), 0), Some(replacement));
        let previous = model.home_serial.get();
        let mut changed_favorite = project(CachePhase::Live, Some(&view), Viewport::default());
        changed_favorite.home.observations[0].id = Some("private-other-favorite".parse().unwrap());
        model.apply(generation, changed_favorite);
        assert!(
            model
                .resolve_observation(dashboard::OBSERVATION_TOKEN_BASE, previous, 0)
                .is_none()
        );
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
        let request = std::sync::Arc::new(std::sync::Mutex::new(ProjectionRequest {
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(dashboard::fixture())),
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
            project(CachePhase::Live, Some(&home_view()), Viewport::default()),
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
        let request = std::sync::Arc::new(std::sync::Mutex::new(ProjectionRequest {
            dashboard: dashboard::LayoutState::Ready(std::sync::Arc::new(dashboard::fixture())),
            ..Default::default()
        }));
        bind(&window);
        crate::navigation::bind(&window, request.clone(), || {});
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_show_endpoint(false);
        window.show().unwrap();
        let view = home_view();
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
        let current = request.lock().unwrap().clone();
        render(
            &window,
            generation,
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &current),
        );
        window.invoke_observation_open(
            dashboard::OBSERVATION_TOKEN_BASE,
            window.get_home_generation(),
        );
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
        let mut layout = dashboard::fixture();
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
        layout.document_revision += 1;
        layout.document_hash = [9; 32];
        layout.rooms[0].entities =
            dashboard::RoomEntities::new(vec!["private-entity-0001".parse().unwrap()]).unwrap();
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
        layout.authorization_revision += 1;
        request.dashboard = dashboard::LayoutState::Ready(std::sync::Arc::new(layout.clone()));
        let unauthorized_cohort =
            project_request(CachePhase::Live, Some(&view), Viewport::default(), &request);
        assert!(
            unauthorized_cohort.home.layout.is_none()
                && unauthorized_cohort.home.observations.is_empty()
        );
        layout.authorization_revision -= 1;
        layout.config_revision += 1;
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
        use state::observed_state::Value;
        let mut observation = state::ObservedState {
            value: Some(Value::SwitchOn(false)),
            ..Default::default()
        };
        assert_eq!(value_text(&observation), "Off");
        observation.value = Some(Value::Light(Box::new(state::LightValue {
            on: Some(false),
            color_temp_mireds: Some(334),
            ..Default::default()
        })));
        assert_eq!(value_text(&observation), "Off · ≈ 2994.012 K · 334 mireds");
        for color_temp_mireds in [None, Some(0), Some(1001)] {
            observation.value = Some(Value::Light(Box::new(state::LightValue {
                color_temp_mireds,
                ..Default::default()
            })));
            assert_eq!(value_text(&observation), "Value not provided");
        }
        observation.value = Some(Value::SwitchOn(false));
        let mut extended = observation.encode_to_vec();
        extended.extend_from_slice(&[0x9a, 0x06, 0x02, 0x08, 0x01]);
        let extended = state::ObservedState::decode_from_slice(&extended).unwrap();
        assert_eq!(value_text(&extended), "Unsupported value");
        observation.value = Some(Value::Measurement(Box::new(state::Measurement {
            value: 0.0,
            unit: "W".into(),
            ..Default::default()
        })));
        assert_eq!(value_text(&observation), "0 W");
        observation.value = Some(Value::Measurement(Box::new(state::Measurement {
            value: 0.0,
            unit: "CAD".into(),
            decimal_value: Some("0.0100".into()),
            ..Default::default()
        })));
        assert_eq!(value_text(&observation), "0.0100 CAD");
        observation.value = Some(Value::Light(Box::new(state::LightValue {
            on: Some(false),
            brightness_percent: Some(0.0),
            ..Default::default()
        })));
        assert_eq!(value_text(&observation), "Off · 0 %");
        observation.value = Some(Value::Light(Box::new(state::LightValue {
            on: Some(false),
            ..Default::default()
        })));
        assert_eq!(value_text(&observation), "Off");
        observation.value = None;
        assert_eq!(value_text(&observation), "Value not provided");
        let mut bytes = observation.encode_to_vec();
        bytes.extend_from_slice(&[0x9a, 0x06, 0x02, 0x08, 0x01]);
        let future = state::ObservedState::decode_from_slice(&bytes).unwrap();
        assert_eq!(value_text(&future), "Unsupported value");
        let climate = state::ObservedState {
            value: Some(Value::Climate(Box::new(state::ClimateValue {
                current_temperature_celsius: Some(21.5),
                ..Default::default()
            }))),
            ..Default::default()
        };
        assert_eq!(summary_text(&climate), "21.5 °C");
        let mut future_climate = climate.encode_to_vec();
        future_climate.extend_from_slice(&[0x9a, 0x06, 0x02, 0x08, 0x01]);
        let future_climate = state::ObservedState::decode_from_slice(&future_climate).unwrap();
        assert_eq!(summary_text(&future_climate), "Unsupported value");
    }

    #[test]
    fn qualities_are_visible_and_long_unicode_values_stay_bounded() {
        let mut observation = state::ObservedState {
            quality: state::Quality {
                availability: state::Availability::Available.into(),
                freshness: state::Freshness::Stale.into(),
                provenance: state::Provenance::Restored.into(),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        assert_eq!(quality_text(&observation), "Available · Dated · Restored");
        observation.quality.as_option_mut().unwrap().provenance =
            state::Provenance::Estimated.into();
        assert_eq!(quality_text(&observation), "Available · Dated · Estimated");
        observation.quality.as_option_mut().unwrap().availability = EnumValue::Unknown(99);
        assert!(quality_text(&observation).starts_with("Availability unrecognized"));
        let text = "🪟".repeat(4096);
        let clipped = helper_text(&helpers::HelperValue {
            value: Some(helpers::helper_value::Value::Text(text)),
            ..Default::default()
        });
        assert_eq!(clipped.chars().count(), 257);
        assert!(clipped.ends_with('…'));
        assert_eq!(
            helper_text(&helpers::HelperValue {
                value: Some(helpers::helper_value::Value::Text(String::new())),
                ..Default::default()
            }),
            "Empty text"
        );
        assert_eq!(
            helper_text(&helpers::HelperValue::default()),
            "Value not provided"
        );
    }
    #[test]
    fn native_layout_models_survive_switching_and_private_clear_without_losing_their_binding() {
        use home_wire_contract::home::dashboard::v1 as wire;
        use slint::ComponentHandle;
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        window.set_show_endpoint(false);
        window.set_access_stage(crate::ProAccessStage::Connected);
        let node =
            |id: &str, kind: wire::LayoutKind, children: &[&str], title: &str| wire::LayoutNode {
                id: id.into(),
                kind: kind.into(),
                title: title.into(),
                children: children.iter().map(|id| (*id).into()).collect(),
                source: wire::LayoutSource {
                    file: "synthetic.yaml".into(),
                    path: id.into(),
                    variant: "synthetic".into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            };
        let mut layout = dashboard::fixture();
        let fixture: serde_json::Value =
            serde_json::from_str(include_str!("../tests/fixtures/overview.json")).unwrap();
        let mut room_node = node("room", wire::LayoutKind::View, &["room-section"], "Room");
        room_node.route = fixture["rooms"][0]["id"].as_str().unwrap().into();
        layout.native = Some(
            crate::dashboard_layout::NativeLayout::try_from(&wire::NativeLayout {
                version: 1,
                views: vec!["view".into(), "room".into()],
                nodes: vec![
                    node("view", wire::LayoutKind::View, &["section"], "Home"),
                    node(
                        "section",
                        wire::LayoutKind::Section,
                        &["heading", "unknown"],
                        "",
                    ),
                    node("heading", wire::LayoutKind::Heading, &[], "Server heading"),
                    node(
                        "unknown",
                        wire::LayoutKind::Unsupported,
                        &[],
                        "Unknown custom card",
                    ),
                    room_node,
                    node(
                        "room-section",
                        wire::LayoutKind::Section,
                        &["room-heading"],
                        "",
                    ),
                    node(
                        "room-heading",
                        wire::LayoutKind::Heading,
                        &[],
                        "Room heading",
                    ),
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
        let cache = home_view();
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
}
