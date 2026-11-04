use buffa::Message;
use home_wire_contract::home::api::v2 as obs;
use home_wire_contract::observability_contract::admit_trace_response;

fn trace() -> obs::QueryRunTraceResponse {
    let mut row = obs::ObservationTraceRow {
        run: "homes/test/runtimeControllers/c-aeaqcaibaeaqcaibaeaqcaibaeaqcaibaeaqcaibaeaqcaibaeaq/runs/r-7".into(),
        ..Default::default()
    };
    row.ordinal.get_or_insert_default().result =
        Some(obs::observation_trace_ordinal::Result::Native("1".into()));
    row.record = Some(obs::observation_trace_row::Record::Controller(Box::new(
        obs::ObservationControllerTraceRecord {
            decision: obs::ObservationTraceDecision::Admitted.into(),
            max_exceeded: Some(
                obs::observation_controller_trace_record::MaxExceeded::NotApplicable(Box::default()),
            ),
            ..Default::default()
        },
    )));
    let mut page = obs::QueryRunTraceResponse {
        run: "homes/test/runtimeControllers/c-aeaqcaibaeaqcaibaeaqcaibaeaqcaibaeaqcaibaeaqcaibaeaq/runs/r-7".into(),
        rows: vec![row],
        ..Default::default()
    };
    let cuts = page.cuts.get_or_insert_default();
    cuts.controller_evicted = "0".into();
    cuts.node_evicted = "0".into();
    cuts.coherence = obs::ObservationTraceCutCoherence::SeparateOwnerCuts.into();
    cuts.order = obs::ObservationTraceOrderState::Healthy.into();
    cuts.controller_captured_at.get_or_insert_default().seconds = 1_792_314_000;
    cuts.node_captured_at.get_or_insert_default().seconds = 1_792_314_001;
    page
}

fn trace_record(
    value: &mut obs::QueryRunTraceResponse,
) -> &mut obs::ObservationControllerTraceRecord {
    let page = value;
    let Some(obs::observation_trace_row::Record::Controller(record)) = &mut page.rows[0].record
    else {
        panic!("fixture controller record required");
    };
    record
}

#[test]
fn nha114_trace_source_position_absence_boundary_epochs_and_copy() {
    assert!(admit_trace_response(&trace()).is_ok());
    for decision in [
        obs::ObservationTraceDecision::Admitted,
        obs::ObservationTraceDecision::Queued,
        obs::ObservationTraceDecision::RestartPending,
    ] {
        for epoch in [1, 2] {
            for sequence in [0, 42, u64::MAX] {
                let mut value = trace();
                let record = trace_record(&mut value);
                record.decision = decision.into();
                let position = record.trigger_position.get_or_insert_default();
                position.epoch = vec![epoch; 16];
                position.sequence = sequence.to_string();
                let admitted = admit_trace_response(&value).expect("exact source boundary");
                let encoded = admitted.wire().encode_to_vec();
                let decoded = obs::QueryRunTraceResponse::decode_from_slice(&encoded)
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
        obs::ObservationTraceDecision::Finished,
        obs::ObservationTraceDecision::Cancelled,
        obs::ObservationTraceDecision::Preparing,
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
