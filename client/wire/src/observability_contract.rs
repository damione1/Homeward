//! Structural observation admission. These immutable copies grant no rights;
//! the common server transport and current Authority remain authoritative.

use crate::home::api::v2 as obs;
use crate::recorder_evidence_contract::{decimal, number};
use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::{Empty, Timestamp};
use home_client_model::api::HomeName;
use home_client_model::api_observation::{RuntimeControllerName, RuntimeRunName};
use home_client_model::auth::AccountName;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AdmissionError {
    InvalidArgument,
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
        Err(AdmissionError::InvalidArgument)
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
fn account(value: &str) -> bool {
    AccountName::try_from(value).is_ok()
}
fn page(size: i32, token: &str) -> bool {
    size >= 0 && token.len() <= 512
}
fn home(value: &str) -> bool {
    HomeName::try_from(value).is_ok()
}
fn singleton(value: &str, suffix: &str) -> bool {
    value.strip_suffix(suffix).is_some_and(home)
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
fn source(value: EnumValue<obs::ObservationAuditSource>) -> bool {
    match value {
        EnumValue::Known(
            obs::ObservationAuditSource::Authentication | obs::ObservationAuditSource::SecretAccess,
        ) => true,
        EnumValue::Known(obs::ObservationAuditSource::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
fn position(value: &obs::AdmissionPosition) -> bool {
    crate::recorder_evidence_contract::position(value)
}
fn epoch(value: &obs::ObservationProcessObservationEpoch) -> bool {
    identity(&value.value, 16) && value.__buffa_unknown_fields.is_empty()
}
fn recorder_epoch(value: &obs::RecorderEpochIdentity) -> bool {
    identity(&value.value, 16) && value.__buffa_unknown_fields.is_empty()
}
fn controller(value: &str) -> bool {
    RuntimeControllerName::try_from(value).is_ok()
}
fn unavailable(value: &obs::ObservationReadUnavailable) -> bool {
    nonzero(
        value.reason,
        obs::ObservationReadUnavailableReason::Unspecified,
    ) && value.__buffa_unknown_fields.is_empty()
}

pub fn admit_get_health(
    value: &obs::GetHealthRequest,
) -> Result<Admitted<obs::GetHealthRequest>, AdmissionError> {
    admit(
        value,
        singleton(&value.name, "/health") && value.__buffa_unknown_fields.is_empty(),
    )
}
pub fn admit_get_metrics(
    value: &obs::GetMetricsRequest,
) -> Result<Admitted<obs::GetMetricsRequest>, AdmissionError> {
    admit(
        value,
        singleton(&value.name, "/metrics") && value.__buffa_unknown_fields.is_empty(),
    )
}
fn integration_health_name(value: &str) -> bool {
    let Some(instance) = value.strip_suffix("/integrationHealth") else {
        return false;
    };
    let Some((parent, id)) = instance.split_once("/integrationInstances/") else {
        return false;
    };
    home(parent)
        && !id.is_empty()
        && id.len() <= 63
        && id.bytes().next().is_some_and(|b| b.is_ascii_lowercase())
        && id
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}
pub fn admit_get_integration_health(
    value: &obs::GetIntegrationHealthRequest,
) -> Result<Admitted<obs::GetIntegrationHealthRequest>, AdmissionError> {
    admit(
        value,
        integration_health_name(&value.name) && value.__buffa_unknown_fields.is_empty(),
    )
}
pub fn admit_query_runtime_logs(
    value: &obs::QueryRuntimeLogsRequest,
) -> Result<Admitted<obs::QueryRuntimeLogsRequest>, AdmissionError> {
    admit(
        value,
        home(&value.parent)
            && page(value.page_size, &value.page_token)
            && value.__buffa_unknown_fields.is_empty(),
    )
}
pub fn admit_query_audits(
    value: &obs::QueryAuditsRequest,
) -> Result<Admitted<obs::QueryAuditsRequest>, AdmissionError> {
    admit(
        value,
        source(value.source)
            && home(&value.parent)
            && page(value.page_size, &value.page_token)
            && value.__buffa_unknown_fields.is_empty(),
    )
}
pub fn admit_query_run_trace(
    value: &obs::QueryRunTraceRequest,
) -> Result<Admitted<obs::QueryRunTraceRequest>, AdmissionError> {
    admit(
        value,
        RuntimeRunName::try_from(value.parent.as_str()).is_ok()
            && page(value.page_size, &value.page_token)
            && value.__buffa_unknown_fields.is_empty(),
    )
}
pub fn admit_query_recorder_coverage(
    value: &obs::QueryRecorderCoverageRequest,
) -> Result<Admitted<obs::QueryRecorderCoverageRequest>, AdmissionError> {
    use obs::query_recorder_coverage_request::Through;
    let valid = value.from.as_option().is_some_and(|from| {
        position(from)
            && match &value.through {
                Some(Through::Position(through)) => {
                    position(through)
                        && (through.epoch != from.epoch
                            || number(&through.sequence) >= number(&from.sequence))
                }
                Some(Through::LatestInEpoch(through)) => recorder_epoch(through),
                None => false,
            }
    });
    admit(
        value,
        valid && home(&value.parent) && value.__buffa_unknown_fields.is_empty(),
    )
}

fn counter(value: &obs::ObservationCounterMeasurement) -> bool {
    use obs::observation_counter_measurement::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Exact(value)) => *value >= 0,
            Some(Result::Saturated(value)) => {
                value.lower_bound == i64::MAX && value.__buffa_unknown_fields.is_empty()
            }
            Some(Result::NotMeasured(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn positive_counter(value: &obs::ObservationCounterMeasurement) -> bool {
    counter(value)
        && match &value.result {
            Some(obs::observation_counter_measurement::Result::Exact(value)) => *value > 0,
            Some(obs::observation_counter_measurement::Result::Saturated(_)) => true,
            Some(
                obs::observation_counter_measurement::Result::NotMeasured(_)
                | obs::observation_counter_measurement::Result::Unavailable(_),
            )
            | None => false,
        }
}
fn gauge(value: &obs::ObservationGaugeMeasurement) -> bool {
    use obs::observation_gauge_measurement::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Value(value)) => *value >= 0,
            Some(Result::NotMeasured(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn gauge_le(
    left: &obs::ObservationGaugeMeasurement,
    right: &obs::ObservationGaugeMeasurement,
) -> bool {
    use obs::observation_gauge_measurement::Result;
    if let (Some(Result::Value(left)), Some(Result::Value(right))) = (&left.result, &right.result) {
        left <= right
    } else {
        true
    }
}
fn lag(value: &obs::ObservationLagMeasurement) -> bool {
    use obs::observation_lag_measurement::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::SameEpochPositions(value)) => decimal(value),
            Some(Result::Invalidated(value) | Result::EpochMismatch(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn measured_position(value: &obs::ObservationPositionMeasurement) -> bool {
    use obs::observation_position_measurement::Result;
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
fn observation_runtime_author(value: &obs::ObservationRuntimeAuthor) -> bool {
    use obs::observation_runtime_author::Result;
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
fn event(value: &obs::ObservationRuntimeEvent) -> bool {
    use obs::{ObservationLogCode as C, ObservationLogComponent as P, ObservationLogLevel as L};
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
        && value
            .author
            .as_option()
            .is_some_and(observation_runtime_author)
        && sized(value, 1024)
}
pub fn admit_runtime_event(
    value: &obs::ObservationRuntimeEvent,
) -> Result<Admitted<obs::ObservationRuntimeEvent>, AdmissionError> {
    admit(value, event(value))
}
pub fn admit_counter(
    value: &obs::ObservationCounterMeasurement,
) -> Result<Admitted<obs::ObservationCounterMeasurement>, AdmissionError> {
    admit(value, counter(value))
}

fn runtime_page(value: &obs::QueryRuntimeLogsResponse) -> bool {
    let mut previous = 0;
    decimal(&value.upper_sequence)
        && value.rows.len() <= 100
        && value.next_page_token.len() <= 512
        && value.__buffa_unknown_fields.is_empty()
        && value.process_epoch.as_option().is_some_and(epoch)
        && value.rows.iter().all(|row| {
            let valid = decimal(&row.sequence)
                && number(&row.sequence) > previous
                && number(&row.sequence) <= number(&value.upper_sequence)
                && row.__buffa_unknown_fields.is_empty()
                && row.event.as_option().is_some_and(event);
            previous = number(&row.sequence);
            valid
        })
}
pub fn admit_runtime_logs_response(
    value: &obs::QueryRuntimeLogsResponse,
) -> Result<Admitted<obs::QueryRuntimeLogsResponse>, AdmissionError> {
    admit(value, runtime_page(value) && sized(value, 256 << 10))
}

fn generation(value: &obs::ObservationGenerationPresence) -> bool {
    use obs::observation_generation_presence::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Present(value)) => {
                value.value > 0
                    && value.value <= i64::MAX
                    && value.__buffa_unknown_fields.is_empty()
            }
            Some(Result::NotLoaded(value)) => empty(value),
            None => false,
        }
}
fn observation_server_health(value: &obs::ObservationServerHealth) -> bool {
    let generation = value.generation.as_option().is_some_and(generation);
    let ready = value.readiness == EnumValue::Known(obs::ObservationServerReadiness::Ready);
    let present = value.generation.as_option().is_some_and(|g| {
        matches!(
            g.result,
            Some(obs::observation_generation_presence::Result::Present(_))
        )
    });
    let mut seen = Vec::with_capacity(6);
    generation
        && nonzero(
            value.readiness,
            obs::ObservationServerReadiness::Unspecified,
        )
        && value.causes.len() <= 6
        && value.causes.iter().all(|cause| {
            let valid =
                nonzero(*cause, obs::ObservationCoreCause::Unspecified) && !seen.contains(cause);
            seen.push(*cause);
            valid
        })
        && (!ready || present && value.causes.is_empty())
        && value.__buffa_unknown_fields.is_empty()
}
fn observation_durable_health(value: &obs::ObservationDurableHealth) -> bool {
    use obs::observation_durable_health::Evidence;
    nonzero(
        value.availability,
        obs::ObservationDurableAvailability::Unspecified,
    ) && value.__buffa_unknown_fields.is_empty()
        && match &value.evidence {
            Some(Evidence::ObservedAt(at)) => {
                value.availability != EnumValue::Known(obs::ObservationDurableAvailability::Unknown)
                    && timestamp(at)
            }
            Some(Evidence::NotObserved(none)) => {
                value.availability == EnumValue::Known(obs::ObservationDurableAvailability::Unknown)
                    && empty(none)
            }
            None => false,
        }
}
fn observation_recorder_health(value: &obs::ObservationRecorderHealth) -> bool {
    use obs::observation_recorder_health::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Observed(value)) => {
                nonzero(value.phase, obs::ObservationRecorderPhase::Unspecified)
                    && nonzero(
                        value.state,
                        obs::ObservationRecorderHealthState::Unspecified,
                    )
                    && value.observed_at.as_option().is_some_and(timestamp)
                    && value.__buffa_unknown_fields.is_empty()
            }
            Some(Result::NotInstalled(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
pub fn admit_health_response(value: &obs::Health) -> Result<Admitted<obs::Health>, AdmissionError> {
    let valid = singleton(&value.name, "/health") && {
        let health = value;
        let mut seen = Vec::with_capacity(5);
        let mut total = 0i64;
        health.__buffa_unknown_fields.is_empty()
            && health.process_epoch.as_option().is_some_and(epoch)
            && health.liveness == EnumValue::Known(obs::ObservationLiveness::Live)
            && health
                .server
                .as_option()
                .is_some_and(observation_server_health)
            && health
                .durable
                .as_option()
                .is_some_and(observation_durable_health)
            && health
                .recorder
                .as_option()
                .is_some_and(observation_recorder_health)
            && health.pilots.len() == 5
            && health.pilots.iter().all(|pilot| {
                let valid = nonzero(pilot.phase, obs::ObservationPilotPhase::Unspecified)
                    && (0..=1024).contains(&pilot.instances)
                    && !seen.contains(&pilot.phase)
                    && pilot.__buffa_unknown_fields.is_empty();
                seen.push(pilot.phase);
                total = total.saturating_add(pilot.instances);
                valid
            })
            && total <= 1024
    };
    admit(
        value,
        valid && value.__buffa_unknown_fields.is_empty() && sized(value, 16 << 10),
    )
}
pub fn admit_integration_health_response(
    value: &obs::IntegrationHealth,
) -> Result<Admitted<obs::IntegrationHealth>, AdmissionError> {
    use obs::{
        observation_integration_report::AdmissionTime,
        observation_integration_report_presence::Result as R,
        observation_integration_supervision::Result as S,
    };
    let report = value.report.as_option().is_some_and(|report| {
        report.__buffa_unknown_fields.is_empty()
            && match &report.result {
                Some(R::Present(value)) => {
                    nonzero(
                        value.status,
                        obs::ObservationIntegrationHealthStatus::Unspecified,
                    ) && value.__buffa_unknown_fields.is_empty()
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
                    nonzero(value.phase, obs::ObservationPilotPhase::Unspecified)
                        && (0..=17).contains(&value.attempts)
                        && (0..=256).contains(&value.tasks)
                        && value.__buffa_unknown_fields.is_empty()
                }
                Some(S::NotSupervised(value)) => empty(value),
                None => false,
            }
    });
    admit(
        value,
        integration_health_name(&value.name)
            && report
            && supervision
            && value.__buffa_unknown_fields.is_empty()
            && sized(value, 8 << 10),
    )
}

fn installed_queue(value: &obs::ObservationInstalledQueueMetrics) -> bool {
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
fn queue(value: &obs::ObservationQueueAggregate) -> bool {
    use obs::observation_queue_aggregate::Result;
    nonzero(value.queue_class, obs::ObservationQueueClass::Unspecified)
        && value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Installed(installed)) => {
                use obs::observation_gauge_measurement::Result as GaugeResult;
                let inflight_unmeasured = matches!(
                    installed.in_flight_entries.result,
                    Some(GaugeResult::NotMeasured(_))
                );
                let pending_unmeasured = matches!(
                    installed.pending_entries.result,
                    Some(GaugeResult::NotMeasured(_))
                );
                let applicable = match value.queue_class {
                    EnumValue::Known(obs::ObservationQueueClass::WorkerCommands) => {
                        !inflight_unmeasured
                            && !pending_unmeasured
                            && gauge_le(&installed.in_flight_entries, &installed.pending_entries)
                    }
                    EnumValue::Known(obs::ObservationQueueClass::CloudRequests) => {
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
fn observation_queue_lag(value: &obs::ObservationQueueLag) -> bool {
    use obs::observation_queue_lag::Result;
    matches!(
        value.queue_class,
        EnumValue::Known(
            obs::ObservationQueueClass::Ui
                | obs::ObservationQueueClass::Edge
                | obs::ObservationQueueClass::Recorder
                | obs::ObservationQueueClass::View
        )
    ) && value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Installed(value)) => lag(value),
            Some(Result::NotInstalled(value)) => empty(value),
            Some(Result::Unavailable(value)) => unavailable(value),
            None => false,
        }
}
fn observation_active_coverage(value: &obs::ObservationActiveCoverage) -> bool {
    use obs::observation_active_coverage::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::AtRisk(value)) => {
                value.from.as_option().is_some_and(position)
                    && value.through.as_option().is_some_and(position)
                    && value.from.epoch == value.through.epoch
                    && number(&value.from.sequence) > 0
                    && number(&value.through.sequence) >= number(&value.from.sequence)
                    && value.known_lost.as_option().is_some_and(|lost| {
                        known_lost(lost)
                            && match &lost.result {
                                Some(obs::known_lost::Result::LowerBound(count)) => {
                                    u64::try_from(*count).is_ok_and(|count| {
                                        count
                                            <= number(&value.through.sequence)
                                                - number(&value.from.sequence)
                                                + 1
                                    })
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
fn observation_recorder_metrics(value: &obs::ObservationRecorderMetrics) -> bool {
    use obs::observation_recorder_metrics::Result;
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
                        .is_some_and(observation_active_coverage)
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
fn observation_action_metrics(value: &obs::ObservationActionMetrics) -> bool {
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
            nonzero(first.ledger, obs::ObservationAttemptLedger::Unspecified)
                && first.indeterminate_total.as_option().is_some_and(counter)
                && first.__buffa_unknown_fields.is_empty()
        })
}
fn observation_run_metrics(value: &obs::ObservationRunMetrics) -> bool {
    use obs::observation_run_metrics::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Installed(value)) => {
                value.__buffa_unknown_fields.is_empty()
                    && value.preparing.as_option().is_some_and(gauge)
                    && controller(&value.controller)
                    && value.kinds.len() == 2
                    && value.kinds[0].kind != value.kinds[1].kind
                    && value.kinds.iter().all(|kind| {
                        nonzero(kind.kind, obs::ObservationRunKind::Unspecified)
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
    value: &obs::Metrics,
) -> Result<Admitted<obs::Metrics>, AdmissionError> {
    let valid = singleton(&value.name, "/metrics") && {
        let metrics = value;
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
                let valid = observation_queue_lag(q) && !lags.contains(&q.queue_class);
                lags.push(q.queue_class);
                valid
            })
            && metrics
                .recorder
                .as_option()
                .is_some_and(observation_recorder_metrics)
            && metrics
                .actions
                .as_option()
                .is_some_and(observation_action_metrics)
            && metrics
                .runs
                .as_option()
                .is_some_and(observation_run_metrics)
            && metrics.journal.as_option().is_some_and(|journal| {
                journal.retained_rows.as_option().is_some_and(gauge)
                    && journal.charged_bytes.as_option().is_some_and(gauge)
                    && journal.evicted_total.as_option().is_some_and(counter)
                    && journal.__buffa_unknown_fields.is_empty()
            })
            && metrics.cuts.len() <= 8
            && metrics.cuts.iter().all(|cut| {
                let valid = nonzero(cut.owner, obs::ObservationMetricOwner::Unspecified)
                    && !cuts.contains(&cut.owner)
                    && cut.captured_at.as_option().is_some_and(timestamp)
                    && cut.__buffa_unknown_fields.is_empty();
                cuts.push(cut.owner);
                valid
            })
    };
    admit(
        value,
        valid && value.__buffa_unknown_fields.is_empty() && sized(value, 32 << 10),
    )
}

fn observation_integration_reference(value: &obs::ObservationIntegrationReference) -> bool {
    identity(&value.value, 16) && value.__buffa_unknown_fields.is_empty()
}
fn observation_audit_author(value: &obs::ObservationAuditAuthor) -> bool {
    use obs::observation_audit_author::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::HumanAccount(value)) => account(value),
            Some(Result::InternalIntegration(value)) => observation_integration_reference(value),
            Some(Result::NotRecorded(value)) => empty(value),
            None => false,
        }
}
fn observation_audit_route(value: &obs::ObservationAuditRoute) -> bool {
    use obs::observation_audit_route::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::RegisteredProcedure(value)) => text(value, 256) && value.starts_with('/'),
            Some(Result::OtherRegisteredRoute(value) | Result::NotRecorded(value)) => empty(value),
            None => false,
        }
}
fn metadata_invalid(value: &obs::ObservationStoredMetadataInvalid) -> bool {
    let mut seen = Vec::with_capacity(6);
    !value.fields.is_empty()
        && value.fields.len() <= 6
        && value.__buffa_unknown_fields.is_empty()
        && value.fields.iter().all(|field| {
            let valid = nonzero(*field, obs::ObservationStoredMetadataField::Unspecified)
                && !seen.contains(field);
            seen.push(*field);
            valid
        })
}
fn observation_audit_row(
    value: &obs::ObservationAuditRow,
    source: EnumValue<obs::ObservationAuditSource>,
) -> bool {
    use obs::{observation_audit_author::Result as A, observation_audit_row::Result};
    value.id > 0
        && value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Authentication(value)) => {
                source == EnumValue::Known(obs::ObservationAuditSource::Authentication)
                    && value.__buffa_unknown_fields.is_empty()
                    && value.occurred_at.as_option().is_some_and(timestamp)
                    && nonzero(value.action, obs::ObservationAuthAuditAction::Unspecified)
                    && nonzero(value.outcome, obs::ObservationAuditOutcome::Unspecified)
                    && value.author.as_option().is_some_and(|author| {
                        observation_audit_author(author)
                            && !matches!(author.result, Some(A::InternalIntegration(_)))
                    })
                    && value.route.as_option().is_some_and(observation_audit_route)
            }
            Some(Result::SecretAccess(value)) => {
                source == EnumValue::Known(obs::ObservationAuditSource::SecretAccess)
                    && value.__buffa_unknown_fields.is_empty()
                    && value.occurred_at.as_option().is_some_and(timestamp)
                    && nonzero(
                        value.reason,
                        obs::ObservationSecretDenialReason::Unspecified,
                    )
                    && value.author.as_option().is_some_and(|author| {
                        observation_audit_author(author)
                            && matches!(author.result, Some(A::InternalIntegration(_)))
                    })
            }
            Some(Result::StoredMetadataInvalid(value)) => metadata_invalid(value),
            None => false,
        }
}
fn audit_page(value: &obs::QueryAuditsResponse) -> bool {
    let mut previous = None;
    source(value.source)
        && value.rows.len() <= 100
        && value.next_page_token.len() <= 512
        && value.__buffa_unknown_fields.is_empty()
        && value.rows.iter().all(|row| {
            let valid = observation_audit_row(row, value.source)
                && previous.is_none_or(|previous| row.id < previous);
            previous = Some(row.id);
            valid
        })
}
pub fn admit_audits_response(
    value: &obs::QueryAuditsResponse,
) -> Result<Admitted<obs::QueryAuditsResponse>, AdmissionError> {
    admit(value, audit_page(value) && sized(value, 256 << 10))
}
fn observation_trace_ordinal(value: &obs::ObservationTraceOrdinal) -> bool {
    use obs::observation_trace_ordinal::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Native(value)) => {
                decimal(value) && number(value) > 0 && number(value) < u64::MAX
            }
            Some(Result::Exhausted(value)) => empty(value),
            None => false,
        }
}
fn observation_trace_failure(value: &obs::ObservationTraceFailure) -> bool {
    use obs::observation_trace_failure::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::Known(value)) => {
                nonzero(*value, obs::ObservationTraceExecutionErrorKind::Unspecified)
            }
            Some(Result::NoFailure(value) | Result::NotRecorded(value)) => empty(value),
            None => false,
        }
}
fn observation_trace_row(value: &obs::ObservationTraceRow) -> bool {
    use obs::{observation_controller_trace_record::MaxExceeded, observation_trace_row::Record};
    RuntimeRunName::try_from(value.run.as_str()).is_ok()
        && (0..512).contains(&value.local_index)
        && value
            .ordinal
            .as_option()
            .is_some_and(observation_trace_ordinal)
        && value.__buffa_unknown_fields.is_empty()
        && match &value.record {
            Some(Record::Controller(value)) => {
                value.__buffa_unknown_fields.is_empty()
                    && nonzero(value.decision, obs::ObservationTraceDecision::Unspecified)
                    && value
                        .trigger_position
                        .as_option()
                        .is_none_or(|position_value| {
                            position(position_value)
                                && matches!(
                                    value.decision,
                                    EnumValue::Known(
                                        obs::ObservationTraceDecision::Admitted
                                            | obs::ObservationTraceDecision::Queued
                                            | obs::ObservationTraceDecision::RestartPending
                                    )
                                )
                        })
                    && match &value.max_exceeded {
                        Some(MaxExceeded::Mode(value)) => {
                            nonzero(*value, obs::ObservationTraceMaxExceededMode::Unspecified)
                        }
                        Some(MaxExceeded::NotApplicable(value)) => empty(value),
                        None => false,
                    }
            }
            Some(Record::Node(value)) => {
                value.__buffa_unknown_fields.is_empty()
                    && nonzero(
                        value.action_kind,
                        obs::ObservationTraceActionKind::Unspecified,
                    )
                    && nonzero(value.stage, obs::ObservationTraceNodeStage::Unspecified)
                    && value.occurred_at.as_option().is_some_and(timestamp)
                    && value
                        .failure
                        .as_option()
                        .is_some_and(observation_trace_failure)
            }
            None => false,
        }
}
fn trace_cuts(value: &obs::ObservationTraceCutMetadata) -> bool {
    decimal(&value.controller_evicted)
        && decimal(&value.node_evicted)
        && value.coherence == EnumValue::Known(obs::ObservationTraceCutCoherence::SeparateOwnerCuts)
        && nonzero(value.order, obs::ObservationTraceOrderState::Unspecified)
        && value
            .controller_captured_at
            .as_option()
            .is_some_and(timestamp)
        && value.node_captured_at.as_option().is_some_and(timestamp)
        && value.__buffa_unknown_fields.is_empty()
}
fn trace_page(value: &obs::QueryRunTraceResponse) -> bool {
    let mut previous = None;
    let mut run = None;
    RuntimeRunName::try_from(value.run.as_str()).is_ok()
        && value.rows.len() <= 100
        && value.next_page_token.len() <= 512
        && value.cuts.as_option().is_some_and(trace_cuts)
        && value.__buffa_unknown_fields.is_empty()
        && value
            .truncation_reason
            .is_none_or(|r| nonzero(r, obs::ObservationTraceTruncationReason::Unspecified))
        && value.rows.iter().all(|row| {
            let exhausted = row.ordinal.as_option().is_some_and(|o| {
                matches!(
                    o.result,
                    Some(obs::observation_trace_ordinal::Result::Exhausted(_))
                )
            });
            let valid = observation_trace_row(row)
                && row.run == value.run
                && previous.is_none_or(|p| row.local_index > p)
                && run.is_none_or(|r| r == row.run.as_str())
                && (!exhausted
                    || value.cuts.order
                        == EnumValue::Known(obs::ObservationTraceOrderState::Exhausted));
            previous = Some(row.local_index);
            run = Some(row.run.as_str());
            valid
        })
}
pub fn admit_trace_response(
    value: &obs::QueryRunTraceResponse,
) -> Result<Admitted<obs::QueryRunTraceResponse>, AdmissionError> {
    admit(value, trace_page(value) && sized(value, 256 << 10))
}
fn coverage(value: &obs::RecorderCoverageSnapshot) -> bool {
    crate::recorder_evidence_contract::coverage(value)
}

pub fn admit_coverage_response(
    value: &obs::QueryRecorderCoverageResponse,
) -> Result<Admitted<obs::QueryRecorderCoverageResponse>, AdmissionError> {
    admit(
        value,
        value.__buffa_unknown_fields.is_empty()
            && value.coverage.as_option().is_some_and(coverage)
            && sized(value, 256 << 10),
    )
}
