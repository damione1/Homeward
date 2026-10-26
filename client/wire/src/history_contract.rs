//! Bounded structural history admission. These values grant no rights; the
//! existing server authority, trusted catalogue and leased transport admit reads.
use crate::home::{history::v1 as history, registry::v1 as registry, state::v1 as state};
use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::Timestamp;

pub type AdmissionError = history::ErrorCode;
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
        Err(history::ErrorCode::InvalidArgument)
    }
}
fn timestamp(v: &Timestamp) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && (-62_135_596_800..=253_402_300_799).contains(&v.seconds)
        && (0..1_000_000_000).contains(&v.nanos)
        && (v.seconds != -62_135_596_800 || v.nanos != 0)
}
fn period(v: &history::Period, hours: i64, aligned: bool) -> bool {
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
fn page(size: Option<u32>, token: &str, maximum: u32) -> bool {
    size.is_none_or(|v| (1..=maximum).contains(&v)) && token.len() <= 512
}
fn entity(v: &str) -> bool {
    !v.is_empty() && v.len() <= 156 * 1024
}
fn epoch(v: &str) -> bool {
    v.len() == 32
        && v.bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        && v.bytes().any(|b| b != b'0')
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
            && entity(&v.entity_id)
            && v.resolution == history::Resolution::Raw
            && v.period.as_option().is_some_and(|p| period(p, 168, false))
            && page(v.page_size, &v.page_token, 100),
    )
}
pub fn admit_aggregates_request(
    v: &history::QueryAggregatesRequest,
) -> Result<Admitted<history::QueryAggregatesRequest>, AdmissionError> {
    admit(
        v,
        v.__buffa_unknown_fields.is_empty()
            && entity(&v.entity_id)
            && v.resolution == history::Resolution::Hour
            && v.period.as_option().is_some_and(|p| period(p, 500, true))
            && page(v.page_size, &v.page_token, 100),
    )
}
pub fn admit_traces_request(
    v: &history::QueryExecutionTracesRequest,
) -> Result<Admitted<history::QueryExecutionTracesRequest>, AdmissionError> {
    admit(
        v,
        v.__buffa_unknown_fields.is_empty()
            && v.epoch.as_ref().is_none_or(|v| epoch(v))
            && v.period.as_option().is_some_and(|p| period(p, 168, false))
            && page(v.page_size, &v.page_token, 31),
    )
}
fn descriptor(v: &history::NumericDescriptor) -> bool {
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
        && nonzero(v.dimension, history::Dimension::Unspecified)
        && (-308..=308).contains(&v.precision)
        && nonzero(v.semantic, registry::MeasurementSemantic::Unspecified)
        && nonzero(v.aggregation, registry::MeasurementAggregation::Unspecified)
}
fn quality(v: &state::Quality) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && nonzero(v.availability, state::Availability::Unspecified)
        && nonzero(v.freshness, state::Freshness::Unspecified)
        && nonzero(v.provenance, state::Provenance::Unspecified)
}
fn continuation(v: &history::Continuation, _count: usize) -> bool {
    use history::continuation::Result;
    v.__buffa_unknown_fields.is_empty()
        && match &v.result {
            Some(Result::End(v)) => v.__buffa_unknown_fields.is_empty(),
            Some(Result::NextPageToken(v)) => !v.is_empty() && v.len() <= 512,
            None => false,
        }
}
fn scope(v: &history::ReadScope) -> bool {
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
        && nonzero(v.resolution, history::Resolution::Unspecified)
        && v.limits.as_option().is_some_and(|l| {
            l.__buffa_unknown_fields.is_empty()
                && l.max_response_bytes == 256 * 1024
                && l.max_token_bytes == 512
                && l.query_timeout_millis == 2000
                && l.max_readers == 2
                && (l.max_points == 100 || l.max_points == 31)
        })
}
fn position(v: &crate::home::observability::v1::AdmissionPosition) -> bool {
    v.__buffa_unknown_fields.is_empty() && v.epoch.len() == 16 && v.epoch.iter().any(|b| *b != 0)
}
fn coverage(v: &history::Coverage) -> bool {
    use crate::home::observability::v1 as obs;
    if !v.recorder_tail.as_option().is_none_or(|tail| {
        tail.__buffa_unknown_fields.is_empty()
            && tail.from.as_option().is_some_and(position)
            && tail.through.as_option().is_some_and(position)
            && tail
                .unknown_utc
                .as_option()
                .is_some_and(|e| e.__buffa_unknown_fields.is_empty())
            && tail.from.epoch == tail.through.epoch
            && tail.through.sequence > tail.from.sequence
            && v.logical.as_option().is_some_and(|logical| {
                logical.through.as_option().is_some_and(|through| {
                    through.epoch == tail.from.epoch && through.sequence == tail.from.sequence
                })
            })
            && v.verdict == history::CoverageVerdict::Incomplete
    }) {
        return false;
    }
    if !v.__buffa_unknown_fields.is_empty()
        || !nonzero(v.verdict, history::CoverageVerdict::Unspecified)
        || v.retention.len() > 512
        || (v.analytical_tail_unproven
            && v.verdict == history::CoverageVerdict::CompleteUnderPolicy)
    {
        return false;
    }
    let Some(logical) = v.logical.as_option() else {
        return false;
    };
    let response = obs::ReadRecorderCoverageResponse {
        result: Some(obs::read_recorder_coverage_response::Result::Complete(
            Box::new(logical.clone()),
        )),
        ..Default::default()
    };
    crate::observability_contract::admit_coverage_response(&response).is_ok()
        && v.retention.iter().all(|r| {
            r.__buffa_unknown_fields.is_empty()
                && r.from.as_option().is_some_and(position)
                && r.through.as_option().is_some_and(position)
                && r.from.epoch == r.through.epoch
                && r.from.sequence <= r.through.sequence
                && (r.snapshot || r.from.sequence > 0)
        })
}
fn aggregate(v: &history::Aggregate) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && v.bucket.as_option().is_some_and(|p| period(p, 1, true))
        && v.descriptor.as_option().is_some_and(descriptor)
        && v.quality.as_option().is_some_and(quality)
        && v.duration_nanos <= 3_600_000_000_000
        && v.minimum.is_some() == v.maximum.is_some()
        && v.mean.is_some() == (v.duration_nanos > 0)
        && [v.minimum, v.maximum, v.mean, v.contribution]
            .iter()
            .all(|x| x.is_none_or(f64::is_finite))
        && nonzero(v.reset, history::Reset::Unspecified)
        && v.reasons.len() <= 12
        && v.reasons
            .iter()
            .all(|v| nonzero(*v, history::AggregateReason::Unspecified))
}
pub fn admit_aggregates_response(
    v: &history::QueryAggregatesResponse,
) -> Result<Admitted<history::QueryAggregatesResponse>, AdmissionError> {
    use history::query_aggregates_response::Result;
    let valid = match &v.result {
        Some(Result::Page(p)) => {
            p.__buffa_unknown_fields.is_empty()
                && p.scope.as_option().is_some_and(scope)
                && p.points.len() <= 100
                && p.points.iter().all(aggregate)
                && p.coverage.as_option().is_some_and(coverage)
                && p.continuation
                    .as_option()
                    .is_some_and(|c| continuation(c, p.points.len()))
        }
        Some(Result::NotRetained(s)) => scope(s),
        Some(Result::StaleCursor(e)) => e.__buffa_unknown_fields.is_empty(),
        Some(Result::Budget(b)) => {
            b.__buffa_unknown_fields.is_empty()
                && nonzero(b.reason, history::BudgetReason::Unspecified)
        }
        None => false,
    };
    admit(
        v,
        valid
            && v.__buffa_unknown_fields.is_empty()
            && v.compute_size(&mut Default::default()) <= 256 * 1024,
    )
}
pub fn admit_traces_response(
    v: &history::QueryExecutionTracesResponse,
) -> Result<Admitted<history::QueryExecutionTracesResponse>, AdmissionError> {
    use history::query_execution_traces_response::Result;
    let valid = match &v.result {
        Some(Result::Page(p)) => {
            p.__buffa_unknown_fields.is_empty()
                && epoch(&p.epoch)
                && p.previous_epoch.as_ref().is_none_or(|v| epoch(v))
                && p.scope.as_option().is_some_and(scope)
                && p.points.len() <= 31
                && p.losses.len() <= 4
                && p.continuation
                    .as_option()
                    .is_some_and(|c| continuation(c, p.points.len()))
                && p.points.iter().all(trace)
                && p.losses.iter().all(|l| {
                    l.__buffa_unknown_fields.is_empty()
                        && nonzero(l.reason, history::TraceLossReason::Unspecified)
                        && l.known_count > 0
                        && l.from.as_option().is_some_and(timestamp)
                        && l.through.as_option().is_some_and(timestamp)
                        && (l.from.seconds, l.from.nanos) <= (l.through.seconds, l.through.nanos)
                })
        }
        Some(Result::NotRetained(s)) => scope(s),
        Some(Result::StaleCursor(e)) => e.__buffa_unknown_fields.is_empty(),
        Some(Result::Budget(b)) => {
            b.__buffa_unknown_fields.is_empty()
                && nonzero(b.reason, history::BudgetReason::Unspecified)
        }
        None => false,
    };
    admit(
        v,
        valid
            && v.__buffa_unknown_fields.is_empty()
            && v.compute_size(&mut Default::default()) <= 256 * 1024,
    )
}
fn trace(v: &history::ExecutionTrace) -> bool {
    use crate::home::observability::v1 as obs;
    use history::execution_trace::Outcome;
    v.__buffa_unknown_fields.is_empty()
        && v.ordinal > 0
        && v.ordinal <= i64::MAX as u64
        && v.emission_time.as_option().is_some_and(timestamp)
        && [
            v.run_id,
            v.parent_run_id,
            v.controller_generation,
            v.config_revision,
        ]
        .iter()
        .all(|v| v.is_none_or(|n| n > 0 && n <= i64::MAX as u64))
        && v.manifest_sha256
            .as_ref()
            .is_none_or(|v| v.len() == 32 && v.iter().any(|b| *b != 0))
        && v.config_revision.is_some() == v.manifest_sha256.is_some()
        && v.action_kind
            .is_none_or(|v| nonzero(v, obs::TraceActionKind::Unspecified))
        && v.failure_kind
            .is_none_or(|v| nonzero(v, obs::TraceExecutionErrorKind::Unspecified))
        && match &v.outcome {
            Some(Outcome::Decision(v)) => nonzero(*v, obs::TraceDecision::Unspecified),
            Some(Outcome::NodeStage(v)) => nonzero(*v, obs::TraceNodeStage::Unspecified),
            Some(Outcome::CommandOutcome(v)) => nonzero(*v, history::CommandOutcome::Unspecified),
            Some(Outcome::SceneMemberOutcome(v)) => {
                nonzero(*v, history::SceneMemberOutcome::Unspecified)
            }
            Some(Outcome::SceneOutcome(v)) => nonzero(*v, history::SceneOutcome::Unspecified),
            Some(Outcome::ScriptMode(v)) => nonzero(*v, history::ScriptMode::Unspecified),
            None => false,
        }
}

fn historical_state(v: &state::ObservedState) -> bool {
    use registry::entity_ref::Reference;
    use state::observed_state::Value;
    if !v.__buffa_unknown_fields.is_empty()
        || v.revision == 0
        || v.source_session.is_empty()
        || v.source_session.len() > 156 * 1024
        || !v.quality.as_option().is_some_and(quality)
        || !v.entity.as_option().is_some_and(|e| {
            e.__buffa_unknown_fields.is_empty()
                && matches!(&e.reference,Some(Reference::EntityId(id)) if entity(id))
        })
        || ![
            &v.received_at,
            &v.last_changed,
            &v.last_updated,
            &v.last_reported,
        ]
        .iter()
        .all(|t| t.as_option().is_some_and(timestamp))
        || !v.observed_at.as_option().is_none_or(timestamp)
        || (v.quality.as_option().unwrap().availability == state::Availability::Available)
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
                && l.color_temp_mireds.is_none_or(|v| v > 0)
        }
        Some(Value::Fan(f)) => {
            f.__buffa_unknown_fields.is_empty() && nonzero(f.speed, state::FanSpeed::Unspecified)
        }
        Some(Value::Climate(c)) => climate(c),
        Some(Value::Helper(h)) => helper(h),
        Some(Value::Solar(s)) => crate::solar_contract::admit_solar_value(s).is_ok(),
        Some(Value::SolarEvent(s)) => crate::solar_contract::admit_solar_event_value(s).is_ok(),
        Some(Value::Cover(value)) => crate::controls_contract::cover_value(value),
        Some(Value::Lock(value)) => crate::controls_contract::lock_value(value),
        Some(Value::Alarm(value)) => crate::controls_contract::alarm_value(value),
    }
}
fn climate(v: &state::ClimateValue) -> bool {
    use state::climate_value::Setpoint;
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
            .is_none_or(|v| nonzero(v, registry::ClimateMode::Unspecified))
        && v.activity
            .is_none_or(|v| nonzero(v, registry::ClimateActivity::Unspecified))
        && v.humidity_percent
            .is_none_or(|v| v.is_finite() && (0.0..=100.0).contains(&v))
        && v.fan_target.is_none_or(|v| {
            matches!(
                v,
                EnumValue::Known(registry::ClimateFanTarget::On | registry::ClimateFanTarget::Auto)
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
fn helper(v: &crate::home::helpers::v1::HelperValue) -> bool {
    use crate::home::helpers::v1::helper_value::Value;
    if !v.__buffa_unknown_fields.is_empty()
        || v.helper_revision == 0
        || v.helper_revision > i64::MAX as u64
    {
        return false;
    }
    match &v.value {
        Some(Value::Boolean(_)) | Some(Value::Counter(_)) => true,
        Some(Value::Number(v)) => v.is_finite(),
        Some(Value::SelectedOption(v)) => !v.is_empty() && v.len() <= 16384 && !v.contains('\0'),
        Some(Value::Text(v)) => v.len() <= 16384 && !v.contains('\0'),
        Some(Value::ButtonPressedAt(v)) => timestamp(v) && v.nanos % 1000 == 0,
        Some(Value::Date(v)) => civil_date(v),
        Some(Value::Time(v)) => civil_time(v),
        Some(Value::Datetime(v)) => {
            v.__buffa_unknown_fields.is_empty()
                && v.date.as_option().is_some_and(civil_date)
                && v.time.as_option().is_some_and(civil_time)
        }
        None => false,
    }
}
fn civil_date(v: &crate::home::helpers::v1::CivilDate) -> bool {
    let days = match v.month {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 => {
            if v.year.is_multiple_of(4)
                && (!v.year.is_multiple_of(100) || v.year.is_multiple_of(400))
            {
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
fn civil_time(v: &crate::home::helpers::v1::CivilTime) -> bool {
    v.__buffa_unknown_fields.is_empty() && v.hour < 24 && v.minute < 60 && v.second < 60
}
fn observation(v: &history::Observation) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && v.position.as_option().is_some_and(|p| {
            p.__buffa_unknown_fields.is_empty()
                && p.epoch.len() == 16
                && p.epoch.iter().any(|b| *b != 0)
                && p.sequence > 0
        })
        && v.state.as_option().is_some_and(historical_state)
        && v.descriptor.as_option().is_none_or(descriptor)
        && v.state.as_option().is_some_and(|s| {
            matches!(&s.value, Some(state::observed_state::Value::Measurement(_)))
                == v.descriptor.as_option().is_some()
        })
}
pub fn admit_observations_response(
    v: &history::QueryObservationsResponse,
) -> Result<Admitted<history::QueryObservationsResponse>, AdmissionError> {
    use history::query_observations_response::Result;
    let valid = match &v.result {
        Some(Result::Page(p)) => {
            p.__buffa_unknown_fields.is_empty()
                && p.scope.as_option().is_some_and(scope)
                && p.points.len() <= 100
                && p.points.iter().all(observation)
                && p.coverage.as_option().is_some_and(coverage)
                && p.continuation
                    .as_option()
                    .is_some_and(|c| continuation(c, p.points.len()))
        }
        Some(Result::NotRetained(s)) => scope(s),
        Some(Result::StaleCursor(e)) => e.__buffa_unknown_fields.is_empty(),
        Some(Result::Budget(b)) => {
            b.__buffa_unknown_fields.is_empty()
                && nonzero(b.reason, history::BudgetReason::Unspecified)
        }
        None => false,
    };
    admit(
        v,
        valid
            && v.__buffa_unknown_fields.is_empty()
            && v.compute_size(&mut Default::default()) <= 256 * 1024,
    )
}
