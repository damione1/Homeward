//! Authorized environmental presentation. No location lookup or local ephemeris.
use crate::{
    ProEnvironmentFact, ProEnvironmentKind, ProForecastRow, ProPresenceData, ProPresenceStatus,
    ProSolarData, ProWeatherData,
    cache_view::{bounded, number},
    device_view::presentation::nested,
    environment_text,
};
use buffa::EnumValue;
use buffa_types::google::protobuf::Timestamp;
use home_client_model::local_time::{Age, DisplayTimeZone, UtcInstant};
use home_wire_contract::{
    environment_contract as admit,
    home::{environment::v1 as env, solar::v1 as solar, state::v1 as state},
};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) enum ProfileZone {
    #[default]
    Missing,
    Unsupported,
    Known(DisplayTimeZone),
}
impl ProfileZone {
    pub fn from_profile(profile: Option<&env::DisplayProfile>) -> Self {
        match profile {
            None => Self::Missing,
            Some(profile) if admit::display_profile(profile) => {
                Self::Known(profile.time_zone.parse().expect("admitted timezone"))
            }
            Some(_) => Self::Unsupported,
        }
    }
    pub fn timestamp(self, value: Option<&Timestamp>) -> String {
        match value.and_then(instant) {
            None => "Date non fournie ou non prise en charge".into(),
            Some(at) => match self {
                Self::Known(zone) => {
                    let local = zone.local(at);
                    format!("{} {} {}", local.date(), local.clock(), local.offset())
                }
                Self::Missing => "Fuseau du profil non fourni".into(),
                Self::Unsupported => "Fuseau du profil non pris en charge".into(),
            },
        }
    }
    fn label(self) -> String {
        match self {
            Self::Known(zone) => format!("Fuseau du profil · {}", zone.name()),
            Self::Missing => "Fuseau du profil non fourni".into(),
            Self::Unsupported => "Fuseau du profil non pris en charge".into(),
        }
    }
    pub fn clock(self, now: UtcInstant) -> (String, String) {
        match self {
            Self::Known(zone) => {
                let local = zone.local(now);
                (
                    local.clock(),
                    format!("{} · {} · {}", local.date(), zone.name(), local.offset()),
                )
            }
            Self::Missing => ("—".into(), "Fuseau du profil non fourni".into()),
            Self::Unsupported => ("—".into(), "Fuseau du profil non pris en charge".into()),
        }
    }
}
pub(crate) fn now() -> UtcInstant {
    let now = chrono::Utc::now();
    UtcInstant::new(now.timestamp(), now.timestamp_subsec_nanos() as i32)
        .expect("current UTC instant")
}
fn instant(at: &Timestamp) -> Option<UtcInstant> {
    at.__buffa_unknown_fields
        .is_empty()
        .then(|| UtcInstant::new(at.seconds, at.nanos).ok())
        .flatten()
}
fn age(at: Option<&Timestamp>, now: UtcInstant) -> String {
    match at.and_then(instant).map(|at| at.age_at(now)) {
        None => "Âge non fourni".into(),
        Some(Age::Current) => "À l’instant".into(),
        Some(Age::Elapsed(seconds)) => {
            let seconds = seconds.get();
            if seconds < 60 {
                format!("Il y a {seconds} s")
            } else if seconds < 3600 {
                format!("Il y a {} min", seconds / 60)
            } else if seconds < 86400 {
                format!("Il y a {} h", seconds / 3600)
            } else {
                format!("Il y a {} j", seconds / 86400)
            }
        }
        Some(Age::ClockAhead) => "Date future · vérifier l’horloge".into(),
    }
}
pub(crate) fn observed_age(
    observation: Option<&state::ObservedState>,
    zone: ProfileZone,
    now: UtcInstant,
) -> String {
    match observation {
        Some(value) if value.observed_at.is_set() => format!(
            "Observé {} · {}",
            age(value.observed_at.as_option(), now),
            zone.timestamp(value.observed_at.as_option())
        ),
        Some(value) if value.received_at.is_set() => format!(
            "Reçu {} · {}",
            age(value.received_at.as_option(), now),
            zone.timestamp(value.received_at.as_option())
        ),
        Some(_) | None => "Date d’observation non fournie".into(),
    }
}
fn fact(label: &str, value: String) -> ProEnvironmentFact {
    ProEnvironmentFact {
        label: label.into(),
        value: value.into(),
        ..Default::default()
    }
}

#[derive(Clone, Default, Debug)]
pub(crate) struct Weather {
    temperature: String,
    condition: String,
    facts: Vec<ProEnvironmentFact>,
    forecast_status: String,
    forecast: Vec<ProForecastRow>,
    provenance: String,
    notice: String,
}
#[derive(Clone, Default, Debug)]
pub(crate) struct Solar {
    clock: String,
    date: String,
    time_zone: String,
    position: String,
    events: Vec<ProEnvironmentFact>,
    notice: String,
}
#[derive(Clone, Default, Debug)]
pub(crate) enum Presentation {
    #[default]
    Absent,
    Weather(Weather),
    Solar(Solar),
    Presence(ProPresenceData),
}
impl Presentation {
    // Nested renderer models are created only after the cache publication fence,
    // on the UI thread. Transferable projections contain ordinary finite Vecs.
    pub fn ui(
        &self,
        previous: Option<&crate::ProDeviceRow>,
    ) -> (
        ProEnvironmentKind,
        ProWeatherData,
        ProSolarData,
        ProPresenceData,
    ) {
        let mut weather = ProWeatherData::default();
        let mut solar = ProSolarData::default();
        let mut presence = ProPresenceData::default();
        let kind = match self {
            Self::Absent => ProEnvironmentKind::Absent,
            Self::Weather(value) => {
                weather = ProWeatherData {
                    temperature: value.temperature.as_str().into(),
                    condition: value.condition.as_str().into(),
                    facts: nested(&value.facts, previous.map(|v| &v.weather.facts)),
                    forecast: nested(&value.forecast, previous.map(|v| &v.weather.forecast)),
                    forecast_heading: "Prévisions".into(),
                    forecast_status: value.forecast_status.as_str().into(),
                    provenance: value.provenance.as_str().into(),
                    notice: value.notice.as_str().into(),
                    ..Default::default()
                };
                ProEnvironmentKind::Weather
            }
            Self::Solar(value) => {
                solar = ProSolarData {
                    clock: value.clock.as_str().into(),
                    date: value.date.as_str().into(),
                    time_zone: value.time_zone.as_str().into(),
                    position: value.position.as_str().into(),
                    events: nested(&value.events, previous.map(|v| &v.solar.events)),
                    notice: value.notice.as_str().into(),
                    ..Default::default()
                };
                ProEnvironmentKind::Solar
            }
            Self::Presence(value) => {
                presence = value.clone();
                ProEnvironmentKind::Presence
            }
        };
        (kind, weather, solar, presence)
    }
}

fn weather(value: &env::WeatherValue, zone: ProfileZone, now: UtcInstant) -> Weather {
    let mut row = Weather {
        notice: zone.label(),
        temperature: value
            .temperature
            .as_option()
            .map(environment_text::temperature)
            .unwrap_or_else(|| "Température non fournie".into()),
        condition: value
            .condition
            .map(environment_text::condition_label)
            .unwrap_or("Conditions non fournies")
            .into(),
        provenance: if value.source_label.is_empty() {
            "Source non fournie".into()
        } else {
            format!("Source · {}", bounded(&value.source_label))
        },
        ..Default::default()
    };
    if !value.__buffa_unknown_fields.is_empty() {
        row.notice = format!(
            "{} · Certains champs météo ne sont pas pris en charge",
            row.notice
        );
    }
    if let Some(humidity) = value.humidity_percent {
        row.facts.push(fact(
            "Humidité",
            if humidity.is_finite() && (0.0..=100.0).contains(&humidity) {
                format!("{} %", number(humidity))
            } else {
                "Valeur non prise en charge".into()
            },
        ));
    }
    if let Some(wind) = value.wind.as_option() {
        let text = if admit::wind(wind) {
            let unit = match wind.unit {
                EnumValue::Known(env::SpeedUnit::MetersPerSecond) => "m/s",
                EnumValue::Known(env::SpeedUnit::KilometersPerHour) => "km/h",
                EnumValue::Known(env::SpeedUnit::MilesPerHour) => "mph",
                EnumValue::Known(env::SpeedUnit::Knots) => "kn",
                EnumValue::Known(env::SpeedUnit::Unspecified) | EnumValue::Unknown(_) => {
                    "unité non prise en charge"
                }
            };
            format!(
                "{} {unit}{}",
                number(wind.speed.expect("admitted wind")),
                wind.bearing_degrees
                    .map(|v| format!(" · {}°", number(v)))
                    .unwrap_or_default()
            )
        } else {
            "Vent non pris en charge".into()
        };
        row.facts.push(fact("Vent", text));
    }
    let Some(forecast) = value.forecast.as_option() else {
        row.forecast_status = "Prévisions non fournies".into();
        return row;
    };
    if !admit::forecast_structure(forecast) {
        row.forecast_status = "Prévisions non prises en charge".into();
        return row;
    }
    let expired = forecast
        .valid_until
        .as_option()
        .and_then(instant)
        .is_some_and(|until| until <= now);
    row.forecast_status = format!(
        "{} · Émises {} · Valables jusqu’au {}",
        if expired {
            "Périmées"
        } else {
            "Prévisions datées"
        },
        age(forecast.issued_at.as_option(), now),
        zone.timestamp(forecast.valid_until.as_option())
    );
    for period in &forecast.periods {
        let mut temperatures = Vec::new();
        if let Some(v) = period.temperature.as_option() {
            temperatures.push(environment_text::temperature(v));
        }
        if let Some(v) = period.low.as_option() {
            temperatures.push(format!("Min. {}", environment_text::temperature(v)));
        }
        if let Some(v) = period.high.as_option() {
            temperatures.push(format!("Max. {}", environment_text::temperature(v)));
        }
        let mut precipitation = Vec::new();
        if let Some(v) = period.precipitation_probability_percent {
            precipitation.push(if v.is_finite() && (0.0..=100.0).contains(&v) {
                format!("Probabilité {} %", number(v))
            } else {
                "Probabilité non prise en charge".into()
            });
        }
        if let Some(v) = period.precipitation.as_option() {
            let unit = match v.unit {
                EnumValue::Known(env::PrecipitationUnit::Millimeters) => Some("mm"),
                EnumValue::Known(env::PrecipitationUnit::Inches) => Some("in"),
                EnumValue::Known(env::PrecipitationUnit::Unspecified) | EnumValue::Unknown(_) => {
                    None
                }
            };
            precipitation.push(match (admit::precipitation(v), unit, v.amount) {
                (true, Some(unit), Some(amount)) => format!("{} {unit}", number(amount)),
                (false, _, _) | (true, None, _) | (true, Some(_), None) => {
                    "Précipitations non prises en charge".into()
                }
            });
        }
        row.forecast.push(ProForecastRow {
            period: format!(
                "{} → {}",
                zone.timestamp(period.starts_at.as_option()),
                zone.timestamp(period.ends_at.as_option())
            )
            .into(),
            temperature: if temperatures.is_empty() {
                "Température non fournie".into()
            } else {
                temperatures.join(" · ").into()
            },
            condition: period
                .condition
                .map(environment_text::condition_label)
                .unwrap_or("Conditions non fournies")
                .into(),
            precipitation: precipitation.join(" · ").into(),
        });
    }
    row
}

fn event_label(event: solar::SolarEventKind) -> &'static str {
    match event {
        solar::SolarEventKind::Dawn => "Aube",
        solar::SolarEventKind::Dusk => "Crépuscule",
        solar::SolarEventKind::Midnight => "Minuit solaire",
        solar::SolarEventKind::Noon => "Midi solaire",
        solar::SolarEventKind::Rising => "Lever",
        solar::SolarEventKind::Setting => "Coucher",
        solar::SolarEventKind::Unspecified => "Événement non pris en charge",
    }
}
fn occurrence(value: Option<&solar::SolarOccurrence>, zone: ProfileZone) -> String {
    use solar::solar_occurrence::Occurrence;
    match value
        .and_then(|v| v.__buffa_unknown_fields.is_empty().then_some(v))
        .and_then(|v| v.occurrence.as_ref())
    {
        Some(Occurrence::At(at)) => zone.timestamp(Some(at)),
        Some(Occurrence::Absent(reason)) => match reason {
            EnumValue::Known(solar::SolarAbsenceReason::NoEventWithinSearchHorizon) => {
                "Absent dans l’horizon de recherche".into()
            }
            EnumValue::Known(solar::SolarAbsenceReason::NoTransitOnDate) => {
                "Absent à cette date".into()
            }
            EnumValue::Known(solar::SolarAbsenceReason::Unspecified) | EnumValue::Unknown(_) => {
                "Absence non prise en charge".into()
            }
        },
        None => "Événement non fourni".into(),
    }
}
pub(crate) fn project(
    kind: crate::device_view::capability::ObservationKind,
    observation: Option<&state::ObservedState>,
    zone: ProfileZone,
    now: UtcInstant,
) -> Presentation {
    use crate::device_view::capability::ObservationKind as K;
    let value = observation
        .filter(|v| v.__buffa_unknown_fields.is_empty())
        .and_then(|v| v.value.as_ref());
    match kind {
        K::Weather => Presentation::Weather(match value {
            Some(state::observed_state::Value::Weather(value)) => weather(value, zone, now),
            Some(
                state::observed_state::Value::SwitchOn(_)
                | state::observed_state::Value::Measurement(_)
                | state::observed_state::Value::Light(_)
                | state::observed_state::Value::Fan(_)
                | state::observed_state::Value::Climate(_)
                | state::observed_state::Value::Helper(_)
                | state::observed_state::Value::Solar(_)
                | state::observed_state::Value::SolarEvent(_)
                | state::observed_state::Value::Cover(_)
                | state::observed_state::Value::Lock(_)
                | state::observed_state::Value::Alarm(_)
                | state::observed_state::Value::Presence(_)
                | state::observed_state::Value::PresenceDetails(_),
            )
            | None => Weather {
                temperature: "Météo non fournie ou non prise en charge".into(),
                ..Default::default()
            },
        }),
        K::Presence => Presentation::Presence(match value {
            Some(state::observed_state::Value::Presence(value)) if admit::presence_value(value) => {
                ProPresenceData {
                    status: match value.residence {
                        EnumValue::Known(env::Residence::Home) => ProPresenceStatus::Home,
                        EnumValue::Known(env::Residence::Away) => ProPresenceStatus::Away,
                        EnumValue::Known(env::Residence::Unknown) => ProPresenceStatus::Unknown,
                        EnumValue::Known(env::Residence::Unspecified) | EnumValue::Unknown(_) => {
                            ProPresenceStatus::Unsupported
                        }
                    },
                    summary: environment_text::presence(value).into(),
                    detail: match value.confidence {
                        EnumValue::Known(env::PresenceConfidence::Unknown) => "Confiance inconnue",
                        EnumValue::Known(env::PresenceConfidence::Low) => "Confiance faible",
                        EnumValue::Known(env::PresenceConfidence::Medium) => "Confiance moyenne",
                        EnumValue::Known(env::PresenceConfidence::High) => "Confiance élevée",
                        EnumValue::Known(env::PresenceConfidence::Unspecified)
                        | EnumValue::Unknown(_) => "Confiance non prise en charge",
                    }
                    .into(),
                    provenance: format!("Source · {}", bounded(&value.source_label)).into(),
                    ..Default::default()
                }
            }
            Some(
                state::observed_state::Value::SwitchOn(_)
                | state::observed_state::Value::Measurement(_)
                | state::observed_state::Value::Light(_)
                | state::observed_state::Value::Fan(_)
                | state::observed_state::Value::Climate(_)
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
            | None => ProPresenceData {
                status: ProPresenceStatus::Unsupported,
                summary: "Présence non fournie ou non prise en charge".into(),
                ..Default::default()
            },
        }),
        K::Details => Presentation::Absent,
        K::Solar | K::SolarEvent(_) => {
            let (clock, date) = zone.clock(now);
            let mut row = Solar {
                clock,
                date,
                ..Default::default()
            };
            if let ProfileZone::Known(zone) = zone {
                let local = zone.local(now);
                row.date = local.date();
                row.time_zone = format!("{} · {}", zone.name(), local.offset());
            }
            match value {
                Some(state::observed_state::Value::Solar(value))
                    if kind == K::Solar
                        && home_wire_contract::solar_contract::admit_solar_value(value).is_ok() =>
                {
                    row.position = match value.horizon {
                        EnumValue::Known(solar::SolarHorizon::Above) => "Au-dessus de l’horizon",
                        EnumValue::Known(solar::SolarHorizon::Below) => "Sous l’horizon",
                        EnumValue::Known(solar::SolarHorizon::Unspecified)
                        | EnumValue::Unknown(_) => "Horizon non pris en charge",
                    }
                    .into();
                    for (kind, at) in [
                        (solar::SolarEventKind::Rising, value.next_rising.as_option()),
                        (
                            solar::SolarEventKind::Setting,
                            value.next_setting.as_option(),
                        ),
                        (solar::SolarEventKind::Dawn, value.next_dawn.as_option()),
                        (solar::SolarEventKind::Dusk, value.next_dusk.as_option()),
                        (solar::SolarEventKind::Noon, value.next_noon.as_option()),
                        (
                            solar::SolarEventKind::Midnight,
                            value.next_midnight.as_option(),
                        ),
                    ] {
                        row.events
                            .push(fact(event_label(kind), occurrence(at, zone)));
                    }
                }
                Some(state::observed_state::Value::SolarEvent(value))
                    if home_wire_contract::solar_contract::admit_solar_event_value(value)
                        .is_ok()
                        && matches!(kind,K::SolarEvent(event) if value.event==event) =>
                {
                    if let K::SolarEvent(event) = kind {
                        row.events.push(fact(
                            event_label(event),
                            occurrence(value.occurrence.as_option(), zone),
                        ));
                    }
                }
                Some(
                    state::observed_state::Value::SwitchOn(_)
                    | state::observed_state::Value::Measurement(_)
                    | state::observed_state::Value::Light(_)
                    | state::observed_state::Value::Fan(_)
                    | state::observed_state::Value::Climate(_)
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
                | None => {
                    row.notice = "Événements solaires non fournis ou non pris en charge".into()
                }
            }
            Presentation::Solar(row)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::device_view::capability::ObservationKind;
    #[test]
    fn replaced_solar_reading_reaches_the_native_board_with_reused_models() {
        use crate::device_view::presentation::CardRow;
        use crate::{
            ProAccessStage, ProDeviceKind, ProLayoutFlow, ProLayoutInput, ProLayoutKind,
            ProLayoutNode, ViewPage,
        };
        use i_slint_backend_testing::ElementQuery;
        use slint::{ComponentHandle, Model, ModelRc, VecModel};
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        crate::cache_view::bind(&window);
        window.set_access_stage(ProAccessStage::Connected);
        window.set_endpoint_ready(true);
        window.set_show_endpoint(false);
        window.set_view_loaded(true);
        window.set_dashboard_status("".into());
        window.set_page(ViewPage::Home);
        window
            .window()
            .set_size(slint::LogicalSize::new(1280., 1000.));
        let mut row = CardRow {
            kind: ProDeviceKind::Observation,
            token: 1,
            generation: 1,
            title: "Synthetic sun".into(),
            environment: Presentation::Solar(Solar {
                events: vec![fact("Rise", "06:30 -05:00".into())],
                ..Default::default()
            }),
            ..Default::default()
        };
        let nodes = std::rc::Rc::new(VecModel::from(vec![
            ProLayoutNode {
                kind: ProLayoutKind::Container,
                flow: ProLayoutFlow::Vertical,
                children: ModelRc::from([ProLayoutInput {
                    index: 1,
                    rows: 1,
                    ..Default::default()
                }]),
                ..Default::default()
            },
            ProLayoutNode {
                kind: ProLayoutKind::Device,
                device: row.ui(None),
                ..Default::default()
            },
        ]));
        window.set_home_layout_nodes(ModelRc::from(nodes.clone()));
        window.show().unwrap();
        let has = |label: &str| {
            let label = label.to_owned();
            ElementQuery::from_root(&window)
                .match_predicate(move |e| e.accessible_label().as_deref() == Some(label.as_str()))
                .find_first()
                .is_some()
        };
        assert!(has("06:30 -05:00"));
        row.environment = Presentation::Solar(Solar {
            events: vec![fact("Rise", "Absent in search horizon".into())],
            ..Default::default()
        });
        let previous = nodes.row_data(1).unwrap();
        let next = ProLayoutNode {
            device: row.ui(Some(&previous.device)),
            ..previous.clone()
        };
        if previous != next {
            nodes.set_row_data(1, next);
        }
        assert_eq!(
            nodes
                .row_data(1)
                .unwrap()
                .device
                .solar
                .events
                .row_data(0)
                .unwrap()
                .value,
            "Absent in search horizon"
        );
        assert!(
            has("Absent in search horizon"),
            "the host/board adapter retained the old solar instant"
        );
        assert!(!has("06:30 -05:00"));
    }
    fn at(text: &str) -> Timestamp {
        let at = chrono::DateTime::parse_from_rfc3339(text).unwrap();
        Timestamp {
            seconds: at.timestamp(),
            nanos: at.timestamp_subsec_nanos() as i32,
            ..Default::default()
        }
    }
    fn toronto() -> ProfileZone {
        ProfileZone::Known("America/Toronto".parse().unwrap())
    }

    #[test]
    fn forecast_preserves_fold_offsets_expiry_zero_and_independent_unsupported_fields() {
        let value = env::WeatherValue {
            temperature: env::Temperature {
                value: Some(0.),
                unit: env::TemperatureUnit::Celsius.into(),
                ..Default::default()
            }
            .into(),
            condition: Some(env::WeatherCondition::Rainy.into()),
            humidity_percent: Some(0.),
            forecast: env::Forecast {
                issued_at: at("2026-11-01T04:00:00Z").into(),
                valid_until: at("2026-11-01T07:00:00Z").into(),
                periods: vec![env::ForecastPeriod {
                    starts_at: at("2026-11-01T05:30:00Z").into(),
                    ends_at: at("2026-11-01T06:30:00Z").into(),
                    kind: env::ForecastKind::Hourly.into(),
                    condition: Some(env::WeatherCondition::Rainy.into()),
                    temperature: env::Temperature {
                        value: Some(0.),
                        unit: EnumValue::Unknown(255),
                        ..Default::default()
                    }
                    .into(),
                    precipitation_probability_percent: Some(0.),
                    ..Default::default()
                }],
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        let now = instant(&at("2026-11-01T08:00:00Z")).unwrap();
        let row = weather(&value, toronto(), now);
        assert_eq!(row.temperature, "0 °C");
        assert_eq!(row.condition, "Pluie");
        assert_eq!(row.facts[0].value, "0 %");
        assert!(row.forecast_status.contains("Périmées") && row.forecast_status.contains("4 h"));
        assert_eq!(row.forecast.len(), 1);
        let period = &row.forecast[0];
        assert!(period.period.contains("01:30 -04:00") && period.period.contains("01:30 -05:00"));
        assert_eq!(period.temperature, "Température non prise en charge");
        assert_eq!(period.condition, "Pluie");
        assert_eq!(period.precipitation, "Probabilité 0 %");
        let paris = weather(
            &value,
            ProfileZone::Known("Europe/Paris".parse().unwrap()),
            now,
        );
        assert!(paris.forecast[0].period.contains("06:30 +01:00"));
        assert_eq!(
            value.forecast.periods[0].starts_at.as_option(),
            Some(&at("2026-11-01T05:30:00Z"))
        );
        let mut malformed = value.clone();
        malformed.forecast.as_option_mut().unwrap().periods[0].ends_at =
            at("2026-11-01T04:30:00Z").into();
        let refused = weather(&malformed, toronto(), now);
        assert!(refused.forecast.is_empty());
        assert_eq!(refused.temperature, "0 °C");
        assert!(refused.forecast_status.contains("non prises en charge"));
    }

    #[test]
    fn profile_clock_and_absent_solar_event_never_use_system_timezone_or_epoch_zero() {
        let at = at("2026-11-01T06:30:00Z");
        let now = instant(&at).unwrap();
        assert_eq!(toronto().clock(now).0, "01:30");
        assert!(toronto().clock(now).1.contains("-05:00"));
        assert_eq!(ProfileZone::Missing.clock(now).0, "—");
        let invalid = env::DisplayProfile {
            time_zone: "future/zone".into(),
            ..Default::default()
        };
        assert_eq!(
            ProfileZone::from_profile(Some(&invalid)),
            ProfileZone::Unsupported
        );
        assert!(
            ProfileZone::Unsupported
                .timestamp(Some(&at))
                .contains("non pris en charge")
        );
        let event = solar::SolarEventValue {
            event: solar::SolarEventKind::Rising.into(),
            occurrence: solar::SolarOccurrence {
                occurrence: Some(solar::solar_occurrence::Occurrence::Absent(
                    solar::SolarAbsenceReason::NoEventWithinSearchHorizon.into(),
                )),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        let observed = state::ObservedState {
            value: Some(state::observed_state::Value::SolarEvent(Box::new(event))),
            ..Default::default()
        };
        let Presentation::Solar(row) = project(
            ObservationKind::SolarEvent(solar::SolarEventKind::Rising),
            Some(&observed),
            toronto(),
            now,
        ) else {
            panic!("solar projection");
        };
        assert_eq!(row.events.len(), 1);
        assert_eq!(row.events[0].value, "Absent dans l’horizon de recherche");
        let Presentation::Solar(mismatched) = project(
            ObservationKind::SolarEvent(solar::SolarEventKind::Setting),
            Some(&observed),
            toronto(),
            now,
        ) else {
            panic!("solar projection");
        };
        assert!(mismatched.events.is_empty());
        assert!(!mismatched.notice.is_empty());
    }
}
