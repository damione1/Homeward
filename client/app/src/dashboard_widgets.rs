//! Admitted server-owned widget configuration. No installation selector or
//! implicit first matching resource exists at this boundary.
use crate::dashboard::LayoutFailure;
use buffa::EnumValue;
use home_client_model::{
    api::{CapabilityName, DashboardName, EntityName},
    api_calendars::CalendarName,
    api_tasks::TaskListName,
    dashboard::{Destination, Literal, NodeId, Presentation},
    local_time::DisplayTimeZone,
};
use home_wire_contract::home::api::v2 as wire;
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
    pub entity: EntityName,
    pub capability: CapabilityName,
    pub key: FieldKey,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Observation {
    pub field: Field,
    pub label: String,
    pub active_states: Vec<Literal>,
    pub icon: wire::DashboardIcon,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) enum Content {
    ClockWeather {
        zone: DisplayTimeZone,
        weather: Option<CapabilityName>,
    },
    Room {
        room: crate::dashboard::RoomId,
        temperature: Option<Field>,
        activity: Vec<Activity>,
    },
    Thermostat {
        entity: EntityName,
        current: Field,
        target: Field,
        detailed: bool,
        summary_only: bool,
    },
    Agenda {
        calendar: CalendarName,
        zone: DisplayTimeZone,
        window_seconds: u32,
        limit: usize,
    },
    Tasks {
        list: TaskListName,
        limit: usize,
        shopping_only: bool,
    },
    Security {
        capabilities: Vec<CapabilityName>,
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
    pub fn entities(&self) -> Vec<EntityName> {
        match &self.content {
            Content::ClockWeather { weather, .. } => {
                weather.iter().map(CapabilityName::entity).collect()
            }
            Content::Room {
                temperature,
                activity,
                ..
            } => temperature
                .iter()
                .map(|f| f.entity.clone())
                .chain(activity.iter().map(|a| a.field.entity.clone()))
                .collect(),
            Content::Thermostat {
                entity,
                current,
                target,
                ..
            } => vec![
                entity.clone(),
                current.entity.clone(),
                target.entity.clone(),
            ],
            Content::Security { capabilities } => {
                capabilities.iter().map(CapabilityName::entity).collect()
            }
            Content::Observations { items } => {
                items.iter().map(|i| i.field.entity.clone()).collect()
            }
            _ => Vec::new(),
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
            Content::Security { capabilities } => capabilities.len(),
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
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Activity {
    pub field: Field,
    pub active_states: Vec<Literal>,
}
fn entity(value: &str) -> Result<EntityName, LayoutFailure> {
    EntityName::try_from(value).map_err(|_| LayoutFailure::Invalid)
}
fn capability(value: &str) -> Result<CapabilityName, LayoutFailure> {
    CapabilityName::try_from(value).map_err(|_| LayoutFailure::Invalid)
}
fn field(value: &wire::DashboardFieldBinding) -> Result<Field, LayoutFailure> {
    let parsed = home_wire_contract::dashboard_contract::decode_field(value)
        .map_err(|_| LayoutFailure::Unsupported)?;
    let key = match parsed.field {
        home_client_model::dashboard::FieldKey::State => FieldKey::State,
        home_client_model::dashboard::FieldKey::Value => FieldKey::Value,
        home_client_model::dashboard::FieldKey::CurrentTemperature => FieldKey::CurrentTemperature,
        home_client_model::dashboard::FieldKey::Setpoint => FieldKey::Setpoint,
        home_client_model::dashboard::FieldKey::Measurement => FieldKey::Measurement,
    };
    Ok(Field {
        entity: parsed.capability.entity(),
        capability: parsed.capability,
        key,
    })
}
fn states(values: &[wire::DashboardLiteral]) -> Result<Vec<Literal>, LayoutFailure> {
    if values.len() > 64 {
        return Err(LayoutFailure::Budget);
    };
    let mut result = Vec::new();
    for value in values {
        let value = home_wire_contract::dashboard_contract::decode_literal(value)
            .map_err(|_| LayoutFailure::Unsupported)?;
        if result.contains(&value) {
            return Err(LayoutFailure::Invalid);
        };
        result.push(value)
    }
    Ok(result)
}
pub(crate) fn destination(
    value: &wire::DashboardDestination,
) -> Result<Destination, LayoutFailure> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(LayoutFailure::Unsupported);
    };
    let presentation = match value.presentation {
        EnumValue::Known(wire::DashboardPresentation::Page) => Presentation::Page,
        EnumValue::Known(wire::DashboardPresentation::Popup) => Presentation::Popup,
        _ => return Err(LayoutFailure::Unsupported),
    };
    Ok(Destination {
        dashboard: DashboardName::try_from(value.dashboard.as_str())
            .map_err(|_| LayoutFailure::Invalid)?,
        node: NodeId::parse(&value.node_id).map_err(|_| LayoutFailure::Invalid)?,
        presentation,
    })
}
impl TryFrom<&wire::DashboardWidget> for Widget {
    type Error = LayoutFailure;
    fn try_from(value: &wire::DashboardWidget) -> Result<Self, Self::Error> {
        use wire::dashboard_widget::Content as W;
        if !value.__buffa_unknown_fields.is_empty() {
            return Err(LayoutFailure::Unsupported);
        };
        text(&value.action_label, 256, true)?;
        text(&value.empty_label, 4096, true)?;
        let action = value.action.as_option().map(destination).transpose()?;
        let content = match value.content.as_ref().ok_or(LayoutFailure::Invalid)? {
            W::ClockWeather(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                };
                Content::ClockWeather {
                    zone: v
                        .time_zone
                        .parse()
                        .map_err(|_| LayoutFailure::Unsupported)?,
                    weather: if v.weather_capability.is_empty() {
                        None
                    } else {
                        Some(capability(&v.weather_capability)?)
                    },
                }
            }
            W::Room(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                };
                if v.activity.len() > 4096 {
                    return Err(LayoutFailure::Budget);
                };
                let temperature = v.temperature.as_option().map(field).transpose()?;
                if temperature.as_ref().is_some_and(|f| {
                    !matches!(f.key, FieldKey::CurrentTemperature | FieldKey::Measurement)
                }) {
                    return Err(LayoutFailure::Unsupported);
                };
                let mut activity = Vec::new();
                let mut seen = BTreeSet::new();
                for item in &v.activity {
                    if !item.__buffa_unknown_fields.is_empty() {
                        return Err(LayoutFailure::Unsupported);
                    };
                    let field = field(item.field.as_option().ok_or(LayoutFailure::Invalid)?)?;
                    if !seen.insert(field.capability.clone()) {
                        return Err(LayoutFailure::Invalid);
                    };
                    let active_states = states(&item.active_values)?;
                    if active_states.is_empty() {
                        return Err(LayoutFailure::Invalid);
                    };
                    activity.push(Activity {
                        field,
                        active_states,
                    })
                }
                Content::Room {
                    room: crate::dashboard::RoomId::parse(&v.room_id)?,
                    temperature,
                    activity,
                }
            }
            W::Thermostat(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                };
                let entity = entity(&v.entity)?;
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
                };
                let (detailed, summary_only) = match v.presentation {
                    EnumValue::Known(wire::DashboardThermostatPresentation::Compact) => {
                        (false, false)
                    }
                    EnumValue::Known(wire::DashboardThermostatPresentation::Detail) => {
                        (true, false)
                    }
                    EnumValue::Known(wire::DashboardThermostatPresentation::Summary) => {
                        (false, true)
                    }
                    _ => return Err(LayoutFailure::Unsupported),
                };
                Content::Thermostat {
                    entity,
                    current,
                    target,
                    detailed,
                    summary_only,
                }
            }
            W::Agenda(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                };
                let duration = v.time_window.as_option().ok_or(LayoutFailure::Invalid)?;
                if !(1..=1000).contains(&v.limit)
                    || !(1..=31_622_400).contains(&duration.seconds)
                    || duration.nanos != 0
                    || !duration.__buffa_unknown_fields.is_empty()
                {
                    return Err(LayoutFailure::Budget);
                };
                Content::Agenda {
                    calendar: CalendarName::try_from(v.calendar.as_str())
                        .map_err(|_| LayoutFailure::Invalid)?,
                    zone: v
                        .time_zone
                        .parse()
                        .map_err(|_| LayoutFailure::Unsupported)?,
                    window_seconds: u32::try_from(duration.seconds)
                        .map_err(|_| LayoutFailure::Invalid)?,
                    limit: usize::try_from(v.limit).map_err(|_| LayoutFailure::Invalid)?,
                }
            }
            W::Tasks(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                };
                if !(1..=1000).contains(&v.limit) {
                    return Err(LayoutFailure::Budget);
                };
                let shopping_only = match v.presentation {
                    EnumValue::Known(wire::DashboardTasksPresentation::Preview) => false,
                    EnumValue::Known(wire::DashboardTasksPresentation::Shopping) => true,
                    _ => return Err(LayoutFailure::Unsupported),
                };
                Content::Tasks {
                    list: TaskListName::try_from(v.task_list.as_str())
                        .map_err(|_| LayoutFailure::Invalid)?,
                    limit: usize::try_from(v.limit).map_err(|_| LayoutFailure::Invalid)?,
                    shopping_only,
                }
            }
            W::Security(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                };
                if v.capabilities.len() > 64 {
                    return Err(LayoutFailure::Budget);
                };
                let capabilities = v
                    .capabilities
                    .iter()
                    .map(|v| capability(v))
                    .collect::<Result<Vec<_>, _>>()?;
                if capabilities.iter().collect::<BTreeSet<_>>().len() != capabilities.len() {
                    return Err(LayoutFailure::Invalid);
                };
                Content::Security { capabilities }
            }
            W::Observations(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                };
                if v.items.len() > 64 {
                    return Err(LayoutFailure::Budget);
                };
                let mut items = Vec::new();
                for item in &v.items {
                    if !item.__buffa_unknown_fields.is_empty() {
                        return Err(LayoutFailure::Unsupported);
                    };
                    text(&item.label, 256, false)?;
                    let icon = match item.icon {
                        EnumValue::Known(v) => v,
                        _ => return Err(LayoutFailure::Unsupported),
                    };
                    items.push(Observation {
                        field: field(item.field.as_option().ok_or(LayoutFailure::Invalid)?)?,
                        label: item.label.clone(),
                        active_states: states(&item.active_values)?,
                        icon,
                    })
                }
                Content::Observations { items }
            }
            W::Navigation(v) => {
                if !v.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                };
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
        let wire = wire::DashboardWidget {
            content: Some(wire::dashboard_widget::Content::Tasks(Box::new(
                wire::DashboardTasksWidget {
                    task_list: "homes/synthetic/taskLists/second-not-first".into(),
                    limit: 7,
                    presentation: wire::DashboardTasksPresentation::Preview.into(),
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
        assert_eq!(list.as_str(), "homes/synthetic/taskLists/second-not-first");
        assert_eq!(limit, 7);
        let mut missing = wire.clone();
        if let Some(wire::dashboard_widget::Content::Tasks(v)) = missing.content.as_mut() {
            v.presentation = wire::DashboardTasksPresentation::Shopping.into();
        }
        assert!(matches!(
            Widget::try_from(&missing).unwrap().content,
            Content::Tasks {
                shopping_only: true,
                ..
            }
        ));
        if let Some(wire::dashboard_widget::Content::Tasks(v)) = missing.content.as_mut() {
            v.presentation = EnumValue::Unknown(99);
        }
        assert_eq!(Widget::try_from(&missing), Err(LayoutFailure::Unsupported));
        let mut missing = wire;
        if let Some(wire::dashboard_widget::Content::Tasks(v)) = missing.content.as_mut() {
            v.task_list.clear();
        }
        assert_eq!(Widget::try_from(&missing), Err(LayoutFailure::Invalid));
    }
    #[test]
    fn thermostat_cannot_mix_observation_and_command_entities() {
        let binding = |id: &str, key: wire::DashboardFieldKey| wire::DashboardFieldBinding {
            capability: format!("homes/synthetic/entities/{id}/capabilities/temperature"),
            field: key.into(),
            ..Default::default()
        };
        let mut value = wire::DashboardWidget {
            content: Some(wire::dashboard_widget::Content::Thermostat(Box::new(
                wire::DashboardThermostatWidget {
                    entity: "homes/synthetic/entities/climate-target".into(),
                    presentation: wire::DashboardThermostatPresentation::Compact.into(),
                    current_temperature: binding(
                        "climate-target",
                        wire::DashboardFieldKey::CurrentTemperatureCelsius,
                    )
                    .into(),
                    target_temperature: binding(
                        "climate-other",
                        wire::DashboardFieldKey::SetpointCelsius,
                    )
                    .into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        };
        assert_eq!(Widget::try_from(&value), Err(LayoutFailure::Invalid));
        if let Some(wire::dashboard_widget::Content::Thermostat(v)) = value.content.as_mut() {
            v.target_temperature.as_option_mut().unwrap().capability =
                "homes/synthetic/entities/climate-target/capabilities/temperature".into();
        }
        assert!(Widget::try_from(&value).is_ok());
        if let Some(wire::dashboard_widget::Content::Thermostat(v)) = value.content.as_mut() {
            v.presentation = wire::DashboardThermostatPresentation::Detail.into();
        }
        assert!(matches!(
            Widget::try_from(&value).unwrap().content,
            Content::Thermostat { detailed: true, .. }
        ));
        if let Some(wire::dashboard_widget::Content::Thermostat(v)) = value.content.as_mut() {
            v.presentation = wire::DashboardThermostatPresentation::Summary.into();
        }
        assert!(matches!(
            Widget::try_from(&value).unwrap().content,
            Content::Thermostat {
                detailed: false,
                summary_only: true,
                ..
            }
        ));
        if let Some(wire::dashboard_widget::Content::Thermostat(v)) = value.content.as_mut() {
            v.presentation = EnumValue::Unknown(99);
        }
        assert_eq!(Widget::try_from(&value), Err(LayoutFailure::Unsupported));
    }
    #[test]
    fn unknown_fields_never_become_an_implicit_state_reading() {
        let value = wire::DashboardFieldBinding {
            capability: "homes/synthetic/entities/sensor-explicit/capabilities/measurement".into(),
            field: EnumValue::Unknown(99),
            ..Default::default()
        };
        assert_eq!(field(&value), Err(LayoutFailure::Unsupported));
        let duplicate = wire::DashboardWidget {
            content: Some(wire::dashboard_widget::Content::Security(
                wire::DashboardSecurityWidget {
                    capabilities: vec!["homes/synthetic/entities/one/capabilities/lock".into(); 2],
                    ..Default::default()
                }
                .into(),
            )),
            ..Default::default()
        };
        assert_eq!(Widget::try_from(&duplicate), Err(LayoutFailure::Invalid));
    }
}
