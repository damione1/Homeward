//! Read-only environmental payload admission. No command or provider access.
use crate::home::environment::v1 as env;
use buffa::EnumValue;
use buffa_types::google::protobuf::Timestamp;
use home_client_model::local_time::{DisplayTimeZone, UtcInstant};

pub const MAX_FORECAST_PERIODS: usize = 256;

fn instant(value: &Timestamp) -> Option<UtcInstant> {
    if !value.__buffa_unknown_fields.is_empty()
        || (value.seconds == -62_135_596_800 && value.nanos == 0)
    {
        return None;
    }
    UtcInstant::new(value.seconds, value.nanos).ok()
}

pub fn display_profile(value: &env::DisplayProfile) -> bool {
    value.__buffa_unknown_fields.is_empty() && value.time_zone.parse::<DisplayTimeZone>().is_ok()
}

pub fn condition(value: EnumValue<env::WeatherCondition>) -> bool {
    use env::WeatherCondition as C;
    match value {
        EnumValue::Known(
            C::Unknown
            | C::ClearDay
            | C::ClearNight
            | C::Cloudy
            | C::PartlyCloudy
            | C::Rainy
            | C::Pouring
            | C::Snowy
            | C::SnowyRainy
            | C::Hail
            | C::Lightning
            | C::LightningRainy
            | C::Windy
            | C::WindyVariant
            | C::Fog
            | C::Exceptional,
        ) => true,
        EnumValue::Known(C::Unspecified) | EnumValue::Unknown(_) => false,
    }
}

pub fn temperature(value: &env::Temperature) -> bool {
    if !value.__buffa_unknown_fields.is_empty() {
        return false;
    }
    let Some(number) = value.value.filter(|v| v.is_finite()) else {
        return false;
    };
    match value.unit {
        EnumValue::Known(env::TemperatureUnit::Celsius) => number >= -273.15,
        EnumValue::Known(env::TemperatureUnit::Fahrenheit) => number >= -459.67,
        EnumValue::Known(env::TemperatureUnit::Kelvin) => number >= 0.0,
        EnumValue::Known(env::TemperatureUnit::Unspecified) | EnumValue::Unknown(_) => false,
    }
}

fn percent(value: f64) -> bool {
    value.is_finite() && (0.0..=100.0).contains(&value)
}

fn wind(value: &env::Wind) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value.speed.is_some_and(|v| v.is_finite() && v >= 0.0)
        && value
            .bearing_degrees
            .is_none_or(|v| v.is_finite() && (0.0..=360.0).contains(&v))
        && match value.unit {
            EnumValue::Known(
                env::SpeedUnit::MetersPerSecond
                | env::SpeedUnit::KilometersPerHour
                | env::SpeedUnit::MilesPerHour
                | env::SpeedUnit::Knots,
            ) => true,
            EnumValue::Known(env::SpeedUnit::Unspecified) | EnumValue::Unknown(_) => false,
        }
}

fn precipitation(value: &env::Precipitation) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value.amount.is_some_and(|v| v.is_finite() && v >= 0.0)
        && match value.unit {
            EnumValue::Known(
                env::PrecipitationUnit::Millimeters | env::PrecipitationUnit::Inches,
            ) => true,
            EnumValue::Known(env::PrecipitationUnit::Unspecified) | EnumValue::Unknown(_) => false,
        }
}

pub fn forecast_period(value: &env::ForecastPeriod) -> bool {
    if !value.__buffa_unknown_fields.is_empty()
        || !value
            .starts_at
            .as_option()
            .and_then(instant)
            .zip(value.ends_at.as_option().and_then(instant))
            .is_some_and(|(a, b)| a < b)
        || !value.condition.is_none_or(condition)
        || !value.temperature.as_option().is_none_or(temperature)
        || !value.low.as_option().is_none_or(temperature)
        || !value.high.as_option().is_none_or(temperature)
        || !value.precipitation_probability_percent.is_none_or(percent)
        || !value.precipitation.as_option().is_none_or(precipitation)
    {
        return false;
    }
    let kind = match value.kind {
        EnumValue::Known(env::ForecastKind::Hourly | env::ForecastKind::Daily) => true,
        EnumValue::Known(env::ForecastKind::Unspecified) | EnumValue::Unknown(_) => false,
    };
    let range = match (value.low.as_option(), value.high.as_option()) {
        (Some(low), Some(high)) => {
            low.unit == high.unit && low.value.zip(high.value).is_some_and(|(a, b)| a <= b)
        }
        (Some(_), None) | (None, Some(_)) | (None, None) => true,
    };
    kind && range
}

pub fn forecast(value: &env::Forecast) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && !value.periods.is_empty()
        && value.periods.len() <= MAX_FORECAST_PERIODS
        && value
            .issued_at
            .as_option()
            .and_then(instant)
            .zip(value.valid_until.as_option().and_then(instant))
            .is_some_and(|(a, b)| a < b)
        && value.periods.iter().all(forecast_period)
        && value.periods.windows(2).all(|pair| {
            pair[0]
                .ends_at
                .as_option()
                .and_then(instant)
                .zip(pair[1].starts_at.as_option().and_then(instant))
                .is_some_and(|(a, b)| a <= b)
        })
}

pub fn weather_value(value: &env::WeatherValue) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value.source_label.len() <= 160
        && (value.condition.is_some()
            || value.temperature.as_option().is_some()
            || value.humidity_percent.is_some()
            || value.wind.as_option().is_some()
            || value.forecast.as_option().is_some())
        && value.condition.is_none_or(condition)
        && value.temperature.as_option().is_none_or(temperature)
        && value.humidity_percent.is_none_or(percent)
        && value.wind.as_option().is_none_or(wind)
        && value.forecast.as_option().is_none_or(forecast)
}

fn identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 256
}

pub fn presence_value(value: &env::PresenceValue) -> bool {
    use env::{PresenceConfidence as C, Residence as R};
    value.__buffa_unknown_fields.is_empty()
        && identity(&value.person_id)
        && value.source_label.len() <= 160
        && match value.residence {
            EnumValue::Known(R::Home | R::Away | R::Unknown) => true,
            EnumValue::Known(R::Unspecified) | EnumValue::Unknown(_) => false,
        }
        && match value.confidence {
            EnumValue::Known(C::Unknown | C::Low | C::Medium | C::High) => true,
            EnumValue::Known(C::Unspecified) | EnumValue::Unknown(_) => false,
        }
}

fn coordinates(value: &env::Coordinates) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value
            .latitude_degrees
            .is_some_and(|v| v.is_finite() && (-90.0..=90.0).contains(&v))
        && value
            .longitude_degrees
            .is_some_and(|v| v.is_finite() && (-180.0..=180.0).contains(&v))
        && value
            .accuracy_meters
            .is_none_or(|v| v.is_finite() && v >= 0.0)
}

pub fn presence_details_value(value: &env::PresenceDetailsValue) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && identity(&value.person_id)
        && value.source_label.len() <= 160
        && value.zone.as_option().is_none_or(|v| {
            v.__buffa_unknown_fields.is_empty()
                && identity(&v.zone_id)
                && !v.label.is_empty()
                && v.label.len() <= 160
        })
        && value.coordinates.as_option().is_none_or(coordinates)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn weather_zero_is_observed_but_missing_value_and_unknown_unit_are_not() {
        let mut value = env::Temperature {
            value: Some(0.0),
            unit: env::TemperatureUnit::Celsius.into(),
            ..Default::default()
        };
        assert!(temperature(&value));
        value.value = None;
        assert!(!temperature(&value));
        value.value = Some(0.0);
        value.unit = EnumValue::Unknown(255);
        assert!(!temperature(&value));
        assert!(condition(env::WeatherCondition::Unknown.into()));
        assert!(!condition(EnumValue::Unknown(255)));
    }

    #[test]
    fn aggregate_unknown_does_not_become_away_and_partial_coordinates_are_invalid() {
        let mut value = env::PresenceValue {
            person_id: "synthetic-person".into(),
            residence: env::Residence::Unknown.into(),
            confidence: env::PresenceConfidence::Unknown.into(),
            ..Default::default()
        };
        assert!(presence_value(&value));
        value.residence = EnumValue::Unknown(255);
        assert!(!presence_value(&value));
        let mut position = env::Coordinates {
            latitude_degrees: Some(0.0),
            ..Default::default()
        };
        assert!(!coordinates(&position));
        position.longitude_degrees = Some(0.0);
        position.accuracy_meters = Some(0.0);
        assert!(coordinates(&position));
        position.latitude_degrees = Some(f64::NAN);
        assert!(!coordinates(&position));
    }

    #[test]
    fn profile_uses_an_explicit_supported_iana_zone() {
        assert!(display_profile(&env::DisplayProfile {
            time_zone: "America/Toronto".into(),
            ..Default::default()
        }));
        for name in ["", "Local", "Unknown/Zone"] {
            assert!(!display_profile(&env::DisplayProfile {
                time_zone: name.into(),
                ..Default::default()
            }));
        }
    }

    #[test]
    fn forecasts_are_bounded_ordered_and_keep_absolute_periods() {
        fn stamp(seconds: i64) -> Timestamp {
            Timestamp {
                seconds,
                nanos: 42,
                ..Default::default()
            }
        }
        let mut value = env::Forecast {
            issued_at: stamp(100).into(),
            valid_until: stamp(3700).into(),
            periods: (0..MAX_FORECAST_PERIODS)
                .map(|index| {
                    let starts = 100 + index as i64 * 3600;
                    env::ForecastPeriod {
                        starts_at: stamp(starts).into(),
                        ends_at: stamp(starts + 3600).into(),
                        kind: env::ForecastKind::Hourly.into(),
                        ..Default::default()
                    }
                })
                .collect(),
            ..Default::default()
        };
        assert!(forecast(&value));
        let starts = 100 + MAX_FORECAST_PERIODS as i64 * 3600;
        value.periods.push(env::ForecastPeriod {
            starts_at: stamp(starts).into(),
            ends_at: stamp(starts + 3600).into(),
            kind: env::ForecastKind::Hourly.into(),
            ..Default::default()
        });
        assert!(!forecast(&value));
        value.periods.pop();
        value.periods[1].starts_at = stamp(200).into();
        assert!(!forecast(&value));
        value.periods.truncate(1);
        value.periods[0].kind = env::ForecastKind::Daily.into();
        value.periods[0].ends_at = stamp(100 + 25 * 3600).into();
        assert!(forecast(&value));
    }
}
