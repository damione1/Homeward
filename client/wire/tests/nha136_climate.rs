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
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::history_contract::admit_observations_response;
use home_wire_contract::home::api::v2 as history;

// Same retained scope and limits as the current NHA-164 public fixture. The
// historical value is independent of the entity's current capabilities.
fn climate_history_response(
    climate: history::HistoricalTemperatureControl,
) -> history::QueryObservationsResponse {
    let at = Timestamp {
        seconds: 1_792_540_800,
        ..Default::default()
    };
    let period = history::HistoryPeriod {
        start: at.clone().into(),
        end: Timestamp {
            seconds: at.seconds + 3600,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    history::QueryObservationsResponse {
        scope: history::HistoryReadScope {
            requested: period.clone().into(),
            retained: period.into(),
            retention_cutoff: at.clone().into(),
            as_of: Timestamp {
                seconds: at.seconds + 3700,
                ..Default::default()
            }
            .into(),
            resolution: history::HistoryResolution::Raw.into(),
            limits: history::HistoryLimits {
                max_points: 100,
                max_response_bytes: 256 * 1024,
                max_token_bytes: 512,
                query_timeout_millis: 2000,
                max_readers: 2,
                max_period_hours: 168,
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }
        .into(),
        coverage: history::HistoryCoverage {
            verdict: history::HistoryCoverageVerdict::NotProven.into(),
            ..Default::default()
        }
        .into(),
        observations: vec![history::HistoryObservation {
            observation_id: "a".repeat(64),
            state: history::HistoricalState {
                quality: history::ObservationQuality {
                    availability: history::Availability::Available.into(),
                    freshness: history::Freshness::Current.into(),
                    provenance: history::Provenance::Observed.into(),
                    ..Default::default()
                }
                .into(),
                receive_time: at.clone().into(),
                last_change_time: at.clone().into(),
                last_update_time: at.clone().into(),
                last_report_time: at.into(),
                value: Some(history::historical_state::Value::TemperatureControl(
                    climate.into(),
                )),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }],
        ..Default::default()
    }
}

#[test]
fn nha136_history_public_admission_preserves_only_new_fields_zero_and_absence() {
    let values = [
        history::HistoricalTemperatureControl {
            humidity_percent: Some(0.0),
            ..Default::default()
        },
        history::HistoricalTemperatureControl {
            humidity_percent: Some(100.0),
            ..Default::default()
        },
        history::HistoricalTemperatureControl {
            fan_target: Some(history::TemperatureFanTarget::On.into()),
            ..Default::default()
        },
        history::HistoricalTemperatureControl {
            fan_target: Some(history::TemperatureFanTarget::Auto.into()),
            ..Default::default()
        },
        history::HistoricalTemperatureControl {
            current_temperature_celsius: Some(0.0),
            ..Default::default()
        },
        history::HistoricalTemperatureControl {
            humidity_percent: Some(0.0),
            fan_target: Some(history::TemperatureFanTarget::On.into()),
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
        let Some(history::historical_state::Value::TemperatureControl(actual)) =
            &admitted.wire().observations[0].state.value
        else {
            panic!("temperature control state lost");
        };
        assert_eq!(actual.humidity_percent, climate.humidity_percent);
        assert_eq!(actual.fan_target, climate.fan_target);
        assert_eq!(
            actual.current_temperature_celsius,
            climate.current_temperature_celsius
        );
    }
    assert!(
        admit_observations_response(&climate_history_response(
            history::HistoricalTemperatureControl::default()
        ))
        .is_err(),
        "empty climate admitted"
    );
}

#[test]
fn nha136_history_public_admission_rejects_bad_new_fields_even_with_old_presence() {
    for old_temperature in [None, Some(21.5)] {
        for humidity in [f64::NAN, f64::INFINITY, f64::NEG_INFINITY, -0.1, 100.1] {
            let response = climate_history_response(history::HistoricalTemperatureControl {
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
            history::TemperatureFanTarget::Unspecified.into(),
            EnumValue::Unknown(999),
            EnumValue::Unknown(-1),
        ] {
            let response = climate_history_response(history::HistoricalTemperatureControl {
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
