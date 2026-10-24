//! Product projection: private identities/cursors stay in the Rust cache. Slint
//! receives at most 64 human-readable rows and scalar virtual-list positions.
use crate::{HomewardWindow, TableRow};
use buffa::EnumValue;
use home_client_cache::{CacheEvent, CachePhase, CachedView, ProjectionGeneration, Viewport};
use home_wire_contract::home::{helpers::v1 as helpers, state::v1 as state};
use slint::{Model, ModelNotify, ModelRc, ModelTracker, SharedString};
use std::{cell::RefCell, collections::BTreeMap};

#[derive(Clone, PartialEq, Eq)]
struct Row {
    index: usize,
    cells: [String; 3],
}

pub(crate) struct CacheProjection {
    phase: CachePhase,
    total: usize,
    rows: Vec<Row>,
}

pub(crate) fn project(
    phase: CachePhase,
    view: Option<&CachedView>,
    viewport: Viewport,
) -> CacheProjection {
    let total = view.map_or(0, CachedView::len);
    let rows = view
        .map(|view| {
            view.entities()
                .enumerate()
                .skip(viewport.first())
                .take(viewport.count())
                .map(|(index, (_, entity))| {
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
                        index,
                        cells: [title, bounded(&value), bounded(&quality)],
                    }
                })
                .collect()
        })
        .unwrap_or_default();
    CacheProjection { phase, total, rows }
}

fn on_off(value: bool) -> &'static str {
    if value { "Allumé" } else { "Éteint" }
}
fn bounded(text: &str) -> String {
    match text.char_indices().nth(256) {
        Some((end, _)) => format!("{}…", &text[..end]),
        None => text.to_owned(),
    }
}
fn number(value: f64) -> String {
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

fn value_text(observation: &state::ObservedState) -> String {
    if !observation.__buffa_unknown_fields.is_empty() {
        return "Valeur non prise en charge".into();
    }
    use state::observed_state::Value;
    match observation.value.as_ref() {
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
            if let Some(mireds) = value.color_temp_mireds {
                parts.push(format!("{mireds} mired"));
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

fn quality_text(observation: &state::ObservedState) -> String {
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
}
#[derive(Default)]
pub(crate) struct CacheRows {
    state: RefCell<RowStore>,
    notify: ModelNotify,
}
fn table_row(row: &Row, offline: bool) -> TableRow {
    TableRow {
        token: row.index as i32,
        cells: ModelRc::from(row.cells.clone().map(SharedString::from)),
        selected: false,
        disabled: offline,
    }
}
impl CacheRows {
    fn apply(&self, generation: ProjectionGeneration, projection: CacheProjection) {
        let mut old = self.state.borrow_mut();
        let reset = old.generation != Some(generation) || old.total != projection.total;
        let mut changed = Vec::new();
        let offline = projection.phase != CachePhase::Live;
        let mut rows = BTreeMap::new();
        for row in projection.rows {
            let index = row.index;
            if old
                .rows
                .get(&index)
                .is_none_or(|(previous, table)| previous != &row || table.disabled != offline)
            {
                changed.push(index);
            }
            let table = table_row(&row, offline);
            rows.insert(index, (row, table));
        }
        *old = RowStore {
            generation: Some(generation),
            total: projection.total,
            rows,
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
        *self.state.borrow_mut() = RowStore::default();
        self.notify.reset();
    }
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
    let model = window.get_cache_rows();
    if let Some(model) = model.as_any().downcast_ref::<CacheRows>() {
        model.clear();
    }
    window.set_cache_active_index(-1);
    window.set_read_status("".into());
    window.set_cache_error("".into());
}

pub(crate) fn render(
    window: &HomewardWindow,
    generation: ProjectionGeneration,
    projection: CacheProjection,
) {
    let total = projection.total;
    if total == 0 {
        window.set_cache_active_index(-1);
    }
    let label = match projection.phase {
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
    let status = if total > 0 {
        format!(
            "{label} · {total} {}",
            if total == 1 { "entité" } else { "entités" }
        )
    } else {
        label.into()
    };
    window.set_read_status(status.into());
    let model = window.get_cache_rows();
    if let Some(model) = model.as_any().downcast_ref::<CacheRows>() {
        model.apply(generation, projection);
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
            entities: (start..end)
                .map(|index| state::ViewEntity {
                    entity_id: format!("private-entity-{index:04}"),
                    current_alias: format!("Élément {index}"),
                    ..Default::default()
                })
                .collect(),
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
        assert!(model.row_data(700).unwrap().disabled);
        model.clear();
        assert_eq!(model.row_count(), 0);
        assert!(model.state.borrow().rows.is_empty());
        assert!(model.row_data(0).is_none());
    }

    #[test]
    fn visible_text_preserves_false_zero_absence_and_decimal_precision() {
        use state::observed_state::Value;
        let mut observation = state::ObservedState {
            value: Some(Value::SwitchOn(false)),
            ..Default::default()
        };
        assert_eq!(value_text(&observation), "Éteint");
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
}
