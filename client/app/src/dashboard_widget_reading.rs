//! Values for exact configured widget bindings. This transferable projection
//! contains no Slint Rc models and never discovers a replacement resource.
use crate::{
    cache_view::{number, value_text},
    dashboard::Layout,
    dashboard_layout::Node,
    dashboard_widgets::{Content, Field, FieldKey},
    device_view::presentation::CardRow,
    environment_view::{Presentation, ProfileZone},
};
use home_client_cache::CachedView;
use home_wire_contract::home::state::v1 as state;

pub(crate) fn clock(
    zone: home_client_model::local_time::DisplayTimeZone,
    now: home_client_model::local_time::UtcInstant,
) -> (String, String) {
    use chrono::Datelike;
    let local = zone.local(now);
    let date = local.date();
    let date = chrono::NaiveDate::parse_from_str(&date, "%Y-%m-%d")
        .map(|date| {
            let days = [
                "Monday",
                "Tuesday",
                "Wednesday",
                "Thursday",
                "Friday",
                "Saturday",
                "Sunday",
            ];
            let months = [
                "January",
                "February",
                "March",
                "April",
                "May",
                "June",
                "July",
                "August",
                "September",
                "October",
                "November",
                "December",
            ];
            format!(
                "{}, {} {}, {}",
                days[date.weekday().num_days_from_monday() as usize],
                months[date.month0() as usize],
                date.day(),
                date.year()
            )
        })
        .unwrap_or(date);
    (local.clock(), date)
}

impl Field {
    pub fn reading(&self, view: Option<&CachedView>) -> Option<String> {
        let entity = view?.get(&self.entity)?;
        let observation = crate::dashboard_visibility::fresh(entity)?;
        observation.value.as_ref()?;
        match self.key {
            FieldKey::State => match observation.value.as_ref()? {
                state::observed_state::Value::Cover(value) => value
                    .state
                    .and_then(home_wire_contract::controls_contract::cover_state)
                    .and_then(crate::control_text::cover_state)
                    .map(|text| text.label.to_owned()),
                state::observed_state::Value::Lock(value) => {
                    Some(crate::control_text::lock(value).to_owned())
                }
                state::observed_state::Value::Alarm(value) => {
                    Some(crate::control_text::alarm(value).to_owned())
                }
                state::observed_state::Value::SwitchOn(_)
                | state::observed_state::Value::Measurement(_)
                | state::observed_state::Value::Light(_)
                | state::observed_state::Value::Fan(_)
                | state::observed_state::Value::Climate(_)
                | state::observed_state::Value::Helper(_)
                | state::observed_state::Value::Solar(_)
                | state::observed_state::Value::SolarEvent(_)
                | state::observed_state::Value::Weather(_)
                | state::observed_state::Value::Presence(_)
                | state::observed_state::Value::PresenceDetails(_) => {
                    crate::dashboard_visibility::literal(entity).map(str::to_owned)
                }
            },
            FieldKey::Value => Some(value_text(observation)),
            FieldKey::CurrentTemperature => {
                if let state::observed_state::Value::Climate(value) = observation.value.as_ref()?
                    && value.__buffa_unknown_fields.is_empty()
                {
                    value
                        .current_temperature_celsius
                        .filter(|v| v.is_finite())
                        .map(number)
                } else {
                    None
                }
            }
            FieldKey::Setpoint => {
                if let state::observed_state::Value::Climate(value) = observation.value.as_ref()?
                    && value.__buffa_unknown_fields.is_empty()
                {
                    match value.setpoint.as_ref()? {
                        state::climate_value::Setpoint::SetpointCelsius(value)
                            if value.is_finite() =>
                        {
                            Some(number(*value))
                        }
                        state::climate_value::Setpoint::SetpointCelsius(_)
                        | state::climate_value::Setpoint::SetpointRangeCelsius(_) => None,
                    }
                } else {
                    None
                }
            }
            FieldKey::Measurement => {
                if let state::observed_state::Value::Measurement(value) =
                    observation.value.as_ref()?
                    && value.__buffa_unknown_fields.is_empty()
                    && value.value.is_finite()
                {
                    Some(value_text(observation))
                } else {
                    None
                }
            }
        }
    }
}
#[derive(Clone, Default)]
pub(crate) struct ObservationReading {
    pub title: String,
    pub value: String,
    pub active: bool,
    pub available: bool,
}
#[derive(Clone, Default)]
pub(crate) struct Reading {
    pub clock: String,
    pub date: String,
    pub weather: Presentation,
    pub room_title: String,
    pub room_subtitle: String,
    pub value: String,
    pub target: Option<String>,
    pub active: bool,
    pub security: Vec<CardRow>,
    pub observations: Vec<ObservationReading>,
}
impl Reading {
    pub fn ui(
        &self,
        node: &Node,
        device: Option<&CardRow>,
        previous: Option<&crate::ProLayoutNode>,
        identity: (i32, i32),
        icon: &crate::IconGlyph,
        can_open: bool,
    ) -> crate::ProDashboardWidget {
        use crate::device_view::presentation::nested;
        use crate::{
            ProDashboardWidget, ProDashboardWidgetKind as K, ProObservationRow, ProRoomRow,
        };
        let (token, generation) = identity;
        let Some(config) = &node.widget else {
            return ProDashboardWidget::default();
        };
        let previous = previous.filter(|old| old.token == token && old.generation == generation);
        let mut ui = previous.map(|old| old.widget.clone()).unwrap_or_default();
        ui.action_label = config.action_label.as_str().into();
        ui.empty_label = config.empty_label.as_str().into();
        ui.can_open = can_open;
        match &config.content {
            Content::ClockWeather { weather, .. } => {
                ui.kind = K::ClockWeather;
                ui.clock = self.clock.as_str().into();
                ui.date = self.date.as_str().into();
                ui.weather = self.weather.ui(None).1;
                ui.has_weather = weather.is_some();
            }
            Content::Room { .. } => {
                ui.kind = K::Room;
                ui.room = ProRoomRow {
                    token,
                    generation,
                    title: self.room_title.as_str().into(),
                    subtitle: self.room_subtitle.as_str().into(),
                    icon: icon.clone(),
                };
                ui.value = self.value.as_str().into();
                ui.active = self.active;
            }
            Content::Thermostat {
                detailed,
                summary_only,
                ..
            } => {
                ui.detailed = *detailed;
                ui.kind = K::Thermostat;
                let mut thermostat = device
                    .map(|device| device.ui(previous.map(|p| &p.widget.thermostat), icon))
                    .unwrap_or_default();
                if !node.title.is_empty() {
                    thermostat.title = node.title.as_str().into();
                }
                thermostat.value = self.value.as_str().into();
                thermostat.summary_only = *summary_only;
                thermostat.unit = "°C".into();
                let field = device.and_then(|device| {
                    let token = device.field_keys.get("setpoint_celsius")?;
                    device.values.iter().find(|field| &field.token == token)
                });
                let mut field = field.cloned().unwrap_or_default();
                // A dirty/pending/invalid editor must never turn its draft into
                // the observed setpoint or issue another step from that draft.
                let canonical = self
                    .target
                    .as_ref()
                    .is_some_and(|target| field.error.is_empty() && field.lower.as_str() == target);
                if !detailed {
                    field.can_step &= canonical;
                    field.lower = self.target.as_deref().unwrap_or("—").into();
                }
                thermostat.readings = self
                    .target
                    .as_ref()
                    .map(|target| format!("Setpoint {target} °C"))
                    .unwrap_or_else(|| "Setpoint unavailable".into())
                    .into();
                ui.thermostat = thermostat;
                ui.field = field;
            }
            Content::Agenda { .. } => {
                ui.kind = K::Agenda;
                if previous.is_none() {
                    ui.data_ready = false;
                    ui.status = "Loading the configured calendar…".into();
                }
            }
            Content::Tasks { shopping_only, .. } => {
                ui.kind = K::Tasks;
                ui.shopping_only = *shopping_only;
                if previous.is_none() {
                    ui.data_ready = false;
                    ui.status = "Loading the configured list…".into();
                    ui.task_access = crate::ProTaskAccess::Offline;
                }
            }
            Content::Security { .. } => {
                ui.kind = K::Security;
                let rows = self
                    .security
                    .iter()
                    .map(|row| row.ui(None, icon))
                    .collect::<Vec<_>>();
                ui.security = nested(&rows, previous.map(|p| &p.widget.security));
            }
            Content::Observations { items } => {
                ui.kind = K::Observations;
                let rows = self
                    .observations
                    .iter()
                    .enumerate()
                    .map(|(index, row)| ProObservationRow {
                        token: index as i32,
                        generation,
                        title: row.title.as_str().into(),
                        value: row.value.as_str().into(),
                        available: row.available,
                        active: row.active,
                        icon: items
                            .get(index)
                            .map(|item| crate::dashboard_icons::glyph(item.icon, icon))
                            .unwrap_or_else(|| icon.clone()),
                        ..Default::default()
                    })
                    .collect::<Vec<_>>();
                ui.item_limit = rows.len() as i32;
                ui.observations = nested(&rows, previous.map(|p| &p.widget.observations));
            }
            Content::Navigation { .. } => {
                ui.kind = K::Navigation;
            }
        }
        ui
    }
    pub fn project(node: &Node, layout: &Layout, view: Option<&CachedView>) -> Self {
        let Some(widget) = &node.widget else {
            return Self::default();
        };
        let mut reading = Self::default();
        match &widget.content {
            Content::ClockWeather { zone, weather } => {
                let now = crate::environment_view::now();
                (reading.clock, reading.date) = clock(*zone, now);
                if let Some(weather) = weather {
                    reading.weather = crate::environment_view::project(
                        crate::device_view::capability::ObservationKind::Weather,
                        view.and_then(|view| view.get(weather))
                            .and_then(crate::dashboard_visibility::fresh),
                        ProfileZone::Known(*zone),
                        now,
                    );
                }
            }
            Content::Room {
                room,
                temperature,
                activity,
                active_states,
            } => {
                reading.room_title = if node.title.is_empty() {
                    layout
                        .room(room)
                        .map(|room| room.label.clone())
                        .unwrap_or_default()
                } else {
                    node.title.clone()
                };
                reading.room_subtitle = node.secondary.clone();
                reading.value = temperature
                    .as_ref()
                    .and_then(|field| field.reading(view))
                    .map(|value| {
                        if temperature
                            .as_ref()
                            .is_some_and(|field| field.key == FieldKey::CurrentTemperature)
                        {
                            format!("{value} °C")
                        } else {
                            value
                        }
                    })
                    .unwrap_or_else(|| {
                        if temperature.is_some() {
                            "—".into()
                        } else {
                            String::new()
                        }
                    });
                reading.active = activity.iter().any(|id| {
                    view.and_then(|view| view.get(id))
                        .and_then(crate::dashboard_visibility::literal)
                        .is_some_and(|value| active_states.iter().any(|state| state == value))
                });
            }
            Content::Thermostat {
                current, target, ..
            } => {
                reading.value = current.reading(view).unwrap_or_else(|| "—".into());
                reading.target = target.reading(view);
            }
            Content::Security { entities } => {
                reading.security = entities
                    .iter()
                    .map(|id| {
                        let entity = view.and_then(|view| view.get(id));
                        let observation = entity.and_then(crate::dashboard_visibility::fresh);
                        // Security is a read-only summary of the configured
                        // entity's canonical observation. Capability rows may
                        // describe several endpoints and cannot select it.
                        CardRow {
                            title: entity
                                .map(|entity| crate::cache_view::bounded(&entity.current_alias))
                                .unwrap_or_else(|| widget.empty_label.clone())
                                .into(),
                            value: observation
                                .map(value_text)
                                .unwrap_or_else(|| "—".into())
                                .into(),
                            quality: entity
                                .and_then(|entity| entity.state.as_option())
                                .map(crate::cache_view::quality_text)
                                .unwrap_or_else(|| "Source unavailable".into())
                                .into(),
                            ..Default::default()
                        }
                    })
                    .collect();
            }
            Content::Observations { items } => {
                reading.observations = items
                    .iter()
                    .map(|item| {
                        let value = item.field.reading(view);
                        let active = view
                            .and_then(|view| view.get(&item.field.entity))
                            .and_then(crate::dashboard_visibility::literal)
                            .is_some_and(|value| {
                                item.active_states.iter().any(|state| state == value)
                            });
                        ObservationReading {
                            title: item.label.clone(),
                            available: value.is_some(),
                            value: value.unwrap_or_else(|| "—".into()),
                            active,
                        }
                    })
                    .collect();
            }
            Content::Agenda { .. } | Content::Tasks { .. } | Content::Navigation { .. } => {}
        }
        reading
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn configured_clock_zone_controls_the_human_date_across_a_day_boundary() {
        let at = chrono::DateTime::parse_from_rfc3339("2026-11-01T04:30:00Z").unwrap();
        let at = home_client_model::local_time::UtcInstant::new(at.timestamp(), 0).unwrap();
        assert_eq!(
            super::clock("America/Toronto".parse().unwrap(), at),
            ("00:30".into(), "Sunday, November 1, 2026".into())
        );
        assert_eq!(
            super::clock("Pacific/Honolulu".parse().unwrap(), at),
            ("18:30".into(), "Saturday, October 31, 2026".into())
        );
    }
}
