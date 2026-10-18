use buffa::Message;
use home_wire_contract::home::{registry::v1 as registry, state::v1 as state};

fn fixture(name: &str) -> state::ObservedState {
    let bytes = std::fs::read(format!("../../contracts/climate_golden/{name}.bin")).unwrap();
    let observed = state::ObservedState::decode_from_slice(&bytes).unwrap();
    let decoded = state::ObservedState::decode_from_slice(&observed.encode_to_vec()).unwrap();
    assert_eq!(decoded, observed);
    observed
}

#[test]
fn nha174_climate_range_preserves_zero_false_and_physical_activity() {
    let observed = fixture("climate-range");
    assert_eq!(observed.source_seq, Some(0));
    assert_eq!(observed.observed_at.nanos, 123);
    let Some(state::observed_state::Value::Climate(climate)) = observed.value else {
        panic!("climate state missing");
    };
    assert_eq!(climate.current_temperature_celsius, Some(0.0));
    assert_eq!(climate.mode, Some(registry::ClimateMode::Auto.into()));
    assert_eq!(
        climate.activity,
        Some(registry::ClimateActivity::Heating.into())
    );
    assert_eq!(climate.fan_active, Some(false));
    let Some(state::climate_value::Setpoint::SetpointRangeCelsius(range)) = climate.setpoint else {
        panic!("range missing");
    };
    assert_eq!(range.low_celsius, Some(18.0));
    assert_eq!(range.high_celsius, Some(23.0));
}

#[test]
fn nha174_off_does_not_invent_temperature_activity_or_fan() {
    let observed = fixture("climate-off");
    assert_eq!(observed.quality.provenance, state::Provenance::Restored);
    let Some(state::observed_state::Value::Climate(climate)) = observed.value else {
        panic!("climate state missing");
    };
    assert_eq!(climate.current_temperature_celsius, None);
    assert_eq!(climate.mode, Some(registry::ClimateMode::Off.into()));
    assert_eq!(climate.activity, None);
    assert_eq!(climate.fan_active, None);
    assert!(matches!(
        climate.setpoint,
        Some(state::climate_value::Setpoint::SetpointCelsius(0.0))
    ));
    let declaration = registry::ClimateTemperatureObservation::default();
    assert!(declaration.bounds.as_option().is_none());
    let constraints = registry::ClimateConstraints {
        observed_temperature_celsius: declaration.into(),
        ..Default::default()
    };
    let decoded =
        registry::ClimateConstraints::decode_from_slice(&constraints.encode_to_vec()).unwrap();
    assert!(decoded.observed_temperature_celsius.as_option().is_some());
    assert!(decoded.setpoint_celsius.as_option().is_none());
    assert!(decoded.mode_options.is_empty());
}
