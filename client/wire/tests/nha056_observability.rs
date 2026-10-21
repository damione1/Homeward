use buffa::{EnumValue, Message};
use home_wire_contract::home::observability::v1 as obs;
use home_wire_contract::observability_contract::{
    admit_counter, admit_query_audits, admit_query_runtime_logs, admit_runtime_logs_response,
    admit_trace_response,
};

#[test]
fn nha056_presence_limits_and_unknown_source() {
    assert!(admit_query_runtime_logs(&obs::QueryRuntimeLogsRequest::default()).is_ok());
    for size in [0, 101] {
        assert!(
            admit_query_runtime_logs(&obs::QueryRuntimeLogsRequest {
                page_size: Some(size),
                ..Default::default()
            })
            .is_err()
        );
    }
    assert!(
        admit_query_runtime_logs(&obs::QueryRuntimeLogsRequest {
            page_token: Some("a".repeat(513)),
            ..Default::default()
        })
        .is_err()
    );
    assert!(admit_query_audits(&obs::QueryAuditsRequest::default()).is_err());
    assert!(
        admit_query_audits(&obs::QueryAuditsRequest {
            source: buffa::EnumValue::Unknown(999),
            ..Default::default()
        })
        .is_err()
    );
    assert!(
        admit_query_audits(&obs::QueryAuditsRequest {
            source: obs::AuditSource::Authentication.into(),
            page_size: Some(100),
            ..Default::default()
        })
        .is_ok()
    );
}

// These response oracles were added after the first response admission
// implementation. Their execution and any RED/GREEN evidence belong to root.
fn runtime_logs() -> obs::QueryRuntimeLogsResponse {
    let mut event = obs::RuntimeEvent {
        code: obs::LogCode::ProcessReady.into(),
        level: obs::LogLevel::Info.into(),
        component: obs::LogComponent::Process.into(),
        ..Default::default()
    };
    event.occurred_at.get_or_insert_default().seconds = 1_792_314_000;
    event.author.get_or_insert_default().result =
        Some(obs::runtime_author::Result::Process(Box::default()));
    let mut row = obs::RuntimeLogRow {
        sequence: 1,
        ..Default::default()
    };
    *row.event.get_or_insert_default() = event;
    let mut page = obs::RuntimeLogPage {
        upper_sequence: 1,
        rows: vec![row],
        ..Default::default()
    };
    page.process_epoch.get_or_insert_default().value = vec![1; 16];
    page.continuation.get_or_insert_default().result =
        Some(obs::page_continuation::Result::End(Box::default()));
    obs::QueryRuntimeLogsResponse {
        result: Some(obs::query_runtime_logs_response::Result::Page(Box::new(
            page,
        ))),
        ..Default::default()
    }
}

fn log_page(value: &mut obs::QueryRuntimeLogsResponse) -> &mut obs::RuntimeLogPage {
    let Some(obs::query_runtime_logs_response::Result::Page(page)) = &mut value.result else {
        panic!("fixture must contain its runtime page");
    };
    page
}

#[test]
fn nha056_response_admission_preserves_a_valid_snapshot() {
    let mut original = runtime_logs();
    let admitted = admit_runtime_logs_response(&original).expect("valid bounded page");
    let decoded =
        obs::QueryRuntimeLogsResponse::decode_from_slice(&admitted.wire().encode_to_vec())
            .expect("response codec");
    assert!(admit_runtime_logs_response(&decoded).is_ok());
    log_page(&mut original).rows[0]
        .event
        .get_or_insert_default()
        .code = obs::LogCode::MqttInvalidDiscovery.into();
    assert!(admit_runtime_logs_response(&original).is_err());
    assert_eq!(admitted.wire(), &decoded);
}

#[test]
fn nha056_responses_require_nested_presence_and_oneofs() {
    let mut missing_time = runtime_logs();
    log_page(&mut missing_time).rows[0]
        .event
        .get_or_insert_default()
        .occurred_at = Default::default();
    assert!(admit_runtime_logs_response(&missing_time).is_err());

    let mut missing_author_kind = runtime_logs();
    log_page(&mut missing_author_kind).rows[0]
        .event
        .get_or_insert_default()
        .author
        .get_or_insert_default()
        .result = None;
    assert!(admit_runtime_logs_response(&missing_author_kind).is_err());
}

#[test]
fn nha056_codec_preserves_future_nested_values_before_admission_rejects_them() {
    let mut future_enum = runtime_logs();
    log_page(&mut future_enum).rows[0]
        .event
        .get_or_insert_default()
        .level = EnumValue::Unknown(999);
    let mut decoded =
        obs::QueryRuntimeLogsResponse::decode_from_slice(&future_enum.encode_to_vec())
            .expect("codec preserves future enum");
    assert_eq!(
        log_page(&mut decoded).rows[0].event.level,
        EnumValue::Unknown(999)
    );
    assert!(admit_runtime_logs_response(&decoded).is_err());

    let mut future_field = runtime_logs();
    let author = log_page(&mut future_field).rows[0]
        .event
        .get_or_insert_default()
        .author
        .get_or_insert_default();
    let mut encoded_author = author.encode_to_vec();
    // Field 100, wire type varint, value 1: unknown to RuntimeAuthor.
    encoded_author.extend_from_slice(&[0xa0, 0x06, 0x01]);
    *author = obs::RuntimeAuthor::decode_from_slice(&encoded_author)
        .expect("codec preserves future author field");
    let mut decoded =
        obs::QueryRuntimeLogsResponse::decode_from_slice(&future_field.encode_to_vec())
            .expect("outer codec retains nested unknown field");
    assert!(
        !log_page(&mut decoded).rows[0]
            .event
            .author
            .__buffa_unknown_fields
            .is_empty()
    );
    assert!(admit_runtime_logs_response(&decoded).is_err());
}

#[test]
fn nha056_exact_maximum_is_distinct_from_overflow_saturation() {
    let exact = obs::CounterMeasurement {
        result: Some(obs::counter_measurement::Result::Exact(u64::MAX)),
        ..Default::default()
    };
    let mut saturated = obs::CounterMeasurement {
        result: Some(obs::counter_measurement::Result::Saturated(Box::new(
            obs::SaturatedCounter {
                value: u64::MAX,
                ..Default::default()
            },
        ))),
        ..Default::default()
    };
    assert!(admit_counter(&exact).is_ok());
    assert!(admit_counter(&saturated).is_ok());
    assert_ne!(exact.encode_to_vec(), saturated.encode_to_vec());
    let Some(obs::counter_measurement::Result::Saturated(value)) = &mut saturated.result else {
        panic!("fixture must contain saturation");
    };
    value.value = u64::MAX - 1;
    assert!(admit_counter(&saturated).is_err());
    assert!(admit_counter(&obs::CounterMeasurement::default()).is_err());
}

fn trace() -> obs::ReadRunTraceResponse {
    let mut row = obs::TraceRow {
        run_id: 7,
        local_index: 0,
        ..Default::default()
    };
    row.ordinal.get_or_insert_default().result = Some(obs::trace_ordinal::Result::Native(1));
    row.record = Some(obs::trace_row::Record::Controller(Box::new(
        obs::ControllerTraceRecord {
            decision: obs::TraceDecision::Finished.into(),
            max_exceeded: Some(obs::controller_trace_record::MaxExceeded::NotApplicable(
                Box::default(),
            )),
            ..Default::default()
        },
    )));
    let mut page = obs::TracePage {
        run_id: 7,
        rows: vec![row],
        ..Default::default()
    };
    page.controller.get_or_insert_default().value = vec![1; 32];
    let cuts = page.cuts.get_or_insert_default();
    cuts.coherence = obs::TraceCutCoherence::SeparateOwnerCuts.into();
    cuts.order = obs::TraceOrderState::Healthy.into();
    cuts.controller_captured_at.get_or_insert_default().seconds = 1_792_314_000;
    cuts.node_captured_at.get_or_insert_default().seconds = 1_792_314_001;
    page.continuation.get_or_insert_default().result =
        Some(obs::page_continuation::Result::End(Box::default()));
    obs::ReadRunTraceResponse {
        result: Some(obs::read_run_trace_response::Result::Page(Box::new(page))),
        ..Default::default()
    }
}

fn trace_page(value: &mut obs::ReadRunTraceResponse) -> &mut obs::TracePage {
    let Some(obs::read_run_trace_response::Result::Page(page)) = &mut value.result else {
        panic!("fixture must contain its trace page");
    };
    page
}

#[test]
fn nha056_trace_rows_must_match_the_run_and_native_order_cut() {
    let valid = trace();
    assert!(admit_trace_response(&valid).is_ok());
    let mut other_run = valid.clone();
    trace_page(&mut other_run).rows[0].run_id = 8;
    assert!(admit_trace_response(&other_run).is_err());

    let mut exhausted_with_healthy_cut = valid;
    trace_page(&mut exhausted_with_healthy_cut).rows[0]
        .ordinal
        .get_or_insert_default()
        .result = Some(obs::trace_ordinal::Result::Exhausted(Box::default()));
    assert!(admit_trace_response(&exhausted_with_healthy_cut).is_err());
    trace_page(&mut exhausted_with_healthy_cut)
        .cuts
        .get_or_insert_default()
        .order = obs::TraceOrderState::Exhausted.into();
    assert!(admit_trace_response(&exhausted_with_healthy_cut).is_ok());
}
