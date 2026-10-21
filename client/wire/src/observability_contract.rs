//! Structural observation admission. These immutable copies grant no rights;
//! the common server transport and current Authority remain authoritative.

use crate::home::{auth::v1 as auth, integrations::v1 as integrations, observability::v1 as obs};
use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::{Empty, Timestamp};

pub type AdmissionError = obs::ObservabilityErrorCode;
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
        Err(obs::ObservabilityErrorCode::InvalidArgument)
    }
}
fn text(value: &str, maximum: usize) -> bool {
    !value.is_empty()
        && value.len() <= maximum
        && !value.bytes().any(|b| matches!(b, 0 | b'\r' | b'\n'))
}
fn identity(value: &[u8], size: usize) -> bool {
    value.len() == size && value.iter().any(|b| *b != 0)
}
fn timestamp(value: &Timestamp) -> bool {
    (-62_135_596_800..=253_402_300_799).contains(&value.seconds)
        && (0..1_000_000_000).contains(&value.nanos)
        && (value.seconds != -62_135_596_800 || value.nanos != 0)
        && value.__buffa_unknown_fields.is_empty()
}
fn empty(value: &Empty) -> bool {
    value.__buffa_unknown_fields.is_empty()
}
fn account(value: &auth::AccountId) -> bool {
    text(&value.value, 128) && value.__buffa_unknown_fields.is_empty()
}
fn page(size: Option<u32>, token: &Option<String>) -> bool {
    size.is_none_or(|size| (1..=100).contains(&size))
        && token.as_ref().is_none_or(|token| text(token, 512))
}
fn sized<T: Message>(value: &T, maximum: u32) -> bool {
    value.compute_size(&mut Default::default()) <= maximum
}

// There is no parallel enum vocabulary. Identity admission tests generated
// variants against their generated zero; actual semantic conversions below
// match every accepted variant explicitly and reject unknown wire values.
fn nonzero<E: Copy + PartialEq + buffa::Enumeration>(value: EnumValue<E>, zero: E) -> bool {
    match value {
        EnumValue::Known(value) => value != zero,
        EnumValue::Unknown(_) => false,
    }
}
fn source(value: EnumValue<obs::AuditSource>) -> bool {
    match value {
        EnumValue::Known(obs::AuditSource::Authentication | obs::AuditSource::SecretAccess) => true,
        EnumValue::Known(obs::AuditSource::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
fn position(value: &obs::AdmissionPosition) -> bool {
    identity(&value.epoch, 16) && value.__buffa_unknown_fields.is_empty()
}
fn epoch(value: &obs::ProcessObservationEpoch) -> bool {
    identity(&value.value, 16) && value.__buffa_unknown_fields.is_empty()
}
fn recorder_epoch(value: &obs::RecorderEpochIdentity) -> bool {
    identity(&value.value, 16) && value.__buffa_unknown_fields.is_empty()
}
fn controller(value: &obs::ControllerHandle) -> bool {
    value.value.len() == 32
        && identity(&value.value[..16], 16)
        && identity(&value.value[16..], 16)
        && value.__buffa_unknown_fields.is_empty()
}
fn unavailable(value: &obs::ReadUnavailable) -> bool {
    nonzero(value.reason, obs::ReadUnavailableReason::Unspecified)
        && value.__buffa_unknown_fields.is_empty()
}

pub fn admit_get_health(
    value: &obs::GetHealthRequest,
) -> Result<Admitted<obs::GetHealthRequest>, AdmissionError> {
    admit(value, value.__buffa_unknown_fields.is_empty())
}
pub fn admit_get_metrics(
    value: &obs::GetMetricsRequest,
) -> Result<Admitted<obs::GetMetricsRequest>, AdmissionError> {
    admit(value, value.__buffa_unknown_fields.is_empty())
}
pub fn admit_get_integration_health(
    value: &obs::GetIntegrationHealthRequest,
) -> Result<Admitted<obs::GetIntegrationHealthRequest>, AdmissionError> {
    admit(
        value,
        text(&value.integration_instance_id, 256) && value.__buffa_unknown_fields.is_empty(),
    )
}
pub fn admit_query_runtime_logs(
    value: &obs::QueryRuntimeLogsRequest,
) -> Result<Admitted<obs::QueryRuntimeLogsRequest>, AdmissionError> {
    admit(
        value,
        page(value.page_size, &value.page_token) && value.__buffa_unknown_fields.is_empty(),
    )
}
pub fn admit_query_audits(
    value: &obs::QueryAuditsRequest,
) -> Result<Admitted<obs::QueryAuditsRequest>, AdmissionError> {
    admit(
        value,
        source(value.source)
            && page(value.page_size, &value.page_token)
            && value.__buffa_unknown_fields.is_empty(),
    )
}
pub fn admit_read_run_trace(
    value: &obs::ReadRunTraceRequest,
) -> Result<Admitted<obs::ReadRunTraceRequest>, AdmissionError> {
    admit(
        value,
        value.run_id != 0
            && value.controller.as_option().is_some_and(controller)
            && page(value.page_size, &value.page_token)
            && value.__buffa_unknown_fields.is_empty(),
    )
}
pub fn admit_read_recorder_coverage(
    value: &obs::ReadRecorderCoverageRequest,
) -> Result<Admitted<obs::ReadRecorderCoverageRequest>, AdmissionError> {
    use obs::read_recorder_coverage_request::Through;
    let valid = value.from.as_option().is_some_and(|from| {
        position(from)
            && match &value.through {
                Some(Through::Position(through)) => {
                    position(through)
                        && (through.epoch != from.epoch || through.sequence >= from.sequence)
                }
                Some(Through::LatestInEpoch(through)) => recorder_epoch(through),
                None => false,
            }
    });
    admit(value, valid && value.__buffa_unknown_fields.is_empty())
}

fn counter(value: &obs::CounterMeasurement) -> bool {
    use obs::counter_measurement::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Exact(_)) => true,
            Some(Result::Saturated(value)) => {
                value.value == u64::MAX && value.__buffa_unknown_fields.is_empty()
            }
            Some(Result::NotMeasured(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn positive_counter(value: &obs::CounterMeasurement) -> bool {
    counter(value)
        && match &value.result {
            Some(obs::counter_measurement::Result::Exact(value)) => *value > 0,
            Some(obs::counter_measurement::Result::Saturated(_)) => true,
            Some(
                obs::counter_measurement::Result::NotMeasured(_)
                | obs::counter_measurement::Result::Unavailable(_),
            )
            | None => false,
        }
}
fn gauge(value: &obs::GaugeMeasurement) -> bool {
    use obs::gauge_measurement::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Value(_)) => true,
            Some(Result::NotMeasured(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn gauge_le(left: &obs::GaugeMeasurement, right: &obs::GaugeMeasurement) -> bool {
    use obs::gauge_measurement::Result;
    if let (Some(Result::Value(left)), Some(Result::Value(right))) = (&left.result, &right.result) {
        left <= right
    } else {
        true
    }
}
fn lag(value: &obs::LagMeasurement) -> bool {
    use obs::lag_measurement::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::SameEpochPositions(_)) => true,
            Some(Result::Invalidated(value) | Result::EpochMismatch(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn measured_position(value: &obs::PositionMeasurement) -> bool {
    use obs::position_measurement::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Value(value)) => position(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn known_lost(value: &obs::KnownLost) -> bool {
    use obs::known_lost::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::LowerBound(value)) => *value > 0,
            Some(Result::NotKnown(value)) => empty(value),
            None => false,
        }
}
fn runtime_author(value: &obs::RuntimeAuthor) -> bool {
    use obs::runtime_author::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(
                Result::Process(value)
                | Result::InternalAutomation(value)
                | Result::Unauthenticated(value),
            ) => empty(value),
            Some(Result::HumanAccount(value)) => account(value),
            None => false,
        }
}
fn event(value: &obs::RuntimeEvent) -> bool {
    use obs::{LogCode as C, LogComponent as P, LogLevel as L};
    let pair = match value.code {
        EnumValue::Known(
            C::ProcessConfigReadFailed
            | C::ProcessConfigAdmissionFailed
            | C::ProcessRuntimeStartFailed
            | C::ProcessRuntimeFailed,
        ) => {
            value.component == EnumValue::Known(P::Process)
                && value.level == EnumValue::Known(L::Error)
        }
        EnumValue::Known(C::ProcessReady | C::ProcessStopped) => {
            value.component == EnumValue::Known(P::Process)
                && value.level == EnumValue::Known(L::Info)
        }
        EnumValue::Known(C::TransportInternalDiagnostic) => {
            matches!(
                value.component,
                EnumValue::Known(P::PublicTransport | P::OperatorTransport | P::WorkerTransport)
            ) && value.level == EnumValue::Known(L::Warning)
        }
        EnumValue::Known(
            C::MqttUnknownDiscovery
            | C::MqttInvalidDiscovery
            | C::MqttUnconfiguredSource
            | C::MqttUnsupportedEffect
            | C::MqttUnsupportedColorMode
            | C::MqttInvalidState
            | C::MqttInvalidAvailability
            | C::MqttRetirementFailed
            | C::MqttSnapshotOverflow
            | C::MqttUnconfiguredActionAnchor
            | C::MqttAmbiguousActionDevice
            | C::MqttActionOverflow
            | C::MqttInvalidAction,
        ) => {
            value.component == EnumValue::Known(P::Mqtt)
                && value.level == EnumValue::Known(L::Warning)
        }
        EnumValue::Known(C::PilotStarting | C::PilotRunning | C::PilotStopped) => {
            value.component == EnumValue::Known(P::Supervisor)
                && value.level == EnumValue::Known(L::Info)
        }
        EnumValue::Known(C::PilotRetrying | C::PilotDegraded) => {
            value.component == EnumValue::Known(P::Supervisor)
                && value.level == EnumValue::Known(L::Warning)
        }
        EnumValue::Known(C::PilotJoinTimeout | C::PilotTaskCapacity | C::PilotFactoryFailed) => {
            value.component == EnumValue::Known(P::Supervisor)
                && value.level == EnumValue::Known(L::Error)
        }
        EnumValue::Known(C::Unspecified) | EnumValue::Unknown(_) => false,
    };
    pair && value.__buffa_unknown_fields.is_empty()
        && value.occurred_at.as_option().is_some_and(timestamp)
        && value.author.as_option().is_some_and(runtime_author)
        && sized(value, 1024)
}
pub fn admit_runtime_event(
    value: &obs::RuntimeEvent,
) -> Result<Admitted<obs::RuntimeEvent>, AdmissionError> {
    admit(value, event(value))
}
pub fn admit_counter(
    value: &obs::CounterMeasurement,
) -> Result<Admitted<obs::CounterMeasurement>, AdmissionError> {
    admit(value, counter(value))
}

fn continuation(value: &obs::PageContinuation) -> bool {
    use obs::page_continuation::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::NextPageToken(token)) => text(token, 512),
            Some(Result::End(value)) => empty(value),
            None => false,
        }
}
fn runtime_page(value: &obs::RuntimeLogPage) -> bool {
    let mut previous = 0;
    value.rows.len() <= 100
        && value.__buffa_unknown_fields.is_empty()
        && value.process_epoch.as_option().is_some_and(epoch)
        && value.continuation.as_option().is_some_and(continuation)
        && value.rows.iter().all(|row| {
            let valid = row.sequence > previous
                && row.sequence <= value.upper_sequence
                && row.__buffa_unknown_fields.is_empty()
                && row.event.as_option().is_some_and(event);
            previous = row.sequence;
            valid
        })
}
pub fn admit_runtime_logs_response(
    value: &obs::QueryRuntimeLogsResponse,
) -> Result<Admitted<obs::QueryRuntimeLogsResponse>, AdmissionError> {
    use obs::query_runtime_logs_response::Result;
    let valid = value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Page(value)) => runtime_page(value),
            Some(Result::CursorEvicted(value)) => {
                value.retained_from > 0
                    && value.__buffa_unknown_fields.is_empty()
                    && value
                        .evicted_total
                        .as_option()
                        .is_some_and(positive_counter)
            }
            Some(Result::StaleCursor(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        };
    admit(value, valid && sized(value, 256 << 10))
}

fn generation(value: &obs::GenerationPresence) -> bool {
    use obs::generation_presence::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Present(value)) => {
                value.value > 0
                    && value.value <= i64::MAX as u64
                    && value.__buffa_unknown_fields.is_empty()
            }
            Some(Result::NotLoaded(value)) => empty(value),
            None => false,
        }
}
fn server_health(value: &obs::ServerHealth) -> bool {
    let generation = value.generation.as_option().is_some_and(generation);
    let ready = value.readiness == EnumValue::Known(obs::ServerReadiness::Ready);
    let present = value
        .generation
        .as_option()
        .is_some_and(|g| matches!(g.result, Some(obs::generation_presence::Result::Present(_))));
    let mut seen = Vec::with_capacity(6);
    generation
        && nonzero(value.readiness, obs::ServerReadiness::Unspecified)
        && value.causes.len() <= 6
        && value.causes.iter().all(|cause| {
            let valid = nonzero(*cause, obs::CoreCause::Unspecified) && !seen.contains(cause);
            seen.push(*cause);
            valid
        })
        && (!ready || present && value.causes.is_empty())
        && value.__buffa_unknown_fields.is_empty()
}
fn durable_health(value: &obs::DurableHealth) -> bool {
    use obs::durable_health::Evidence;
    nonzero(value.availability, obs::DurableAvailability::Unspecified)
        && value.__buffa_unknown_fields.is_empty()
        && match &value.evidence {
            Some(Evidence::ObservedAt(at)) => {
                value.availability != EnumValue::Known(obs::DurableAvailability::Unknown)
                    && timestamp(at)
            }
            Some(Evidence::NotObserved(none)) => {
                value.availability == EnumValue::Known(obs::DurableAvailability::Unknown)
                    && empty(none)
            }
            None => false,
        }
}
fn recorder_health(value: &obs::RecorderHealth) -> bool {
    use obs::recorder_health::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Observed(value)) => {
                nonzero(value.phase, obs::RecorderPhase::Unspecified)
                    && nonzero(value.state, obs::RecorderHealthState::Unspecified)
                    && value.observed_at.as_option().is_some_and(timestamp)
                    && value.__buffa_unknown_fields.is_empty()
            }
            Some(Result::NotInstalled(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
pub fn admit_health_response(
    value: &obs::GetHealthResponse,
) -> Result<Admitted<obs::GetHealthResponse>, AdmissionError> {
    let valid = value.health.as_option().is_some_and(|health| {
        let mut seen = Vec::with_capacity(5);
        let mut total = 0u64;
        health.__buffa_unknown_fields.is_empty()
            && health.process_epoch.as_option().is_some_and(epoch)
            && health.liveness == EnumValue::Known(obs::Liveness::Live)
            && health.server.as_option().is_some_and(server_health)
            && health.durable.as_option().is_some_and(durable_health)
            && health.recorder.as_option().is_some_and(recorder_health)
            && health.pilots.len() == 5
            && health.pilots.iter().all(|pilot| {
                let valid = nonzero(pilot.phase, obs::PilotPhase::Unspecified)
                    && pilot.instances <= 1024
                    && !seen.contains(&pilot.phase)
                    && pilot.__buffa_unknown_fields.is_empty();
                seen.push(pilot.phase);
                total = total.saturating_add(pilot.instances);
                valid
            })
            && total <= 1024
    });
    admit(
        value,
        valid && value.__buffa_unknown_fields.is_empty() && sized(value, 16 << 10),
    )
}
pub fn admit_integration_health_response(
    value: &obs::GetIntegrationHealthResponse,
) -> Result<Admitted<obs::GetIntegrationHealthResponse>, AdmissionError> {
    use obs::{
        integration_report::AdmissionTime, integration_report_presence::Result as R,
        integration_supervision::Result as S,
    };
    let report = value.report.as_option().is_some_and(|report| {
        report.__buffa_unknown_fields.is_empty()
            && match &report.result {
                Some(R::Present(value)) => {
                    nonzero(value.status, integrations::HealthStatus::Unspecified)
                        && value.__buffa_unknown_fields.is_empty()
                        && match &value.admission_time {
                            Some(AdmissionTime::AdmittedAt(at)) => timestamp(at),
                            Some(AdmissionTime::NotRecorded(value)) => empty(value),
                            None => false,
                        }
                }
                Some(R::NoReport(value)) => empty(value),
                None => false,
            }
    });
    let supervision = value.supervision.as_option().is_some_and(|supervision| {
        supervision.__buffa_unknown_fields.is_empty()
            && match &supervision.result {
                Some(S::Supervised(value)) => {
                    nonzero(value.phase, obs::PilotPhase::Unspecified)
                        && value.attempts <= 17
                        && value.tasks <= 256
                        && value.__buffa_unknown_fields.is_empty()
                }
                Some(S::NotSupervised(value)) => empty(value),
                None => false,
            }
    });
    admit(
        value,
        report && supervision && value.__buffa_unknown_fields.is_empty() && sized(value, 8 << 10),
    )
}

fn installed_queue(value: &obs::InstalledQueueMetrics) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value.active_queues.as_option().is_some_and(gauge)
        && value.queued_entries.as_option().is_some_and(gauge)
        && value.capacity_entries.as_option().is_some_and(gauge)
        && value.rejected_total.as_option().is_some_and(counter)
        && value.lost_deliveries_total.as_option().is_some_and(counter)
        && value.coalesced_total.as_option().is_some_and(counter)
        && value.degraded_queues.as_option().is_some_and(gauge)
        && value.queued_accounted_bytes.as_option().is_some_and(gauge)
        && value
            .capacity_accounted_bytes
            .as_option()
            .is_some_and(gauge)
        && value.in_flight_entries.as_option().is_some_and(gauge)
        && value.pending_entries.as_option().is_some_and(gauge)
        && gauge_le(&value.queued_entries, &value.capacity_entries)
        && gauge_le(&value.degraded_queues, &value.active_queues)
        && gauge_le(
            &value.queued_accounted_bytes,
            &value.capacity_accounted_bytes,
        )
}
fn queue(value: &obs::QueueAggregate) -> bool {
    use obs::queue_aggregate::Result;
    nonzero(value.queue_class, obs::QueueClass::Unspecified)
        && value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Installed(installed)) => {
                use obs::gauge_measurement::Result as GaugeResult;
                let inflight_unmeasured = matches!(
                    installed.in_flight_entries.result,
                    Some(GaugeResult::NotMeasured(_))
                );
                let pending_unmeasured = matches!(
                    installed.pending_entries.result,
                    Some(GaugeResult::NotMeasured(_))
                );
                let applicable = match value.queue_class {
                    EnumValue::Known(obs::QueueClass::WorkerCommands) => {
                        !inflight_unmeasured
                            && !pending_unmeasured
                            && gauge_le(&installed.in_flight_entries, &installed.pending_entries)
                    }
                    EnumValue::Known(obs::QueueClass::CloudRequests) => {
                        !inflight_unmeasured && pending_unmeasured
                    }
                    EnumValue::Known(_) | EnumValue::Unknown(_) => {
                        inflight_unmeasured && pending_unmeasured
                    }
                };
                installed_queue(installed) && applicable
            }
            Some(Result::NotInstalled(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn queue_lag(value: &obs::QueueLag) -> bool {
    use obs::queue_lag::Result;
    matches!(
        value.queue_class,
        EnumValue::Known(
            obs::QueueClass::Ui
                | obs::QueueClass::Edge
                | obs::QueueClass::Recorder
                | obs::QueueClass::View
        )
    ) && value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Installed(value)) => lag(value),
            Some(Result::NotInstalled(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn active_coverage(value: &obs::ActiveCoverage) -> bool {
    use obs::active_coverage::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::AtRisk(value)) => {
                value.from.as_option().is_some_and(position)
                    && value.through.as_option().is_some_and(position)
                    && value.from.epoch == value.through.epoch
                    && value.from.sequence > 0
                    && value.through.sequence >= value.from.sequence
                    && value.known_lost.as_option().is_some_and(|lost| {
                        known_lost(lost)
                            && match &lost.result {
                                Some(obs::known_lost::Result::LowerBound(count)) => {
                                    *count <= value.through.sequence - value.from.sequence + 1
                                }
                                Some(obs::known_lost::Result::NotKnown(_)) => true,
                                None => false,
                            }
                    })
                    && value.__buffa_unknown_fields.is_empty()
            }
            Some(Result::NoActiveRisk(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn recorder_metrics(value: &obs::RecorderMetrics) -> bool {
    use obs::recorder_metrics::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Installed(value)) => {
                let mut seen = Vec::with_capacity(3);
                value.__buffa_unknown_fields.is_empty()
                    && value.queued_bytes.as_option().is_some_and(gauge)
                    && value.pending_bytes.as_option().is_some_and(gauge)
                    && value.total_capacity_bytes.as_option().is_some_and(gauge)
                    && value.queue_capacity_bytes.as_option().is_some_and(gauge)
                    && value.queued_entries.as_option().is_some_and(gauge)
                    && value.retries_total.as_option().is_some_and(counter)
                    && value.lost_entries_total.as_option().is_some_and(counter)
                    && value.policy_excluded_total.as_option().is_some_and(counter)
                    && value.checkpoint.as_option().is_some_and(measured_position)
                    && value.tail.as_option().is_some_and(measured_position)
                    && value.recorder_lag_positions.as_option().is_some_and(lag)
                    && value
                        .active_coverage
                        .as_option()
                        .is_some_and(active_coverage)
                    && value.committed_gaps.len() == 3
                    && value.committed_gaps.iter().all(|gap| {
                        let valid = nonzero(gap.reason, obs::GapReason::Unspecified)
                            && !seen.contains(&gap.reason)
                            && gap.rows_total.as_option().is_some_and(counter)
                            && gap
                                .known_lost_lower_bound_total
                                .as_option()
                                .is_some_and(counter)
                            && gap
                                .rows_without_known_count_total
                                .as_option()
                                .is_some_and(counter)
                            && gap.__buffa_unknown_fields.is_empty();
                        seen.push(gap.reason);
                        valid
                    })
            }
            Some(Result::NotInstalled(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn action_metrics(value: &obs::ActionMetrics) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value
            .progress_indeterminate_total
            .as_option()
            .is_some_and(counter)
        && value
            .durable_recovered_indeterminate_total
            .as_option()
            .is_some_and(counter)
        && value.first_attempts.len() == 2
        && value.first_attempts[0].ledger != value.first_attempts[1].ledger
        && value.first_attempts.iter().all(|first| {
            nonzero(first.ledger, obs::AttemptLedger::Unspecified)
                && first.indeterminate_total.as_option().is_some_and(counter)
                && first.__buffa_unknown_fields.is_empty()
        })
}
fn run_metrics(value: &obs::RunMetrics) -> bool {
    use obs::run_metrics::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Installed(value)) => {
                value.__buffa_unknown_fields.is_empty()
                    && value.preparing.as_option().is_some_and(gauge)
                    && value.controller.as_option().is_some_and(controller)
                    && value.kinds.len() == 2
                    && value.kinds[0].kind != value.kinds[1].kind
                    && value.kinds.iter().all(|kind| {
                        nonzero(kind.kind, obs::RunKind::Unspecified)
                            && kind.active.as_option().is_some_and(gauge)
                            && kind.pending.as_option().is_some_and(gauge)
                            && kind.cancelled_total.as_option().is_some_and(counter)
                            && kind.superseded_total.as_option().is_some_and(counter)
                            && kind.__buffa_unknown_fields.is_empty()
                    })
            }
            Some(Result::NotInstalled(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
pub fn admit_metrics_response(
    value: &obs::GetMetricsResponse,
) -> Result<Admitted<obs::GetMetricsResponse>, AdmissionError> {
    let valid = value.metrics.as_option().is_some_and(|metrics| {
        let mut queues = Vec::with_capacity(9);
        let mut lags = Vec::with_capacity(4);
        let mut cuts = Vec::with_capacity(8);
        metrics.__buffa_unknown_fields.is_empty()
            && metrics.process_epoch.as_option().is_some_and(epoch)
            && metrics.queues.len() == 9
            && metrics.queue_lags.len() == 4
            && metrics.queues.iter().all(|q| {
                let valid = queue(q) && !queues.contains(&q.queue_class);
                queues.push(q.queue_class);
                valid
            })
            && metrics.queue_lags.iter().all(|q| {
                let valid = queue_lag(q) && !lags.contains(&q.queue_class);
                lags.push(q.queue_class);
                valid
            })
            && metrics.recorder.as_option().is_some_and(recorder_metrics)
            && metrics.actions.as_option().is_some_and(action_metrics)
            && metrics.runs.as_option().is_some_and(run_metrics)
            && metrics.journal.as_option().is_some_and(|journal| {
                journal.retained_rows.as_option().is_some_and(gauge)
                    && journal.charged_bytes.as_option().is_some_and(gauge)
                    && journal.evicted_total.as_option().is_some_and(counter)
                    && journal.__buffa_unknown_fields.is_empty()
            })
            && metrics.cuts.len() <= 8
            && metrics.cuts.iter().all(|cut| {
                let valid = nonzero(cut.owner, obs::MetricOwner::Unspecified)
                    && !cuts.contains(&cut.owner)
                    && cut.captured_at.as_option().is_some_and(timestamp)
                    && cut.__buffa_unknown_fields.is_empty();
                cuts.push(cut.owner);
                valid
            })
    });
    admit(
        value,
        valid && value.__buffa_unknown_fields.is_empty() && sized(value, 32 << 10),
    )
}

fn integration_reference(value: &obs::IntegrationReference) -> bool {
    identity(&value.value, 16) && value.__buffa_unknown_fields.is_empty()
}
fn audit_author(value: &obs::AuditAuthor) -> bool {
    use obs::audit_author::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::HumanAccount(value)) => account(value),
            Some(Result::InternalIntegration(value)) => integration_reference(value),
            Some(Result::NotRecorded(value)) => empty(value),
            None => false,
        }
}
fn audit_route(value: &obs::AuditRoute) -> bool {
    use obs::audit_route::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::RegisteredProcedure(value)) => text(value, 256) && value.starts_with('/'),
            Some(Result::OtherRegisteredRoute(value) | Result::NotRecorded(value)) => empty(value),
            None => false,
        }
}
fn metadata_invalid(value: &obs::StoredMetadataInvalid) -> bool {
    let mut seen = Vec::with_capacity(6);
    !value.fields.is_empty()
        && value.fields.len() <= 6
        && value.__buffa_unknown_fields.is_empty()
        && value.fields.iter().all(|field| {
            let valid =
                nonzero(*field, obs::StoredMetadataField::Unspecified) && !seen.contains(field);
            seen.push(*field);
            valid
        })
}
fn audit_row(value: &obs::AuditRow, source: EnumValue<obs::AuditSource>) -> bool {
    use obs::{audit_author::Result as A, audit_row::Result};
    value.id > 0
        && value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Authentication(value)) => {
                source == EnumValue::Known(obs::AuditSource::Authentication)
                    && value.__buffa_unknown_fields.is_empty()
                    && value.occurred_at.as_option().is_some_and(timestamp)
                    && nonzero(value.action, obs::AuthAuditAction::Unspecified)
                    && nonzero(value.outcome, obs::AuditOutcome::Unspecified)
                    && value.author.as_option().is_some_and(|author| {
                        audit_author(author)
                            && !matches!(author.result, Some(A::InternalIntegration(_)))
                    })
                    && value.route.as_option().is_some_and(audit_route)
            }
            Some(Result::SecretAccess(value)) => {
                source == EnumValue::Known(obs::AuditSource::SecretAccess)
                    && value.__buffa_unknown_fields.is_empty()
                    && value.occurred_at.as_option().is_some_and(timestamp)
                    && nonzero(value.reason, obs::SecretDenialReason::Unspecified)
                    && value.author.as_option().is_some_and(|author| {
                        audit_author(author)
                            && matches!(author.result, Some(A::InternalIntegration(_)))
                    })
            }
            Some(Result::StoredMetadataInvalid(value)) => metadata_invalid(value),
            None => false,
        }
}
fn audit_page(value: &obs::AuditPage) -> bool {
    let mut previous = None;
    source(value.source)
        && value.upper_id >= 0
        && (value.upper_id != 0 || value.rows.is_empty())
        && value.rows.len() <= 100
        && value.continuation.as_option().is_some_and(continuation)
        && value.__buffa_unknown_fields.is_empty()
        && value.rows.iter().all(|row| {
            let valid = audit_row(row, value.source)
                && row.id <= value.upper_id
                && previous.is_none_or(|previous| row.id < previous);
            previous = Some(row.id);
            valid
        })
}
pub fn admit_audits_response(
    value: &obs::QueryAuditsResponse,
) -> Result<Admitted<obs::QueryAuditsResponse>, AdmissionError> {
    use obs::query_audits_response::Result;
    let valid = value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Page(value)) => audit_page(value),
            Some(Result::StaleCursor(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        };
    admit(value, valid && sized(value, 256 << 10))
}
fn trace_ordinal(value: &obs::TraceOrdinal) -> bool {
    use obs::trace_ordinal::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Native(value)) => *value > 0 && *value < u64::MAX,
            Some(Result::Exhausted(value)) => empty(value),
            None => false,
        }
}
fn trace_failure(value: &obs::TraceFailure) -> bool {
    use obs::trace_failure::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Known(value)) => {
                nonzero(*value, obs::TraceExecutionErrorKind::Unspecified)
            }
            Some(Result::NoFailure(value) | Result::NotRecorded(value)) => empty(value),
            None => false,
        }
}
fn trace_row(value: &obs::TraceRow) -> bool {
    use obs::{controller_trace_record::MaxExceeded, trace_row::Record};
    value.run_id > 0
        && value.local_index < 512
        && value.ordinal.as_option().is_some_and(trace_ordinal)
        && value.__buffa_unknown_fields.is_empty()
        && match &value.record {
            Some(Record::Controller(value)) => {
                value.__buffa_unknown_fields.is_empty()
                    && nonzero(value.decision, obs::TraceDecision::Unspecified)
                    && match &value.max_exceeded {
                        Some(MaxExceeded::Mode(value)) => {
                            nonzero(*value, obs::TraceMaxExceededMode::Unspecified)
                        }
                        Some(MaxExceeded::NotApplicable(value)) => empty(value),
                        None => false,
                    }
            }
            Some(Record::Node(value)) => {
                value.__buffa_unknown_fields.is_empty()
                    && nonzero(value.action_kind, obs::TraceActionKind::Unspecified)
                    && nonzero(value.stage, obs::TraceNodeStage::Unspecified)
                    && value.occurred_at.as_option().is_some_and(timestamp)
                    && value.failure.as_option().is_some_and(trace_failure)
            }
            None => false,
        }
}
fn trace_cuts(value: &obs::TraceCutMetadata) -> bool {
    value.coherence == EnumValue::Known(obs::TraceCutCoherence::SeparateOwnerCuts)
        && nonzero(value.order, obs::TraceOrderState::Unspecified)
        && value
            .controller_captured_at
            .as_option()
            .is_some_and(timestamp)
        && value.node_captured_at.as_option().is_some_and(timestamp)
        && value.__buffa_unknown_fields.is_empty()
}
fn trace_page(value: &obs::TracePage) -> bool {
    let mut previous = None;
    value.run_id > 0
        && value.controller.as_option().is_some_and(controller)
        && value.cuts.as_option().is_some_and(trace_cuts)
        && value.rows.len() <= 100
        && value.continuation.as_option().is_some_and(continuation)
        && value.__buffa_unknown_fields.is_empty()
        && value.rows.iter().all(|row| {
            let exhausted = row.ordinal.as_option().is_some_and(|ordinal| {
                matches!(
                    ordinal.result,
                    Some(obs::trace_ordinal::Result::Exhausted(_))
                )
            });
            let valid = trace_row(row)
                && row.run_id == value.run_id
                && previous.is_none_or(|previous| row.local_index > previous)
                && (!exhausted
                    || value.cuts.order == EnumValue::Known(obs::TraceOrderState::Exhausted));
            previous = Some(row.local_index);
            valid
        })
}
pub fn admit_trace_response(
    value: &obs::ReadRunTraceResponse,
) -> Result<Admitted<obs::ReadRunTraceResponse>, AdmissionError> {
    use obs::read_run_trace_response::Result;
    let valid = value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Page(value)) => trace_page(value),
            Some(Result::Truncated(value)) => {
                nonzero(value.reason, obs::TraceTruncationReason::Unspecified)
                    && value.page.as_option().is_some_and(trace_page)
                    && value.__buffa_unknown_fields.is_empty()
            }
            Some(Result::NotRetained(value) | Result::StaleCursor(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        };
    admit(value, valid && sized(value, 256 << 10))
}
fn coverage_epoch(value: &obs::RecorderEpochCoverage) -> bool {
    use obs::recorder_epoch_coverage::{CloseTime, Predecessor};
    let closed = match &value.close_time {
        Some(CloseTime::ClosedAt(value)) => timestamp(value),
        Some(CloseTime::NotRecorded(none)) => !value.clean_close && empty(none),
        None => false,
    };
    let previous = match &value.predecessor {
        Some(Predecessor::Previous(value)) => {
            value.epoch.as_option().is_some_and(recorder_epoch)
                && value.__buffa_unknown_fields.is_empty()
        }
        Some(Predecessor::NoPrevious(value)) => empty(value),
        None => false,
    };
    value.epoch.as_option().is_some_and(recorder_epoch)
        && value.started_at.as_option().is_some_and(timestamp)
        && nonzero(
            value.reconciliation,
            obs::RecorderReconciliationState::Unspecified,
        )
        && closed
        && previous
        && value.__buffa_unknown_fields.is_empty()
}
fn coverage_gap(value: &obs::RecorderCoverageGap) -> bool {
    use obs::{known_lost::Result, recorder_coverage_gap::UtcBounds};
    if value.from_sequence == 0 || value.through_sequence < value.from_sequence {
        return false;
    }
    let count = value.known_lost.as_option().is_some_and(|count| {
        known_lost(count)
            && match &count.result {
                Some(Result::LowerBound(count)) => {
                    *count
                        <= value
                            .through_sequence
                            .saturating_sub(value.from_sequence)
                            .saturating_add(1)
                        && (value.envelope != EnumValue::Known(obs::GapEnvelope::ExactPositions)
                            || *count == value.through_sequence - value.from_sequence + 1)
                }
                Some(Result::NotKnown(_)) => {
                    value.envelope != EnumValue::Known(obs::GapEnvelope::ExactPositions)
                }
                None => false,
            }
    });
    value.from_sequence > 0
        && value.through_sequence >= value.from_sequence
        && value.epoch.as_option().is_some_and(recorder_epoch)
        && nonzero(value.reason, obs::GapReason::Unspecified)
        && nonzero(value.envelope, obs::GapEnvelope::Unspecified)
        && count
        && value.__buffa_unknown_fields.is_empty()
        && match &value.utc_bounds {
            Some(UtcBounds::UnknownUtc(value)) => empty(value),
            None => false,
        }
}
fn coverage(value: &obs::RecorderCoverageSnapshot) -> bool {
    if value.epochs.is_empty() || value.epochs.len() > 64 || value.gaps.len() > 512 {
        return false;
    }
    value.from.as_option().is_some_and(position)
        && value.through.as_option().is_some_and(position)
        && value.from.epoch == value.epochs[0].epoch.value
        && value.through.epoch == value.epochs[value.epochs.len() - 1].epoch.value
        && value.from.sequence <= value.epochs[0].checkpoint
        && value.through.sequence <= value.epochs[value.epochs.len() - 1].checkpoint
        && (value.from.epoch != value.through.epoch
            || value.from.sequence <= value.through.sequence)
        && value.epochs.iter().enumerate().all(|(i, epoch)| {
            coverage_epoch(epoch)
                && value.epochs[..i]
                    .iter()
                    .all(|previous| previous.epoch.value != epoch.epoch.value)
        })
        && value.gaps.iter().all(|gap| {
            coverage_gap(gap)
                && value.epochs.iter().any(|epoch| {
                    epoch.epoch.value == gap.epoch.value && gap.through_sequence <= epoch.checkpoint
                })
        })
        && value.__buffa_unknown_fields.is_empty()
}
pub fn admit_coverage_response(
    value: &obs::ReadRecorderCoverageResponse,
) -> Result<Admitted<obs::ReadRecorderCoverageResponse>, AdmissionError> {
    use obs::read_recorder_coverage_response::Result;
    let valid = value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Complete(value)) => coverage(value),
            Some(Result::Truncated(value)) => {
                nonzero(value.reason, obs::CoverageTruncationReason::Unspecified)
                    && value.__buffa_unknown_fields.is_empty()
            }
            Some(Result::NotRetained(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        };
    admit(value, valid && sized(value, 256 << 10))
}
