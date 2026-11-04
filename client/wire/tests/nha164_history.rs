use buffa::Message;
use buffa_types::google::protobuf::Timestamp;
use home_client_model::api::HomeName;
use home_wire_contract::{history_contract::*, home::api::v2 as history};

fn time(seconds: i64) -> Timestamp {
    Timestamp {
        seconds,
        ..Default::default()
    }
}
fn observations() -> history::QueryObservationsRequest {
    history::QueryObservationsRequest {
        parent: "homes/test/entities/history-fixture".into(),
        period: history::HistoryPeriod {
            start: time(1_792_540_800).into(),
            end: time(1_792_544_400).into(),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn scope(
    resolution: history::HistoryResolution,
    traces: bool,
    retained: bool,
) -> history::HistoryReadScope {
    let period = observations().period;
    history::HistoryReadScope {
        requested: period.clone(),
        retained: if retained {
            period.clone()
        } else {
            Default::default()
        },
        not_retained_prefix: if retained { Default::default() } else { period },
        retention_cutoff: time(1_792_540_800).into(),
        as_of: time(1_792_544_500).into(),
        resolution: resolution.into(),
        limits: history::HistoryLimits {
            max_points: if traces { 31 } else { 100 },
            max_response_bytes: 256 * 1024,
            max_token_bytes: 512,
            query_timeout_millis: 2000,
            max_readers: 2,
            max_period_hours: if resolution == history::HistoryResolution::Hour {
                500
            } else {
                168
            },
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
#[test]
fn nha164_history_admission_roundtrip_and_absence() {
    let request = observations();
    assert!(admit_observations_request(&request).is_ok());
    let decoded =
        history::QueryObservationsRequest::decode_from_slice(&request.encode_to_vec()).unwrap();
    assert!(admit_observations_request(&decoded).is_ok());
    assert_eq!(decoded.page_size, 0); // Standard default, no invented presence.
    for size in [-1, i32::MIN] {
        let mut changed = request.clone();
        changed.page_size = size;
        assert!(admit_observations_request(&changed).is_err());
    }
    // Positive requests above the server bound are clamped by the owner.
    let mut changed = request.clone();
    changed.page_size = 101;
    assert!(admit_observations_request(&changed).is_ok());
    changed = request.clone();
    changed.page_token = "x".repeat(513);
    assert!(admit_observations_request(&changed).is_err());
    changed = request.clone();
    changed
        .period
        .get_or_insert_default()
        .end
        .get_or_insert_default()
        .seconds += 168 * 3600;
    assert!(admit_observations_request(&changed).is_err());
    let mut bytes = request.encode_to_vec();
    bytes.extend_from_slice(&[0xa0, 0x06, 0x01]);
    let future = history::QueryObservationsRequest::decode_from_slice(&bytes).unwrap();
    assert!(admit_observations_request(&future).is_err());
    assert!(admit_observations_request(&history::QueryObservationsRequest::default()).is_err());
}
#[test]
fn nha164_history_aggregate_alignment_and_trace_parent() {
    let raw = observations();
    let mut aggregate = history::QueryAggregatesRequest {
        parent: raw.parent,
        period: raw.period.clone(),
        ..Default::default()
    };
    assert!(admit_aggregates_request(&aggregate).is_ok());
    aggregate
        .period
        .get_or_insert_default()
        .start
        .get_or_insert_default()
        .nanos = 1;
    assert!(admit_aggregates_request(&aggregate).is_err());
    let mut trace = history::QueryExecutionTracesRequest {
        parent: "homes/test".into(),
        period: raw.period,
        ..Default::default()
    };
    assert!(admit_traces_request(&trace).is_ok());
    trace.parent = "native-home".into();
    assert!(admit_traces_request(&trace).is_err());
    trace.parent = "homes/test".into();
    trace.page_size = -1;
    assert!(admit_traces_request(&trace).is_err());
}
#[test]
fn nha164_current_response_roundtrip_presence_unknown_and_progress() {
    // The retired v1 hex fixture remains archival data; current responses use
    // direct bounded pages and explicit retained scope instead of a result union.
    let observations = history::QueryObservationsResponse {
        scope: scope(history::HistoryResolution::Raw, false, false).into(),
        ..Default::default()
    };
    let aggregates = history::QueryAggregatesResponse {
        scope: scope(history::HistoryResolution::Hour, false, false).into(),
        ..Default::default()
    };
    let home = HomeName::try_from("homes/test").unwrap();
    let mut traces = history::QueryExecutionTracesResponse {
        scope: scope(history::HistoryResolution::Raw, true, false).into(),
        checkpoint: "0".into(),
        commit_high: "0".into(),
        ..Default::default()
    };
    assert!(admit_observations_response(&observations).is_ok());
    assert!(admit_aggregates_response(&aggregates).is_ok());
    assert!(admit_traces_response(&traces, &home).is_ok());
    let bytes = observations.encode_to_vec();
    let decoded = history::QueryObservationsResponse::decode_from_slice(&bytes).unwrap();
    assert_eq!(decoded.encode_to_vec(), bytes);
    assert!(decoded.scope.retained.as_option().is_none());
    assert!(decoded.scope.not_retained_prefix.as_option().is_some());
    traces.scope = scope(history::HistoryResolution::Raw, true, true).into();
    traces.execution_epoch = "epoch-fixture".into();
    traces.next_page_token = "bounded-physical-progress".into();
    assert!(admit_traces_response(&traces, &home).is_ok());
    traces.next_page_token.clear();
    assert!(admit_traces_response(&traces, &home).is_ok());
    traces.scope = Default::default();
    assert!(admit_traces_response(&traces, &home).is_err());
    let mut unknown = bytes;
    unknown.extend_from_slice(&[0xa0, 0x06, 0x01]);
    let future = history::QueryObservationsResponse::decode_from_slice(&unknown).unwrap();
    assert!(admit_observations_response(&future).is_err());
}
