use home_client_model::decimal_u64::DecimalU64;
use home_wire_contract::{home::api::v2 as api, observability_contract::admit_counter};
#[test]
fn canonical_decimal_positions_keep_full_width_and_numeric_order() {
    for (text, expected) in [("0", 0), ("18446744073709551615", u64::MAX)] {
        assert_eq!(DecimalU64::try_from(text).unwrap().value(), expected);
    }
    for text in ["", "00", "01", "-1", "+1", "18446744073709551616"] {
        assert!(DecimalU64::try_from(text).is_err());
    }
    assert!(DecimalU64::try_from("9").unwrap() < DecimalU64::try_from("10").unwrap());
}
#[test]
fn signed_counter_requires_nonnegative_exact_or_explicit_lower_bound() {
    for n in [0, i64::MAX] {
        assert!(
            admit_counter(&api::ObservationCounterMeasurement {
                result: Some(api::observation_counter_measurement::Result::Exact(n)),
                ..Default::default()
            })
            .is_ok()
        );
    }
    assert!(
        admit_counter(&api::ObservationCounterMeasurement {
            result: Some(api::observation_counter_measurement::Result::Exact(-1)),
            ..Default::default()
        })
        .is_err()
    );
    for (n, valid) in [(i64::MAX, true), (-1, false), (0, false)] {
        let value = api::ObservationCounterMeasurement {
            result: Some(api::observation_counter_measurement::Result::Saturated(
                api::ObservationSaturatedCounter {
                    lower_bound: n,
                    ..Default::default()
                }
                .into(),
            )),
            ..Default::default()
        };
        assert_eq!(admit_counter(&value).is_ok(), valid);
    }
}
