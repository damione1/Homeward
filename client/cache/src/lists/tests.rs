use super::*;
use home_client_model::api_tasks::{TaskListName, TaskName};
use wire::watch_task_lists_response::Frame;
pub(super) fn home() -> HomeName {
    HomeName::try_from("homes/synthetic").unwrap()
}
pub(super) fn name(id: &str) -> TaskListName {
    TaskListName::new(home(), id.parse().unwrap())
}
pub(super) fn list(id: &str) -> wire::TaskList {
    wire::TaskList {
        name: name(id).as_str().to_owned(),
        display_name: format!("List {id}"),
        etag: "\"opaque-list\"".to_owned(),
        origin: wire::TaskListOrigin::Native.into(),
        update_time: buffa_types::google::protobuf::Timestamp {
            seconds: 1,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
pub(super) fn item(list: &str, uid: &str, position: i64) -> wire::Task {
    wire::Task {
        name: TaskName::new(name(list), uid.parse().unwrap())
            .as_str()
            .to_owned(),
        summary: format!("Task {uid}"),
        etag: "\"opaque-task\"".to_owned(),
        position,
        ..Default::default()
    }
}
pub(super) fn ready() -> wire::WatchTaskListsResponse {
    wire::WatchTaskListsResponse {
        sequence: 1,
        frame: Some(Frame::Ready(Default::default())),
        ..Default::default()
    }
}
pub(super) fn changed(sequence: i64, list: &str) -> wire::WatchTaskListsResponse {
    wire::WatchTaskListsResponse {
        sequence,
        frame: Some(Frame::Changed(Box::new(wire::TaskListChanged {
            task_list: name(list).as_str().to_owned(),
            etag: "\"next\"".to_owned(),
            ..Default::default()
        }))),
        ..Default::default()
    }
}
fn watch(scope: Scope) -> ListReadWatch {
    let mut watch = ListReadWatch::new(scope, home());
    assert_eq!(watch.apply(ready()), Ok(WatchEffect::Reload));
    watch
}
#[test]
fn invalidation_and_stream_identity_discard_provisional_pages() {
    let mut old = watch(Scope::Catalog);
    let mut capture = old.begin_capture().unwrap();
    capture
        .push_lists(wire::ListTaskListsResponse {
            task_lists: vec![list("a")],
            next_page_token: "next".to_owned(),
            total_size: 2,
            ..Default::default()
        })
        .unwrap();
    old.apply(changed(2, "a")).unwrap();
    capture
        .push_lists(wire::ListTaskListsResponse {
            task_lists: vec![list("b")],
            total_size: 2,
            ..Default::default()
        })
        .unwrap();
    assert!(matches!(
        old.finish(capture),
        Err(ReadFailure::ChangedDuringRead)
    ));
    let mut capture = old.begin_capture().unwrap();
    capture
        .push_lists(wire::ListTaskListsResponse::default())
        .unwrap();
    let replacement = watch(Scope::Catalog);
    assert!(matches!(
        replacement.finish(capture),
        Err(ReadFailure::Abandoned)
    ));
    old.close();
    assert!(old.begin_capture().is_err());
}
#[test]
fn gaps_wrong_scope_duplicate_ready_and_resync_close_the_fence() {
    let mut gap = watch(Scope::Catalog);
    assert_eq!(gap.apply(changed(3, "a")), Err(ReadFailure::OutOfOrder));
    assert_eq!(gap.apply(changed(2, "a")), Err(ReadFailure::Abandoned));
    let mut duplicate = watch(Scope::Catalog);
    let mut ready = ready();
    ready.sequence = 2;
    assert_eq!(duplicate.apply(ready), Err(ReadFailure::OutOfOrder));
    let mut scoped = watch(Scope::Items("a".parse().unwrap()));
    assert_eq!(scoped.apply(changed(2, "b")), Err(ReadFailure::WrongScope));
    let mut resync = watch(Scope::Catalog);
    let capture = resync.begin_capture().unwrap();
    assert_eq!(
        resync.apply(wire::WatchTaskListsResponse {
            sequence: 2,
            frame: Some(Frame::ResyncRequired(Default::default())),
            ..Default::default()
        }),
        Ok(WatchEffect::Resubscribe)
    );
    assert!(resync.finish(capture).is_err());
}
#[test]
fn foreign_home_foreign_list_unknown_content_and_reversed_positions_never_publish() {
    let watch = watch(Scope::Items("a".parse().unwrap()));
    let mut foreign_home = item("a", "uid", 0);
    foreign_home.name = foreign_home.name.replace("homes/synthetic", "homes/other");
    for row in [item("b", "uid", 0), foreign_home] {
        let mut capture = watch.begin_capture().unwrap();
        assert_eq!(
            capture.push_items(wire::ListTasksResponse {
                tasks: vec![row],
                total_size: 1,
                ..Default::default()
            }),
            Err(ReadFailure::WrongScope)
        );
        assert!(watch.finish(capture).is_err());
    }
    let mut invalid = item("a", "uid", 0);
    invalid.etag.clear();
    let mut capture = watch.begin_capture().unwrap();
    assert_eq!(
        capture.push_items(wire::ListTasksResponse {
            tasks: vec![invalid],
            total_size: 1,
            ..Default::default()
        }),
        Err(ReadFailure::InvalidResponse)
    );
    let mut capture = watch.begin_capture().unwrap();
    assert_eq!(
        capture.push_items(wire::ListTasksResponse {
            tasks: vec![item("a", "one", 1), item("a", "two", 0)],
            total_size: 2,
            ..Default::default()
        }),
        Err(ReadFailure::OutOfOrder)
    );
}
#[test]
fn duplicates_token_loops_and_budget_failures_poison_the_entire_capture() {
    let watch = watch(Scope::Catalog);
    for duplicate in [true, false] {
        let mut capture = watch.begin_capture().unwrap();
        capture
            .push_lists(wire::ListTaskListsResponse {
                task_lists: vec![list("a")],
                next_page_token: "same".to_owned(),
                total_size: 2,
                ..Default::default()
            })
            .unwrap();
        let result = capture.push_lists(wire::ListTaskListsResponse {
            task_lists: vec![list(if duplicate { "a" } else { "b" })],
            next_page_token: if duplicate {
                String::new()
            } else {
                "same".to_owned()
            },
            total_size: 2,
            ..Default::default()
        });
        assert_eq!(
            result,
            Err(if duplicate {
                ReadFailure::Duplicate
            } else {
                ReadFailure::InvalidResponse
            })
        );
        assert!(watch.finish(capture).is_err());
    }
    let mut capture = watch.begin_capture().unwrap();
    assert_eq!(
        capture.push_lists(wire::ListTaskListsResponse {
            total_size: i32::MAX,
            ..Default::default()
        }),
        Err(ReadFailure::BudgetExceeded)
    );
    let mut capture = watch.begin_capture().unwrap();
    assert_eq!(
        capture.push_lists(wire::ListTaskListsResponse {
            task_lists: (0..257).map(|i| list(&i.to_string())).collect(),
            total_size: 257,
            ..Default::default()
        }),
        Err(ReadFailure::BudgetExceeded)
    );
}
#[test]
fn empty_aip_pages_continue_and_server_totals_must_remain_exact() {
    let watch = watch(Scope::Catalog);
    let mut capture = watch.begin_capture().unwrap();
    capture
        .push_lists(wire::ListTaskListsResponse {
            next_page_token: "opaque".to_owned(),
            total_size: 1,
            ..Default::default()
        })
        .unwrap();
    capture
        .push_lists(wire::ListTaskListsResponse {
            task_lists: vec![list("a")],
            total_size: 1,
            ..Default::default()
        })
        .unwrap();
    assert_eq!(capture.total(), Ok(1));
    assert!(watch.finish(capture).is_ok());
    let mut capture = watch.begin_capture().unwrap();
    capture
        .push_lists(wire::ListTaskListsResponse {
            total_size: 1,
            ..Default::default()
        })
        .unwrap();
    assert_eq!(capture.total(), Err(ReadFailure::Incomplete));
}
#[test]
fn ten_thousand_tasks_keep_exact_imported_keys_false_and_position_zero() {
    let watch = watch(Scope::Items(" opaque / 项目 ".parse().unwrap()));
    let mut capture = watch.begin_capture().unwrap();
    for start in (0..10_000).step_by(PAGE_SIZE) {
        let end = (start + PAGE_SIZE).min(10_000);
        capture
            .push_items(wire::ListTasksResponse {
                tasks: (start..end)
                    .map(|i| item(" opaque / 项目 ", &format!("task-{i}"), i as i64))
                    .collect(),
                next_page_token: if end == 10_000 {
                    String::new()
                } else {
                    format!("opaque-{end}")
                },
                total_size: 10_000,
                ..Default::default()
            })
            .unwrap();
    }
    assert_eq!(capture.total(), Ok(10_000));
    let CapturedRows::Items { rows, .. } = watch.finish(capture).unwrap() else {
        panic!("items")
    };
    assert_eq!(rows[9999].key().uid().as_str(), "task-9999");
    assert!(!rows[0].content().completed());
    assert_eq!(rows[0].position(), 0);
}
