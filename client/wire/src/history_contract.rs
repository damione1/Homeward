//! Bounded structural history admission. These values grant no rights; the
//! existing server authority, trusted catalogue and leased transport admit reads.
use crate::home::api::v2 as history;
use crate::recorder_evidence_contract::{decimal, number};
use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::Timestamp;
use home_client_model::api::{EntityName, HomeName};
use home_client_model::api_history::{HistoryControllerName, HistoryRunName};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AdmissionError {
    Invalid,
}
pub struct Admitted<T> {
    wire: T,
}
impl<T> Admitted<T> {
    pub fn wire(&self) -> &T {
        &self.wire
    }
}
fn admit<T: Clone>(wire: &T, valid: bool) -> Result<Admitted<T>, AdmissionError> {
    if valid {
        Ok(Admitted { wire: wire.clone() })
    } else {
        Err(AdmissionError::Invalid)
    }
}
fn timestamp(v: &Timestamp) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && (-62_135_596_800..=253_402_300_799).contains(&v.seconds)
        && (0..1_000_000_000).contains(&v.nanos)
        && (v.seconds != -62_135_596_800 || v.nanos != 0)
}
fn period(v: &history::HistoryPeriod, hours: i64, aligned: bool) -> bool {
    if !v.__buffa_unknown_fields.is_empty() {
        return false;
    }
    let (Some(start), Some(end)) = (v.start.as_option(), v.end.as_option()) else {
        return false;
    };
    if !timestamp(start)
        || !timestamp(end)
        || (start.seconds, start.nanos) >= (end.seconds, end.nanos)
    {
        return false;
    }
    if end.seconds - start.seconds > hours * 3600 {
        return false;
    }
    let span = (end.seconds - start.seconds) * 1_000_000_000 + i64::from(end.nanos)
        - i64::from(start.nanos);
    span <= hours * 3600 * 1_000_000_000
        && (!aligned
            || start.seconds % 3600 == 0
                && end.seconds % 3600 == 0
                && start.nanos == 0
                && end.nanos == 0)
}
fn page(size: i32, token: &str) -> bool {
    size >= 0 && token.len() <= 512
}

fn nonzero<E: Copy + PartialEq + buffa::Enumeration>(value: EnumValue<E>, zero: E) -> bool {
    match value {
        EnumValue::Known(value) => value != zero,
        EnumValue::Unknown(_) => false,
    }
}
pub fn admit_observations_request(
    v: &history::QueryObservationsRequest,
) -> Result<Admitted<history::QueryObservationsRequest>, AdmissionError> {
    admit(
        v,
        v.__buffa_unknown_fields.is_empty()
            && EntityName::try_from(v.parent.as_str()).is_ok()
            && v.period.as_option().is_some_and(|p| period(p, 168, false))
            && page(v.page_size, &v.page_token),
    )
}
pub fn admit_aggregates_request(
    v: &history::QueryAggregatesRequest,
) -> Result<Admitted<history::QueryAggregatesRequest>, AdmissionError> {
    admit(
        v,
        v.__buffa_unknown_fields.is_empty()
            && EntityName::try_from(v.parent.as_str()).is_ok()
            && v.period.as_option().is_some_and(|p| period(p, 500, true))
            && page(v.page_size, &v.page_token),
    )
}
pub fn admit_traces_request(
    v: &history::QueryExecutionTracesRequest,
) -> Result<Admitted<history::QueryExecutionTracesRequest>, AdmissionError> {
    admit(
        v,
        v.__buffa_unknown_fields.is_empty()
            && HomeName::try_from(v.parent.as_str()).is_ok()
            && v.execution_epoch.len() <= 512
            && v.period.as_option().is_some_and(|p| period(p, 168, false))
            && page(v.page_size, &v.page_token),
    )
}
fn descriptor(v: &history::HistoryNumericDescriptor) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && !v.unit.is_empty()
        && !v.unit.chars().any(|c| c.is_whitespace() || c.is_control())
        && v.unit.len() <= 156 * 1024
        && !v.canonical_unit.is_empty()
        && v.canonical_unit.len() <= 156 * 1024
        && !v
            .canonical_unit
            .chars()
            .any(|c| c.is_whitespace() || c.is_control())
        && v.scale.is_finite()
        && v.scale > 0.0
        && v.offset.is_finite()
        && nonzero(v.dimension, history::HistoryDimension::Unspecified)
        && (-308..=308).contains(&v.precision)
        && nonzero(v.semantic, history::MeasurementSemantic::Unspecified)
        && nonzero(v.aggregation, history::MeasurementAggregation::Unspecified)
}
fn quality(v: &history::ObservationQuality) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && nonzero(v.availability, history::Availability::Unspecified)
        && nonzero(v.freshness, history::Freshness::Unspecified)
        && nonzero(v.provenance, history::Provenance::Unspecified)
}

fn scope(v: &history::HistoryReadScope) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && v.requested
            .as_option()
            .is_some_and(|p| period(p, 500, false))
        && v.retained.as_option().is_none_or(|p| period(p, 500, false))
        && v.not_retained_prefix
            .as_option()
            .is_none_or(|p| period(p, 500, false))
        && v.retention_cutoff.as_option().is_some_and(timestamp)
        && v.as_of.as_option().is_some_and(timestamp)
        && nonzero(v.resolution, history::HistoryResolution::Unspecified)
        && v.limits.as_option().is_some_and(|l| {
            l.__buffa_unknown_fields.is_empty()
                && l.max_response_bytes == 256 * 1024
                && l.max_token_bytes == 512
                && l.query_timeout_millis == 2000
                && l.max_readers == 2
                && (l.max_points == 100 || l.max_points == 31)
        })
}
fn coverage(v: &history::HistoryCoverage) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && v.verdict == history::HistoryCoverageVerdict::NotProven
        && v.provenance_started_at.as_option().is_none_or(timestamp)
}

fn aggregate(v: &history::HistoryAggregate) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && v.aggregate_id.len() == 64
        && v.aggregate_id
            .bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
        && v.bucket.as_option().is_some_and(|p| period(p, 1, true))
        && v.descriptor.as_option().is_some_and(descriptor)
        && v.quality.as_option().is_some_and(quality)
        && (0..=3_600_000_000_000).contains(&v.duration_nanos)
        && v.minimum.is_some() == v.maximum.is_some()
        && v.mean.is_some() == (v.duration_nanos > 0)
        && [v.minimum, v.maximum, v.mean, v.contribution]
            .iter()
            .all(|x| x.is_none_or(f64::is_finite))
        && nonzero(v.reset, history::HistoryReset::Unspecified)
        && v.reasons.len() <= 12
        && v.reasons
            .iter()
            .all(|v| nonzero(*v, history::HistoryAggregateReason::Unspecified))
}

fn historical_state(v: &history::HistoricalState) -> bool {
    use history::historical_state::Value;
    if !v.__buffa_unknown_fields.is_empty()
        || !v.quality.as_option().is_some_and(quality)
        || ![
            &v.receive_time,
            &v.last_change_time,
            &v.last_update_time,
            &v.last_report_time,
        ]
        .iter()
        .all(|t| t.as_option().is_some_and(timestamp))
        || !v.observed_time.as_option().is_none_or(timestamp)
        || (v.quality.as_option().unwrap().availability == history::Availability::Available)
            != v.value.is_some()
    {
        return false;
    }
    match &v.value {
        Some(Value::SwitchOn(_)) | None => true,
        Some(Value::Measurement(m)) => {
            m.__buffa_unknown_fields.is_empty()
                && m.value.is_finite()
                && !m.unit.is_empty()
                && m.decimal_value.as_ref().is_none_or(|d| !d.is_empty())
        }
        Some(Value::Light(l)) => {
            l.__buffa_unknown_fields.is_empty()
                && l.on.is_some()
                && l.brightness_percent
                    .is_none_or(|v| v.is_finite() && (0.0..=100.0).contains(&v))
                && l.color_temp_mireds.is_none_or(|v| (1..=1000).contains(&v))
                && !(l.color_temp_mireds.is_some() && l.rgb.is_set())
                && l.rgb
                    .as_option()
                    .is_none_or(|v| crate::api_contract::parse_rgb(v).is_ok())
                && l.preset
                    .as_deref()
                    .is_none_or(|s| home_client_model::api::SelectorKey::try_from(s).is_ok())
        }
        Some(Value::Fan(f)) => {
            f.__buffa_unknown_fields.is_empty()
                && (f.on.is_some() || f.speed.is_some() || f.level.is_some())
                && !(f.speed.is_some() && f.level.is_some())
                && f.level.is_none_or(f64::is_finite)
                && f.speed
                    .as_deref()
                    .is_none_or(|s| home_client_model::api::SelectorKey::try_from(s).is_ok())
        }
        Some(Value::TemperatureControl(c)) => climate(c),
        Some(Value::Helper(h)) => helper(h),
        Some(Value::Solar(s)) => crate::api_solar_contract::admit_solar_value(s).is_ok(),
        Some(Value::SolarEvent(s)) => crate::api_solar_contract::admit_solar_event_value(s).is_ok(),
        Some(Value::Cover(value)) => crate::api_controls_contract::cover_value(value),
        Some(Value::Lock(value)) => crate::api_controls_contract::lock_value(value),
        Some(Value::Alarm(value)) => crate::api_controls_contract::alarm_value(value),
        Some(Value::Weather(value)) => crate::api_environment_contract::weather_value(value),
        Some(Value::Presence(value)) => crate::api_environment_contract::presence_value(value),
        Some(Value::PresenceDetails(value)) => {
            crate::api_environment_contract::presence_details_value(value)
        }
    }
}
fn climate(v: &history::HistoricalTemperatureControl) -> bool {
    use history::historical_temperature_control::Setpoint;
    v.__buffa_unknown_fields.is_empty()
        && (v.current_temperature_celsius.is_some()
            || v.setpoint.is_some()
            || v.mode.is_some()
            || v.activity.is_some()
            || v.fan_active.is_some()
            || v.humidity_percent.is_some()
            || v.fan_target.is_some())
        && v.current_temperature_celsius
            .is_none_or(|t| t.is_finite() && t >= -273.15)
        && v.mode
            .is_none_or(|v| nonzero(v, history::TemperatureMode::Unspecified))
        && v.activity
            .is_none_or(|v| nonzero(v, history::TemperatureActivity::Unspecified))
        && v.humidity_percent
            .is_none_or(|v| v.is_finite() && (0.0..=100.0).contains(&v))
        && v.fan_target.is_none_or(|v| {
            matches!(
                v,
                EnumValue::Known(
                    history::TemperatureFanTarget::On | history::TemperatureFanTarget::Auto
                )
            )
        })
        && match &v.setpoint {
            Some(Setpoint::SetpointCelsius(v)) => v.is_finite() && *v >= -273.15,
            Some(Setpoint::SetpointRangeCelsius(v)) => {
                v.__buffa_unknown_fields.is_empty()
                    && matches!((v.low_celsius,v.high_celsius),(Some(l),Some(h)) if l.is_finite()&&h.is_finite()&&l>=-273.15&&l<=h)
            }
            None => true,
        }
}
fn helper(v: &history::HelperValue) -> bool {
    use history::helper_value::Value;
    if !v.__buffa_unknown_fields.is_empty() || v.helper_revision <= 0 {
        return false;
    }
    match &v.value {
        Some(Value::BooleanValue(_)) | Some(Value::Counter(_)) => true,
        Some(Value::Number(v)) => v.is_finite(),
        Some(Value::SelectedOption(v)) => !v.is_empty() && v.len() <= 16384 && !v.contains('\0'),
        Some(Value::Text(v)) => v.len() <= 16384 && !v.contains('\0'),
        Some(Value::TriggerTime(v)) => timestamp(v) && v.nanos % 1000 == 0,
        Some(Value::Date(v)) => civil_date(v),
        Some(Value::Time(v)) => civil_time(v),
        Some(Value::LocalValue(v)) => {
            v.__buffa_unknown_fields.is_empty()
                && v.date.as_option().is_some_and(civil_date)
                && v.time.as_option().is_some_and(civil_time)
        }
        None => false,
    }
}
fn civil_date(v: &crate::google::r#type::Date) -> bool {
    let days = match v.month {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 => {
            if v.year % 4 == 0 && (v.year % 100 != 0 || v.year % 400 == 0) {
                29
            } else {
                28
            }
        }
        _ => 0,
    };
    v.__buffa_unknown_fields.is_empty()
        && (1..=9999).contains(&v.year)
        && v.day >= 1
        && v.day <= days
}
fn civil_time(v: &crate::google::r#type::TimeOfDay) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && (0..24).contains(&v.hours)
        && (0..60).contains(&v.minutes)
        && (0..60).contains(&v.seconds)
        && v.nanos == 0
}
fn observation(v: &history::HistoryObservation) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && v.observation_id.len() == 64
        && v.observation_id
            .bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
        && v.state.as_option().is_some_and(historical_state)
        && v.descriptor.as_option().is_none_or(descriptor)
        && v.state.as_option().is_some_and(|s| {
            matches!(
                &s.value,
                Some(history::historical_state::Value::Measurement(_))
            ) == v.descriptor.as_option().is_some()
        })
}

fn response_scope(
    value: &history::HistoryReadScope,
    token: &str,
    count: usize,
    resolution: history::HistoryResolution,
    traces: bool,
    evidence: Option<&history::HistoryCoverage>,
) -> bool {
    let maximum = if traces { 31 } else { 100 };
    let hours = if resolution == history::HistoryResolution::Hour {
        500
    } else {
        168
    };
    scope(value)
        && value.resolution == resolution
        && value.limits.max_points == maximum
        && value.limits.max_period_hours == hours
        && count <= maximum as usize
        && token.len() <= 512
        && if value.retained.is_set() {
            traces || evidence.is_some_and(coverage)
        } else {
            value.not_retained_prefix.is_set()
                && count == 0
                && token.is_empty()
                && evidence.is_none()
        }
}

pub fn admit_observations_response(
    value: &history::QueryObservationsResponse,
) -> Result<Admitted<history::QueryObservationsResponse>, AdmissionError> {
    admit(
        value,
        value.__buffa_unknown_fields.is_empty()
            && value.compute_size(&mut Default::default()) <= 256 * 1024
            && value.scope.as_option().is_some_and(|scope| {
                response_scope(
                    scope,
                    &value.next_page_token,
                    value.observations.len(),
                    history::HistoryResolution::Raw,
                    false,
                    value.coverage.as_option(),
                )
            })
            && value.observations.iter().all(observation),
    )
}

pub fn admit_aggregates_response(
    value: &history::QueryAggregatesResponse,
) -> Result<Admitted<history::QueryAggregatesResponse>, AdmissionError> {
    admit(
        value,
        value.__buffa_unknown_fields.is_empty()
            && value.compute_size(&mut Default::default()) <= 256 * 1024
            && value.scope.as_option().is_some_and(|scope| {
                response_scope(
                    scope,
                    &value.next_page_token,
                    value.aggregates.len(),
                    history::HistoryResolution::Hour,
                    false,
                    value.coverage.as_option(),
                )
            })
            && value.aggregates.iter().all(aggregate),
    )
}

fn trace(value: &history::HistoryExecutionTrace, home: &HomeName) -> bool {
    use history::history_execution_trace::Outcome;
    value.__buffa_unknown_fields.is_empty()
        && decimal(&value.ordinal)
        && number(&value.ordinal) > 0
        && value.emission_time.as_option().is_some_and(timestamp)
        && [&value.run, &value.parent_run].iter().all(|reference| {
            reference.as_ref().is_none_or(|reference| {
                HistoryRunName::try_from(reference.as_str()).is_ok_and(|name| name.belongs_to(home))
            })
        })
        && value.controller.as_ref().is_none_or(|reference| {
            HistoryControllerName::try_from(reference.as_str())
                .is_ok_and(|name| name.belongs_to(home))
        })
        && value
            .configuration_generation
            .as_ref()
            .is_none_or(|reference| {
                reference
                    .strip_prefix(home.as_str())
                    .and_then(|suffix| suffix.strip_prefix("/configurationGenerations/g-"))
                    .is_some_and(|revision| {
                        revision
                            .parse::<i64>()
                            .is_ok_and(|number| number > 0 && number.to_string() == revision)
                    })
            })
        && value
            .manifest_sha256
            .as_ref()
            .is_none_or(|hash| hash.len() == 32 && hash.iter().any(|byte| *byte != 0))
        && value.configuration_generation.is_some() == value.manifest_sha256.is_some()
        && value
            .action_kind
            .is_none_or(|kind| nonzero(kind, history::RecorderTraceActionKind::Unspecified))
        && value
            .failure_kind
            .is_none_or(|kind| nonzero(kind, history::RecorderTraceExecutionErrorKind::Unspecified))
        && match &value.outcome {
            Some(Outcome::Decision(value)) => {
                nonzero(*value, history::RecorderTraceDecision::Unspecified)
            }
            Some(Outcome::NodeStage(value)) => {
                nonzero(*value, history::RecorderTraceNodeStage::Unspecified)
            }
            Some(Outcome::CommandOutcome(value)) => {
                nonzero(*value, history::HistoryCommandOutcome::Unspecified)
            }
            Some(Outcome::SceneMemberOutcome(value)) => {
                nonzero(*value, history::HistorySceneMemberOutcome::Unspecified)
            }
            Some(Outcome::SceneOutcome(value)) => {
                nonzero(*value, history::HistorySceneOutcome::Unspecified)
            }
            Some(Outcome::ScriptMode(value)) => {
                nonzero(*value, history::HistoryScriptMode::Unspecified)
            }
            None => false,
        }
}

pub fn admit_traces_response(
    value: &history::QueryExecutionTracesResponse,
    home: &HomeName,
) -> Result<Admitted<history::QueryExecutionTracesResponse>, AdmissionError> {
    admit(
        value,
        value.__buffa_unknown_fields.is_empty()
            && value.compute_size(&mut Default::default()) <= 256 * 1024
            && value.execution_epoch.len() <= 512
            && decimal(&value.checkpoint)
            && decimal(&value.commit_high)
            && (!value.scope.retained.is_set() || !value.execution_epoch.is_empty())
            && value.previous_execution_epoch.len() <= 512
            && value.scope.as_option().is_some_and(|scope| {
                response_scope(
                    scope,
                    &value.next_page_token,
                    value.execution_traces.len(),
                    history::HistoryResolution::Raw,
                    true,
                    None,
                )
            })
            && value
                .execution_traces
                .iter()
                .all(|value| trace(value, home))
            && value.losses.len() <= 4
            && value.losses.iter().all(|loss| {
                loss.__buffa_unknown_fields.is_empty()
                    && nonzero(loss.reason, history::HistoryTraceLossReason::Unspecified)
                    && decimal(&loss.known_count)
                    && number(&loss.known_count) > 0
                    && loss.from.as_option().is_some_and(timestamp)
                    && loss.through.as_option().is_some_and(timestamp)
                    && (loss.from.seconds, loss.from.nanos)
                        <= (loss.through.seconds, loss.through.nanos)
            }),
    )
}
