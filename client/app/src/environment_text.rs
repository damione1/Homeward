//! Labels for independent read-only fields; unknown units never become °C.
use crate::control_text::StateText;
use buffa::EnumValue;
use home_wire_contract::{environment_contract as admit, home::environment::v1 as env};

fn condition(value: EnumValue<env::WeatherCondition>) -> Option<StateText> {
    use env::WeatherCondition as C;
    let (label, literal) = match value {
        EnumValue::Known(C::Unknown) => ("Conditions inconnues", "unknown"),
        EnumValue::Known(C::ClearDay) => ("Ensoleillé", "sunny"),
        EnumValue::Known(C::ClearNight) => ("Nuit dégagée", "clear-night"),
        EnumValue::Known(C::Cloudy) => ("Nuageux", "cloudy"),
        EnumValue::Known(C::PartlyCloudy) => ("Partiellement nuageux", "partlycloudy"),
        EnumValue::Known(C::Rainy) => ("Pluie", "rainy"),
        EnumValue::Known(C::Pouring) => ("Forte pluie", "pouring"),
        EnumValue::Known(C::Snowy) => ("Neige", "snowy"),
        EnumValue::Known(C::SnowyRainy) => ("Pluie et neige", "snowy-rainy"),
        EnumValue::Known(C::Hail) => ("Grêle", "hail"),
        EnumValue::Known(C::Lightning) => ("Orage", "lightning"),
        EnumValue::Known(C::LightningRainy) => ("Pluie et orage", "lightning-rainy"),
        EnumValue::Known(C::Windy) => ("Venteux", "windy"),
        EnumValue::Known(C::WindyVariant) => ("Vent et nuages", "windy-variant"),
        EnumValue::Known(C::Fog) => ("Brouillard", "fog"),
        EnumValue::Known(C::Exceptional) => ("Conditions exceptionnelles", "exceptional"),
        EnumValue::Known(C::Unspecified) | EnumValue::Unknown(_) => return None,
    };
    Some(StateText { label, literal })
}

pub(crate) fn weather_literal(value: &env::WeatherValue) -> Option<&'static str> {
    value.condition.and_then(condition).map(|v| v.literal)
}
pub(crate) fn presence_literal(value: &env::PresenceValue) -> Option<&'static str> {
    if !admit::presence_value(value) {
        return None;
    }
    match value.residence {
        EnumValue::Known(env::Residence::Home) => Some("home"),
        EnumValue::Known(env::Residence::Away) => Some("not_home"),
        EnumValue::Known(env::Residence::Unknown) => Some("unknown"),
        EnumValue::Known(env::Residence::Unspecified) | EnumValue::Unknown(_) => None,
    }
}

pub(crate) fn temperature(value: &env::Temperature) -> String {
    if !admit::temperature(value) {
        return "Température non prise en charge".into();
    }
    let unit = match value.unit {
        EnumValue::Known(env::TemperatureUnit::Celsius) => "°C",
        EnumValue::Known(env::TemperatureUnit::Fahrenheit) => "°F",
        EnumValue::Known(env::TemperatureUnit::Kelvin) => "K",
        EnumValue::Known(env::TemperatureUnit::Unspecified) | EnumValue::Unknown(_) => {
            return "Température non prise en charge".into();
        }
    };
    format!("{} {unit}", crate::cache_view::number(value.value.unwrap()))
}

pub(crate) fn weather(value: &env::WeatherValue) -> String {
    let mut parts = Vec::new();
    if let Some(value) = value.temperature.as_option() {
        parts.push(temperature(value));
    }
    if let Some(value) = value.condition {
        parts.push(
            condition(value)
                .map_or("Condition non prise en charge", |v| v.label)
                .into(),
        );
    }
    if parts.is_empty() {
        "Météo non renseignée".into()
    } else {
        parts.join(" · ")
    }
}
pub(crate) fn presence(value: &env::PresenceValue) -> String {
    let label = match value.residence {
        EnumValue::Known(env::Residence::Home) => "À la maison",
        EnumValue::Known(env::Residence::Away) => "Absent",
        EnumValue::Known(env::Residence::Unknown) => "Présence inconnue",
        EnumValue::Known(env::Residence::Unspecified) | EnumValue::Unknown(_) => {
            "Présence non prise en charge"
        }
    };
    label.into()
}
pub(crate) fn details(value: &env::PresenceDetailsValue) -> String {
    if !admit::presence_details_value(value) {
        return "Localisation non prise en charge".into();
    }
    if let Some(zone) = value.zone.as_option() {
        return zone.label.clone();
    }
    if value.coordinates.as_option().is_some() {
        "Position privée disponible".into()
    } else {
        "Zone non fournie".into()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unknown_temperature_unit_does_not_hide_known_weather_or_invent_celsius() {
        let value = env::WeatherValue {
            condition: Some(env::WeatherCondition::Rainy.into()),
            temperature: env::Temperature {
                value: Some(0.0),
                unit: EnumValue::Unknown(255),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        let text = weather(&value);
        assert!(text.contains("non prise en charge") && text.contains("Pluie"));
        assert!(!text.contains("°C"));
        assert_eq!(weather_literal(&value), Some("rainy"));
        assert_eq!(
            temperature(&env::Temperature {
                value: Some(0.0),
                unit: env::TemperatureUnit::Fahrenheit.into(),
                ..Default::default()
            }),
            "0 °F"
        );
    }

    #[test]
    fn unknown_presence_and_private_details_do_not_infer_away_or_publish_coordinates() {
        let mut value = env::PresenceValue {
            person_id: "synthetic-person".into(),
            residence: env::Residence::Unknown.into(),
            confidence: env::PresenceConfidence::Unknown.into(),
            ..Default::default()
        };
        assert_eq!(presence(&value), "Présence inconnue");
        assert_eq!(presence_literal(&value), Some("unknown"));
        value.residence = EnumValue::Unknown(255);
        assert_eq!(presence_literal(&value), None);
        let private = env::PresenceDetailsValue {
            person_id: "synthetic-person".into(),
            coordinates: env::Coordinates {
                latitude_degrees: Some(12.3456),
                longitude_degrees: Some(-45.6789),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        assert_eq!(details(&private), "Position privée disponible");
        assert!(!details(&private).contains("12.3456"));
    }
}
