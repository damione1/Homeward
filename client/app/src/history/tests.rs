use super::*;
use buffa_types::google::protobuf::Timestamp as WireTime;
use home_client_cache::history::{Query, Resolution};

fn time(nanos: i32) -> WireTime {
    WireTime {
        seconds: 1_792_540_900,
        nanos,
        ..Default::default()
    }
}
fn observation(
    sequence: u64,
    nanos: i32,
    unit: &str,
    decimal: Option<&str>,
    value: f64,
) -> wire::Observation {
    wire::Observation {
        position: home_wire_contract::home::observability::v1::AdmissionPosition {
            epoch: vec![1; 16],
            sequence,
            ..Default::default()
        }
        .into(),
        descriptor: Some(wire::NumericDescriptor {
            unit: unit.into(),
            canonical_unit: unit.into(),
            scale: 1.,
            ..Default::default()
        })
        .into(),
        state: state::ObservedState {
            received_at: time(nanos).into(),
            quality: state::Quality {
                availability: state::Availability::Available.into(),
                freshness: state::Freshness::Stale.into(),
                provenance: state::Provenance::Restored.into(),
                ..Default::default()
            }
            .into(),
            value: Some(state::observed_state::Value::Measurement(Box::new(
                state::Measurement {
                    unit: unit.into(),
                    decimal_value: decimal.map(str::to_owned),
                    value,
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn snapshot(content: Content) -> Arc<Snapshot> {
    let resolution = match &content {
        Content::Retained(Pages::Hourly(_)) => Resolution::Hourly,
        Content::NotRetained(_) | Content::Retained(Pages::Observations(_)) => {
            Resolution::Observations
        }
    };
    Arc::new(Snapshot {
        query: Query::new(
            "synthetic-history".parse().unwrap(),
            wire::Period {
                start: WireTime {
                    seconds: 1_792_540_800,
                    ..Default::default()
                }
                .into(),
                end: WireTime {
                    seconds: 1_792_544_400,
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            },
            resolution,
        )
        .unwrap(),
        content,
        encoded_bytes: 0,
        points: 0,
    })
}

#[test]
fn display_sort_preserves_logical_positions_nanoseconds_and_coincident_source_indices() {
    let points = vec![
        observation(1, 999, "kWh", None, 0.),
        observation(2, 1, "kWh", None, 1.),
        observation(3, 1, "kWh", None, 2.),
    ];
    let original: Vec<_> = points.iter().map(Message::encode_to_vec).collect();
    let source = snapshot(Content::Retained(Pages::Observations(vec![
        wire::ObservationPage {
            points,
            ..Default::default()
        },
    ])));
    let data = Data::from_snapshot(source.clone(), Statistic::Observation).unwrap();
    assert_eq!(data.groups.len(), 1);
    let group = &data.groups[0];
    assert_eq!(group.series.samples().len(), 3);
    assert_eq!(
        group.source(0),
        Some(SourceRef::Observation { page: 0, point: 1 })
    );
    assert_eq!(
        group.source(1),
        Some(SourceRef::Observation { page: 0, point: 2 })
    );
    assert_eq!(
        group.source(2),
        Some(SourceRef::Observation { page: 0, point: 0 })
    );
    assert_eq!(
        group.series.samples()[2].time().unix_nanos()
            - group.series.samples()[0].time().unix_nanos(),
        998
    );
    assert_eq!(group.value(2), Some(&Value::Number(0.)));
    let Content::Retained(Pages::Observations(pages)) = &source.content else {
        unreachable!()
    };
    assert_eq!(
        pages[0]
            .points
            .iter()
            .map(Message::encode_to_vec)
            .collect::<Vec<_>>(),
        original
    );
}
#[test]
fn decimal_text_and_currency_survive_pixel_projection_beyond_editor_precision() {
    let exact = "12345678901234567890.000000000000000001";
    assert!(slint_kit::DecimalValue::parse_canonical(exact).is_err());
    let source = snapshot(Content::Retained(Pages::Observations(vec![
        wire::ObservationPage {
            points: vec![observation(1, 1, "CAD", Some(exact), 0.)],
            ..Default::default()
        },
    ])));
    let data = Data::from_snapshot(source, Statistic::Observation).unwrap();
    assert_eq!(data.groups[0].value(0).unwrap().text(), exact);
    assert_eq!(data.groups[0].descriptor.unit, "CAD");
    assert!(data.groups[0].series.samples()[0].value() > 1e19);
    assert_eq!(
        data.groups[0].quality.provenance,
        state::Provenance::Restored
    );
    for malformed in ["NaN", "inf", "1e3", "1.2.3", "", "-", " 1", "1\n", "1."] {
        assert!(
            Value::Decimal(Arc::from(malformed)).coordinate().is_err(),
            "{malformed:?}"
        );
    }
}
#[test]
fn source_units_and_quality_are_separate_groups_and_absence_never_becomes_zero() {
    let mut unavailable = observation(4, 4, "kWh", None, 0.);
    unavailable.state.get_or_insert_default().value = None;
    unavailable
        .state
        .get_or_insert_default()
        .quality
        .get_or_insert_default()
        .availability = state::Availability::Unavailable.into();
    let mut fresh = observation(3, 3, "kWh", None, 0.);
    fresh
        .state
        .get_or_insert_default()
        .quality
        .get_or_insert_default()
        .freshness = state::Freshness::Fresh.into();
    let data = Data::from_snapshot(
        snapshot(Content::Retained(Pages::Observations(vec![
            wire::ObservationPage {
                points: vec![
                    observation(1, 1, "kWh", None, 0.),
                    observation(2, 2, "Wh", None, 0.),
                    fresh,
                    unavailable,
                ],
                ..Default::default()
            },
        ]))),
        Statistic::Observation,
    )
    .unwrap();
    assert_eq!(data.groups.len(), 3);
    assert_eq!(data.non_numeric_or_absent, 1);
    assert_eq!(
        data.groups
            .iter()
            .map(|g| g.series.samples().len())
            .sum::<usize>(),
        3
    );
}
#[test]
fn unknown_utc_logical_evidence_never_becomes_a_chart_time_gap() {
    let coverage = wire::Coverage {
        verdict: wire::CoverageVerdict::Incomplete.into(),
        logical: home_wire_contract::home::observability::v1::RecorderCoverageSnapshot {
            gaps: vec![Default::default()],
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let data = Data::from_snapshot(
        snapshot(Content::Retained(Pages::Observations(vec![
            wire::ObservationPage {
                points: vec![observation(1, 1, "kWh", None, 0.)],
                coverage: coverage.into(),
                ..Default::default()
            },
        ]))),
        Statistic::Observation,
    )
    .unwrap();
    assert!(data.incomplete_coverage && data.unknown_utc);
    assert!(data.groups[0].series.gaps().is_empty());
}
#[test]
fn server_statistic_presence_and_numeric_zero_remain_distinct() {
    let row = wire::Aggregate {
        descriptor: wire::NumericDescriptor {
            unit: "kWh".into(),
            ..Default::default()
        }
        .into(),
        bucket: wire::Period {
            start: time(1).into(),
            end: time(2).into(),
            ..Default::default()
        }
        .into(),
        minimum: Some(0.),
        mean: None,
        contribution: Some(12.375),
        ..Default::default()
    };
    let source = snapshot(Content::Retained(Pages::Hourly(vec![
        wire::AggregatePage {
            points: vec![row],
            ..Default::default()
        },
    ])));
    let mean = Data::from_snapshot(source.clone(), Statistic::Mean).unwrap();
    assert!(mean.groups.is_empty());
    assert_eq!(mean.non_numeric_or_absent, 1);
    let contribution = Data::from_snapshot(source.clone(), Statistic::Contribution).unwrap();
    assert_eq!(
        contribution.groups[0].value(0),
        Some(&Value::Number(12.375))
    );
    let minimum = Data::from_snapshot(source, Statistic::Minimum).unwrap();
    assert_eq!(minimum.groups[0].value(0), Some(&Value::Number(0.)));
}
