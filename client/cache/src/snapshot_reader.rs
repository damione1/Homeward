use crate::{CacheFailure, CachedView, Filter, MAX_PAGE_SIZE, SnapshotCapture, wall_now};
use async_trait::async_trait;
use home_client_model::api::HomeName;
use home_client_session::AccessLease;
use home_client_transport::{EntityViewTransport, Failure, HomeTransport};
use home_wire_contract::home::api::v2 as wire;
use std::{sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;

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
    fn from(value: CacheFailure) -> Self {
        Self::Contract(value)
    }
}

#[async_trait]
trait SnapshotApi: Send + Sync {
    fn track(&self, _name: &str) -> Option<crate::snapshot_cleanup::ReleaseGuard> {
        None
    }
    async fn home(&self) -> Result<wire::ListHomesResponse, Failure>;
    async fn create(
        &self,
        request: wire::CreateEntityViewRequest,
    ) -> Result<wire::EntityView, Failure>;
    async fn query(
        &self,
        request: wire::QueryEntityViewRequest,
    ) -> Result<wire::QueryEntityViewResponse, Failure>;
}
struct NativeApi<'a> {
    transport: &'a EntityViewTransport,
    homes: &'a HomeTransport,
    lease: &'a AccessLease,
    cleanup: Option<&'a crate::snapshot_cleanup::CleanupHandle>,
}
#[async_trait]
impl SnapshotApi for NativeApi<'_> {
    fn track(&self, name: &str) -> Option<crate::snapshot_cleanup::ReleaseGuard> {
        self.cleanup.and_then(|owner| owner.track(name, self.lease))
    }
    async fn home(&self) -> Result<wire::ListHomesResponse, Failure> {
        self.homes
            .list_homes(
                wire::ListHomesRequest {
                    page_size: 1,
                    ..Default::default()
                },
                self.lease.token(),
                Duration::from_secs(10),
                &self.lease.cancellation,
            )
            .await
    }
    async fn create(
        &self,
        request: wire::CreateEntityViewRequest,
    ) -> Result<wire::EntityView, Failure> {
        self.transport
            .create_entity_view(
                request,
                self.lease.token(),
                Duration::from_secs(10),
                &self.lease.cancellation,
            )
            .await
    }
    async fn query(
        &self,
        request: wire::QueryEntityViewRequest,
    ) -> Result<wire::QueryEntityViewResponse, Failure> {
        self.transport
            .query_entity_view(
                request,
                self.lease.token(),
                Duration::from_secs(10),
                &self.lease.cancellation,
            )
            .await
    }
}
pub async fn read_snapshot(
    transport: &EntityViewTransport,
    homes: &HomeTransport,
    lease: &AccessLease,
    filter: Filter,
    page_size: u32,
    cancel: &CancellationToken,
) -> Result<CachedView, SnapshotFailure> {
    read_from(
        &NativeApi {
            transport,
            homes,
            lease,
            cleanup: None,
        },
        filter,
        page_size,
        cancel,
        &lease.cancellation,
    )
    .await
    .map(|mut view| {
        view.source = Some(lease.source_proof());
        view
    })
}
pub(crate) async fn read_snapshot_owned(
    transport: &EntityViewTransport,
    homes: &HomeTransport,
    lease: &AccessLease,
    filter: Filter,
    page_size: u32,
    cancel: &CancellationToken,
    cleanup: &crate::snapshot_cleanup::CleanupHandle,
) -> Result<CachedView, SnapshotFailure> {
    read_from(
        &NativeApi {
            transport,
            homes,
            lease,
            cleanup: Some(cleanup),
        },
        filter,
        page_size,
        cancel,
        &lease.cancellation,
    )
    .await
    .map(|mut view| {
        view.source = Some(lease.source_proof());
        view
    })
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
) -> Result<CachedView, SnapshotFailure> {
    if !(1..=MAX_PAGE_SIZE).contains(&page_size) {
        return Err(CacheFailure::InvalidPageSize.into());
    }
    check_cancel(cancel, access)?;
    let discovery = tokio::select! {biased;
     ()=cancel.cancelled()=>return Err(SnapshotFailure::Cancelled),
     ()=access.cancelled()=>return Err(SnapshotFailure::Cancelled),
     result=api.home()=>result.map_err(SnapshotFailure::Transport)?,
    };
    if !discovery.__buffa_unknown_fields.is_empty()
        || discovery.homes.len() != 1
        || !discovery.next_page_token.is_empty()
    {
        return Err(CacheFailure::InvalidResponse.into());
    }
    let resource = &discovery.homes[0];
    if !resource.__buffa_unknown_fields.is_empty() {
        return Err(CacheFailure::InvalidResponse.into());
    }
    let home =
        HomeName::try_from(resource.name.as_str()).map_err(|_| CacheFailure::InvalidResponse)?;
    check_cancel(cancel, access)?;
    let request = wire::CreateEntityViewRequest {
        parent: home.as_str().to_owned(),
        entity_view: wire::EntityView {
            filter: filter.expression().to_owned(),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let descriptor = tokio::select! {biased;
     ()=cancel.cancelled()=>return Err(SnapshotFailure::Cancelled),
     ()=access.cancelled()=>return Err(SnapshotFailure::Cancelled),
     result=api.create(request)=>result.map_err(SnapshotFailure::Transport)?,
    };
    // Own cleanup before checking cancellation. Unlike the old pagination-only
    // capture, EntityViews must remain alive throughout watching and resumption.
    let release = api.track(&descriptor.name).map(Arc::new);
    check_cancel(cancel, access)?;
    let mut capture = SnapshotCapture::begin(home, filter, page_size, descriptor, &wall_now()?)?;
    while let Some(request) = capture.next_request() {
        let page = tokio::select! {biased;
         ()=cancel.cancelled()=>return Err(SnapshotFailure::Cancelled),
         ()=access.cancelled()=>return Err(SnapshotFailure::Cancelled),
         result=api.query(request)=>result.map_err(SnapshotFailure::Transport)?,
        };
        check_cancel(cancel, access)?;
        capture.push(page, &wall_now()?)?;
    }
    check_cancel(cancel, access)?;
    let mut complete = capture.finish(&wall_now()?)?;
    complete._cleanup = release;
    Ok(complete)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    struct Api {
        creates: AtomicUsize,
        queries: AtomicUsize,
        home_has_more: bool,
        cancel_query: Option<CancellationToken>,
    }
    impl Api {
        fn new() -> Self {
            Self {
                creates: AtomicUsize::new(0),
                queries: AtomicUsize::new(0),
                home_has_more: false,
                cancel_query: None,
            }
        }
    }
    #[async_trait]
    impl SnapshotApi for Api {
        async fn home(&self) -> Result<wire::ListHomesResponse, Failure> {
            Ok(wire::ListHomesResponse {
                homes: vec![wire::Home {
                    name: "homes/synthetic".into(),
                    ..Default::default()
                }],
                next_page_token: if self.home_has_more {
                    "next".into()
                } else {
                    String::new()
                },
                ..Default::default()
            })
        }
        async fn create(
            &self,
            request: wire::CreateEntityViewRequest,
        ) -> Result<wire::EntityView, Failure> {
            assert_eq!(
                self.creates.fetch_add(1, Ordering::SeqCst),
                0,
                "capture creation replayed"
            );
            assert_eq!(request.parent, "homes/synthetic");
            let now = wall_now().unwrap();
            let mut expiry = now.clone();
            expiry.seconds += 60;
            Ok(wire::EntityView {
                name: "homes/synthetic/entityViews/reader-test".into(),
                filter: request.entity_view.as_option().unwrap().filter.clone(),
                create_time: now.into(),
                expire_time: expiry.into(),
                ..Default::default()
            })
        }
        async fn query(
            &self,
            request: wire::QueryEntityViewRequest,
        ) -> Result<wire::QueryEntityViewResponse, Failure> {
            let ordinal = self.queries.fetch_add(1, Ordering::SeqCst);
            assert_eq!(request.name, "homes/synthetic/entityViews/reader-test");
            assert_eq!(
                request.page_token,
                if ordinal == 0 { "" } else { "page-two" }
            );
            if let Some(cancel) = &self.cancel_query {
                cancel.cancel();
            }
            Ok(wire::QueryEntityViewResponse {
                entries: if ordinal == 0 {
                    vec![wire::EntityViewEntry {
                        entity: wire::Entity {
                            name: "homes/synthetic/entities/relay".into(),
                            display_name: "Relay".into(),
                            etag: "metadata-one".into(),
                            ..Default::default()
                        }
                        .into(),
                        ..Default::default()
                    }]
                } else {
                    Vec::new()
                },
                next_page_token: if ordinal == 0 {
                    "page-two".into()
                } else {
                    String::new()
                },
                resume_token: if ordinal == 0 {
                    String::new()
                } else {
                    "complete-checkpoint".into()
                },
                ..Default::default()
            })
        }
    }
    #[tokio::test]
    async fn capture_is_published_only_after_the_final_checkpoint() {
        let api = Api::new();
        let view = read_from(
            &api,
            Filter::all(),
            1,
            &CancellationToken::new(),
            &CancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(view.len(), 1);
        assert_eq!(view.checkpoint(), "complete-checkpoint");
        assert_eq!(api.creates.load(Ordering::SeqCst), 1);
        assert_eq!(api.queries.load(Ordering::SeqCst), 2);
    }
    #[tokio::test]
    async fn cancellation_after_a_page_never_publishes_or_recreates_the_capture() {
        let cancel = CancellationToken::new();
        let mut api = Api::new();
        api.cancel_query = Some(cancel.clone());
        assert!(matches!(
            read_from(&api, Filter::all(), 1, &cancel, &CancellationToken::new()).await,
            Err(SnapshotFailure::Cancelled)
        ));
        assert_eq!(api.creates.load(Ordering::SeqCst), 1);
        assert_eq!(api.queries.load(Ordering::SeqCst), 1);
    }
    #[tokio::test]
    async fn ambiguous_home_discovery_does_not_choose_an_authority() {
        let mut api = Api::new();
        api.home_has_more = true;
        assert!(matches!(
            read_from(
                &api,
                Filter::all(),
                1,
                &CancellationToken::new(),
                &CancellationToken::new()
            )
            .await,
            Err(SnapshotFailure::Contract(CacheFailure::InvalidResponse))
        ));
        assert_eq!(api.creates.load(Ordering::SeqCst), 0);
        assert_eq!(api.queries.load(Ordering::SeqCst), 0);
    }
}
