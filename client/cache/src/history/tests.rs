use super::*;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::home::{
    observability::v1 as obs, registry::v1 as registry, state::v1 as state,
};
use std::{
    collections::VecDeque,
    sync::{
        Mutex,
        atomic::{AtomicUsize, Ordering},
    },
};

fn time(seconds: i64, nanos: i32) -> Timestamp {
    Timestamp {
        seconds,
        nanos,
        ..Default::default()
    }
}
fn query(resolution: Resolution) -> Query {
    Query::new(
        "history-fixture".parse().unwrap(),
        wire::Period {
            start: time(1_792_540_800, 0).into(),
            end: time(1_792_544_400, 0).into(),
            ..Default::default()
        },
        resolution,
    )
    .unwrap()
}
fn scope(resolution: Resolution) -> wire::ReadScope {
    let period = query(resolution).period;
    wire::ReadScope {
        requested: period.clone().into(),
        retained: period.into(),
        retention_cutoff: time(1_792_540_000, 0).into(),
        as_of: time(1_792_544_500, 0).into(),
        resolution: resolution.wire().into(),
        limits: wire::Limits {
            max_points: 100,
            max_response_bytes: 256 * 1024,
            max_token_bytes: 512,
            query_timeout_millis: 2000,
            max_readers: 2,
            max_period_hours: match resolution {
                Resolution::Observations => 168,
                Resolution::Hourly => 500,
            },
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn coverage() -> wire::Coverage {
    let epoch = vec![1; 16];
    wire::Coverage {
        verdict: wire::CoverageVerdict::CompleteUnderPolicy.into(),
        logical: obs::RecorderCoverageSnapshot {
            from: obs::AdmissionPosition {
                epoch: epoch.clone(),
                sequence: 0,
                ..Default::default()
            }
            .into(),
            through: obs::AdmissionPosition {
                epoch: epoch.clone(),
                sequence: 30_000,
                ..Default::default()
            }
            .into(),
            epochs: vec![obs::RecorderEpochCoverage {
                epoch: obs::RecorderEpochIdentity {
                    value: epoch,
                    ..Default::default()
                }
                .into(),
                checkpoint: 30_000,
                started_at: time(1_792_540_000, 0).into(),
                close_time: Some(obs::recorder_epoch_coverage::CloseTime::NotRecorded(
                    Default::default(),
                )),
                predecessor: Some(obs::recorder_epoch_coverage::Predecessor::NoPrevious(
                    Default::default(),
                )),
                reconciliation: obs::RecorderReconciliationState::Complete.into(),
                ..Default::default()
            }],
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn descriptor() -> wire::NumericDescriptor {
    wire::NumericDescriptor {
        unit: "kWh".into(),
        canonical_unit: "kWh".into(),
        scale: 1.,
        dimension: wire::Dimension::Energy.into(),
        precision: 3,
        semantic: registry::MeasurementSemantic::Measurement.into(),
        aggregation: registry::MeasurementAggregation::Mean.into(),
        ..Default::default()
    }
}
fn observation(sequence: u64, nanos: i32, value: f64, decimal: Option<&str>) -> wire::Observation {
    let at = time(1_792_540_900, nanos);
    wire::Observation {
        position: obs::AdmissionPosition {
            epoch: vec![1; 16],
            sequence,
            ..Default::default()
        }
        .into(),
        descriptor: Some(descriptor()).into(),
        state: state::ObservedState {
            entity: registry::EntityRef {
                reference: Some(registry::entity_ref::Reference::EntityId(
                    "history-fixture".into(),
                )),
                ..Default::default()
            }
            .into(),
            quality: state::Quality {
                availability: state::Availability::Available.into(),
                freshness: state::Freshness::Stale.into(),
                provenance: state::Provenance::Restored.into(),
                ..Default::default()
            }
            .into(),
            revision: sequence,
            received_at: at.clone().into(),
            last_changed: at.clone().into(),
            last_updated: at.clone().into(),
            last_reported: at.into(),
            source_session: "synthetic".into(),
            value: Some(state::observed_state::Value::Measurement(Box::new(
                state::Measurement {
                    value,
                    unit: "kWh".into(),
                    decimal_value: decimal.map(str::to_owned),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn raw(points: Vec<wire::Observation>, next: Option<&str>) -> Reply {
    Reply::Observations(wire::QueryObservationsResponse {
        result: Some(wire::query_observations_response::Result::Page(Box::new(
            wire::ObservationPage {
                scope: scope(Resolution::Observations).into(),
                points,
                coverage: coverage().into(),
                continuation: wire::Continuation {
                    result: Some(match next {
                        Some(token) => wire::continuation::Result::NextPageToken(token.into()),
                        None => wire::continuation::Result::End(Default::default()),
                    }),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            },
        ))),
        ..Default::default()
    })
}

#[test]
fn preserves_zero_decimal_nanos_quality_and_logical_order_despite_clock_regression() {
    let points = [
        observation(1, 999, 0., None),
        observation(2, 1, 1., Some("12345678901234567890.000000000000000001")),
        observation(3, 1, 2., None),
    ];
    let originals: Vec<_> = points.iter().map(Message::encode_to_vec).collect();
    let mut capture = Capture::new(query(Resolution::Observations));
    assert!(matches!(
        capture
            .push(raw(vec![points[0].clone()], Some("next")))
            .unwrap(),
        Progress::Next(_)
    ));
    assert!(matches!(
        capture.push(raw(points[1..].to_vec(), None)).unwrap(),
        Progress::Complete
    ));
    let snapshot = capture.finish(None);
    assert_eq!(snapshot.points, 3);
    let Content::Retained(Pages::Observations(pages)) = snapshot.content else {
        panic!("wrong content")
    };
    let actual: Vec<_> = pages
        .iter()
        .flat_map(|p| &p.points)
        .map(Message::encode_to_vec)
        .collect();
    assert_eq!(actual, originals);
    assert!(pages[1].continuation.is_set());
}
#[test]
fn empty_progress_does_not_end_capture_and_repeated_tokens_are_rejected() {
    let mut capture = Capture::new(query(Resolution::Observations));
    assert!(matches!(
        capture.push(raw(vec![], Some("next"))).unwrap(),
        Progress::Next(_)
    ));
    assert!(matches!(
        capture.push(raw(vec![], Some("next"))),
        Err(ReadError::Contract(ReadFailure::DuplicateOrOutOfOrder))
    ));
    let mut capture = Capture::new(query(Resolution::Observations));
    assert!(matches!(
        capture.push(raw(vec![], None)).unwrap(),
        Progress::Complete
    ));
    assert_eq!(capture.finish(None).points, 0);
}
#[test]
fn rejects_duplicate_positions_scope_changes_and_foreign_entity() {
    for changed in [Change::Position, Change::Scope, Change::Entity] {
        let mut capture = Capture::new(query(Resolution::Observations));
        capture
            .push(raw(vec![observation(1, 1, 0., None)], Some("two")))
            .unwrap();
        let mut reply = raw(vec![observation(2, 2, 1., None)], None);
        let Reply::Observations(response) = &mut reply else {
            unreachable!()
        };
        let Some(wire::query_observations_response::Result::Page(page)) = &mut response.result
        else {
            unreachable!()
        };
        match changed {
            Change::Position => page.points[0].position.get_or_insert_default().sequence = 1,
            Change::Scope => {
                page.scope
                    .get_or_insert_default()
                    .as_of
                    .get_or_insert_default()
                    .seconds += 1
            }
            Change::Entity => {
                page.points[0]
                    .state
                    .get_or_insert_default()
                    .entity
                    .get_or_insert_default()
                    .reference = Some(registry::entity_ref::Reference::EntityId("other".into()))
            }
        }
        assert!(capture.push(reply).is_err());
    }
}
enum Change {
    Position,
    Scope,
    Entity,
}

fn hourly(points: Vec<wire::Aggregate>, next: Option<&str>) -> Reply {
    Reply::Hourly(wire::QueryAggregatesResponse {
        result: Some(wire::query_aggregates_response::Result::Page(Box::new(
            wire::AggregatePage {
                scope: scope(Resolution::Hourly).into(),
                points,
                coverage: coverage().into(),
                continuation: wire::Continuation {
                    result: Some(match next {
                        Some(token) => wire::continuation::Result::NextPageToken(token.into()),
                        None => wire::continuation::Result::End(Default::default()),
                    }),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            },
        ))),
        ..Default::default()
    })
}
fn aggregate(provenance: state::Provenance) -> wire::Aggregate {
    wire::Aggregate {
        bucket: query(Resolution::Hourly).period.into(),
        descriptor: descriptor().into(),
        quality: state::Quality {
            availability: state::Availability::Available.into(),
            freshness: state::Freshness::Fresh.into(),
            provenance: provenance.into(),
            ..Default::default()
        }
        .into(),
        duration_nanos: 1_000_000_000,
        minimum: Some(0.),
        maximum: Some(2.),
        mean: Some(1.),
        contribution: None,
        reset: wire::Reset::NotApplicable.into(),
        reasons: vec![wire::AggregateReason::QualityBreak.into()],
        ..Default::default()
    }
}
#[test]
fn keeps_distinct_hour_groups_and_server_statistics_without_merging_or_zero_filling() {
    let first = aggregate(state::Provenance::Observed);
    let second = aggregate(state::Provenance::Estimated);
    let originals = [first.encode_to_vec(), second.encode_to_vec()];
    let mut capture = Capture::new(query(Resolution::Hourly));
    capture
        .push(hourly(vec![first.clone()], Some("group-two")))
        .unwrap();
    assert!(matches!(
        capture.push(hourly(vec![second], None)).unwrap(),
        Progress::Complete
    ));
    let snapshot = capture.finish(None);
    let Content::Retained(Pages::Hourly(pages)) = snapshot.content else {
        panic!("wrong content")
    };
    let actual: Vec<_> = pages
        .iter()
        .flat_map(|p| &p.points)
        .map(Message::encode_to_vec)
        .collect();
    assert_eq!(actual, originals);
    let mut capture = Capture::new(query(Resolution::Hourly));
    capture
        .push(hourly(vec![first.clone()], Some("duplicate")))
        .unwrap();
    assert!(matches!(
        capture.push(hourly(vec![first], None)),
        Err(ReadError::Contract(ReadFailure::DuplicateOrOutOfOrder))
    ));
}
#[test]
fn not_retained_is_distinct_from_empty_and_cannot_replace_partial_pages() {
    fn absent() -> Reply {
        let mut scope = scope(Resolution::Observations);
        scope.retained = Default::default();
        Reply::Observations(wire::QueryObservationsResponse {
            result: Some(wire::query_observations_response::Result::NotRetained(
                Box::new(scope),
            )),
            ..Default::default()
        })
    }
    let mut capture = Capture::new(query(Resolution::Observations));
    let Progress::NotRetained(scope) = capture.push(absent()).unwrap() else {
        panic!("not empty")
    };
    assert!(matches!(
        capture.finish(Some(scope)).content,
        Content::NotRetained(_)
    ));
    let mut capture = Capture::new(query(Resolution::Observations));
    capture.push(raw(vec![], Some("next"))).unwrap();
    assert!(matches!(
        capture.push(absent()),
        Err(ReadError::Contract(ReadFailure::ChangedScope))
    ));
}
#[test]
fn stale_last_page_never_becomes_a_complete_snapshot() {
    let mut capture = Capture::new(query(Resolution::Observations));
    capture
        .push(raw(vec![observation(1, 1, 0., None)], Some("two")))
        .unwrap();
    let stale = Reply::Observations(wire::QueryObservationsResponse {
        result: Some(wire::query_observations_response::Result::StaleCursor(
            Default::default(),
        )),
        ..Default::default()
    });
    assert!(matches!(capture.push(stale), Err(ReadError::StaleCursor)));
}
#[test]
fn twenty_thousand_original_points_fit_and_page_scan_budget_is_finite() {
    let mut capture = Capture::new(query(Resolution::Observations));
    for page in 0..200 {
        let points = (1..=100)
            .map(|n| observation(page * 100 + n, n as i32, n as f64, None))
            .collect();
        let token = format!("page-{page}");
        capture
            .push(raw(points, if page == 199 { None } else { Some(&token) }))
            .unwrap();
    }
    let snapshot = capture.finish(None);
    assert_eq!(snapshot.points, 20_000);
    assert!(snapshot.encoded_bytes < MAX_ENCODED_BYTES);
    let mut capture = Capture::new(query(Resolution::Observations));
    for page in 0..MAX_PAGES {
        capture
            .push(raw(vec![], Some(&format!("scan-{page}"))))
            .unwrap();
    }
    assert!(matches!(
        capture.push(raw(vec![], None)),
        Err(ReadError::Contract(ReadFailure::BudgetExceeded))
    ));
}
#[test]
fn rejects_too_wide_or_unaligned_query_before_network() {
    for resolution in [Resolution::Observations, Resolution::Hourly] {
        let mut q = query(resolution);
        q.period.end.get_or_insert_default().seconds += 501 * 3600;
        assert!(Query::new(q.entity, q.period, resolution).is_err());
    }
    let mut q = query(Resolution::Hourly);
    q.period.start.get_or_insert_default().nanos = 1;
    assert!(Query::new(q.entity, q.period, q.resolution).is_err());
}

struct Api {
    replies: Mutex<VecDeque<Reply>>,
    calls: AtomicUsize,
    entered: tokio::sync::Notify,
}
#[async_trait::async_trait]
impl reader::Api for Api {
    async fn page(&self, _: &Query, _: String) -> Result<Reply, Failure> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let reply = self.replies.lock().unwrap().pop_front();
        if let Some(reply) = reply {
            return Ok(reply);
        }
        self.entered.notify_one();
        std::future::pending().await
    }
}
#[tokio::test]
async fn cancel_access_or_view_drops_a_blocked_last_page_without_retry() {
    for cancel_access in [false, true] {
        let api = Api {
            replies: Mutex::new(VecDeque::from([raw(
                vec![observation(1, 1, 0., None)],
                Some("two"),
            )])),
            calls: AtomicUsize::new(0),
            entered: tokio::sync::Notify::new(),
        };
        let cancel = CancellationToken::new();
        let access = CancellationToken::new();
        let work = reader::read_from(&api, query(Resolution::Observations), &cancel, &access);
        let interrupt = async {
            api.entered.notified().await;
            if cancel_access {
                access.cancel();
            } else {
                cancel.cancel();
            }
        };
        let (result, ()) = tokio::join!(work, interrupt);
        assert!(matches!(result, Err(ReadError::Cancelled)));
        assert_eq!(api.calls.load(Ordering::SeqCst), 2);
        assert!(matches!(
            reader::read_from(&api, query(Resolution::Observations), &cancel, &access).await,
            Err(ReadError::Cancelled)
        ));
        assert_eq!(api.calls.load(Ordering::SeqCst), 2);
    }
}
