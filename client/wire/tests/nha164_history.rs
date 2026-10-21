use buffa::{EnumValue, Message};
use home_wire_contract::history_contract::{
    admit_aggregates_request, admit_observations_request, admit_traces_request,
};
use home_wire_contract::history_contract::{
    admit_aggregates_response, admit_observations_response, admit_traces_response,
};
use home_wire_contract::home::history::v1 as history;

fn observations() -> history::QueryObservationsRequest {
    let mut request = history::QueryObservationsRequest {
        entity_id: "nha164-entity".into(),
        resolution: history::Resolution::Raw.into(),
        ..Default::default()
    };
    request
        .period
        .get_or_insert_default()
        .start
        .get_or_insert_default()
        .seconds = 1_792_540_800;
    request
        .period
        .get_or_insert_default()
        .end
        .get_or_insert_default()
        .seconds = 1_792_544_400;
    request
}
#[test]
fn nha164_history_admission_roundtrip_and_absence() {
    let request = observations();
    assert!(admit_observations_request(&request).is_ok());
    let bytes = request.encode_to_vec();
    let decoded = history::QueryObservationsRequest::decode_from_slice(&bytes).unwrap();
    assert!(admit_observations_request(&decoded).is_ok());
    assert_eq!(decoded.page_size, None);
    for size in [0, 101] {
        let mut changed = request.clone();
        changed.page_size = Some(size);
        assert!(admit_observations_request(&changed).is_err());
    }
    let mut changed = request.clone();
    changed.resolution = EnumValue::Unknown(999);
    assert!(admit_observations_request(&changed).is_err());
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
    assert!(admit_observations_request(&history::QueryObservationsRequest::default()).is_err());
}
#[test]
fn nha164_history_aggregate_alignment_and_trace_epoch() {
    let raw = observations();
    let mut aggregate = history::QueryAggregatesRequest {
        entity_id: raw.entity_id.clone(),
        period: raw.period.clone(),
        resolution: history::Resolution::Hour.into(),
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
        period: raw.period.clone(),
        ..Default::default()
    };
    assert!(admit_traces_request(&trace).is_ok());
    trace.page_size = Some(32);
    assert!(admit_traces_request(&trace).is_err());
    trace.page_size = Some(31);
    trace.epoch = Some("0".repeat(32));
    assert!(admit_traces_request(&trace).is_err());
    trace.epoch = Some("00000000000000000000000000000164".into());
    assert!(admit_traces_request(&trace).is_ok());
}

#[test]
fn nha164_go_response_wire_roundtrip_presence_unknown_and_progress() {
    // The Go encoder test reproduces exactly this checked-in wire oracle.
    let text = include_str!("fixtures/nha164_not_retained.hex").trim();
    let bytes: Vec<u8> = (0..text.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&text[i..i + 2], 16).unwrap())
        .collect();
    let observations = history::QueryObservationsResponse::decode_from_slice(&bytes).unwrap();
    let aggregate_bytes = [0x22, 0x02, 0x08, 0x01];
    let aggregates = history::QueryAggregatesResponse::decode_from_slice(&aggregate_bytes).unwrap();
    let mut trace_bytes = bytes.clone();
    let offset = trace_bytes
        .windows(2)
        .position(|v| v == [0x08, 0x64])
        .unwrap();
    trace_bytes[offset + 1] = 31;
    let traces = history::QueryExecutionTracesResponse::decode_from_slice(&trace_bytes).unwrap();
    assert!(admit_observations_response(&observations).is_ok());
    assert!(admit_aggregates_response(&aggregates).is_ok());
    assert!(admit_traces_response(&traces).is_ok());
    assert_eq!(observations.encode_to_vec(), bytes);
    assert_eq!(aggregates.encode_to_vec(), aggregate_bytes);
    assert_eq!(traces.encode_to_vec(), trace_bytes);
    use history::query_execution_traces_response::Result;
    let Some(Result::NotRetained(scope)) = traces.result else {
        panic!("response outcome changed")
    };
    assert!(scope.retained.as_option().is_none());
    assert!(scope.not_retained_prefix.as_option().is_none());
    let mut page = history::ExecutionTracePage {
        scope: (*scope).into(),
        epoch: "00000000000000000000000000000164".into(),
        ..Default::default()
    };
    page.scope
        .get_or_insert_default()
        .limits
        .get_or_insert_default()
        .max_points = 31;
    page.continuation.get_or_insert_default().result = Some(
        history::continuation::Result::NextPageToken("bounded-physical-progress".into()),
    );
    let mut progress = history::QueryExecutionTracesResponse {
        result: Some(Result::Page(Box::new(page))),
        ..Default::default()
    };
    assert!(admit_traces_response(&progress).is_ok());
    if let Some(Result::Page(page)) = &mut progress.result {
        page.continuation = Default::default();
    }
    assert!(admit_traces_response(&progress).is_err());
    let mut unknown = bytes.clone();
    unknown.extend_from_slice(&[0xf8, 0x07, 0x01]);
    assert!(
        admit_observations_response(
            &history::QueryObservationsResponse::decode_from_slice(&unknown).unwrap()
        )
        .is_err()
    );
    let mut unknown = observations;
    if let Some(history::query_observations_response::Result::NotRetained(scope)) =
        &mut unknown.result
    {
        scope.resolution = EnumValue::Unknown(999);
    }
    assert!(admit_observations_response(&unknown).is_err());
    assert!(admit_traces_response(&history::QueryExecutionTracesResponse::default()).is_err());
}
