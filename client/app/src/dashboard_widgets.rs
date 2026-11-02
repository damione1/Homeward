//! Admitted server-owned widget configuration. No installation selector or
//! implicit first matching resource exists at this boundary.
use crate::dashboard::LayoutFailure;
use buffa::EnumValue;
use home_client_model::{
    EntityId,
    calendars::CalendarId,
    dashboard::{DashboardId, Destination, NodeId, Presentation},
    lists::ListId,
    local_time::DisplayTimeZone,
};
use home_wire_contract::home::dashboard::v1 as wire;
use std::collections::BTreeSet;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum FieldKey {
    State,
    Value,
    CurrentTemperature,
    Setpoint,
    Measurement,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Field {
    pub entity: EntityId,
    pub key: FieldKey,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Observation {
    pub field: Field,
    pub label: String,
    pub active_states: Vec<String>,
    pub icon: wire::NativeIcon,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) enum Content {
    ClockWeather {
        zone: DisplayTimeZone,
        weather: Option<EntityId>,
    },
    Room {
        room: crate::dashboard::RoomId,
        temperature: Option<Field>,
        activity: Vec<EntityId>,
        active_states: Vec<String>,
    },
    Thermostat {
        entity: EntityId,
        current: Field,
        target: Field,
        detailed: bool,
        summary_only: bool,
    },
    Agenda {
        calendar: CalendarId,
        zone: DisplayTimeZone,
        window_seconds: u32,
        limit: usize,
    },
    Tasks {
        list: ListId,
        limit: usize,
        shopping_only: bool,
    },
    Security {
        entities: Vec<EntityId>,
    },
    Observations {
        items: Vec<Observation>,
    },
    Navigation {
        destination: Destination,
    },
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Widget {
    pub content: Content,
    pub action: Option<Destination>,
    pub action_label: String,
    pub empty_label: String,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Binding {
    pub token: i32,
    pub generation: i32,
    pub node: NodeId,
    pub config: Widget,
}
impl Widget {
    pub fn entities(&self) -> Vec<&EntityId> {
        match &self.content {
            Content::ClockWeather { weather, .. } => weather.iter().collect(),
            Content::Room {
                temperature,
                activity,
                ..
            } => temperature
                .iter()
                .map(|f| &f.entity)
                .chain(activity.iter())
                .collect(),
            Content::Thermostat {
                entity,
                current,
                target,
                ..
            } => vec![entity, &current.entity, &target.entity],
            Content::Security { entities } => entities.iter().collect(),
            Content::Observations { items } => {
                items.iter().map(|item| &item.field.entity).collect()
            }
            Content::Agenda { .. } | Content::Tasks { .. } | Content::Navigation { .. } => {
                Vec::new()
            }
        }
    }
    pub fn destination(&self) -> Option<&Destination> {
        match &self.content {
            Content::Navigation { destination } => Some(destination),
            Content::ClockWeather { .. }
            | Content::Room { .. }
            | Content::Thermostat { .. }
            | Content::Agenda { .. }
            | Content::Tasks { .. }
            | Content::Security { .. }
            | Content::Observations { .. } => self.action.as_ref(),
        }
    }
    pub fn binding_count(&self) -> usize {
        match &self.content {
            Content::ClockWeather { weather, .. } => usize::from(weather.is_some()),
            Content::Room {
                temperature,
                activity,
                ..
            } => usize::from(temperature.is_some()) + activity.len(),
            Content::Thermostat { .. } => 3,
            Content::Agenda { .. } | Content::Tasks { .. } => 1,
            Content::Security { entities } => entities.len(),
            Content::Observations { items } => items.len(),
            Content::Navigation { .. } => 0,
        }
    }
}
fn text(value: &str, maximum: usize, empty: bool) -> Result<(), LayoutFailure> {
    if (!empty && value.is_empty()) || value.len() > maximum || value.chars().any(char::is_control)
    {
        Err(LayoutFailure::Invalid)
    } else {
        Ok(())
    }
}
fn entity(value: &str) -> Result<EntityId, LayoutFailure> {
    text(value, 4096, false)?;
    value.parse().map_err(|_| LayoutFailure::Invalid)
}
fn field(value: &wire::NativeFieldBinding) -> Result<Field, LayoutFailure> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(LayoutFailure::Unsupported);
    }
    let key = match value.field_key.as_str() {
        "state" => FieldKey::State,
        "value" => FieldKey::Value,
        "current_temperature_celsius" => FieldKey::CurrentTemperature,
        "setpoint_celsius" => FieldKey::Setpoint,
        "measurement" => FieldKey::Measurement,
        _ => return Err(LayoutFailure::Unsupported),
    };
    Ok(Field {
        entity: entity(&value.entity_id)?,
        key,
    })
}
fn entities(values: &[String], maximum: usize) -> Result<Vec<EntityId>, LayoutFailure> {
    if values.len() > maximum {
        return Err(LayoutFailure::Budget);
    }
    let result = values
        .iter()
        .map(|value| entity(value))
        .collect::<Result<Vec<_>, _>>()?;
    let unique = result.iter().collect::<BTreeSet<_>>();
    if unique.len() != result.len() {
        return Err(LayoutFailure::Invalid);
    }
    Ok(result)
}
fn states(values: &[String]) -> Result<Vec<String>, LayoutFailure> {
    if values.len() > 64 {
        return Err(LayoutFailure::Budget);
    }
    let mut seen = BTreeSet::new();
    for value in values {
        text(value, 256, false)?;
        if !seen.insert(value) {
            return Err(LayoutFailure::Invalid);
        }
    }
    Ok(values.to_vec())
}
pub(crate) fn destination(value: &wire::NativeDestination) -> Result<Destination, LayoutFailure> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(LayoutFailure::Unsupported);
    }
    let dashboard = value
        .dashboard_id
        .as_option()
        .ok_or(LayoutFailure::Invalid)?;
    if !dashboard.__buffa_unknown_fields.is_empty() {
        return Err(LayoutFailure::Unsupported);
    }
    let presentation = match value.presentation {
        EnumValue::Known(wire::NativePresentation::Page) => Presentation::Page,
        EnumValue::Known(wire::NativePresentation::Popup) => Presentation::Popup,
        EnumValue::Known(wire::NativePresentation::Unspecified) | EnumValue::Unknown(_) => {
            return Err(LayoutFailure::Unsupported);
        }
    };
    Ok(Destination {
        dashboard: DashboardId::parse(&dashboard.value).map_err(|_| LayoutFailure::Invalid)?,
        node: NodeId::parse(&value.node_id).map_err(|_| LayoutFailure::Invalid)?,
        presentation,
    })
}
impl TryFrom<&wire::NativeWidget> for Widget {
    type Error = LayoutFailure;
    fn try_from(value: &wire::NativeWidget) -> Result<Self, Self::Error> {
        use wire::native_widget::Content as W;
        if !value.__buffa_unknown_fields.is_empty() {
            return Err(LayoutFailure::Unsupported);
        }
        text(&value.action_label, 256, true)?;
        text(&value.empty_label, 4096, true)?;
        let action = value.action.as_option().map(destination).transpose()?;
        let content = match value.content.as_ref().ok_or(LayoutFailure::Invalid)? {
            W::ClockWeather(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                }
                Content::ClockWeather {
                    zone: v.timezone.parse().map_err(|_| LayoutFailure::Unsupported)?,
                    weather: (!v.weather_entity_id.is_empty())
                        .then(|| entity(&v.weather_entity_id))
                        .transpose()?,
                }
            }
            W::Room(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                }
                let temperature = v.temperature.as_option().map(field).transpose()?;
                if temperature.as_ref().is_some_and(|f| {
                    !matches!(f.key, FieldKey::CurrentTemperature | FieldKey::Measurement)
                }) {
                    return Err(LayoutFailure::Unsupported);
                }
                let activity = entities(&v.activity_entity_ids, 4096)?;
                let active_states = states(&v.active_states)?;
                if !activity.is_empty() && active_states.is_empty() {
                    return Err(LayoutFailure::Invalid);
                }
                Content::Room {
                    room: crate::dashboard::RoomId::parse(&v.room_id)?,
                    temperature,
                    activity,
                    active_states,
                }
            }
            W::Thermostat(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                }
                let entity = entity(&v.entity_id)?;
                let current = field(
                    v.current_temperature
                        .as_option()
                        .ok_or(LayoutFailure::Invalid)?,
                )?;
                let target = field(
                    v.target_temperature
                        .as_option()
                        .ok_or(LayoutFailure::Invalid)?,
                )?;
                if current.entity != entity
                    || target.entity != entity
                    || current.key != FieldKey::CurrentTemperature
                    || target.key != FieldKey::Setpoint
                {
                    return Err(LayoutFailure::Invalid);
                }
                Content::Thermostat {
                    entity,
                    current,
                    target,
                    detailed: match v.presentation {
                        EnumValue::Known(wire::NativeThermostatPresentation::Compact) => false,
                        EnumValue::Known(wire::NativeThermostatPresentation::Detail) => true,
                        EnumValue::Known(wire::NativeThermostatPresentation::Summary) => false,
                        EnumValue::Unknown(_) => return Err(LayoutFailure::Unsupported),
                    },
                    summary_only: v.presentation == wire::NativeThermostatPresentation::Summary,
                }
            }
            W::Agenda(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                }
                if !(1..=1000).contains(&v.limit) || !(1..=31_622_400).contains(&v.window_seconds) {
                    return Err(LayoutFailure::Budget);
                }
                Content::Agenda {
                    calendar: CalendarId::new(entity(&v.calendar_id)?),
                    zone: v.timezone.parse().map_err(|_| LayoutFailure::Unsupported)?,
                    window_seconds: v.window_seconds,
                    limit: v.limit as usize,
                }
            }
            W::Tasks(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                }
                if !(1..=1000).contains(&v.limit) {
                    return Err(LayoutFailure::Budget);
                }
                Content::Tasks {
                    list: v.list_id.parse().map_err(|_| LayoutFailure::Invalid)?,
                    limit: v.limit as usize,
                    shopping_only: match v.presentation {
                        EnumValue::Known(wire::NativeTasksPresentation::Preview) => false,
                        EnumValue::Known(wire::NativeTasksPresentation::Shopping) => true,
                        EnumValue::Unknown(_) => return Err(LayoutFailure::Unsupported),
                    },
                }
            }
            W::Security(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                }
                Content::Security {
                    entities: entities(&v.entity_ids, 64)?,
                }
            }
            W::Observations(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                }
                if v.items.len() > 64 {
                    return Err(LayoutFailure::Budget);
                }
                let items = v
                    .items
                    .iter()
                    .map(|item| {
                        if !item.__buffa_unknown_fields.is_empty() {
                            return Err(LayoutFailure::Unsupported);
                        }
                        text(&item.label, 256, false)?;
                        Ok(Observation {
                            field: field(item.field.as_option().ok_or(LayoutFailure::Invalid)?)?,
                            label: item.label.clone(),
                            active_states: states(&item.active_states)?,
                            icon: match item.icon {
                                EnumValue::Known(icon) => icon,
                                EnumValue::Unknown(_) => return Err(LayoutFailure::Unsupported),
                            },
                        })
                    })
                    .collect::<Result<Vec<_>, LayoutFailure>>()?;
                Content::Observations { items }
            }
            W::Navigation(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                }
                Content::Navigation {
                    destination: destination(
                        v.destination.as_option().ok_or(LayoutFailure::Invalid)?,
                    )?,
                }
            }
        };
        Ok(Self {
            content,
            action,
            action_label: value.action_label.clone(),
            empty_label: value.empty_label.clone(),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn task_source_and_limit_are_exact_server_values() {
        let wire = wire::NativeWidget {
            content: Some(wire::native_widget::Content::Tasks(Box::new(
                wire::NativeTasksWidget {
                    list_id: "tasks.second-not-first".into(),
                    limit: 7,
                    ..Default::default()
                },
            ))),
            ..Default::default()
        };
        let Widget {
            content: Content::Tasks { list, limit, .. },
            ..
        } = Widget::try_from(&wire).unwrap()
        else {
            panic!("task widget");
        };
        assert_eq!(list.as_str(), "tasks.second-not-first");
        assert_eq!(limit, 7);
        let mut missing = wire.clone();
        if let Some(wire::native_widget::Content::Tasks(v)) = missing.content.as_mut() {
            v.presentation = wire::NativeTasksPresentation::Shopping.into();
        }
        assert!(matches!(
            Widget::try_from(&missing).unwrap().content,
            Content::Tasks {
                shopping_only: true,
                ..
            }
        ));
        if let Some(wire::native_widget::Content::Tasks(v)) = missing.content.as_mut() {
            v.presentation = EnumValue::Unknown(99);
        }
        assert_eq!(Widget::try_from(&missing), Err(LayoutFailure::Unsupported));
        let mut missing = wire;
        if let Some(wire::native_widget::Content::Tasks(v)) = missing.content.as_mut() {
            v.list_id.clear();
        }
        assert_eq!(Widget::try_from(&missing), Err(LayoutFailure::Invalid));
    }
    #[test]
    fn thermostat_cannot_mix_observation_and_command_entities() {
        let binding = |id: &str, key: &str| wire::NativeFieldBinding {
            entity_id: id.into(),
            field_key: key.into(),
            ..Default::default()
        };
        let mut value = wire::NativeWidget {
            content: Some(wire::native_widget::Content::Thermostat(Box::new(
                wire::NativeThermostatWidget {
                    entity_id: "climate.target".into(),
                    current_temperature: binding("climate.target", "current_temperature_celsius")
                        .into(),
                    target_temperature: binding("climate.other", "setpoint_celsius").into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        };
        assert_eq!(Widget::try_from(&value), Err(LayoutFailure::Invalid));
        if let Some(wire::native_widget::Content::Thermostat(v)) = value.content.as_mut() {
            v.target_temperature.as_option_mut().unwrap().entity_id = "climate.target".into();
        }
        assert!(Widget::try_from(&value).is_ok());
        if let Some(wire::native_widget::Content::Thermostat(v)) = value.content.as_mut() {
            v.presentation = wire::NativeThermostatPresentation::Detail.into();
        }
        assert!(matches!(
            Widget::try_from(&value).unwrap().content,
            Content::Thermostat { detailed: true, .. }
        ));
        if let Some(wire::native_widget::Content::Thermostat(v)) = value.content.as_mut() {
            v.presentation = wire::NativeThermostatPresentation::Summary.into();
        }
        assert!(matches!(
            Widget::try_from(&value).unwrap().content,
            Content::Thermostat {
                detailed: false,
                summary_only: true,
                ..
            }
        ));
        if let Some(wire::native_widget::Content::Thermostat(v)) = value.content.as_mut() {
            v.presentation = EnumValue::Unknown(99);
        }
        assert_eq!(Widget::try_from(&value), Err(LayoutFailure::Unsupported));
    }
    #[test]
    fn unknown_fields_never_become_an_implicit_state_reading() {
        let value = wire::NativeFieldBinding {
            entity_id: "sensor.explicit".into(),
            field_key: "pick-first-temperature".into(),
            ..Default::default()
        };
        assert_eq!(field(&value), Err(LayoutFailure::Unsupported));
        assert_eq!(
            entities(&["one".into(), "one".into()], 64),
            Err(LayoutFailure::Invalid)
        );
    }
}
