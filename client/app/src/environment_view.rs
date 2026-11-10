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
use home_wire_contract::{api_environment_contract as admit, home::api::v2 as wire};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) enum ProfileZone {
    #[default]
    Missing,
    Unsupported,
    Known(DisplayTimeZone),
}
impl ProfileZone {
    pub fn from_time_zone(value: Option<&str>) -> Self {
        match value {
            None => Self::Missing,
            Some(value) => value.parse().map(Self::Known).unwrap_or(Self::Unsupported),
        }
    }
    pub fn timestamp(self, value: Option<&Timestamp>) -> String {
        match value.and_then(instant) {
            None => "Date missing or unsupported".into(),
            Some(at) => match self {
                Self::Known(zone) => {
                    let local = zone.local(at);
                    format!("{} {} {}", local.date(), local.clock(), local.offset())
                }
                Self::Missing => "Profile time zone missing".into(),
                Self::Unsupported => "Profile time zone unsupported".into(),
            },
        }
    }
    fn label(self) -> String {
        match self {
            Self::Known(zone) => format!("Profile time zone · {}", zone.name()),
            Self::Missing => "Profile time zone missing".into(),
            Self::Unsupported => "Profile time zone unsupported".into(),
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
            Self::Missing => ("—".into(), "Profile time zone missing".into()),
            Self::Unsupported => ("—".into(), "Profile time zone unsupported".into()),
        }
    }
    pub fn greeting(self, now: UtcInstant) -> &'static str {
        match self {
            Self::Known(zone) => {
                let hour = zone.local(now).hour();
                if (5..12).contains(&hour) {
                    "Good Morning"
                } else if (12..18).contains(&hour) {
                    "Good Afternoon"
                } else {
                    "Good Evening"
                }
            }
            Self::Missing | Self::Unsupported => "Welcome",
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
        None => "Age not provided".into(),
        Some(Age::Current) => "Just Now".into(),
        Some(Age::Elapsed(seconds)) => {
            let seconds = seconds.get();
            if seconds < 60 {
                format!("{seconds} s ago")
            } else if seconds < 3600 {
                format!("{} min ago", seconds / 60)
            } else if seconds < 86400 {
                format!("{} h ago", seconds / 3600)
            } else {
                format!("{} d ago", seconds / 86400)
            }
        }
        Some(Age::ClockAhead) => "Future date · check the clock".into(),
    }
}
pub(crate) fn observed_age(
    capability: Option<&wire::Capability>,
    zone: ProfileZone,
    now: UtcInstant,
) -> String {
    match capability
        .and_then(crate::device_view::observation::evidence)
        .and_then(|(_, time)| time)
    {
        Some(time) => format!(
            "Observed {} · {}",
            age(Some(time), now),
            zone.timestamp(Some(time))
        ),
        None => "Observation date not provided".into(),
    }
}
fn fact(label: &str, value: String) -> ProEnvironmentFact {
    ProEnvironmentFact {
        label: label.into(),
        value: value.into(),
        ..Default::default()
    }
}

#[derive(Clone, Default, Debug, PartialEq)]
pub(crate) struct Weather {
    temperature: String,
    condition: String,
    facts: Vec<ProEnvironmentFact>,
    forecast_status: String,
    forecast: Vec<ProForecastRow>,
    provenance: String,
    notice: String,
}
#[derive(Clone, Default, Debug, PartialEq)]
pub(crate) struct Solar {
    clock: String,
    date: String,
    time_zone: String,
    position: String,
    events: Vec<ProEnvironmentFact>,
    notice: String,
}
#[derive(Clone, Default, Debug, PartialEq)]
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
                    forecast_heading: "Forecast".into(),
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

fn weather(value: &wire::WeatherValue, zone: ProfileZone, now: UtcInstant) -> Weather {
    let mut row = Weather {
        notice: zone.label(),
        temperature: value
            .temperature
            .as_option()
            .map(environment_text::temperature)
            .unwrap_or_else(|| "Temperature not provided".into()),
        condition: value
            .condition
            .map(environment_text::condition_label)
            .unwrap_or("Conditions not provided")
            .into(),
        provenance: if value.source_label.is_empty() {
            "Source not provided".into()
        } else {
            format!("Source · {}", bounded(&value.source_label))
        },
        ..Default::default()
    };
    if !value.__buffa_unknown_fields.is_empty() {
        row.notice = format!("{} · Some weather fields are unsupported", row.notice);
    }
    if let Some(humidity) = value.humidity_percent {
        row.facts.push(fact(
            "Humidity",
            if humidity.is_finite() && (0.0..=100.0).contains(&humidity) {
                format!("{} %", number(humidity))
            } else {
                "Unsupported value".into()
            },
        ));
    }
    if let Some(wind) = value.wind.as_option() {
        let text = if admit::wind(wind) {
            let unit = match wind.unit {
                EnumValue::Known(wire::SpeedUnit::MetersPerSecond) => "m/s",
                EnumValue::Known(wire::SpeedUnit::KilometersPerHour) => "km/h",
                EnumValue::Known(wire::SpeedUnit::MilesPerHour) => "mph",
                EnumValue::Known(wire::SpeedUnit::Knots) => "kn",
                EnumValue::Known(wire::SpeedUnit::Unspecified) | EnumValue::Unknown(_) => {
                    "unsupported unit"
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
            "Unsupported wind value".into()
        };
        row.facts.push(fact("Wind", text));
    }
    let Some(forecast) = value.forecast.as_option() else {
        row.forecast_status = "Forecast not provided".into();
        return row;
    };
    if !admit::forecast_structure(forecast) {
        row.forecast_status = "Unsupported forecast".into();
        return row;
    }
    let expired = forecast
        .expire_time
        .as_option()
        .and_then(instant)
        .is_some_and(|until| until <= now);
    row.forecast_status = format!(
        "{} · Issued {} · Valid until {}",
        if expired {
            "Outdated"
        } else {
            "Dated forecast"
        },
        age(forecast.issue_time.as_option(), now),
        zone.timestamp(forecast.expire_time.as_option())
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
                format!("Probability {} %", number(v))
            } else {
                "Unsupported probability".into()
            });
        }
        if let Some(v) = period.precipitation.as_option() {
            let unit = match v.unit {
                EnumValue::Known(wire::PrecipitationUnit::Millimeters) => Some("mm"),
                EnumValue::Known(wire::PrecipitationUnit::Inches) => Some("in"),
                EnumValue::Known(wire::PrecipitationUnit::Unspecified) | EnumValue::Unknown(_) => {
                    None
                }
            };
            precipitation.push(match (admit::precipitation(v), unit, v.amount) {
                (true, Some(unit), Some(amount)) => format!("{} {unit}", number(amount)),
                (false, _, _) | (true, None, _) | (true, Some(_), None) => {
                    "Unsupported precipitation".into()
                }
            });
        }
        row.forecast.push(ProForecastRow {
            period: format!(
                "{} → {}",
                zone.timestamp(period.start_time.as_option()),
                zone.timestamp(period.end_time.as_option())
            )
            .into(),
            temperature: if temperatures.is_empty() {
                "Temperature not provided".into()
            } else {
                temperatures.join(" · ").into()
            },
            condition: period
                .condition
                .map(environment_text::condition_label)
                .unwrap_or("Conditions not provided")
                .into(),
            precipitation: precipitation.join(" · ").into(),
        });
    }
    row
}

fn event_label(event: wire::SolarEventKind) -> &'static str {
    match event {
        wire::SolarEventKind::Dawn => "Dawn",
        wire::SolarEventKind::Dusk => "Twilight",
        wire::SolarEventKind::Midnight => "Solar Midnight",
        wire::SolarEventKind::Noon => "Solar Noon",
        wire::SolarEventKind::Rising => "Sunrise",
        wire::SolarEventKind::Setting => "Sunset",
        wire::SolarEventKind::Unspecified => "Unsupported event",
    }
}
fn occurrence(value: Option<&wire::SolarOccurrence>, zone: ProfileZone) -> String {
    use wire::solar_occurrence::Occurrence;
    match value
        .and_then(|v| v.__buffa_unknown_fields.is_empty().then_some(v))
        .and_then(|v| v.occurrence.as_ref())
    {
        Some(Occurrence::EventTime(at)) => zone.timestamp(Some(at)),
        Some(Occurrence::Absent(reason)) => match reason {
            EnumValue::Known(wire::SolarAbsenceReason::NoEventWithinSearchHorizon) => {
                "Not found within the search range".into()
            }
            EnumValue::Known(wire::SolarAbsenceReason::NoTransitOnDate) => {
                "Not found on this date".into()
            }
            EnumValue::Known(wire::SolarAbsenceReason::Unspecified) | EnumValue::Unknown(_) => {
                "Unsupported absence value".into()
            }
        },
        None => "Event not provided".into(),
    }
}
pub(crate) fn project(
    kind: crate::device_view::capability::ObservationKind,
    capability: Option<&wire::Capability>,
    zone: ProfileZone,
    now: UtcInstant,
) -> Presentation {
    use crate::device_view::capability::ObservationKind as K;
    use wire::capability::Trait as T;
    let trait_value = capability
        .filter(|capability| crate::device_view::observation::evidence(capability).is_some())
        .and_then(|capability| capability.r#trait.as_ref());
    match kind {
        K::Weather => {
            let weather_value = if let Some(T::Weather(value)) = trait_value {
                value.observation.value.as_option()
            } else {
                None
            };
            Presentation::Weather(
                weather_value
                    .map(|value| weather(value, zone, now))
                    .unwrap_or_else(|| Weather {
                        temperature: "Weather missing or unsupported".into(),
                        ..Default::default()
                    }),
            )
        }
        K::Presence => {
            let presence_value = if let Some(T::Presence(value)) = trait_value {
                value
                    .observation
                    .value
                    .as_option()
                    .filter(|value| admit::presence_value(value))
            } else {
                None
            };
            Presentation::Presence(match presence_value {
                Some(value) => ProPresenceData {
                    status: match value.residence {
                        EnumValue::Known(wire::Residence::Home) => ProPresenceStatus::Home,
                        EnumValue::Known(wire::Residence::Away) => ProPresenceStatus::Away,
                        EnumValue::Known(wire::Residence::Unknown) => ProPresenceStatus::Unknown,
                        EnumValue::Known(wire::Residence::Unspecified) | EnumValue::Unknown(_) => {
                            ProPresenceStatus::Unsupported
                        }
                    },
                    summary: environment_text::presence(value).into(),
                    detail: match value.confidence {
                        EnumValue::Known(wire::PresenceConfidence::Unknown) => "Confidence unknown",
                        EnumValue::Known(wire::PresenceConfidence::Low) => "Low confidence",
                        EnumValue::Known(wire::PresenceConfidence::Medium) => "Medium confidence",
                        EnumValue::Known(wire::PresenceConfidence::High) => "High confidence",
                        EnumValue::Known(wire::PresenceConfidence::Unspecified)
                        | EnumValue::Unknown(_) => "Unsupported confidence value",
                    }
                    .into(),
                    provenance: format!("Source · {}", bounded(&value.source_label)).into(),
                    ..Default::default()
                },
                None => ProPresenceData {
                    status: ProPresenceStatus::Unsupported,
                    summary: "Presence missing or unsupported".into(),
                    ..Default::default()
                },
            })
        }
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
            let value = if let Some(T::Solar(value)) = trait_value {
                value.observation.value.as_option().filter(|value| {
                    home_wire_contract::api_solar_contract::admit_solar_value(value).is_ok()
                })
            } else {
                None
            };
            let event = if let Some(T::SolarEvent(value)) = trait_value {
                value.observation.value.as_option().filter(|value| {
                    home_wire_contract::api_solar_contract::admit_solar_event_value(value).is_ok()
                })
            } else {
                None
            };
            if let Some(value) = value.filter(|_| kind == K::Solar) {
                row.position = match value.horizon {
                    EnumValue::Known(wire::SolarHorizon::Above) => "Above the horizon",
                    EnumValue::Known(wire::SolarHorizon::Below) => "Below the horizon",
                    EnumValue::Known(wire::SolarHorizon::Unspecified) | EnumValue::Unknown(_) => {
                        "Unsupported horizon"
                    }
                }
                .into();
                for (kind, at) in [
                    (wire::SolarEventKind::Rising, value.next_rising.as_option()),
                    (
                        wire::SolarEventKind::Setting,
                        value.next_setting.as_option(),
                    ),
                    (wire::SolarEventKind::Dawn, value.next_dawn.as_option()),
                    (wire::SolarEventKind::Dusk, value.next_dusk.as_option()),
                    (wire::SolarEventKind::Noon, value.next_noon.as_option()),
                    (
                        wire::SolarEventKind::Midnight,
                        value.next_midnight.as_option(),
                    ),
                ] {
                    row.events
                        .push(fact(event_label(kind), occurrence(at, zone)));
                }
            } else if let Some(value) = event
                && let K::SolarEvent(event) = kind
                && value.event == event
            {
                row.events.push(fact(
                    event_label(event),
                    occurrence(value.occurrence.as_option(), zone),
                ));
            } else {
                row.notice = "Solar events missing or unsupported".into();
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
                device: row.ui(None, &crate::IconGlyph::default()),
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
            device: row.ui(Some(&previous.device), &crate::IconGlyph::default()),
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
    fn greeting_uses_the_profile_timezone_and_keeps_missing_profiles_neutral() {
        let morning = instant(&at("2026-11-01T13:00:00Z")).unwrap();
        let evening = instant(&at("2026-11-01T23:00:00Z")).unwrap();
        assert_eq!(toronto().greeting(morning), "Good Morning");
        assert_eq!(toronto().greeting(evening), "Good Evening");
        assert_eq!(ProfileZone::Missing.greeting(morning), "Welcome");
        assert_eq!(ProfileZone::Unsupported.greeting(evening), "Welcome");
    }

    #[test]
    fn forecast_preserves_fold_offsets_expiry_zero_and_independent_unsupported_fields() {
        let value = wire::WeatherValue {
            temperature: wire::Temperature {
                value: Some(0.),
                unit: wire::TemperatureUnit::Celsius.into(),
                ..Default::default()
            }
            .into(),
            condition: Some(wire::WeatherCondition::Rainy.into()),
            humidity_percent: Some(0.),
            forecast: wire::Forecast {
                issue_time: at("2026-11-01T04:00:00Z").into(),
                expire_time: at("2026-11-01T07:00:00Z").into(),
                periods: vec![wire::ForecastPeriod {
                    start_time: at("2026-11-01T05:30:00Z").into(),
                    end_time: at("2026-11-01T06:30:00Z").into(),
                    kind: wire::ForecastKind::Hourly.into(),
                    condition: Some(wire::WeatherCondition::Rainy.into()),
                    temperature: wire::Temperature {
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
        assert_eq!(row.condition, "Rain");
        assert_eq!(row.facts[0].value, "0 %");
        assert!(row.forecast_status.contains("Outdated") && row.forecast_status.contains("4 h"));
        assert_eq!(row.forecast.len(), 1);
        let period = &row.forecast[0];
        assert!(period.period.contains("01:30 -04:00") && period.period.contains("01:30 -05:00"));
        assert_eq!(period.temperature, "Unsupported temperature");
        assert_eq!(period.condition, "Rain");
        assert_eq!(period.precipitation, "Probability 0 %");
        let paris = weather(
            &value,
            ProfileZone::Known("Europe/Paris".parse().unwrap()),
            now,
        );
        assert!(paris.forecast[0].period.contains("06:30 +01:00"));
        assert_eq!(
            value.forecast.periods[0].start_time.as_option(),
            Some(&at("2026-11-01T05:30:00Z"))
        );
        let mut malformed = value.clone();
        malformed.forecast.as_option_mut().unwrap().periods[0].end_time =
            at("2026-11-01T04:30:00Z").into();
        let refused = weather(&malformed, toronto(), now);
        assert!(refused.forecast.is_empty());
        assert_eq!(refused.temperature, "0 °C");
        assert!(refused.forecast_status.contains("Unsupported"));
    }

    #[test]
    fn profile_clock_and_absent_solar_event_never_use_system_timezone_or_epoch_zero() {
        let at = at("2026-11-01T06:30:00Z");
        let now = instant(&at).unwrap();
        assert_eq!(toronto().clock(now).0, "01:30");
        assert!(toronto().clock(now).1.contains("-05:00"));
        assert_eq!(ProfileZone::Missing.clock(now).0, "—");
        assert_eq!(
            ProfileZone::from_time_zone(Some("future/zone")),
            ProfileZone::Unsupported
        );
        assert!(
            ProfileZone::Unsupported
                .timestamp(Some(&at))
                .contains("unsupported")
        );
        let event = wire::SolarEventValue {
            event: wire::SolarEventKind::Rising.into(),
            occurrence: wire::SolarOccurrence {
                occurrence: Some(wire::solar_occurrence::Occurrence::Absent(
                    wire::SolarAbsenceReason::NoEventWithinSearchHorizon.into(),
                )),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        let observed = wire::Capability {
            r#trait: Some(wire::capability::Trait::SolarEvent(Box::new(
                wire::SolarEventCapability {
                    observation: wire::SolarEventObservation {
                        value: event.into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        };
        let Presentation::Solar(row) = project(
            ObservationKind::SolarEvent(wire::SolarEventKind::Rising),
            Some(&observed),
            toronto(),
            now,
        ) else {
            panic!("solar projection");
        };
        assert_eq!(row.events.len(), 1);
        assert_eq!(row.events[0].value, "Not found within the search range");
        let Presentation::Solar(mismatched) = project(
            ObservationKind::SolarEvent(wire::SolarEventKind::Setting),
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
