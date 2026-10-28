use buffa::EnumValue;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{
    home::{
        auth::v1 as auth, configuration::v1 as cfg, configurationplan::v1 as plan,
        logic::v1 as wire,
    },
    logic_contract::{Request, Response, admit_request, admit_response},
};

fn context() -> wire::Context {
    wire::Context {
        deployment: wire::Deployment::SyntheticOnly.into(),
        epoch: "isolated-epoch".into(),
        catalog_revision: 1,
        generation: cfg::ActiveGeneration {
            revision: 2,
            manifest_sha256: vec![3; 32],
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn reference() -> wire::DefinitionRef {
    wire::DefinitionRef {
        id: "synthetic-definition".into(),
        version: cfg::DocumentVersion {
            revision: 4,
            source_sha256: vec![5; 32],
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn definition() -> wire::Definition {
    wire::Definition {
        reference: reference().into(),
        title: "Synthetic definition".into(),
        family: plan::SourceFamily::Script.into(),
        source_document: cfg::DocumentId {
            value: "synthetic.script".into(),
            ..Default::default()
        }
        .into(),
        protected_source_reference: vec![6; 32],
        schema_version: "synthetic-v1".into(),
        compatibility: wire::Compatibility::Supported.into(),
        mode: wire::RunMode::Single.into(),
        can_run: true,
        can_simulate: true,
        ..Default::default()
    }
}
fn complete() -> wire::Continuation {
    wire::Continuation {
        result: Some(wire::continuation::Result::Complete(Default::default())),
        ..Default::default()
    }
}
fn run() -> wire::Run {
    wire::Run {
        id: "synthetic-run".into(),
        definition: reference().into(),
        generation: context().generation,
        actor: wire::Actor {
            observation: Some(wire::actor::Observation::Account(Box::new(
                auth::AccountId {
                    value: "synthetic-actor".into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
        .into(),
        mode: wire::RunMode::Single.into(),
        status: wire::RunStatus::Running.into(),
        reason: wire::RunReason::ExplicitRequest.into(),
        created_at: Timestamp {
            seconds: 1_800_000_000,
            ..Default::default()
        }
        .into(),
        start_request_id: "explicit-request".into(),
        can_cancel: true,
        simulated: true,
        ..Default::default()
    }
}
fn trace() -> wire::ReadTraceResponse {
    wire::ReadTraceResponse {
        context: context().into(),
        run: run().into(),
        capture_id: "capture-1".into(),
        rows: [1, 2, 3]
            .into_iter()
            .map(|ordinal| wire::TraceRow {
                ordinal,
                at: run().created_at,
                event: Some(wire::trace_row::Event::Lifecycle(
                    wire::RunReason::ExplicitRequest.into(),
                )),
                ..Default::default()
            })
            .collect(),
        continuation: complete().into(),
        ..Default::default()
    }
}

#[test]
fn catalogue_is_a_protected_projection_not_a_reconstructed_source() {
    let mut value = wire::ListDefinitionsResponse {
        context: context().into(),
        definitions: vec![definition()],
        retained_count: 17,
        reported_count: 72,
        continuation: complete().into(),
        ..Default::default()
    };
    assert!(admit_response(Response::Definitions(&value)).is_ok());
    value.definitions[0].protected_source_reference =
        b"scripts:\n  dangerous reconstruction".to_vec();
    assert!(admit_response(Response::Definitions(&value)).is_err());
    value.definitions[0] = definition();
    value.definitions.push(definition());
    assert!(admit_response(Response::Definitions(&value)).is_err());
    value.definitions.truncate(1);
    value.definitions[0].mode = EnumValue::Unknown(987);
    assert!(admit_response(Response::Definitions(&value)).is_err());
    value.definitions[0] = definition();
    value.definitions[0].family = plan::SourceFamily::Operational.into();
    assert!(admit_response(Response::Definitions(&value)).is_err());
}

#[test]
fn missing_actor_never_becomes_the_current_account() {
    let mut value = wire::GetRunResponse {
        context: context().into(),
        run: run().into(),
        ..Default::default()
    };
    assert!(admit_response(Response::Get(&value)).is_ok());
    value.run.get_or_insert_default().actor = Default::default();
    assert!(admit_response(Response::Get(&value)).is_err());
    for observation in [
        wire::actor::Observation::Redacted(Default::default()),
        wire::actor::Observation::NotRecorded(Default::default()),
    ] {
        value.run.get_or_insert_default().actor = wire::Actor {
            observation: Some(observation),
            ..Default::default()
        }
        .into();
        assert!(admit_response(Response::Get(&value)).is_ok());
    }
    value.run.get_or_insert_default().hardware_validated = true;
    assert!(admit_response(Response::Get(&value)).is_err());
    value.run.get_or_insert_default().hardware_validated = false;
    value.context.get_or_insert_default().deployment = EnumValue::Unknown(2);
    assert!(admit_response(Response::Get(&value)).is_err());
}

#[test]
fn sparse_trace_requires_an_explicit_gap_and_never_wraps() {
    let mut value = trace();
    assert!(admit_response(Response::Trace(&value)).is_ok());
    value.rows[2].ordinal = 90;
    assert!(admit_response(Response::Trace(&value)).is_err());
    value.rows[1].event = Some(wire::trace_row::Event::Gap(Box::new(wire::Gap {
        reason: wire::GapReason::Evicted.into(),
        known_missing_count: 88,
        ..Default::default()
    })));
    assert!(admit_response(Response::Trace(&value)).is_ok());
    if let Some(wire::trace_row::Event::Gap(gap)) = &mut value.rows[1].event {
        gap.known_missing_count = u64::MAX;
    }
    assert!(admit_response(Response::Trace(&value)).is_err());
    if let Some(wire::trace_row::Event::Gap(gap)) = &mut value.rows[1].event {
        gap.known_missing_count = 0;
        gap.count_unknown = true;
        gap.reason = wire::GapReason::Redacted.into();
    }
    assert!(admit_response(Response::Trace(&value)).is_ok());
    value.rows[2].ordinal = 1;
    assert!(admit_response(Response::Trace(&value)).is_err());
}

#[test]
fn cancellation_never_claims_to_reverse_dispatched_effects() {
    let mut value = wire::CancelRunResponse {
        context: context().into(),
        run: run().into(),
        cancellation: wire::Cancellation::FutureStepsStopped.into(),
        ..Default::default()
    };
    assert!(admit_response(Response::Cancel(&value)).is_err());
    value.run.get_or_insert_default().status = wire::RunStatus::Canceled.into();
    value.run.get_or_insert_default().can_cancel = false;
    assert!(admit_response(Response::Cancel(&value)).is_ok());
    value.dispatched_effects_reversed = true;
    assert!(admit_response(Response::Cancel(&value)).is_err());
    value.dispatched_effects_reversed = false;
    value.cancellation = wire::Cancellation::AlreadyTerminal.into();
    value.run = run().into();
    assert!(admit_response(Response::Cancel(&value)).is_err());
    value.run.get_or_insert_default().status = wire::RunStatus::Completed.into();
    value.run.get_or_insert_default().can_cancel = false;
    assert!(admit_response(Response::Cancel(&value)).is_ok());
}

#[test]
fn request_selector_and_nonempty_progress_are_required() {
    let mut request = wire::GetRunRequest {
        context: context().into(),
        ..Default::default()
    };
    assert!(admit_request(Request::Get(&request)).is_err());
    request.selector = Some(wire::get_run_request::Selector::StartRequestId(
        "explicit-request".into(),
    ));
    assert!(admit_request(Request::Get(&request)).is_ok());
    let mut response = wire::ListRunsResponse {
        context: context().into(),
        continuation: wire::Continuation {
            result: Some(wire::continuation::Result::NextPageToken("again".into())),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    assert!(admit_response(Response::Runs(&response)).is_err());
    response.continuation = complete().into();
    assert!(admit_response(Response::Runs(&response)).is_ok());
    response.runs = vec![run(); 101];
    assert!(admit_response(Response::Runs(&response)).is_err());
}
