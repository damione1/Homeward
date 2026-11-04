use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::Timestamp;
use home_client_cache::{CacheFailure, CachedView, Filter, FrameEffect, LiveView, SnapshotCapture};
use home_client_model::api::{EntityName, HomeName};
use home_wire_contract::home::api::v2 as wire;
use std::time::{SystemTime, UNIX_EPOCH};

fn now() -> Timestamp {
    Timestamp::from_unix_secs(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64,
    )
}
fn name(id: &str) -> EntityName {
    EntityName::try_from(format!("homes/synthetic/entities/{id}").as_str()).unwrap()
}
fn entry(id: &str, value: Option<f64>) -> wire::EntityViewEntry {
    let entity = name(id);
    wire::EntityViewEntry {
        entity: wire::Entity {
            name: entity.as_str().to_owned(),
            display_name: id.to_owned(),
            etag: "metadata-one".into(),
            ..Default::default()
        }
        .into(),
        capabilities: vec![wire::Capability {
            name: format!("{}/capabilities/level", entity.as_str()),
            etag: "W/\"definition-one\"".into(),
            action_access: EnumValue::Known(wire::ActionAccess::Denied),
            r#trait: Some(wire::capability::Trait::Level(Box::new(
                wire::LevelCapability {
                    observation: wire::LevelObservation {
                        percentage: value,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }],
        ..Default::default()
    }
}
fn capture(filter: Filter, size: u32) -> SnapshotCapture {
    let start = now();
    let mut end = start.clone();
    end.seconds += 60;
    SnapshotCapture::begin(
        HomeName::try_from("homes/synthetic").unwrap(),
        filter.clone(),
        size,
        wire::EntityView {
            name: "homes/synthetic/entityViews/view-one".into(),
            filter: filter.expression().into(),
            create_time: start.clone().into(),
            expire_time: end.into(),
            ..Default::default()
        },
        &start,
    )
    .unwrap()
}
fn page(
    entries: Vec<wire::EntityViewEntry>,
    next: &str,
    resume: &str,
) -> wire::QueryEntityViewResponse {
    wire::QueryEntityViewResponse {
        entries,
        next_page_token: next.into(),
        resume_token: resume.into(),
        ..Default::default()
    }
}
fn completed() -> CachedView {
    let mut value = capture(Filter::all(), 2);
    value
        .push(
            page(vec![entry("a", Some(0.0))], "", "checkpoint-one"),
            &now(),
        )
        .unwrap();
    value.finish(&now()).unwrap()
}

#[test]
fn incomplete_pages_never_become_a_view_and_failure_erases_provisional_data() {
    let mut value = capture(Filter::all(), 1);
    value
        .push(page(vec![entry("a", None)], "page-two", ""), &now())
        .unwrap();
    assert!(matches!(
        value.finish(&now()),
        Err(CacheFailure::Incomplete)
    ));
    let mut value = capture(Filter::all(), 1);
    value
        .push(page(vec![entry("a", None)], "page-two", ""), &now())
        .unwrap();
    assert_eq!(
        value.push(page(vec![entry("a", None)], "", "checkpoint-two"), &now()),
        Err(CacheFailure::OutOfOrder)
    );
    assert!(matches!(value.finish(&now()), Err(CacheFailure::Abandoned)));
}
#[test]
fn invalid_mixed_resume_or_repeated_page_cannot_complete() {
    for bad in [
        page(vec![], "page-two", "checkpoint"),
        page(vec![], "", ""),
        page(vec![entry("a", None)], "bad token", ""),
    ] {
        let mut value = capture(Filter::all(), 1);
        assert!(value.push(bad, &now()).is_err());
    }
    let mut value = capture(Filter::all(), 1);
    value
        .push(page(vec![entry("a", None)], "page-two", ""), &now())
        .unwrap();
    assert!(
        value
            .push(page(vec![entry("b", None)], "page-two", ""), &now())
            .is_err()
    );
}
#[test]
fn foreign_parent_slot_and_selection_are_rejected() {
    let mut foreign = entry("a", None);
    foreign.entity.as_option_mut().unwrap().name = "homes/foreign/entities/a".into();
    let mut slot = entry("a", None);
    slot.capabilities[0].name = "homes/synthetic/entities/b/capabilities/level".into();
    for bad in [foreign, slot] {
        let mut value = capture(Filter::all(), 1);
        assert!(
            value
                .push(page(vec![bad], "", "checkpoint"), &now())
                .is_err()
        );
    }
    let mut value = capture(Filter::only([name("a")]).unwrap(), 1);
    assert_eq!(
        value.push(page(vec![entry("b", None)], "", "checkpoint"), &now()),
        Err(CacheFailure::ForeignEntity)
    );
    assert!(EntityName::try_from("native/#a\nlabel").is_err());
}
#[test]
fn whole_membership_replacements_preserve_absence_zero_and_old_immutable_views() {
    let original = completed();
    let mut live = LiveView::new(original.clone());
    let frame = wire::WatchEntityViewResponse {
        resume_token: "checkpoint-two".into(),
        change: Some(wire::watch_entity_view_response::Change::Upsert(Box::new(
            entry("a", None),
        ))),
        ..Default::default()
    };
    assert_eq!(live.apply(frame).unwrap(), FrameEffect::Applied);
    let reading = |view: &CachedView| {
        let Some(wire::capability::Trait::Level(level)) =
            view.get(&name("a")).unwrap().capabilities[0]
                .r#trait
                .as_ref()
        else {
            panic!("level trait")
        };
        level.observation.as_option().unwrap().percentage
    };
    assert_eq!(reading(&original), Some(0.0));
    assert_eq!(reading(live.current().unwrap()), None);
    assert_eq!(live.current().unwrap().checkpoint(), "checkpoint-two");
    let remove = wire::WatchEntityViewResponse {
        resume_token: "checkpoint-three".into(),
        change: Some(wire::watch_entity_view_response::Change::Remove(
            name("a").as_str().into(),
        )),
        ..Default::default()
    };
    assert_eq!(live.apply(remove).unwrap(), FrameEffect::Applied);
    assert!(live.current().unwrap().is_empty());
    let add = wire::WatchEntityViewResponse {
        resume_token: "checkpoint-four".into(),
        change: Some(wire::watch_entity_view_response::Change::Upsert(Box::new(
            entry("b", Some(1.0)),
        ))),
        ..Default::default()
    };
    live.apply(add).unwrap();
    assert!(live.current().unwrap().get(&name("b")).is_some());
}
#[test]
fn repeated_checkpoint_and_oversized_frames_withdraw_the_view() {
    let mut live = LiveView::new(completed());
    let frame = wire::WatchEntityViewResponse {
        resume_token: "checkpoint-one".into(),
        change: Some(wire::watch_entity_view_response::Change::Remove(
            name("a").as_str().into(),
        )),
        ..Default::default()
    };
    assert_eq!(live.apply(frame), Err(CacheFailure::OutOfOrder));
    assert!(live.current().is_none());
    let mut huge = entry("a", None);
    huge.entity.as_option_mut().unwrap().display_name = "x".repeat(1024 * 1024);
    let frame = wire::WatchEntityViewResponse {
        resume_token: "checkpoint-two".into(),
        change: Some(wire::watch_entity_view_response::Change::Upsert(Box::new(
            huge,
        ))),
        ..Default::default()
    };
    assert!(frame.encoded_len() > 1024 * 1024);
    let mut live = LiveView::new(completed());
    assert!(live.apply(frame).is_err());
    assert!(live.current().is_none());
}

#[test]
fn capture_incarnation_survives_updates_but_not_recreation_with_identical_metadata() {
    let start = now();
    let mut end = start.clone();
    end.seconds += 60;
    let resource = wire::EntityView {
        name: "homes/synthetic/entityViews/reused-name".into(),
        create_time: start.clone().into(),
        expire_time: end.into(),
        ..Default::default()
    };
    let materialize = || {
        let mut pending = SnapshotCapture::begin(
            HomeName::try_from("homes/synthetic").unwrap(),
            Filter::all(),
            2,
            resource.clone(),
            &start,
        )
        .unwrap();
        pending
            .push(
                page(vec![entry("a", Some(1.0))], "", "same-checkpoint"),
                &start,
            )
            .unwrap();
        pending.finish(&start).unwrap()
    };
    let original = materialize();
    let old_incarnation = original.incarnation().clone();
    assert_eq!(original.clone().incarnation(), &old_incarnation);
    let replacement = materialize();
    assert_eq!(original.name(), replacement.name());
    assert_eq!(original.expires(), replacement.expires());
    assert_eq!(original.checkpoint(), replacement.checkpoint());
    assert_ne!(original.incarnation(), replacement.incarnation());
    let mut live = LiveView::new(original);
    live.apply(wire::WatchEntityViewResponse {
        resume_token: "new-checkpoint".into(),
        change: Some(wire::watch_entity_view_response::Change::Remove(
            name("a").as_str().into(),
        )),
        ..Default::default()
    })
    .unwrap();
    assert_eq!(live.current().unwrap().incarnation(), &old_incarnation);
}
