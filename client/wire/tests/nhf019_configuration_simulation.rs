use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{
    configuration_simulation_contract::{admit_simulation_request, admit_simulation_response},
    home::{configurationplan::v1 as plan, configurationsimulation::v1 as wire},
};
fn request() -> wire::SimulatePlanRequest {
    wire::SimulatePlanRequest {
        candidate: plan::Candidate {
            family: plan::SourceFamily::Script.into(),
            identity: "synthetic.script".into(),
            entry: "entry.yaml".into(),
            files: vec![plan::SourceFile {
                logical_path: "entry.yaml".into(),
                exact_source: b"sequence: []\n".to_vec(),
                ..Default::default()
            }],
            ..Default::default()
        }
        .into(),
        fixtures: vec![wire::Fixture {
            alias: "synthetic.switch".into(),
            state: Some("off".into()),
            supports: vec![wire::Support::SwitchTurnOn.into()],
            ..Default::default()
        }],
        now: Timestamp {
            seconds: 1_700_000_000,
            ..Default::default()
        }
        .into(),
        time_zone: "UTC".into(),
        virtual_limit_millis: 60_000,
        ..Default::default()
    }
}
fn response() -> wire::SimulatePlanResponse {
    wire::SimulatePlanResponse {
        report: wire::Report {
            simulated: true,
            status: wire::Status::Completed.into(),
            limitations: vec!["Synthetic simulation only".into()],
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
#[test]
fn only_logic_sources_and_known_declared_supports_are_admitted() {
    let mut value = request();
    assert!(admit_simulation_request(&value).is_ok());
    value.candidate.as_option_mut().unwrap().family = plan::SourceFamily::Operational.into();
    assert!(admit_simulation_request(&value).is_err());
    value = request();
    value.fixtures[0].supports.push(999.into());
    assert!(admit_simulation_request(&value).is_err());
    value = request();
    value.fixtures[0]
        .supports
        .push(wire::Support::SwitchTurnOn.into());
    assert!(admit_simulation_request(&value).is_err());
}
#[test]
fn absent_state_does_not_fabricate_observed_attributes_and_fixtures_have_a_total_budget() {
    let mut value = request();
    value.fixtures[0].state = None;
    value.fixtures[0].attributes.push(wire::Attribute {
        name: "present".into(),
        value: Some(wire::attribute::Value::Boolean(false)),
        ..Default::default()
    });
    assert!(admit_simulation_request(&value).is_err());
    value = request();
    value.fixtures[0].attributes = (0..20)
        .map(|index| wire::Attribute {
            name: format!("attribute-{index}"),
            value: Some(wire::attribute::Value::Text("x".repeat(4096))),
            ..Default::default()
        })
        .collect();
    assert!(admit_simulation_request(&value).is_err());
    value = request();
    value.virtual_limit_millis = 3_600_001;
    assert!(admit_simulation_request(&value).is_err());
}
#[test]
fn a_simulation_cannot_claim_hardware_validation_or_hide_partial_failure() {
    let mut value = response();
    assert!(admit_simulation_response(&value).is_ok());
    value.report.as_option_mut().unwrap().hardware_validated = true;
    assert!(admit_simulation_response(&value).is_err());
    value = response();
    value.report.as_option_mut().unwrap().status = wire::Status::Partial.into();
    assert!(admit_simulation_response(&value).is_err());
    value
        .report
        .as_option_mut()
        .unwrap()
        .diagnostics
        .push(wire::Diagnostic {
            code: wire::DiagnosticCode::MissingFixture.into(),
            position: wire::Position::default().into(),
            ..Default::default()
        });
    assert!(admit_simulation_response(&value).is_ok());
    value.report.as_option_mut().unwrap().diagnostics[0].code = 999.into();
    assert!(admit_simulation_response(&value).is_err());
}
#[test]
fn recorded_intentions_need_known_parameters_finite_values_and_real_source_positions() {
    let mut value = response();
    let intent = wire::Intent {
        alias: "synthetic.light".into(),
        position: wire::Position {
            file: "entry.yaml".into(),
            path: "sequence.0".into(),
            line: 1,
            column: 1,
            ..Default::default()
        }
        .into(),
        outcome: wire::IntentOutcome::Recorded.into(),
        at: Timestamp {
            seconds: 1_700_000_000,
            ..Default::default()
        }
        .into(),
        parameters: Some(wire::intent::Parameters::Light(Box::new(
            wire::LightIntent {
                brightness_percent: Some(50.),
                ..Default::default()
            },
        ))),
        ..Default::default()
    };
    value.report.as_option_mut().unwrap().intents.push(intent);
    assert!(admit_simulation_response(&value).is_ok());
    let item = &mut value.report.as_option_mut().unwrap().intents[0];
    let Some(wire::intent::Parameters::Light(light)) = &mut item.parameters else {
        panic!("light")
    };
    light.brightness_percent = Some(f64::NAN);
    assert!(admit_simulation_response(&value).is_err());
    value.report.as_option_mut().unwrap().intents[0].parameters = None;
    assert!(admit_simulation_response(&value).is_err());
    value.report.as_option_mut().unwrap().intents[0].outcome = wire::IntentOutcome::Refused.into();
    assert!(admit_simulation_response(&value).is_ok());
    value.report.as_option_mut().unwrap().intents[0]
        .position
        .as_option_mut()
        .unwrap()
        .line = 0;
    assert!(admit_simulation_response(&value).is_err());
}
