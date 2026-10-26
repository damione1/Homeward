use super::*;
use wire::watch_lists_response::Frame;

fn ready(watch: &mut ListReadWatch) {
    assert_eq!(
        watch.apply(wire::WatchListsResponse {
            stream_sequence: 1,
            frame: Some(Frame::Ready(wire::WatchReady::default().into())),
            ..Default::default()
        }),
        Ok(WatchEffect::Reload)
    );
}

fn changed(sequence: u64, list: &str) -> wire::WatchListsResponse {
    let mut change = wire::ListChanged::default();
    change.list_id.get_or_insert_default().value = list.into();
    change.revision.get_or_insert_default().list_id = change.list_id.clone();
    change.revision.get_or_insert_default().number = 2;
    wire::WatchListsResponse {
        stream_sequence: sequence,
        frame: Some(Frame::Changed(change.into())),
        ..Default::default()
    }
}

fn item(list: &str, uid: &str, position: i64) -> wire::ItemSnapshot {
    let mut row = wire::ItemSnapshot {
        position,
        ..Default::default()
    };
    row.list_id.get_or_insert_default().value = list.into();
    row.uid.get_or_insert_default().value = uid.into();
    row.revision.get_or_insert_default().list_id = row.list_id.clone();
    row.revision.get_or_insert_default().item_uid = row.uid.clone();
    row.revision.get_or_insert_default().number = 1;
    row.value.get_or_insert_default().summary = format!("Task {uid}");
    row.value.get_or_insert_default().completed = Some(false);
    row
}

fn list(id: &str) -> wire::ListSnapshot {
    let mut row = wire::ListSnapshot {
        name: format!("List {id}"),
        origin: wire::ListOrigin::Native.into(),
        ..Default::default()
    };
    row.id.get_or_insert_default().value = id.into();
    row.revision.get_or_insert_default().list_id = row.id.clone();
    row.revision.get_or_insert_default().number = 1;
    row.updated_at.get_or_insert_default().seconds = 1;
    row
}

#[test]
fn watch_must_be_ready_and_a_change_during_pagination_discards_every_page() {
    let mut watch = ListReadWatch::new(Scope::Items("a".parse().unwrap()));
    assert!(matches!(watch.begin_capture(), Err(ReadFailure::Abandoned)));
    ready(&mut watch);
    let mut capture = watch.begin_capture().unwrap();
    capture
        .push_items(wire::ListItemsResponse {
            items: vec![item("a", "first", 0)],
            next_page_token: "opaque-not-decoded".into(),
            ..Default::default()
        })
        .unwrap();
    assert_eq!(capture.next_token(), "opaque-not-decoded");
    assert_eq!(watch.apply(changed(2, "a")), Ok(WatchEffect::Reload));
    capture
        .push_items(wire::ListItemsResponse {
            items: vec![item("a", "last", 1)],
            ..Default::default()
        })
        .unwrap();
    assert!(matches!(
        watch.finish(capture),
        Err(ReadFailure::ChangedDuringRead)
    ));
    let mut replacement = watch.begin_capture().unwrap();
    replacement
        .push_items(wire::ListItemsResponse::default())
        .unwrap();
    assert!(
        matches!(watch.finish(replacement), Ok(CapturedRows::Items { rows, .. }) if rows.is_empty())
    );
}

#[test]
fn stream_identity_prevents_a_late_reply_from_the_previous_connection() {
    let mut old = ListReadWatch::new(Scope::Catalog);
    ready(&mut old);
    let mut capture = old.begin_capture().unwrap();
    capture
        .push_lists(wire::ListListsResponse {
            lists: vec![list("a")],
            ..Default::default()
        })
        .unwrap();
    old.close();
    let mut new = ListReadWatch::new(Scope::Catalog);
    ready(&mut new);
    assert!(matches!(new.finish(capture), Err(ReadFailure::Abandoned)));
    assert!(matches!(old.begin_capture(), Err(ReadFailure::Abandoned)));
}

#[test]
fn gaps_duplicate_ready_wrong_scope_and_resync_are_terminal() {
    let mut gap = ListReadWatch::new(Scope::Catalog);
    ready(&mut gap);
    assert_eq!(gap.apply(changed(3, "a")), Err(ReadFailure::OutOfOrder));
    assert_eq!(gap.apply(changed(2, "a")), Err(ReadFailure::Abandoned));
    let mut duplicate = ListReadWatch::new(Scope::Catalog);
    ready(&mut duplicate);
    assert_eq!(
        duplicate.apply(wire::WatchListsResponse {
            stream_sequence: 2,
            frame: Some(Frame::Ready(wire::WatchReady::default().into())),
            ..Default::default()
        }),
        Err(ReadFailure::OutOfOrder)
    );
    let mut scoped = ListReadWatch::new(Scope::Items("a".parse().unwrap()));
    ready(&mut scoped);
    assert_eq!(scoped.apply(changed(2, "b")), Err(ReadFailure::WrongScope));
    let mut resync = ListReadWatch::new(Scope::Catalog);
    ready(&mut resync);
    let capture = resync.begin_capture().unwrap();
    assert_eq!(
        resync.apply(wire::WatchListsResponse {
            stream_sequence: 2,
            frame: Some(Frame::ResyncRequired(
                wire::ListResyncRequired::default().into()
            )),
            ..Default::default()
        }),
        Ok(WatchEffect::Resubscribe)
    );
    assert!(matches!(
        resync.finish(capture),
        Err(ReadFailure::Abandoned)
    ));
}

#[test]
fn malformed_pages_poison_the_capture_instead_of_publishing_a_prefix() {
    let mut watch = ListReadWatch::new(Scope::Catalog);
    ready(&mut watch);
    let mut capture = watch.begin_capture().unwrap();
    capture
        .push_lists(wire::ListListsResponse {
            lists: vec![list("a")],
            next_page_token: "next".into(),
            ..Default::default()
        })
        .unwrap();
    assert_eq!(
        capture.push_lists(wire::ListListsResponse {
            lists: vec![list("a")],
            ..Default::default()
        }),
        Err(ReadFailure::Duplicate)
    );
    assert!(matches!(watch.finish(capture), Err(ReadFailure::Abandoned)));

    let mut capture = watch.begin_capture().unwrap();
    capture
        .push_lists(wire::ListListsResponse {
            lists: vec![list("a")],
            next_page_token: "loop".into(),
            ..Default::default()
        })
        .unwrap();
    assert_eq!(
        capture.push_lists(wire::ListListsResponse {
            lists: vec![list("b")],
            next_page_token: "loop".into(),
            ..Default::default()
        }),
        Err(ReadFailure::InvalidResponse)
    );
    assert!(matches!(watch.finish(capture), Err(ReadFailure::Abandoned)));
}

#[test]
fn foreign_items_missing_false_and_reversed_positions_are_rejected() {
    let mut watch = ListReadWatch::new(Scope::Items("a".parse().unwrap()));
    ready(&mut watch);
    let mut capture = watch.begin_capture().unwrap();
    assert_eq!(
        capture.push_items(wire::ListItemsResponse {
            items: vec![item("b", "same-uid", 0)],
            ..Default::default()
        }),
        Err(ReadFailure::WrongScope)
    );
    let mut absent = item("a", "absent", 0);
    absent.value.get_or_insert_default().completed = None;
    let mut capture = watch.begin_capture().unwrap();
    assert_eq!(
        capture.push_items(wire::ListItemsResponse {
            items: vec![absent],
            ..Default::default()
        }),
        Err(ReadFailure::InvalidResponse)
    );
    let mut capture = watch.begin_capture().unwrap();
    assert_eq!(
        capture.push_items(wire::ListItemsResponse {
            items: vec![item("a", "first", 10), item("a", "second", 9)],
            ..Default::default()
        }),
        Err(ReadFailure::OutOfOrder)
    );
}

#[test]
fn ten_thousand_items_are_captured_in_pages_preserving_exact_identity_and_false() {
    let mut watch = ListReadWatch::new(Scope::Items(" opaque / 项目 ".parse().unwrap()));
    ready(&mut watch);
    let mut capture = watch.begin_capture().unwrap();
    for start in (0..10_000).step_by(PAGE_SIZE) {
        let end = (start + PAGE_SIZE).min(10_000);
        capture
            .push_items(wire::ListItemsResponse {
                items: (start..end)
                    .map(|index| item(" opaque / 项目 ", &format!("item-{index}"), index as i64))
                    .collect(),
                next_page_token: if end == 10_000 {
                    String::new()
                } else {
                    format!("opaque-{end}")
                },
                ..Default::default()
            })
            .unwrap();
    }
    let CapturedRows::Items { list, rows } = watch.finish(capture).unwrap() else {
        panic!("items scope")
    };
    assert_eq!(list.as_str(), " opaque / 项目 ");
    assert_eq!(rows.len(), 10_000);
    assert_eq!(rows[9_999].uid.value, "item-9999");
    assert_eq!(rows[0].value.completed, Some(false));
}

#[test]
fn partial_oversize_empty_continuation_and_extra_pages_cannot_publish() {
    let mut watch = ListReadWatch::new(Scope::Catalog);
    ready(&mut watch);
    assert!(matches!(
        watch.finish(watch.begin_capture().unwrap()),
        Err(ReadFailure::Incomplete)
    ));
    let mut partial = watch.begin_capture().unwrap();
    partial
        .push_lists(wire::ListListsResponse {
            lists: vec![list("a")],
            next_page_token: "next".into(),
            ..Default::default()
        })
        .unwrap();
    assert!(matches!(
        watch.finish(partial),
        Err(ReadFailure::Incomplete)
    ));
    let mut oversized = watch.begin_capture().unwrap();
    assert_eq!(
        oversized.push_lists(wire::ListListsResponse {
            lists: (0..257).map(|i| list(&i.to_string())).collect(),
            ..Default::default()
        }),
        Err(ReadFailure::BudgetExceeded)
    );
    let mut empty = watch.begin_capture().unwrap();
    assert_eq!(
        empty.push_lists(wire::ListListsResponse {
            next_page_token: "next".into(),
            ..Default::default()
        }),
        Err(ReadFailure::InvalidResponse)
    );
    let mut complete = watch.begin_capture().unwrap();
    complete
        .push_lists(wire::ListListsResponse::default())
        .unwrap();
    assert_eq!(
        complete.push_lists(wire::ListListsResponse::default()),
        Err(ReadFailure::Abandoned)
    );
    assert!(matches!(
        watch.finish(complete),
        Err(ReadFailure::Abandoned)
    ));
}
