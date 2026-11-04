use super::*;
use buffa::MessageName;
use buffa_types::google::protobuf::Timestamp;
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
        EntityName::try_from("homes/test/entities/history-fixture").unwrap(),
        wire::HistoryPeriod {
            start: time(1_792_540_800, 0).into(),
            end: time(1_792_544_400, 0).into(),
            ..Default::default()
        },
        resolution,
    )
    .unwrap()
}
fn scope(resolution: Resolution) -> wire::HistoryReadScope {
    let period = query(resolution).period;
    wire::HistoryReadScope {
        requested: period.clone().into(),
        retained: period.into(),
        retention_cutoff: time(1_792_540_000, 0).into(),
        as_of: time(1_792_544_500, 0).into(),
        resolution: resolution.wire().into(),
        limits: wire::HistoryLimits {
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
fn coverage() -> wire::HistoryCoverage {
    wire::HistoryCoverage {
        verdict: wire::HistoryCoverageVerdict::NotProven.into(),
        ..Default::default()
    }
}
fn descriptor() -> wire::HistoryNumericDescriptor {
    wire::HistoryNumericDescriptor {
        unit: "kWh".into(),
        canonical_unit: "kWh".into(),
        scale: 1.,
        dimension: wire::HistoryDimension::Energy.into(),
        precision: 3,
        semantic: wire::MeasurementSemantic::Measurement.into(),
        aggregation: wire::MeasurementAggregation::Mean.into(),
        ..Default::default()
    }
}
fn observation(
    sequence: u64,
    nanos: i32,
    value: f64,
    decimal: Option<&str>,
) -> wire::HistoryObservation {
    let at = time(1_792_540_900, nanos);
    wire::HistoryObservation {
        observation_id: format!("{sequence:064x}"),
        descriptor: Some(descriptor()).into(),
        state: wire::HistoricalState {
            quality: wire::ObservationQuality {
                availability: wire::Availability::Available.into(),
                freshness: wire::Freshness::Stale.into(),
                provenance: wire::Provenance::Restored.into(),
                ..Default::default()
            }
            .into(),
            receive_time: at.clone().into(),
            last_change_time: at.clone().into(),
            last_update_time: at.clone().into(),
            last_report_time: at.into(),
            value: Some(wire::historical_state::Value::Measurement(Box::new(
                wire::HistoricalMeasurement {
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
fn raw(points: Vec<wire::HistoryObservation>, next: Option<&str>) -> Reply {
    Reply::Observations(wire::QueryObservationsResponse {
        observations: points,
        next_page_token: next.unwrap_or_default().into(),
        scope: scope(Resolution::Observations).into(),
        coverage: coverage().into(),
        ..Default::default()
    })
}

#[test]
fn preserves_zero_decimal_nanos_quality_and_ids_despite_clock_regression() {
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
        .flat_map(|p| &p.observations)
        .map(Message::encode_to_vec)
        .collect();
    assert_eq!(actual, originals);
    assert!(pages[1].next_page_token.is_empty());
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
fn rejects_duplicate_ids_scope_changes_and_malformed_ids() {
    for changed in [Change::Identity, Change::Scope, Change::MalformedIdentity] {
        let mut capture = Capture::new(query(Resolution::Observations));
        capture
            .push(raw(vec![observation(1, 1, 0., None)], Some("two")))
            .unwrap();
        let mut reply = raw(vec![observation(2, 2, 1., None)], None);
        let Reply::Observations(response) = &mut reply else {
            unreachable!()
        };
        let page = response;
        match changed {
            Change::Identity => page.observations[0].observation_id = format!("{:064x}", 1),
            Change::Scope => {
                page.scope
                    .get_or_insert_default()
                    .as_of
                    .get_or_insert_default()
                    .seconds += 1
            }
            Change::MalformedIdentity => {
                page.observations[0].observation_id = "not-an-admitted-id".into()
            }
        }
        assert!(capture.push(reply).is_err());
    }
}
enum Change {
    Identity,
    Scope,
    MalformedIdentity,
}

fn hourly(points: Vec<wire::HistoryAggregate>, next: Option<&str>) -> Reply {
    Reply::Hourly(wire::QueryAggregatesResponse {
        aggregates: points,
        next_page_token: next.unwrap_or_default().into(),
        scope: scope(Resolution::Hourly).into(),
        coverage: coverage().into(),
        ..Default::default()
    })
}
fn aggregate(provenance: wire::Provenance) -> wire::HistoryAggregate {
    wire::HistoryAggregate {
        aggregate_id: format!("{:064x}", provenance as i32),
        bucket: query(Resolution::Hourly).period.into(),
        descriptor: descriptor().into(),
        quality: wire::ObservationQuality {
            availability: wire::Availability::Available.into(),
            freshness: wire::Freshness::Current.into(),
            provenance: provenance.into(),
            ..Default::default()
        }
        .into(),
        duration_nanos: 1_000_000_000,
        minimum: Some(0.),
        maximum: Some(2.),
        mean: Some(1.),
        contribution: None,
        reset: wire::HistoryReset::NotApplicable.into(),
        reasons: vec![wire::HistoryAggregateReason::QualityBreak.into()],
        ..Default::default()
    }
}
#[test]
fn keeps_distinct_hour_groups_and_server_statistics_without_merging_or_zero_filling() {
    let first = aggregate(wire::Provenance::Observed);
    let second = aggregate(wire::Provenance::Estimated);
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
        .flat_map(|p| &p.aggregates)
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
        scope.not_retained_prefix = scope.requested.clone();
        scope.retained = Default::default();
        Reply::Observations(wire::QueryObservationsResponse {
            scope: scope.into(),
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
    // v2 rejects a stale cursor through canonical RPC failure, never a page variant.
    let info = home_wire_contract::google::rpc::ErrorInfo {
        reason: "RESYNC_REQUIRED".into(),
        domain: home_wire_contract::api_errors::DOMAIN.into(),
        ..Default::default()
    };
    let failure = Failure::from(
        connectrpc::ConnectError::new(connectrpc::ErrorCode::FailedPrecondition, "stale capture")
            .with_detail(connectrpc::ErrorDetail::from_message(
                home_wire_contract::google::rpc::ErrorInfo::FULL_NAME,
                &info,
            )),
    );
    assert!(matches!(transport_error(failure), ReadError::StaleCursor));
    assert_eq!(capture.points, 1);
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
