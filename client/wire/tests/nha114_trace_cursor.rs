use buffa::Message;
use home_wire_contract::home::observability::v1 as obs;
use home_wire_contract::observability_contract::admit_trace_response;

fn trace() -> obs::ReadRunTraceResponse {
    let mut row = obs::TraceRow {
        run_id: 7,
        ..Default::default()
    };
    row.ordinal.get_or_insert_default().result = Some(obs::trace_ordinal::Result::Native(1));
    row.record = Some(obs::trace_row::Record::Controller(Box::new(
        obs::ControllerTraceRecord {
            decision: obs::TraceDecision::Admitted.into(),
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

fn trace_record(value: &mut obs::ReadRunTraceResponse) -> &mut obs::ControllerTraceRecord {
    let Some(obs::read_run_trace_response::Result::Page(page)) = &mut value.result else {
        panic!("fixture page required");
    };
    let Some(obs::trace_row::Record::Controller(record)) = &mut page.rows[0].record else {
        panic!("fixture controller record required");
    };
    record
}

#[test]
fn nha114_trace_source_position_absence_boundary_epochs_and_copy() {
    assert!(admit_trace_response(&trace()).is_ok());
    for decision in [
        obs::TraceDecision::Admitted,
        obs::TraceDecision::Queued,
        obs::TraceDecision::RestartPending,
    ] {
        for epoch in [1, 2] {
            for sequence in [0, 42, u64::MAX] {
                let mut value = trace();
                let record = trace_record(&mut value);
                record.decision = decision.into();
                let position = record.trigger_position.get_or_insert_default();
                position.epoch = vec![epoch; 16];
                position.sequence = sequence;
                let admitted = admit_trace_response(&value).expect("exact source boundary");
                let encoded = admitted.wire().encode_to_vec();
                let decoded = obs::ReadRunTraceResponse::decode_from_slice(&encoded)
                    .expect("source position codec");
                assert_eq!(decoded, value);
                trace_record(&mut value)
                    .trigger_position
                    .get_or_insert_default()
                    .epoch[0] = 255;
                assert_eq!(admitted.wire(), &decoded);
            }
        }
    }
}

#[test]
fn nha114_trace_source_position_rejects_malformed_noninitial_and_unknown() {
    for epoch in [vec![], vec![0; 16], vec![1; 15], vec![1; 17], vec![1; 513]] {
        let mut value = trace();
        trace_record(&mut value)
            .trigger_position
            .get_or_insert_default()
            .epoch = epoch;
        assert!(admit_trace_response(&value).is_err());
    }
    for decision in [
        obs::TraceDecision::Finished,
        obs::TraceDecision::Cancelled,
        obs::TraceDecision::Preparing,
    ] {
        let mut value = trace();
        let record = trace_record(&mut value);
        record.decision = decision.into();
        record.trigger_position.get_or_insert_default().epoch = vec![1; 16];
        assert!(admit_trace_response(&value).is_err());
    }
    let mut value = trace();
    let position = trace_record(&mut value)
        .trigger_position
        .get_or_insert_default();
    position.epoch = vec![1; 16];
    let mut encoded = position.encode_to_vec();
    encoded.extend_from_slice(&[0x18, 0x01]);
    *position = obs::AdmissionPosition::decode_from_slice(&encoded)
        .expect("codec retains unknown source-position fields");
    assert!(!position.__buffa_unknown_fields.is_empty());
    assert!(admit_trace_response(&value).is_err());
}
