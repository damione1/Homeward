use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::Timestamp;
use home_client_cache::{CacheFailure, CachedView, Filter, FrameEffect, LiveView, SnapshotCapture};
use home_client_model::EntityId;
use home_wire_contract::home::{registry::v1 as registry, state::v1 as wire};

fn at(seconds: i64) -> Timestamp {
    Timestamp {
        seconds,
        ..Default::default()
    }
}

fn cursor(filter: &Filter, sequence: u64) -> wire::ViewCursor {
    wire::ViewCursor {
        epoch: "opaque-server-epoch".into(),
        global_sequence: sequence,
        config_revision: 9_007_199_254_740_993,
        authorization_revision: 7,
        filter_sha256: filter.digest().to_vec(),
        config_sha256: vec![1; 32],
        grant_sha256: vec![2; 32],
        admission_proof: vec![3; 40],
        ..Default::default()
    }
}

fn observed(id: &str, revision: u64) -> wire::ObservedState {
    wire::ObservedState {
        entity: registry::EntityRef {
            reference: Some(registry::entity_ref::Reference::EntityId(id.into())),
            ..Default::default()
        }
        .into(),
        quality: wire::Quality {
            availability: wire::Availability::Available.into(),
            freshness: wire::Freshness::Fresh.into(),
            provenance: wire::Provenance::Observed.into(),
            ..Default::default()
        }
        .into(),
        revision,
        value: Some(wire::observed_state::Value::SwitchOn(false)),
        source_seq: Some(0),
        ..Default::default()
    }
}

fn entity(id: &str) -> wire::ViewEntity {
    wire::ViewEntity {
        entity_id: id.into(),
        device_id: "opaque-device".into(),
        current_alias: format!("alias.{id}"),
        state: observed(id, 1).into(),
        ..Default::default()
    }
}

fn page(filter: &Filter, ids: &[&str], next: &str) -> wire::OpenViewResponse {
    wire::OpenViewResponse {
        snapshot_token: "opaque-capture-token".into(),
        cursor: cursor(filter, 100).into(),
        expires_at: at(60).into(),
        entities: ids.iter().map(|id| entity(id)).collect(),
        next_page_token: next.into(),
        ..Default::default()
    }
}

fn complete() -> CachedView {
    let filter = Filter::all();
    SnapshotCapture::begin(
        filter.clone(),
        2,
        page(&filter, &["opaque-a", "opaque-b"], ""),
        &at(0),
    )
    .unwrap()
    .finish(&at(1))
    .unwrap()
}

fn delta(
    view: &CachedView,
    stream: u64,
    global: u64,
    states: Vec<wire::ObservedState>,
) -> wire::WatchViewResponse {
    let mut cursor = view.cursor().clone();
    cursor.global_sequence = global;
    // The admission proof belongs to the new global position, not the generation.
    cursor.admission_proof = vec![4; 40];
    wire::WatchViewResponse {
        stream_sequence: stream,
        cursor: cursor.into(),
        frame: Some(wire::watch_view_response::Frame::Delta(Box::new(
            wire::StateDelta {
                states,
                ..Default::default()
            },
        ))),
        ..Default::default()
    }
}

#[test]
fn last_page_expiry_refusal_and_mixed_generation_never_publish_a_partial_capture() {
    let filter = Filter::all();
    let start = || {
        SnapshotCapture::begin(filter.clone(), 1, page(&filter, &["a"], "page-two"), &at(0))
            .unwrap()
    };
    assert!(matches!(
        start().finish(&at(1)),
        Err(CacheFailure::Incomplete)
    ));
    let mut expired = start();
    assert_eq!(
        expired.push(page(&filter, &["b"], ""), &at(60)),
        Err(CacheFailure::Expired)
    );
    assert!(expired.next_request().is_none());
    assert!(matches!(
        expired.finish(&at(1)),
        Err(CacheFailure::Abandoned)
    ));
    let mut refused = start();
    refused.abandon(); // A final ReadPage RPC refusal follows this path.
    assert!(matches!(
        refused.finish(&at(1)),
        Err(CacheFailure::Abandoned)
    ));
    let mut mixed = start();
    let mut second = page(&filter, &["b"], "");
    second
        .cursor
        .as_option_mut()
        .unwrap()
        .authorization_revision += 1;
    assert_eq!(
        mixed.push(second, &at(1)),
        Err(CacheFailure::GenerationChanged)
    );
    assert!(matches!(mixed.finish(&at(1)), Err(CacheFailure::Abandoned)));
}

#[test]
fn opaque_identity_joins_entities_and_never_zips_legacy_parallel_arrays() {
    let filter = Filter::all();
    let mut first = page(&filter, &["opaque/#a\nβ"], "next");
    first.states = vec![observed("wrong-legacy-row", 500)];
    first.capabilities = vec![registry::CapabilityDescription {
        display_name: "unpaired legacy descriptor".into(),
        ..Default::default()
    }];
    let mut capture = SnapshotCapture::begin(filter.clone(), 1, first, &at(0)).unwrap();
    let next = capture.next_request().unwrap();
    assert_eq!(next.page_token, "next");
    assert_eq!(next.snapshot_token, "opaque-capture-token");
    capture
        .push(page(&filter, &["opaque-b"], ""), &at(1))
        .unwrap();
    let result = capture.finish(&at(2)).unwrap();
    assert_eq!(result.len(), 2);
    let id: EntityId = "opaque/#a\nβ".parse().unwrap();
    assert_eq!(result.get(&id).unwrap().state.revision, 1);
    assert_eq!(result.get(&id).unwrap().state.source_seq, Some(0));
    assert!(result.get(&"wrong-legacy-row".parse().unwrap()).is_none());
}

#[test]
fn duplicate_identity_foreign_entity_and_cyclic_pages_abandon_the_capture() {
    let filter = Filter::all();
    let mut duplicate =
        SnapshotCapture::begin(filter.clone(), 1, page(&filter, &["a"], "two"), &at(0)).unwrap();
    assert_eq!(
        duplicate.push(page(&filter, &["a"], ""), &at(1)),
        Err(CacheFailure::DuplicateEntity)
    );
    assert!(matches!(
        duplicate.finish(&at(2)),
        Err(CacheFailure::Abandoned)
    ));
    let mut cyclic =
        SnapshotCapture::begin(filter.clone(), 1, page(&filter, &["a"], "two"), &at(0)).unwrap();
    assert_eq!(
        cyclic.push(page(&filter, &["b"], "two"), &at(1)),
        Err(CacheFailure::OutOfOrder)
    );
    assert!(matches!(
        cyclic.finish(&at(2)),
        Err(CacheFailure::Abandoned)
    ));
    let only = Filter::only(["a".parse().unwrap()]).unwrap();
    assert!(matches!(
        SnapshotCapture::begin(only.clone(), 1, page(&only, &["b"], ""), &at(0)),
        Err(CacheFailure::ForeignEntity)
    ));
    for size in [0, 257, u32::MAX] {
        assert!(matches!(
            SnapshotCapture::begin(filter.clone(), size, page(&filter, &[], ""), &at(0)),
            Err(CacheFailure::InvalidPageSize)
        ));
    }
}

#[test]
fn full_observation_replacement_preserves_false_zero_absence_quality_and_future_data() {
    let original = complete();
    let id = "opaque-a".parse().unwrap();
    let mut live = LiveView::new(original.clone());
    let mut light = observed("opaque-a", 2);
    light.value = Some(wire::observed_state::Value::Light(Box::new(
        wire::LightValue {
            on: Some(false),
            brightness_percent: Some(0.0),
            color_temp_mireds: Some(0),
            ..Default::default()
        },
    )));
    light.quality.as_option_mut().unwrap().freshness = wire::Freshness::Stale.into();
    light.quality.as_option_mut().unwrap().provenance = wire::Provenance::Restored.into();
    live.apply(delta(live.current().unwrap(), 1, 109, vec![light]))
        .unwrap();
    let first = &live.current().unwrap().get(&id).unwrap().state;
    assert_eq!(first.quality.freshness, wire::Freshness::Stale);
    assert_eq!(first.quality.provenance, wire::Provenance::Restored);
    assert_eq!(first.source_seq, Some(0));
    let mut replacement = observed("opaque-a", 3);
    replacement.value = Some(wire::observed_state::Value::Light(Box::new(
        wire::LightValue {
            on: Some(false),
            ..Default::default()
        },
    )));
    replacement.source_seq = None;
    replacement.quality.as_option_mut().unwrap().provenance = wire::Provenance::Estimated.into();
    live.apply(delta(live.current().unwrap(), 2, 113, vec![replacement]))
        .unwrap();
    let state = &live.current().unwrap().get(&id).unwrap().state;
    assert_eq!(state.source_seq, None);
    match state.value.as_ref().unwrap() {
        wire::observed_state::Value::Light(light) => {
            assert_eq!(light.on, Some(false));
            assert_eq!(light.brightness_percent, None);
            assert_eq!(light.color_temp_mireds, None);
        }
        wire::observed_state::Value::SwitchOn(_)
        | wire::observed_state::Value::Measurement(_)
        | wire::observed_state::Value::Fan(_)
        | wire::observed_state::Value::Climate(_)
        | wire::observed_state::Value::Helper(_)
        | wire::observed_state::Value::Solar(_)
        | wire::observed_state::Value::SolarEvent(_) => panic!("expected light state"),
    }
    assert_eq!(state.quality.provenance, wire::Provenance::Estimated);
    assert_eq!(original.get(&id).unwrap().state.revision, 1); // Published clones are immutable.
    let mut future = observed("opaque-a", 4);
    future.value = None;
    future.quality.as_option_mut().unwrap().availability = EnumValue::Unknown(99);
    let mut bytes = future.encode_to_vec();
    bytes.extend_from_slice(&[0x9a, 0x06, 0x02, 0x08, 0x01]);
    let future = wire::ObservedState::decode_from_slice(&bytes).unwrap();
    live.apply(delta(live.current().unwrap(), 3, 190, vec![future]))
        .unwrap();
    let state = &live.current().unwrap().get(&id).unwrap().state;
    assert!(state.value.is_none());
    assert_eq!(state.quality.availability, EnumValue::Unknown(99));
    assert_eq!(state.encode_to_vec(), bytes);
}

#[test]
fn invalid_final_observation_cannot_advance_a_resume_cursor_and_resync_erases_everything() {
    let original = complete();
    let mut live = LiveView::new(original.clone());
    let frame = delta(
        &original,
        1,
        120,
        vec![observed("opaque-a", 2), observed("not-in-the-view", 1)],
    );
    assert_eq!(live.apply(frame), Err(CacheFailure::ForeignEntity));
    assert!(live.current().is_none());
    assert_eq!(original.cursor().global_sequence, 100);
    assert_eq!(
        original
            .get(&"opaque-a".parse().unwrap())
            .unwrap()
            .state
            .revision,
        1
    );
    for reason in [
        wire::ResyncReason::ReplayExpired.into(),
        wire::ResyncReason::AuthorizationChanged.into(),
        wire::ResyncReason::CapabilitiesChanged.into(),
        EnumValue::Unknown(99),
    ] {
        let mut live = LiveView::new(original.clone());
        let frame = wire::WatchViewResponse {
            stream_sequence: 1,
            frame: Some(wire::watch_view_response::Frame::ResyncRequired(Box::new(
                wire::ResyncRequired {
                    reason,
                    ..Default::default()
                },
            ))),
            ..Default::default()
        };
        assert_eq!(live.apply(frame), Ok(FrameEffect::Resync(reason)));
        assert!(live.current().is_none());
        assert!(live.into_current().is_none());
    }
}

#[test]
fn reconnect_resets_only_stream_sequence_and_retains_the_complete_applied_cursor_and_filter() {
    let original = complete();
    let mut live = LiveView::new(original);
    live.apply(delta(
        live.current().unwrap(),
        1,
        150,
        vec![observed("opaque-a", 2)],
    ))
    .unwrap();
    let resumed = live.into_current().unwrap();
    assert_eq!(resumed.cursor().global_sequence, 150);
    assert_eq!(resumed.cursor().admission_proof, vec![4; 40]);
    assert_eq!(resumed.filter().to_wire(), Filter::all().to_wire());
    let mut next = LiveView::new(resumed);
    next.apply(delta(
        next.current().unwrap(),
        1,
        200,
        vec![observed("opaque-a", 3)],
    ))
    .unwrap();
    assert_eq!(next.current().unwrap().cursor().global_sequence, 200);
    assert_eq!(
        next.apply(delta(
            next.current().unwrap(),
            3,
            201,
            vec![observed("opaque-a", 4)]
        )),
        Err(CacheFailure::OutOfOrder)
    );
    assert!(next.current().is_none());
}

#[test]
fn eight_hundred_fifty_entities_capture_atomically_across_four_pages() {
    let ids: Vec<_> = (0..850).map(|i| format!("opaque-{i:04}")).collect();
    let filter = Filter::only(ids.iter().map(|s| s.parse().unwrap())).unwrap();
    let pages: Vec<_> = ids
        .chunks(256)
        .enumerate()
        .map(|(index, chunk)| {
            let names: Vec<_> = chunk.iter().map(String::as_str).collect();
            page(
                &filter,
                &names,
                if index == 3 {
                    ""
                } else {
                    ["page-2", "page-3", "page-4"][index]
                },
            )
        })
        .collect();
    let mut pages = pages.into_iter();
    let mut capture =
        SnapshotCapture::begin(filter.clone(), 256, pages.next().unwrap(), &at(0)).unwrap();
    for page in pages {
        capture.push(page, &at(1)).unwrap();
    }
    let snapshot = capture.finish(&at(2)).unwrap();
    assert_eq!(snapshot.len(), 850);
    assert_eq!(snapshot.filter().to_wire().entity_ids.len(), 850);
    for id in ids {
        assert!(snapshot.get(&id.parse().unwrap()).is_some());
    }
}
