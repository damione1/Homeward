//! Per-capability observation evidence. This never joins sibling traits or
//! treats a value as command acknowledgement/confirmation.
use buffa_types::google::protobuf::Timestamp;
use home_client_model::api_observations::{Availability, Freshness, Provenance};
use home_wire_contract::{api_observations::parse_quality, home::api::v2 as wire};

pub(crate) fn evidence(
    capability: &wire::Capability,
) -> Option<(Option<&wire::ObservationQuality>, Option<&Timestamp>)> {
    use wire::capability::Trait as T;
    if !capability.__buffa_unknown_fields.is_empty() {
        return None;
    }
    macro_rules! read {
        ($value:expr) => {{
            let value = $value;
            if !value.__buffa_unknown_fields.is_empty() {
                return None;
            }
            let observation = value.observation.as_option()?;
            if !observation.__buffa_unknown_fields.is_empty() {
                return None;
            }
            Some((
                observation.quality.as_option(),
                observation.observed_time.as_option(),
            ))
        }};
    }
    match capability.r#trait.as_ref()? {
        T::Power(v) => read!(v),
        T::Level(v) => read!(v),
        T::TemperatureControl(v) => read!(v),
        T::NumericMeasurement(v) => read!(v),
        T::BinaryObservation(v) => read!(v),
        T::LightControl(v) => read!(v),
        T::FanControl(v) => read!(v),
        T::Cover(v) => read!(v),
        T::Lock(v) => read!(v),
        T::Alarm(v) => read!(v),
        T::Helper(v) => read!(v),
        T::Solar(v) => read!(v),
        T::SolarEvent(v) => read!(v),
        T::Weather(v) => read!(v),
        T::Presence(v) => read!(v),
        T::PresenceDetails(v) => read!(v),
        T::Opaque(_) | T::MediaPlayback(_) => None,
    }
}
pub(crate) fn fresh(capability: &wire::Capability) -> bool {
    evidence(capability)
        .and_then(|(quality, _)| quality)
        .and_then(|quality| parse_quality(quality).ok())
        .is_some_and(|quality| quality.current_available())
}
// Toggle requires explicit fresh power from the selected capability. Fan speed,
// numeric zero and sibling capabilities cannot supply this evidence.
pub(crate) fn power(capability: &wire::Capability) -> Option<bool> {
    let quality = parse_quality(evidence(capability)?.0?).ok()?;
    if !quality.current_available() || quality.provenance() != Provenance::Observed {
        return None;
    }
    use wire::capability::Trait as T;
    match capability.r#trait.as_ref()? {
        T::Power(value) => match value.observation.value?.as_known()? {
            wire::PowerValue::On => Some(true),
            wire::PowerValue::Off => Some(false),
            wire::PowerValue::Unspecified => None,
        },
        T::LightControl(value) => value.observation.on,
        T::Level(_)
        | T::TemperatureControl(_)
        | T::NumericMeasurement(_)
        | T::BinaryObservation(_)
        | T::FanControl(_)
        | T::Cover(_)
        | T::Lock(_)
        | T::Alarm(_)
        | T::Helper(_)
        | T::Solar(_)
        | T::SolarEvent(_)
        | T::Weather(_)
        | T::Presence(_)
        | T::PresenceDetails(_)
        | T::Opaque(_)
        | T::MediaPlayback(_) => None,
    }
}

pub(crate) fn quality_text(capability: &wire::Capability) -> String {
    let Some(quality) = evidence(capability)
        .and_then(|(quality, _)| quality)
        .and_then(|value| parse_quality(value).ok())
    else {
        return "Quality not provided".into();
    };
    let availability = match quality.availability() {
        Availability::Available => "Available",
        Availability::Unavailable => "Unavailable",
        Availability::Unknown => "Reachability unknown",
        Availability::Unspecified => "Reachability not provided",
    };
    let freshness = match quality.freshness() {
        Freshness::Current => "Current",
        Freshness::Stale => "Stale",
        Freshness::Unspecified => "Freshness not provided",
    };
    let provenance = match quality.provenance() {
        Provenance::Observed => "Observed",
        Provenance::Derived => "Derived",
        Provenance::Restored => "Restored",
        Provenance::Estimated => "Estimated",
        Provenance::Unspecified => "Evidence not provided",
    };
    format!("{availability} · {freshness} · {provenance}")
}
pub(crate) fn observed_time(
    capability: &wire::Capability,
) -> Option<chrono::DateTime<chrono::Utc>> {
    let time = evidence(capability)?.1?;
    if !time.__buffa_unknown_fields.is_empty() || !(0..1_000_000_000).contains(&time.nanos) {
        return None;
    }
    chrono::DateTime::from_timestamp(time.seconds, time.nanos as u32)
}
pub(crate) fn value_text(capability: &wire::Capability) -> String {
    use wire::capability::Trait as T;
    let missing = || "No observation".to_owned();
    if evidence(capability).is_none() {
        return missing();
    }
    match capability.r#trait.as_ref() {
        Some(T::Power(v)) => v
            .observation
            .value
            .and_then(|v| v.as_known())
            .and_then(|v| match v {
                wire::PowerValue::On => Some("On"),
                wire::PowerValue::Off => Some("Off"),
                wire::PowerValue::Unspecified => None,
            })
            .map(str::to_owned)
            .unwrap_or_else(missing),
        Some(T::Level(v)) => v
            .observation
            .percentage
            .filter(|n| n.is_finite() && (0.0..=100.0).contains(n))
            .map(|n| format!("{n} %"))
            .unwrap_or_else(missing),
        Some(T::NumericMeasurement(v)) => numeric_text(v).unwrap_or_else(missing),
        Some(T::BinaryObservation(v)) => v
            .observation
            .value
            .map(|v| if v { "Active" } else { "Inactive" }.into())
            .unwrap_or_else(missing),
        Some(T::LightControl(v)) => {
            let mut values = Vec::new();
            if let Some(on) = v.observation.on {
                values.push(if on { "On" } else { "Off" }.to_owned());
            }
            if let Some(level) = v
                .observation
                .brightness_percent
                .filter(|n| n.is_finite() && (0.0..=100.0).contains(n))
            {
                values.push(format!("{level} %"));
            }
            if let Some(color) = v
                .observation
                .color_temperature_mireds
                .and_then(|value| u32::try_from(value).ok())
                .and_then(super::light::Mireds::from_wire)
            {
                values.push(color.observation());
            }
            if values.is_empty() {
                missing()
            } else {
                values.join(" · ")
            }
        }
        Some(T::FanControl(v)) => v
            .observation
            .speed
            .and_then(|speed| speed.as_known())
            .and_then(|speed| match speed {
                wire::FanSpeed::Off => Some("Off"),
                wire::FanSpeed::Low => Some("Low"),
                wire::FanSpeed::Medium => Some("Medium"),
                wire::FanSpeed::MediumHigh => Some("Medium high"),
                wire::FanSpeed::High => Some("High"),
                wire::FanSpeed::Unspecified => None,
            })
            .map(str::to_owned)
            .unwrap_or_else(missing),
        Some(T::Cover(v)) => v
            .observation
            .value
            .as_option()
            .map(crate::control_text::cover)
            .unwrap_or_else(missing),
        Some(T::Lock(v)) => v
            .observation
            .value
            .as_option()
            .map(|v| crate::control_text::lock(v).into())
            .unwrap_or_else(missing),
        Some(T::Alarm(v)) => v
            .observation
            .value
            .as_option()
            .map(|v| crate::control_text::alarm(v).into())
            .unwrap_or_else(missing),
        Some(T::TemperatureControl(v)) => {
            let mut values = Vec::new();
            match v.observation.setpoint.as_ref() {
                Some(wire::temperature_control_observation::Setpoint::SetpointCelsius(value))
                    if value.is_finite() =>
                {
                    values.push(format!("Setpoint {value} °C"))
                }
                Some(wire::temperature_control_observation::Setpoint::SetpointRangeCelsius(
                    value,
                )) if value.__buffa_unknown_fields.is_empty() => {
                    if let Some(low) = value.low_celsius.filter(|v| v.is_finite()) {
                        values.push(format!("Minimum {low} °C"));
                    }
                    if let Some(high) = value.high_celsius.filter(|v| v.is_finite()) {
                        values.push(format!("Maximum {high} °C"));
                    }
                }
                Some(
                    wire::temperature_control_observation::Setpoint::SetpointCelsius(_)
                    | wire::temperature_control_observation::Setpoint::SetpointRangeCelsius(_),
                )
                | None => {}
            }
            if let Some(mode) = v
                .observation
                .mode
                .and_then(|mode| mode.as_known())
                .filter(|mode| *mode != wire::TemperatureMode::Unspecified)
            {
                values.push(super::panel::mode_label(mode).into());
            }
            if let Some(active) = v.observation.fan_active {
                values.push(if active { "Fan active" } else { "Fan inactive" }.into());
            }
            if values.is_empty() {
                missing()
            } else {
                values.join(" · ")
            }
        }
        Some(T::Helper(_)) => super::helper::observation(capability)
            .map(helper_text)
            .unwrap_or_else(missing),
        Some(T::Weather(v)) => v
            .observation
            .value
            .as_option()
            .map(crate::environment_text::weather)
            .unwrap_or_else(missing),
        Some(T::Presence(v)) => v
            .observation
            .value
            .as_option()
            .map(crate::environment_text::presence)
            .unwrap_or_else(missing),
        Some(T::PresenceDetails(v)) => v
            .observation
            .value
            .as_option()
            .map(crate::environment_text::details)
            .unwrap_or_else(missing),
        Some(T::Solar(v)) => {
            let Some(value) = v.observation.value.as_option().filter(|value| {
                home_wire_contract::api_solar_contract::admit_solar_value(value).is_ok()
            }) else {
                return missing();
            };
            let mut values = Vec::new();
            if let Some(value) = value.elevation_degrees {
                values.push(format!("Elevation {value}°"));
            }
            if let Some(value) = value.azimuth_degrees {
                values.push(format!("Azimuth {value}°"));
            }
            if let Some(value) = value.rising {
                values.push(if value { "Rising" } else { "Falling" }.into());
            }
            values.join(" · ")
        }
        Some(T::SolarEvent(v)) => {
            let Some(value) = v.observation.value.as_option().filter(|value| {
                home_wire_contract::api_solar_contract::admit_solar_event_value(value).is_ok()
            }) else {
                return missing();
            };
            match value.occurrence.occurrence.as_ref() {
                Some(wire::solar_occurrence::Occurrence::EventTime(_)) => "Calculated event".into(),
                Some(wire::solar_occurrence::Occurrence::Absent(_)) => {
                    "Event missing from calculation".into()
                }
                None => missing(),
            }
        }
        Some(T::Opaque(_) | T::MediaPlayback(_)) | None => missing(),
    }
}

fn helper_text(value: &wire::HelperValue) -> String {
    use wire::helper_value::Value as V;
    match value.value.as_ref() {
        Some(V::BooleanValue(value)) => if *value { "On" } else { "Off" }.into(),
        Some(V::Number(value)) => crate::cache_view::number(*value),
        Some(V::Counter(value)) => value.to_string(),
        Some(V::SelectedOption(value) | V::Text(value)) => {
            if value.is_empty() {
                "Empty text".into()
            } else {
                crate::cache_view::bounded(value)
            }
        }
        Some(V::Date(date)) => format!("{:04}-{:02}-{:02}", date.year, date.month, date.day),
        Some(V::Time(time)) => format!("{:02}:{:02}:{:02}", time.hours, time.minutes, time.seconds),
        Some(V::LocalValue(value)) => match (value.date.as_option(), value.time.as_option()) {
            (Some(date), Some(time)) => format!(
                "{:04}-{:02}-{:02} {:02}:{:02}:{:02}",
                date.year, date.month, date.day, time.hours, time.minutes, time.seconds
            ),
            (None, _) | (_, None) => "Date or time missing".into(),
        },
        Some(V::TriggerTime(_)) => "Activation observed".into(),
        None => "Value not provided".into(),
    }
}

// Exact source decimal text is a value, never a label or a floating-point
// conversion. Preserve its scale even when the approximate double differs.
fn numeric_text(capability: &wire::NumericMeasurementCapability) -> Option<String> {
    let observation = capability.observation.as_option()?;
    let number = match observation.decimal_value.as_deref() {
        Some(decimal) => {
            let unsigned = decimal.strip_prefix('-').unwrap_or(decimal);
            let (integer, fraction) = unsigned
                .split_once('.')
                .map_or((unsigned, None), |(integer, fraction)| {
                    (integer, Some(fraction))
                });
            if integer.is_empty()
                || !integer.bytes().all(|byte| byte.is_ascii_digit())
                || (integer.len() > 1 && integer.starts_with('0'))
                || fraction.is_some_and(|fraction| {
                    fraction.is_empty() || !fraction.bytes().all(|byte| byte.is_ascii_digit())
                })
            {
                return None;
            }
            crate::cache_view::bounded(decimal)
        }
        None => observation
            .value
            .filter(|value| value.is_finite())?
            .to_string(),
    };
    Some(crate::cache_view::bounded(&format!(
        "{number} {}",
        crate::cache_view::bounded(&capability.unit)
    )))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decimal_reading_keeps_source_precision_without_double_coercion() {
        let mut capability = wire::NumericMeasurementCapability {
            unit: "CAD".into(),
            observation: wire::NumericObservation {
                value: Some(0.0),
                decimal_value: Some("0.0100".into()),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        assert_eq!(numeric_text(&capability).as_deref(), Some("0.0100 CAD"));
        for decimal in ["-12345678901234567890.012300", "0", "-0.00"] {
            capability
                .observation
                .as_option_mut()
                .unwrap()
                .decimal_value = Some(decimal.into());
            assert_eq!(numeric_text(&capability), Some(format!("{decimal} CAD")));
        }
        for decimal in ["", "NaN", "1e4", "01.2", "+1", "1.", ".1", "1.2.3", "1 CAD"] {
            capability
                .observation
                .as_option_mut()
                .unwrap()
                .decimal_value = Some(decimal.into());
            // Invalid exact evidence cannot silently fall back to approximate zero.
            assert_eq!(numeric_text(&capability), None);
        }
        capability
            .observation
            .as_option_mut()
            .unwrap()
            .decimal_value = None;
        assert_eq!(numeric_text(&capability).as_deref(), Some("0 CAD"));
        capability.observation.as_option_mut().unwrap().value = None;
        assert_eq!(numeric_text(&capability), None);
    }
    #[test]
    fn decimal_display_is_bounded_and_marks_truncation_without_rounding() {
        let mut capability = wire::NumericMeasurementCapability {
            unit: "W".into(),
            observation: wire::NumericObservation {
                value: Some(0.0),
                decimal_value: Some(format!("1.{}", "2".repeat(4096))),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        let displayed = numeric_text(&capability).unwrap();
        assert_eq!(displayed.chars().count(), 257);
        assert!(displayed.ends_with('…'));
        assert!(displayed.starts_with("1.2222"));
        capability
            .observation
            .as_option_mut()
            .unwrap()
            .decimal_value = Some("0.0100".into());
        capability.unit = "測".repeat(4096);
        let displayed = numeric_text(&capability).unwrap();
        assert_eq!(displayed.chars().count(), 257);
        assert!(displayed.starts_with("0.0100 測"));
        assert!(displayed.ends_with('…'));
    }

    #[test]
    fn toggle_requires_explicit_current_observed_power() {
        let quality = wire::ObservationQuality {
            availability: wire::Availability::Available.into(),
            freshness: wire::Freshness::Current.into(),
            provenance: wire::Provenance::Observed.into(),
            ..Default::default()
        };
        let mut light = wire::LightControlCapability {
            observation: wire::LightControlObservation {
                on: Some(false),
                brightness_percent: Some(80.0),
                quality: quality.clone().into(),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        let cap = |value| wire::Capability {
            r#trait: Some(wire::capability::Trait::LightControl(Box::new(value))),
            ..Default::default()
        };
        assert_eq!(power(&cap(light.clone())), Some(false));
        light.observation.as_option_mut().unwrap().on = None;
        assert_eq!(
            power(&cap(light.clone())),
            None,
            "brightness cannot supply missing power"
        );
        light.observation.as_option_mut().unwrap().on = Some(true);
        for provenance in [
            wire::Provenance::Restored,
            wire::Provenance::Estimated,
            wire::Provenance::Derived,
        ] {
            light
                .observation
                .as_option_mut()
                .unwrap()
                .quality
                .as_option_mut()
                .unwrap()
                .provenance = provenance.into();
            assert_eq!(power(&cap(light.clone())), None);
        }
        light.observation.as_option_mut().unwrap().quality = quality.into();
        light
            .observation
            .as_option_mut()
            .unwrap()
            .quality
            .as_option_mut()
            .unwrap()
            .freshness = wire::Freshness::Stale.into();
        assert_eq!(power(&cap(light)), None);
    }
}
