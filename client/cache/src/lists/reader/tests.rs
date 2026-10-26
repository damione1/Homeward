use super::*;
use std::{
    collections::VecDeque,
    sync::{
        Mutex,
        atomic::{AtomicUsize, Ordering},
    },
};
use tokio::sync::{Notify, mpsc};
use wire::watch_lists_response::Frame;

struct Frames(mpsc::UnboundedReceiver<wire::WatchListsResponse>);
#[async_trait]
impl Stream for Frames {
    async fn next(&mut self) -> Result<Option<wire::WatchListsResponse>, Failure> {
        Ok(self.0.recv().await)
    }
}

struct Api {
    pages: Mutex<VecDeque<wire::ListListsResponse>>,
    tokens: Mutex<Vec<String>>,
    counts: AtomicUsize,
    changes: mpsc::UnboundedSender<wire::WatchListsResponse>,
    changed_on_page: Option<usize>,
}

#[async_trait]
impl ReadApi for Api {
    async fn metadata(&self, _: &Scope) -> Result<Option<wire::ListSnapshot>, ListReadExit> {
        Ok(None)
    }
    async fn count(&self, _: &Scope) -> Result<u64, ListReadExit> {
        self.counts.fetch_add(1, Ordering::SeqCst);
        Ok(93) // Deliberately separate from the loaded row count.
    }
    async fn page(&self, _: &Scope, token: String) -> Result<Page, ListReadExit> {
        let mut tokens = self.tokens.lock().unwrap();
        tokens.push(token);
        if self.changed_on_page == Some(tokens.len()) {
            let mut change = wire::ListChanged::default();
            change.list_id.get_or_insert_default().value = "old".into();
            change.revision.get_or_insert_default().list_id = change.list_id.clone();
            change.revision.get_or_insert_default().number = 2;
            self.changes
                .send(wire::WatchListsResponse {
                    stream_sequence: 2,
                    frame: Some(Frame::Changed(change.into())),
                    ..Default::default()
                })
                .unwrap();
        }
        Ok(Page::Catalog(
            self.pages
                .lock()
                .unwrap()
                .pop_front()
                .expect("expected page RPC"),
        ))
    }
}

fn ready() -> wire::WatchListsResponse {
    wire::WatchListsResponse {
        stream_sequence: 1,
        frame: Some(Frame::Ready(Default::default())),
        ..Default::default()
    }
}

fn page(name: &str, token: &str) -> wire::ListListsResponse {
    let mut row = wire::ListSnapshot {
        name: name.into(),
        origin: wire::ListOrigin::Native.into(),
        ..Default::default()
    };
    row.id.get_or_insert_default().value = name.into();
    row.revision.get_or_insert_default().list_id = row.id.clone();
    row.revision.get_or_insert_default().number = 1;
    row.updated_at.get_or_insert_default().seconds = 1;
    wire::ListListsResponse {
        lists: vec![row],
        next_page_token: token.into(),
        ..Default::default()
    }
}

#[tokio::test]
async fn last_page_race_discards_the_old_prefix_and_rereads_count_without_a_new_stream() {
    let (sender, receiver) = mpsc::unbounded_channel();
    sender.send(ready()).unwrap();
    let api = Api {
        pages: Mutex::new(VecDeque::from([
            page("old", "next"),
            page("stale", ""),
            page("fresh", ""),
        ])),
        tokens: Mutex::new(Vec::new()),
        counts: AtomicUsize::new(0),
        changes: sender,
        changed_on_page: Some(2),
    };
    let cancel = CancellationToken::new();
    let access = CancellationToken::new();
    let mut publications = Vec::new();
    let mut invalidations = 0;
    let exit = tokio::time::timeout(
        Duration::from_secs(2),
        drive(
            &api,
            &mut Frames(receiver),
            Scope::Catalog,
            &cancel,
            &access,
            |event| match event {
                ListReadEvent::Invalidated => invalidations += 1,
                ListReadEvent::Published(snapshot) => {
                    assert_eq!(snapshot.count, 93);
                    assert!(snapshot.fetched_at <= SystemTime::now());
                    let CapturedRows::Catalog(rows) = &snapshot.rows else {
                        panic!("catalog")
                    };
                    publications.push(
                        rows.iter()
                            .map(|row| row.id.value.clone())
                            .collect::<Vec<_>>(),
                    );
                    cancel.cancel();
                }
            },
        ),
    )
    .await
    .expect("driver must terminate on cancellation");
    assert!(matches!(exit, ListReadExit::Cancelled));
    assert_eq!(publications, [vec!["fresh"]]);
    assert_eq!(invalidations, 1);
    assert_eq!(api.counts.load(Ordering::SeqCst), 2);
    assert_eq!(*api.tokens.lock().unwrap(), ["", "next", ""]);
}

#[tokio::test]
async fn no_count_or_page_rpc_runs_before_ready_or_after_initial_cancellation() {
    for cancelled in [false, true] {
        let (sender, receiver) = mpsc::unbounded_channel();
        // Malformed first frame must terminate before any read RPC.
        sender.send(wire::WatchListsResponse::default()).unwrap();
        let api = Api {
            pages: Mutex::new(VecDeque::new()),
            tokens: Mutex::new(Vec::new()),
            counts: AtomicUsize::new(0),
            changes: sender,
            changed_on_page: None,
        };
        let cancel = CancellationToken::new();
        if cancelled {
            cancel.cancel();
        }
        let exit = drive(
            &api,
            &mut Frames(receiver),
            Scope::Catalog,
            &cancel,
            &CancellationToken::new(),
            |_| panic!("no publication"),
        )
        .await;
        if cancelled {
            assert!(matches!(exit, ListReadExit::Cancelled));
        } else {
            assert!(matches!(
                exit,
                ListReadExit::Contract(ReadFailure::InvalidResponse)
            ));
        }
        assert_eq!(api.counts.load(Ordering::SeqCst), 0);
        assert!(api.tokens.lock().unwrap().is_empty());
    }
}

struct BlockedApi {
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
impl ReadApi for BlockedApi {
    async fn metadata(&self, _: &Scope) -> Result<Option<wire::ListSnapshot>, ListReadExit> {
        panic!("blocked Count precedes metadata")
    }
    async fn count(&self, _: &Scope) -> Result<u64, ListReadExit> {
        let _drop = DropRead(&self.dropped);
        self.started.notify_one();
        std::future::pending().await
    }
    async fn page(&self, _: &Scope, _: String) -> Result<Page, ListReadExit> {
        panic!("blocked Count must be cancelled before any page RPC");
    }
}

#[tokio::test]
async fn authority_resync_and_access_cancellation_drop_a_blocked_read_without_publication() {
    enum Stop {
        Resync,
        Access,
    }
    for stop in [Stop::Resync, Stop::Access] {
        let (sender, receiver) = mpsc::unbounded_channel();
        sender.send(ready()).unwrap();
        let api = BlockedApi {
            started: Notify::new(),
            dropped: AtomicUsize::new(0),
        };
        let access = CancellationToken::new();
        let cancel = CancellationToken::new();
        let stopping = async {
            api.started.notified().await;
            match stop {
                Stop::Resync => sender
                    .send(wire::WatchListsResponse {
                        stream_sequence: 2,
                        frame: Some(Frame::ResyncRequired(Default::default())),
                        ..Default::default()
                    })
                    .unwrap(),
                Stop::Access => access.cancel(),
            }
        };
        let mut stream = Frames(receiver);
        let driving = drive(&api, &mut stream, Scope::Catalog, &cancel, &access, |_| {
            panic!("no publication")
        });
        let (exit, ()) = tokio::time::timeout(Duration::from_secs(2), async {
            tokio::join!(driving, stopping)
        })
        .await
        .unwrap();
        match stop {
            Stop::Resync => assert!(matches!(exit, ListReadExit::Resync)),
            Stop::Access => assert!(matches!(exit, ListReadExit::Cancelled)),
        }
        assert_eq!(api.dropped.load(Ordering::SeqCst), 1);
    }
}

#[tokio::test]
async fn item_capture_carries_its_list_revision_and_refuses_missing_foreign_or_invalid_metadata() {
    struct MetadataApi {
        list: Option<wire::ListSnapshot>,
        pages: AtomicUsize,
    }
    #[async_trait]
    impl ReadApi for MetadataApi {
        async fn count(&self, _: &Scope) -> Result<u64, ListReadExit> {
            Ok(93)
        }
        async fn metadata(&self, _: &Scope) -> Result<Option<wire::ListSnapshot>, ListReadExit> {
            Ok(self.list.clone())
        }
        async fn page(&self, _: &Scope, _: String) -> Result<Page, ListReadExit> {
            self.pages.fetch_add(1, Ordering::SeqCst);
            Ok(Page::Items(wire::ListItemsResponse::default()))
        }
    }
    enum Metadata {
        Missing,
        Foreign,
        InvalidRevision,
        Valid,
    }
    for scenario in [
        Metadata::Missing,
        Metadata::Foreign,
        Metadata::InvalidRevision,
        Metadata::Valid,
    ] {
        let list = match scenario {
            Metadata::Missing => None,
            Metadata::Foreign => Some(page("other", "").lists.remove(0)),
            Metadata::InvalidRevision | Metadata::Valid => {
                let mut row = page("a", "").lists.remove(0);
                row.revision.get_or_insert_default().number = match scenario {
                    Metadata::InvalidRevision => 0,
                    Metadata::Valid => 12,
                    Metadata::Missing | Metadata::Foreign => unreachable!(),
                };
                Some(row)
            }
        };
        let api = MetadataApi {
            list,
            pages: AtomicUsize::new(0),
        };
        let (sender, receiver) = mpsc::unbounded_channel();
        sender.send(ready()).unwrap();
        let mut stream = Frames(receiver);
        let cancel = CancellationToken::new();
        let access = CancellationToken::new();
        let mut published = 0;
        let exit = drive(&api, &mut stream, Scope::Items("a".parse().unwrap()), &cancel, &access, |event| {
            let ListReadEvent::Published(snapshot) = event else { panic!("no changed frame") };
            let ListReadMetadata::List(list) = &snapshot.metadata else { panic!("item capture needs list metadata") };
            assert_eq!(list.id.value, "a");
            assert_eq!(list.revision.number, 12);
            assert_eq!(snapshot.count, 93);
            assert!(matches!(&snapshot.rows, CapturedRows::Items { list, rows } if list.as_str() == "a" && rows.is_empty()));
            published += 1;
            cancel.cancel();
        }).await;
        match scenario {
            Metadata::Valid => {
                assert!(matches!(exit, ListReadExit::Cancelled));
                assert_eq!(published, 1);
                assert_eq!(api.pages.load(Ordering::SeqCst), 1);
            }
            Metadata::Foreign => {
                assert!(matches!(
                    exit,
                    ListReadExit::Contract(ReadFailure::WrongScope)
                ));
                assert_eq!(published, 0);
                assert_eq!(api.pages.load(Ordering::SeqCst), 0);
            }
            Metadata::Missing | Metadata::InvalidRevision => {
                assert!(matches!(
                    exit,
                    ListReadExit::Contract(ReadFailure::InvalidResponse)
                ));
                assert_eq!(published, 0);
                assert_eq!(api.pages.load(Ordering::SeqCst), 0);
            }
        }
    }
}
