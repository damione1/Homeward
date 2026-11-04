use super::*;
use crate::lists::tests::{changed, home, list, name, ready};
use std::{
    collections::VecDeque,
    sync::{
        Mutex,
        atomic::{AtomicUsize, Ordering},
    },
};
use tokio::sync::{Notify, mpsc};
struct Frames(mpsc::UnboundedReceiver<wire::WatchTaskListsResponse>);
#[async_trait]
impl Stream for Frames {
    async fn next(&mut self) -> Result<Option<wire::WatchTaskListsResponse>, Failure> {
        Ok(self.0.recv().await)
    }
}
struct Api {
    pages: Mutex<VecDeque<wire::ListTaskListsResponse>>,
    tokens: Mutex<Vec<String>>,
    changes: mpsc::UnboundedSender<wire::WatchTaskListsResponse>,
    change_on: Option<usize>,
}
#[async_trait]
impl ReadApi for Api {
    async fn metadata(
        &self,
        _: &Scope,
    ) -> Result<Option<home_client_model::api_tasks::TaskListResource>, ListReadExit> {
        Ok(None)
    }
    async fn page(&self, _: &Scope, token: String) -> Result<Page, ListReadExit> {
        let mut tokens = self.tokens.lock().unwrap();
        tokens.push(token);
        if self.change_on == Some(tokens.len()) {
            self.changes.send(changed(2, "old")).unwrap();
        }
        Ok(Page::Catalog(
            self.pages
                .lock()
                .unwrap()
                .pop_front()
                .expect("bounded expected RPC"),
        ))
    }
}
fn page(id: &str, token: &str, total: i32) -> wire::ListTaskListsResponse {
    wire::ListTaskListsResponse {
        task_lists: vec![list(id)],
        next_page_token: token.to_owned(),
        total_size: total,
        ..Default::default()
    }
}
#[tokio::test]
async fn queued_last_page_invalidation_discards_all_old_rows_and_reloads_without_new_stream() {
    let (sender, receiver) = mpsc::unbounded_channel();
    sender.send(ready()).unwrap();
    let api = Api {
        pages: Mutex::new(VecDeque::from([
            page("old", "next", 2),
            page("stale", "", 2),
            page("fresh", "", 1),
        ])),
        tokens: Mutex::new(Vec::new()),
        changes: sender,
        change_on: Some(2),
    };
    let cancel = CancellationToken::new();
    let mut publications = Vec::new();
    let mut invalidations = 0;
    let exit = tokio::time::timeout(
        Duration::from_secs(2),
        drive(
            &api,
            &mut Frames(receiver),
            Scope::Catalog,
            home(),
            &cancel,
            &CancellationToken::new(),
            |event| match event {
                ListReadEvent::Invalidated => invalidations += 1,
                ListReadEvent::Published(snapshot) => {
                    assert_eq!(snapshot.count, 1);
                    assert_eq!(snapshot.home, home());
                    let CapturedRows::Catalog(rows) = &snapshot.rows else {
                        panic!("catalog")
                    };
                    publications.push(
                        rows.iter()
                            .map(|row| row.native_id().as_str().to_owned())
                            .collect::<Vec<_>>(),
                    );
                    cancel.cancel();
                }
            },
        ),
    )
    .await
    .unwrap();
    assert!(matches!(exit, ListReadExit::Cancelled));
    assert_eq!(publications, [vec!["fresh"]]);
    assert_eq!(invalidations, 1);
    assert_eq!(*api.tokens.lock().unwrap(), ["", "next", ""]);
}
#[tokio::test]
async fn malformed_ready_or_initial_cancellation_never_starts_reads() {
    for cancelled in [false, true] {
        let (sender, receiver) = mpsc::unbounded_channel();
        sender
            .send(wire::WatchTaskListsResponse::default())
            .unwrap();
        let api = Api {
            pages: Mutex::new(VecDeque::new()),
            tokens: Mutex::new(Vec::new()),
            changes: sender,
            change_on: None,
        };
        let cancel = CancellationToken::new();
        if cancelled {
            cancel.cancel()
        }
        let exit = drive(
            &api,
            &mut Frames(receiver),
            Scope::Catalog,
            home(),
            &cancel,
            &CancellationToken::new(),
            |_| panic!("no publish"),
        )
        .await;
        assert!(if cancelled {
            matches!(exit, ListReadExit::Cancelled)
        } else {
            matches!(exit, ListReadExit::Contract(ReadFailure::InvalidResponse))
        });
        assert!(api.tokens.lock().unwrap().is_empty());
    }
}
struct Blocked {
    started: Notify,
    dropped: AtomicUsize,
}
struct DropRead<'a>(&'a AtomicUsize);
impl Drop for DropRead<'_> {
    fn drop(&mut self) {
        self.0.fetch_add(1, Ordering::SeqCst);
    }
}
#[async_trait]
impl ReadApi for Blocked {
    async fn metadata(
        &self,
        _: &Scope,
    ) -> Result<Option<home_client_model::api_tasks::TaskListResource>, ListReadExit> {
        let _drop = DropRead(&self.dropped);
        self.started.notify_one();
        std::future::pending().await
    }
    async fn page(&self, _: &Scope, _: String) -> Result<Page, ListReadExit> {
        panic!("metadata blocked")
    }
}
#[tokio::test]
async fn resync_cancellation_and_eof_drop_blocked_capture_without_publication() {
    for mode in 0..3 {
        let (sender, receiver) = mpsc::unbounded_channel();
        sender.send(ready()).unwrap();
        let api = Blocked {
            started: Notify::new(),
            dropped: AtomicUsize::new(0),
        };
        let cancel = CancellationToken::new();
        let access = CancellationToken::new();
        let mut stream = Frames(receiver);
        let driving = drive(
            &api,
            &mut stream,
            Scope::Catalog,
            home(),
            &cancel,
            &access,
            |_| panic!("no publication"),
        );
        let stopping = async {
            api.started.notified().await;
            match mode {
                0 => {
                    sender
                        .send(wire::WatchTaskListsResponse {
                            sequence: 2,
                            frame: Some(wire::watch_task_lists_response::Frame::ResyncRequired(
                                Default::default(),
                            )),
                            ..Default::default()
                        })
                        .unwrap();
                }
                1 => access.cancel(),
                2 => drop(sender),
                _ => unreachable!(),
            }
        };
        let (exit, ()) = tokio::time::timeout(Duration::from_secs(2), async {
            tokio::join!(driving, stopping)
        })
        .await
        .unwrap();
        assert!(match mode {
            0 => matches!(exit, ListReadExit::Resync),
            1 => matches!(exit, ListReadExit::Cancelled),
            2 => matches!(exit, ListReadExit::Ended),
            _ => false,
        });
        assert_eq!(api.dropped.load(Ordering::SeqCst), 1);
    }
}
#[tokio::test]
async fn captured_list_metadata_must_match_selected_resource_and_remain_with_items() {
    struct Metadata(Option<home_client_model::api_tasks::TaskListResource>);
    #[async_trait]
    impl ReadApi for Metadata {
        async fn metadata(
            &self,
            _: &Scope,
        ) -> Result<Option<home_client_model::api_tasks::TaskListResource>, ListReadExit> {
            Ok(self.0.clone())
        }
        async fn page(&self, _: &Scope, _: String) -> Result<Page, ListReadExit> {
            Ok(Page::Items(wire::ListTasksResponse::default()))
        }
    }
    for id in [None, Some("other"), Some("a")] {
        let api = Metadata(
            id.map(|id| home_wire_contract::tasks_contract::parse_task_list(&list(id)).unwrap()),
        );
        let (sender, receiver) = mpsc::unbounded_channel();
        sender.send(ready()).unwrap();
        let cancel = CancellationToken::new();
        let exit = drive(
            &api,
            &mut Frames(receiver),
            Scope::Items("a".parse().unwrap()),
            home(),
            &cancel,
            &CancellationToken::new(),
            |event| {
                let ListReadEvent::Published(snapshot) = event else {
                    panic!("no change")
                };
                let ListReadMetadata::List(resource) = &snapshot.metadata else {
                    panic!("metadata")
                };
                assert_eq!(resource.name(), &name("a"));
                assert_eq!(resource.revision().etag().as_str(), "\"opaque-list\"");
                assert_eq!(snapshot.count, 0);
                cancel.cancel();
            },
        )
        .await;
        assert!(match id {
            None => matches!(exit, ListReadExit::Contract(ReadFailure::InvalidResponse)),
            Some("a") => matches!(exit, ListReadExit::Cancelled),
            Some(_) => matches!(exit, ListReadExit::Contract(ReadFailure::WrongScope)),
        });
    }
}
