//! Product projection: private identities/cursors stay in the Rust cache. Slint
//! keeps the inventory virtualized at 64 rows. Configured dashboard forests
//! are separately bounded by their negotiated profile.
use crate::{
    HomewardWindow, IconKind, ProObservationRow, ProRoomRow, TableRow,
    dashboard::{self, Icon, Layout, ProjectionRequest},
};
use buffa::EnumValue;
use home_client_cache::{CacheEvent, CachePhase, CachedView, ProjectionGeneration, Viewport};
use home_client_model::EntityId;
use home_wire_contract::home::{helpers::v1 as helpers, state::v1 as state};
use slint::{Model, ModelNotify, ModelRc, ModelTracker, SharedString};
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
    revision: i32,
    phase: CachePhase,
    total: usize,
    rows: Vec<Row>,
    home: HomeProjection,
    devices: crate::device_view::DeviceProjection,
    board: crate::dashboard_presentation::Board,
    profile_zone: crate::environment_view::ProfileZone,
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
                        "Élément sans nom".into()
                    } else {
                        bounded(&entity.current_alias)
                    };
                    let (value, quality) = match entity.state.as_option() {
                        Some(observation) => (value_text(observation), quality_text(observation)),
                        None => ("Aucune observation".into(), "Non renseignée".into()),
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
                        .unwrap_or_else(|| "Aucune observation".into());
                    Observation {
                        token: dashboard::OBSERVATION_TOKEN_BASE + index as i32,
                        label: item.label.clone(),
                        icon: item.icon,
                        id: entity.map(|_| item.entity.clone()),
                        value: bounded(&value),
                        description: entity
                            .and_then(|entity| entity.state.as_option())
                            .map(quality_text)
                            .unwrap_or_else(|| {
                                "Aucune observation autorisée pour cet élément.".into()
                            }),
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
                        .unwrap_or_else(|| "Date non renseignée".into()),
                ),
                None => (
                    "Aucune observation".into(),
                    "Non renseignée".into(),
                    "Date non renseignée".into(),
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
    let board = crate::dashboard_presentation::Board::project(layout, request, &devices);
    CacheProjection {
        devices,
        board,
        profile_zone,
        revision: request.revision,
        phase,
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
    if value { "Allumé" } else { "Éteint" }
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
        "Valeur non prise en charge".into()
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
                "Texte vide".into()
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
            (None, _) | (_, None) => "Date ou heure absente".into(),
        },
        Some(Value::ButtonPressedAt(_)) => "Activation observée".into(),
        None => "Valeur absente".into(),
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
        return "Valeur non prise en charge".into();
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
                "Valeur absente".into()
            } else {
                parts.join(" · ")
            }
        }
        Some(Value::Fan(value)) => match value.speed {
            EnumValue::Known(state::FanSpeed::Unspecified) => "Vitesse absente",
            EnumValue::Known(state::FanSpeed::Off) => "Arrêté",
            EnumValue::Known(state::FanSpeed::Low) => "Vitesse faible",
            EnumValue::Known(state::FanSpeed::Medium) => "Vitesse moyenne",
            EnumValue::Known(state::FanSpeed::MediumHigh) => "Vitesse moyenne haute",
            EnumValue::Known(state::FanSpeed::High) => "Vitesse haute",
            EnumValue::Unknown(_) => "Vitesse non reconnue",
        }
        .into(),
        Some(Value::Climate(value)) => {
            let mut parts = Vec::new();
            if let Some(temperature) = value.current_temperature_celsius {
                parts.push(format!("{} °C", number(temperature)));
            }
            match value.setpoint.as_ref() {
                Some(state::climate_value::Setpoint::SetpointCelsius(value)) => {
                    parts.push(format!("Cible {} °C", number(*value)))
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
                parts.push(
                    if active {
                        "Ventilation active"
                    } else {
                        "Ventilation inactive"
                    }
                    .into(),
                );
            }
            if parts.is_empty() {
                "Observation climatique reçue".into()
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
                parts.push(format!("Élévation {}°", number(elevation)));
            }
            if let Some(azimuth) = value.azimuth_degrees {
                parts.push(format!("Azimut {}°", number(azimuth)));
            }
            if let Some(rising) = value.rising {
                parts.push(if rising { "Ascendant" } else { "Descendant" }.into());
            }
            if parts.is_empty() {
                "Observation solaire reçue".into()
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
                "Événement calculé"
            }
            Some(home_wire_contract::home::solar::v1::solar_occurrence::Occurrence::Absent(_)) => {
                "Événement absent du calcul"
            }
            None => "Événement non renseigné",
        }
        .into(),
        None => if observation.__buffa_unknown_fields.is_empty() {
            "Valeur absente"
        } else {
            "Valeur non prise en charge"
        }
        .into(),
    }
}

pub(crate) fn quality_text(observation: &state::ObservedState) -> String {
    let Some(quality) = observation.quality.as_option() else {
        return "Qualité absente".into();
    };
    if !quality.__buffa_unknown_fields.is_empty() {
        return "Qualité non prise en charge".into();
    }
    let availability = match quality.availability {
        EnumValue::Known(state::Availability::Unspecified) => "Disponibilité absente",
        EnumValue::Known(state::Availability::Available) => "Disponible",
        EnumValue::Known(state::Availability::Unknown) => "État inconnu",
        EnumValue::Known(state::Availability::Unavailable) => "Indisponible",
        EnumValue::Unknown(_) => "Disponibilité non reconnue",
    };
    let freshness = match quality.freshness {
        EnumValue::Known(state::Freshness::Unspecified) => "Fraîcheur absente",
        EnumValue::Known(state::Freshness::Fresh) => "À jour",
        EnumValue::Known(state::Freshness::Stale) => "Daté",
        EnumValue::Unknown(_) => "Fraîcheur non reconnue",
    };
    let provenance = match quality.provenance {
        EnumValue::Known(state::Provenance::Unspecified) => "Origine absente",
        EnumValue::Known(state::Provenance::Observed) => "Observé",
        EnumValue::Known(state::Provenance::Restored) => "Restauré",
        EnumValue::Known(state::Provenance::Estimated) => "Estimé",
        EnumValue::Unknown(_) => "Origine non reconnue",
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
    room_layout_nodes: std::rc::Rc<slint::VecModel<crate::ProLayoutNode>>,
    popup_layout_nodes: std::rc::Rc<slint::VecModel<crate::ProLayoutNode>>,
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
        self.room_layout_nodes.set_vec(Vec::new());
        self.popup_layout_nodes.set_vec(Vec::new());
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
    window.set_room_layout_nodes(ModelRc::from(model.room_layout_nodes.clone()));
    window.set_popup_layout_nodes(ModelRc::from(model.popup_layout_nodes.clone()));
    crate::dashboard_presentation::bind(window);
    window.set_cache_rows(ModelRc::from(model));
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
                            cells: ["Chargement…".into(), String::new(), String::new()],
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
    window.set_view_loaded(false);
    window.set_commands_ready(false);
    window.set_action_feedback("".into());
    window.set_dashboard_title("Accueil".into());
    window.set_dashboard_synthetic(false);
    window.set_dashboard_status("Dashboard indisponible".into());
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
    refresh_clock(window);
}

pub(crate) fn refresh_clock(window: &HomewardWindow) {
    let rows = window.get_cache_rows();
    let zone = rows
        .as_any()
        .downcast_ref::<CacheRows>()
        .map(|model| model.profile_zone.get())
        .unwrap_or_default();
    let (clock, date) = zone.clock(crate::environment_view::now());
    window.set_local_clock(clock.into());
    window.set_local_date(date.into());
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
        return;
    }
    if projection.revision != window.get_projection_version() {
        return;
    }
    let revision = projection.revision;
    let home = &projection.home;
    window.set_view_loaded(home.captured);
    window.set_dashboard_status(home.status.as_str().into());
    window.set_dashboard_title(
        home.layout
            .as_ref()
            .map_or("Accueil", |layout| layout.title.as_str())
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
                    subtitle: format!("{} éléments observés", home.room_counts[index]).into(),
                    icon: IconKind::Home,
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
        window.set_detail_title("Détail indisponible".into());
        window.set_detail_value("Aucune observation autorisée pour cet élément.".into());
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
                Icon::Home => IconKind::Home,
                Icon::Information => IconKind::Information,
                Icon::Clock => IconKind::Clock,
                Icon::Grid => IconKind::Grid,
            },
            available: item.id.is_some(),
        })
        .collect::<Vec<_>>();
    let total = projection.total;
    if total == 0 {
        window.set_cache_active_index(-1);
    }
    window.set_read_status(cache_status(projection.phase, total).into());
    let model = window.get_cache_rows();
    if let Some(model) = model.as_any().downcast_ref::<CacheRows>() {
        window.set_commands_ready(projection.devices.ready);
        match projection.board.target() {
            crate::dashboard_presentation::BoardTarget::Clear => {
                sync(&model.home_layout_nodes, Vec::new());
                sync(&model.room_layout_nodes, Vec::new());
                sync(&model.popup_layout_nodes, Vec::new());
            }
            crate::dashboard_presentation::BoardTarget::Retain => {}
            crate::dashboard_presentation::BoardTarget::Home => sync(
                &model.home_layout_nodes,
                projection.board.rows(&model.home_layout_nodes),
            ),
            crate::dashboard_presentation::BoardTarget::Room => sync(
                &model.room_layout_nodes,
                projection.board.rows(&model.room_layout_nodes),
            ),
            crate::dashboard_presentation::BoardTarget::Popup => sync(
                &model.popup_layout_nodes,
                projection.board.rows(&model.popup_layout_nodes),
            ),
        }
        sync(
            &model.devices,
            std::mem::take(&mut projection.devices.rows)
                .iter()
                .enumerate()
                .map(|(index, row)| row.ui(model.devices.row_data(index).as_ref()))
                .collect(),
        );
        model.apply(generation, projection);
        window.set_cache_generation(model.serial.get());
        window.set_home_generation(model.home_serial.get());
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

fn cache_status(phase: CachePhase, total: usize) -> String {
    let label = match phase {
        CachePhase::Idle => "",
        CachePhase::Loading => "Chargement des états…",
        CachePhase::Live => {
            if total == 0 {
                "Aucun élément autorisé dans cette vue."
            } else {
                "États à jour"
            }
        }
        CachePhase::Offline => "Hors ligne · dernières données reçues",
        CachePhase::Resyncing => "Actualisation de la vue autorisée…",
        CachePhase::Suspended => "Lecture suspendue",
        CachePhase::Blocked => "Lecture indisponible · réessayez ou reconnectez-vous",
        CachePhase::Invalid => "La réponse du serveur ne permet pas d’afficher une vue cohérente.",
        CachePhase::Closed => "",
    };
    if total > 0 {
        format!(
            "{label} · {total} {}",
            if total == 1 { "entité" } else { "entités" }
        )
    } else {
        label.into()
    }
}

pub(crate) fn event_message(event: CacheEvent) -> &'static str {
    match event {
        CacheEvent::Transport(_) => "Dernier incident : la lecture réseau a été interrompue.",
        CacheEvent::Session(_) => {
            "Dernier incident : la session ne permet plus de poursuivre la lecture."
        }
        CacheEvent::Contract(_) => {
            "Dernier incident : une réponse ne respecte pas le contrat de lecture."
        }
        CacheEvent::Resync(_) => "La vue a été invalidée ; une capture complète est nécessaire.",
        CacheEvent::StreamEnded => "Dernier incident : le flux de lecture a été fermé.",
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
        assert_eq!(unloaded.cells.row_data(0).unwrap().as_str(), "Chargement…");
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
        assert_eq!(detail[1], "Éteint");
        assert_eq!(detail[3], "1970-01-01T00:00:10+00:00");
        let weather = projection
            .home
            .observations
            .iter()
            .find(|item| item.token == dashboard::OBSERVATION_TOKEN_BASE + 1)
            .unwrap();
        assert!(weather.id.is_none());
        assert_eq!(weather.value, "Aucune observation");
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
        updated.rows[0].cells[1] = "Allumé".into();
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
        let window = HomewardWindow::new().unwrap();
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
            .match_predicate(|item| {
                item.accessible_label().as_deref() == Some("Liste complète des éléments")
            })
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
        let window = HomewardWindow::new().unwrap();
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
        window.invoke_room_open(i32::MIN, 1);
        assert_eq!(window.get_page(), crate::ViewPage::Home);
        window.invoke_room_open(dashboard::ROOM_TOKEN_BASE, 99);
        assert_eq!(window.get_page(), crate::ViewPage::Home);
        window.invoke_room_open(dashboard::ROOM_TOKEN_BASE, window.get_home_generation());
        assert_eq!(window.get_page(), crate::ViewPage::Room);
        assert!(window.invoke_system_back());
        assert_eq!(window.get_page(), crate::ViewPage::Home);
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
        assert!(!window.invoke_system_back());
        window.invoke_choose_room();
        assert_eq!(window.get_page(), crate::ViewPage::Rooms);
        window.invoke_go_back();
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
            crate::ViewPage::Rooms,
            "the workspace selector is also a registered restore target"
        );
        window.invoke_go_back();
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
        assert!(window.get_read_status().starts_with("Lecture indisponible"));
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
        assert_eq!(value_text(&observation), "Éteint");
        observation.value = Some(Value::Light(Box::new(state::LightValue {
            on: Some(false),
            color_temp_mireds: Some(334),
            ..Default::default()
        })));
        assert_eq!(
            value_text(&observation),
            "Éteint · ≈ 2994.012 K · 334 mireds"
        );
        for color_temp_mireds in [None, Some(0), Some(1001)] {
            observation.value = Some(Value::Light(Box::new(state::LightValue {
                color_temp_mireds,
                ..Default::default()
            })));
            assert_eq!(value_text(&observation), "Valeur absente");
        }
        observation.value = Some(Value::SwitchOn(false));
        let mut extended = observation.encode_to_vec();
        extended.extend_from_slice(&[0x9a, 0x06, 0x02, 0x08, 0x01]);
        let extended = state::ObservedState::decode_from_slice(&extended).unwrap();
        assert_eq!(value_text(&extended), "Valeur non prise en charge");
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
        assert_eq!(value_text(&observation), "Éteint · 0 %");
        observation.value = Some(Value::Light(Box::new(state::LightValue {
            on: Some(false),
            ..Default::default()
        })));
        assert_eq!(value_text(&observation), "Éteint");
        observation.value = None;
        assert_eq!(value_text(&observation), "Valeur absente");
        let mut bytes = observation.encode_to_vec();
        bytes.extend_from_slice(&[0x9a, 0x06, 0x02, 0x08, 0x01]);
        let future = state::ObservedState::decode_from_slice(&bytes).unwrap();
        assert_eq!(value_text(&future), "Valeur non prise en charge");
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
        assert_eq!(summary_text(&future_climate), "Valeur non prise en charge");
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
        assert_eq!(quality_text(&observation), "Disponible · Daté · Restauré");
        observation.quality.as_option_mut().unwrap().provenance =
            state::Provenance::Estimated.into();
        assert_eq!(quality_text(&observation), "Disponible · Daté · Estimé");
        observation.quality.as_option_mut().unwrap().availability = EnumValue::Unknown(99);
        assert!(quality_text(&observation).starts_with("Disponibilité non reconnue"));
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
            "Texte vide"
        );
        assert_eq!(
            helper_text(&helpers::HelperValue::default()),
            "Valeur absente"
        );
    }
    #[test]
    fn native_layout_models_survive_switching_and_private_clear_without_losing_their_binding() {
        use home_wire_contract::home::dashboard::v1 as wire;
        use slint::ComponentHandle;
        i_slint_backend_testing::init_no_event_loop();
        let window = HomewardWindow::new().unwrap();
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
