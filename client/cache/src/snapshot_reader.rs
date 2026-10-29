use std::time::{Duration, SystemTime, UNIX_EPOCH};

use async_trait::async_trait;
use buffa_types::google::protobuf::Timestamp;
use home_client_session::AccessLease;
use home_client_transport::{Failure, StateTransport};
use home_wire_contract::home::state::v1 as wire;
use tokio_util::sync::CancellationToken;

use crate::{CacheFailure, CachedView, Filter, MAX_PAGE_SIZE, SnapshotCapture};

#[derive(Debug)]
pub enum SnapshotFailure {
    Contract(CacheFailure),
    Transport(Failure),
    Cancelled,
}

impl std::fmt::Display for SnapshotFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for SnapshotFailure {}
impl From<CacheFailure> for SnapshotFailure {
    fn from(error: CacheFailure) -> Self {
        Self::Contract(error)
    }
}

#[async_trait]
trait SnapshotApi: Send + Sync {
    fn track(&self, _token: &str) -> Option<crate::snapshot_cleanup::ReleaseGuard> {
        None
    }
    async fn open(&self, request: wire::OpenViewRequest)
    -> Result<wire::OpenViewResponse, Failure>;
    async fn read(&self, request: wire::ReadPageRequest)
    -> Result<wire::ReadPageResponse, Failure>;
}

struct NativeApi<'a> {
    transport: &'a StateTransport,
    lease: &'a AccessLease,
    cleanup: Option<&'a crate::snapshot_cleanup::CleanupHandle>,
}

#[async_trait]
impl SnapshotApi for NativeApi<'_> {
    fn track(&self, token: &str) -> Option<crate::snapshot_cleanup::ReleaseGuard> {
        self.cleanup
            .and_then(|cleanup| cleanup.track(token, self.lease))
    }
    async fn open(
        &self,
        request: wire::OpenViewRequest,
    ) -> Result<wire::OpenViewResponse, Failure> {
        self.transport
            .open_view(
                request,
                self.lease.token(),
                Duration::from_secs(10),
                &self.lease.cancellation,
            )
            .await
    }

    async fn read(
        &self,
        request: wire::ReadPageRequest,
    ) -> Result<wire::ReadPageResponse, Failure> {
        self.transport
            .read_page(
                request,
                self.lease.token(),
                Duration::from_secs(10),
                &self.lease.cancellation,
            )
            .await
    }
}

fn wall_now() -> Result<Timestamp, CacheFailure> {
    let time = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| CacheFailure::InvalidResponse)?;
    Ok(Timestamp {
        seconds: i64::try_from(time.as_secs()).map_err(|_| CacheFailure::InvalidResponse)?,
        nanos: time.subsec_nanos() as i32,
        ..Default::default()
    })
}

/// Capture all pages under one admitted access generation. A refused last page
/// exposes no provisional data. The caller must still gate subsequent publication
/// against its owner generation, since an already returned view can be invalidated.
/// Low-level protocol probes use this unmanaged variant; production hosts use
/// CacheOwner, which also relinquishes pagination storage on completion/drop.
pub async fn read_snapshot(
    transport: &StateTransport,
    lease: &AccessLease,
    filter: Filter,
    page_size: u32,
    cancel: &CancellationToken,
) -> Result<CachedView, SnapshotFailure> {
    read_from(
        &NativeApi {
            transport,
            lease,
            cleanup: None,
        },
        filter,
        page_size,
        cancel,
        &lease.cancellation,
        wall_now,
    )
    .await
}

pub(crate) async fn read_snapshot_owned(
    transport: &StateTransport,
    lease: &AccessLease,
    filter: Filter,
    page_size: u32,
    cancel: &CancellationToken,
    cleanup: &crate::snapshot_cleanup::CleanupHandle,
) -> Result<CachedView, SnapshotFailure> {
    read_from(
        &NativeApi {
            transport,
            lease,
            cleanup: Some(cleanup),
        },
        filter,
        page_size,
        cancel,
        &lease.cancellation,
        wall_now,
    )
    .await
}

fn check_cancel(
    cancel: &CancellationToken,
    access: &CancellationToken,
) -> Result<(), SnapshotFailure> {
    if cancel.is_cancelled() || access.is_cancelled() {
        Err(SnapshotFailure::Cancelled)
    } else {
        Ok(())
    }
}

async fn read_from(
    api: &impl SnapshotApi,
    filter: Filter,
    page_size: u32,
    cancel: &CancellationToken,
    access: &CancellationToken,
    now: impl Fn() -> Result<Timestamp, CacheFailure>,
) -> Result<CachedView, SnapshotFailure> {
    if !(1..=MAX_PAGE_SIZE).contains(&page_size) {
        return Err(CacheFailure::InvalidPageSize.into());
    }
    check_cancel(cancel, access)?;
    let request = wire::OpenViewRequest {
        filter: filter.to_wire().into(),
        page_size,
        ..Default::default()
    };
    let first = tokio::select! {
        biased;
        () = cancel.cancelled() => return Err(SnapshotFailure::Cancelled),
        () = access.cancelled() => return Err(SnapshotFailure::Cancelled),
        response = api.open(request) => response.map_err(SnapshotFailure::Transport)?,
    };
    // Acquire before checking cancellation: even a response arriving at a
    // suspension boundary has an owned server reservation to relinquish.
    let _release = api.track(&first.snapshot_token);
    check_cancel(cancel, access)?;
    let mut capture = SnapshotCapture::begin(filter, page_size, first, &now()?)?;
    while let Some(request) = capture.next_request() {
        let response = tokio::select! {
            biased;
            () = cancel.cancelled() => return Err(SnapshotFailure::Cancelled),
            () = access.cancelled() => return Err(SnapshotFailure::Cancelled),
            response = api.read(request) => response.map_err(SnapshotFailure::Transport)?,
        };
        check_cancel(cancel, access)?;
        let page = response
            .page
            .into_option()
            .ok_or(CacheFailure::InvalidResponse)?;
        capture.push(page, &now()?)?;
    }
    check_cancel(cancel, access)?;
    Ok(capture.finish(&now()?)?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use connectrpc::ConnectError;
    use std::{
        collections::VecDeque,
        sync::{
            Mutex,
            atomic::{AtomicI64, AtomicUsize, Ordering},
        },
    };

    #[derive(Clone, Copy)]
    enum LastPage {
        Valid,
        Refused,
        Expired,
        CancelAccess,
        Blocked,
    }

    struct Api {
        pages: Mutex<VecDeque<wire::OpenViewResponse>>,
        calls: AtomicUsize,
        last: LastPage,
        now: AtomicI64,
        access: CancellationToken,
        entered: tokio::sync::Notify,
    }

    fn timestamp(seconds: i64) -> Timestamp {
        Timestamp {
            seconds,
            ..Default::default()
        }
    }

    impl Api {
        fn new(last: LastPage) -> Self {
            let filter = Filter::all();
            let cursor = wire::ViewCursor {
                epoch: "fixture-epoch".into(),
                config_revision: 1,
                authorization_revision: 1,
                global_sequence: 100,
                filter_sha256: filter.digest().to_vec(),
                config_sha256: vec![1; 32],
                grant_sha256: vec![2; 32],
                admission_proof: vec![3; 40],
                ..Default::default()
            };
            let pages = ["two", "three", ""]
                .into_iter()
                .enumerate()
                .map(|(index, next)| wire::OpenViewResponse {
                    snapshot_token: "fixture-capture".into(),
                    cursor: cursor.clone().into(),
                    expires_at: timestamp(60).into(),
                    next_page_token: next.into(),
                    entities: vec![wire::ViewEntity {
                        entity_id: format!("entity-{index}"),
                        ..Default::default()
                    }],
                    ..Default::default()
                })
                .collect();
            Self {
                pages: Mutex::new(pages),
                calls: AtomicUsize::new(0),
                last,
                now: AtomicI64::new(0),
                access: CancellationToken::new(),
                entered: tokio::sync::Notify::new(),
            }
        }
        fn clock(&self) -> Result<Timestamp, CacheFailure> {
            Ok(timestamp(self.now.load(Ordering::SeqCst)))
        }
    }

    #[async_trait]
    impl SnapshotApi for Api {
        async fn open(
            &self,
            request: wire::OpenViewRequest,
        ) -> Result<wire::OpenViewResponse, Failure> {
            assert!(request.filter.is_set());
            assert_eq!(request.page_size, 1);
            self.calls.fetch_add(1, Ordering::SeqCst);
            Ok(self.pages.lock().unwrap().pop_front().unwrap())
        }
        async fn read(
            &self,
            request: wire::ReadPageRequest,
        ) -> Result<wire::ReadPageResponse, Failure> {
            assert_eq!(request.snapshot_token, "fixture-capture");
            let call = self.calls.fetch_add(1, Ordering::SeqCst);
            assert_eq!(request.page_token, if call == 1 { "two" } else { "three" });
            if call == 2 {
                match self.last {
                    LastPage::Valid => {}
                    LastPage::Refused => {
                        return Err(Failure::from(ConnectError::permission_denied("fixture")));
                    }
                    LastPage::Expired => self.now.store(60, Ordering::SeqCst),
                    LastPage::CancelAccess => self.access.cancel(),
                    LastPage::Blocked => {
                        self.entered.notify_one();
                        std::future::pending::<()>().await;
                    }
                }
            }
            Ok(wire::ReadPageResponse {
                page: self.pages.lock().unwrap().pop_front().unwrap().into(),
                ..Default::default()
            })
        }
    }

    #[tokio::test]
    async fn driver_collects_all_pages_before_returning_a_complete_view() {
        let api = Api::new(LastPage::Valid);
        let result = read_from(
            &api,
            Filter::all(),
            1,
            &CancellationToken::new(),
            &api.access,
            || api.clock(),
        )
        .await
        .unwrap();
        assert_eq!(result.len(), 3);
        assert_eq!(api.calls.load(Ordering::SeqCst), 3);
    }

    #[tokio::test]
    async fn last_rpc_refusal_expiry_and_generation_cancellation_discard_every_page() {
        for last in [LastPage::Refused, LastPage::Expired, LastPage::CancelAccess] {
            let api = Api::new(last);
            let result = read_from(
                &api,
                Filter::all(),
                1,
                &CancellationToken::new(),
                &api.access,
                || api.clock(),
            )
            .await;
            match last {
                LastPage::Refused => assert!(matches!(result, Err(SnapshotFailure::Transport(_)))),
                LastPage::Expired => assert!(matches!(
                    result,
                    Err(SnapshotFailure::Contract(CacheFailure::Expired))
                )),
                LastPage::CancelAccess => {
                    assert!(matches!(result, Err(SnapshotFailure::Cancelled)))
                }
                LastPage::Valid | LastPage::Blocked => unreachable!("not selected by this test"),
            }
            assert_eq!(api.calls.load(Ordering::SeqCst), 3);
        }
    }

    #[tokio::test]
    async fn cancellation_interrupts_a_blocked_last_page_and_prevents_an_initial_rpc() {
        let api = Api::new(LastPage::Blocked);
        let cancel = CancellationToken::new();
        let work = read_from(&api, Filter::all(), 1, &cancel, &api.access, || api.clock());
        let cancel_when_entered = async {
            api.entered.notified().await;
            cancel.cancel();
        };
        let (result, ()) = tokio::join!(work, cancel_when_entered);
        assert!(matches!(result, Err(SnapshotFailure::Cancelled)));
        let api = Api::new(LastPage::Valid);
        let result = read_from(&api, Filter::all(), 1, &cancel, &api.access, || api.clock()).await;
        assert!(matches!(result, Err(SnapshotFailure::Cancelled)));
        assert_eq!(api.calls.load(Ordering::SeqCst), 0);
    }
}
