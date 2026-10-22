use buffa::Message;
use home_wire_contract::home::{
    actions::v1::SetClimate, registry::v1::ClimateFanTarget, state::v1::ClimateValue,
};

#[test]
fn nha136_climate_optional_zero_and_target_survive_wire_roundtrip() {
    let value = ClimateValue {
        humidity_percent: Some(0.0),
        fan_target: Some(ClimateFanTarget::On.into()),
        ..Default::default()
    };
    let bytes = value.encode_to_vec();
    let decoded = ClimateValue::decode_from_slice(&bytes).expect("climate decode");
    assert_eq!(decoded.humidity_percent, Some(0.0));
    assert_eq!(decoded.fan_target, Some(ClimateFanTarget::On.into()));
    let command = SetClimate {
        heat_threshold_celsius: Some(20.5),
        cool_threshold_celsius: Some(24.5),
        fan_target: Some(ClimateFanTarget::Auto.into()),
        ..Default::default()
    };
    let bytes = command.encode_to_vec();
    let decoded = SetClimate::decode_from_slice(&bytes).expect("command decode");
    assert_eq!(decoded.heat_threshold_celsius, Some(20.5));
    assert_eq!(decoded.cool_threshold_celsius, Some(24.5));
    assert!(decoded.setpoint_celsius.is_none());
}

use buffa::EnumValue;
use buffa_types::google::protobuf::{Empty, Timestamp};
use home_wire_contract::history_contract::admit_observations_response;
use home_wire_contract::home::{
    history::v1 as history, observability::v1 as obs, registry::v1 as registry, state::v1 as state,
};

// A complete valid NHA-164 response envelope reaches the private climate
// admission through the public history boundary. No test-only API is exposed.
fn climate_history_response(climate: ClimateValue) -> history::QueryObservationsResponse {
    let scope_hex = include_str!("fixtures/nha164_not_retained.hex").trim();
    let scope_bytes: Vec<u8> = (0..scope_hex.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&scope_hex[i..i + 2], 16).unwrap())
        .collect();
    let scope_response =
        history::QueryObservationsResponse::decode_from_slice(&scope_bytes).unwrap();
    let Some(history::query_observations_response::Result::NotRetained(scope)) =
        scope_response.result
    else {
        panic!("NHA-164 scope fixture changed");
    };
    let at = Timestamp {
        seconds: 1_792_540_800,
        ..Default::default()
    };
    let position = obs::AdmissionPosition {
        epoch: vec![136; 16],
        sequence: 1,
        ..Default::default()
    };
    let epoch = obs::RecorderEpochCoverage {
        epoch: obs::RecorderEpochIdentity {
            value: position.epoch.clone(),
            ..Default::default()
        }
        .into(),
        checkpoint: 1,
        started_at: at.clone().into(),
        close_time: Some(obs::recorder_epoch_coverage::CloseTime::NotRecorded(
            Empty::default().into(),
        )),
        reconciliation: obs::RecorderReconciliationState::Complete.into(),
        predecessor: Some(obs::recorder_epoch_coverage::Predecessor::NoPrevious(
            Empty::default().into(),
        )),
        ..Default::default()
    };
    let observed = state::ObservedState {
        entity: registry::EntityRef {
            reference: Some(registry::entity_ref::Reference::EntityId(
                "nha136-history-entity".into(),
            )),
            ..Default::default()
        }
        .into(),
        quality: state::Quality {
            availability: state::Availability::Available.into(),
            freshness: state::Freshness::Fresh.into(),
            provenance: state::Provenance::Observed.into(),
            ..Default::default()
        }
        .into(),
        received_at: at.clone().into(),
        last_changed: at.clone().into(),
        last_updated: at.clone().into(),
        last_reported: at.into(),
        revision: 1,
        source_session: "nha136-synthetic-session".into(),
        value: Some(state::observed_state::Value::Climate(climate.into())),
        ..Default::default()
    };
    let page = history::ObservationPage {
        scope: (*scope).into(),
        points: vec![history::Observation {
            position: position.clone().into(),
            state: observed.into(),
            ..Default::default()
        }],
        coverage: history::Coverage {
            logical: obs::RecorderCoverageSnapshot {
                from: position.clone().into(),
                through: position.into(),
                epochs: vec![epoch],
                ..Default::default()
            }
            .into(),
            verdict: history::CoverageVerdict::CompleteUnderPolicy.into(),
            ..Default::default()
        }
        .into(),
        continuation: history::Continuation {
            result: Some(history::continuation::Result::End(Empty::default().into())),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    history::QueryObservationsResponse {
        result: Some(history::query_observations_response::Result::Page(
            page.into(),
        )),
        ..Default::default()
    }
}

#[test]
fn nha136_history_public_admission_preserves_only_new_fields_zero_and_absence() {
    let values = [
        ClimateValue {
            humidity_percent: Some(0.0),
            ..Default::default()
        },
        ClimateValue {
            humidity_percent: Some(100.0),
            ..Default::default()
        },
        ClimateValue {
            fan_target: Some(ClimateFanTarget::On.into()),
            ..Default::default()
        },
        ClimateValue {
            fan_target: Some(ClimateFanTarget::Auto.into()),
            ..Default::default()
        },
        ClimateValue {
            current_temperature_celsius: Some(0.0),
            ..Default::default()
        },
        ClimateValue {
            humidity_percent: Some(0.0),
            fan_target: Some(ClimateFanTarget::On.into()),
            ..Default::default()
        },
    ];
    for climate in values {
        let response = climate_history_response(climate.clone());
        assert!(
            admit_observations_response(&response).is_ok(),
            "valid NHA-136 history refused"
        );
        let decoded =
            history::QueryObservationsResponse::decode_from_slice(&response.encode_to_vec())
                .unwrap();
        let admitted = admit_observations_response(&decoded).expect("public history admission");
        let Some(history::query_observations_response::Result::Page(page)) =
            &admitted.wire().result
        else {
            panic!("history page lost")
        };
        let Some(state::observed_state::Value::Climate(actual)) = &page.points[0].state.value
        else {
            panic!("climate state lost")
        };
        assert_eq!(actual.humidity_percent, climate.humidity_percent);
        assert_eq!(actual.fan_target, climate.fan_target);
        assert_eq!(
            actual.current_temperature_celsius,
            climate.current_temperature_celsius
        );
    }
    assert!(
        admit_observations_response(&climate_history_response(ClimateValue::default())).is_err(),
        "empty climate admitted"
    );
}

#[test]
fn nha136_history_public_admission_rejects_bad_new_fields_even_with_old_presence() {
    for old_temperature in [None, Some(21.5)] {
        for humidity in [f64::NAN, f64::INFINITY, f64::NEG_INFINITY, -0.1, 100.1] {
            let response = climate_history_response(ClimateValue {
                current_temperature_celsius: old_temperature,
                humidity_percent: Some(humidity),
                ..Default::default()
            });
            assert!(
                admit_observations_response(&response).is_err(),
                "invalid humidity bypassed public history admission"
            );
            let decoded =
                history::QueryObservationsResponse::decode_from_slice(&response.encode_to_vec())
                    .unwrap();
            assert!(
                admit_observations_response(&decoded).is_err(),
                "invalid humidity admitted after wire decode"
            );
        }
        for target in [
            ClimateFanTarget::Unspecified.into(),
            EnumValue::Unknown(999),
            EnumValue::Unknown(-1),
        ] {
            let response = climate_history_response(ClimateValue {
                current_temperature_celsius: old_temperature,
                fan_target: Some(target),
                ..Default::default()
            });
            assert!(
                admit_observations_response(&response).is_err(),
                "invalid fan target bypassed public history admission"
            );
            let decoded =
                history::QueryObservationsResponse::decode_from_slice(&response.encode_to_vec())
                    .unwrap();
            assert!(
                admit_observations_response(&decoded).is_err(),
                "invalid fan target admitted after wire decode"
            );
        }
    }
}
