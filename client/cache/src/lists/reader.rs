use super::{CapturedRows, ListCapture, ListReadWatch, ReadFailure, Scope, WatchEffect, wire};
use async_trait::async_trait;
use home_client_session::AccessLease;
use home_client_transport::{Failure, ListTransport, ListWatch};
use std::{
    sync::Arc,
    time::{Duration, SystemTime},
};
use tokio_util::sync::CancellationToken;

/// Read data only. Count is a distinct server result, not inferred from the
/// number of delegates or the number of currently visible UI rows.
pub struct ListReadSnapshot {
    pub rows: CapturedRows,
    pub metadata: ListReadMetadata,
    pub count: u64,
    pub fetched_at: SystemTime,
}

pub enum ListReadMetadata {
    Catalog,
    List(Arc<wire::ListSnapshot>),
}

pub enum ListReadEvent {
    Invalidated,
    Published(Arc<ListReadSnapshot>),
}

/// The owner must clear every private projection on cancellation, resync,
/// contract/authority failure. Only a qualified transient read failure may
/// retain an already published snapshot as dated, non-interactive offline data.
pub enum ListReadExit {
    Cancelled,
    Ended,
    Resync,
    Contract(ReadFailure),
    Transport(Failure),
}

enum Page {
    Catalog(wire::ListListsResponse),
    Items(wire::ListItemsResponse),
}

#[async_trait]
trait ReadApi: Send + Sync {
    async fn count(&self, scope: &Scope) -> Result<u64, ListReadExit>;
    async fn metadata(&self, scope: &Scope) -> Result<Option<wire::ListSnapshot>, ListReadExit>;
    async fn page(&self, scope: &Scope, token: String) -> Result<Page, ListReadExit>;
}
#[async_trait]
trait Stream: Send {
    async fn next(&mut self) -> Result<Option<wire::WatchListsResponse>, Failure>;
}
#[async_trait]
impl Stream for ListWatch {
    async fn next(&mut self) -> Result<Option<wire::WatchListsResponse>, Failure> {
        ListWatch::next(self).await
    }
}

struct NativeApi<'a> {
    transport: &'a ListTransport,
    lease: &'a AccessLease,
}

fn id(value: &home_client_model::lists::ListId) -> wire::ListId {
    wire::ListId {
        value: value.as_str().into(),
        ..Default::default()
    }
}

#[async_trait]
impl ReadApi for NativeApi<'_> {
    async fn metadata(&self, scope: &Scope) -> Result<Option<wire::ListSnapshot>, ListReadExit> {
        match scope {
            Scope::Catalog => Ok(None),
            Scope::Items(list) => {
                let response = self
                    .transport
                    .get_list(
                        wire::GetListRequest {
                            list_id: id(list).into(),
                            ..Default::default()
                        },
                        self.lease.token(),
                        Duration::from_secs(10),
                        &self.lease.cancellation,
                    )
                    .await
                    .map_err(ListReadExit::Transport)?;
                if !response.__buffa_unknown_fields.is_empty() {
                    return Err(ListReadExit::Contract(ReadFailure::InvalidResponse));
                }
                Ok(response.list.as_option().cloned())
            }
        }
    }
    async fn count(&self, scope: &Scope) -> Result<u64, ListReadExit> {
        let timeout = Duration::from_secs(10);
        let (count, clean) = match scope {
            Scope::Catalog => {
                let response = self
                    .transport
                    .count_lists(
                        wire::CountListsRequest::default(),
                        self.lease.token(),
                        timeout,
                        &self.lease.cancellation,
                    )
                    .await
                    .map_err(ListReadExit::Transport)?;
                (response.count, response.__buffa_unknown_fields.is_empty())
            }
            Scope::Items(list) => {
                let response = self
                    .transport
                    .count_items(
                        wire::CountItemsRequest {
                            list_id: id(list).into(),
                            ..Default::default()
                        },
                        self.lease.token(),
                        timeout,
                        &self.lease.cancellation,
                    )
                    .await
                    .map_err(ListReadExit::Transport)?;
                (response.count, response.__buffa_unknown_fields.is_empty())
            }
        };
        if clean {
            Ok(count)
        } else {
            Err(ListReadExit::Contract(ReadFailure::InvalidResponse))
        }
    }

    async fn page(&self, scope: &Scope, token: String) -> Result<Page, ListReadExit> {
        let timeout = Duration::from_secs(10);
        match scope {
            Scope::Catalog => self
                .transport
                .list_lists(
                    wire::ListListsRequest {
                        page_size: super::PAGE_SIZE as u32,
                        page_token: token,
                        ..Default::default()
                    },
                    self.lease.token(),
                    timeout,
                    &self.lease.cancellation,
                )
                .await
                .map(Page::Catalog)
                .map_err(ListReadExit::Transport),
            Scope::Items(list) => self
                .transport
                .list_items(
                    wire::ListItemsRequest {
                        list_id: id(list).into(),
                        page_size: super::PAGE_SIZE as u32,
                        page_token: token,
                        ..Default::default()
                    },
                    self.lease.token(),
                    timeout,
                    &self.lease.cancellation,
                )
                .await
                .map(Page::Items)
                .map_err(ListReadExit::Transport),
        }
    }
}

/// One access lease and one Watch subscription, without a detached frame queue.
/// Ready precedes Count and page RPCs; every queued invalidation is consumed
/// before a ready page result. Reconnect opens a fresh stream and rereads;
/// this protocol has no resumable cursor and this function never sends writes.
pub async fn read_lists_once(
    transport: &ListTransport,
    lease: &AccessLease,
    scope: Scope,
    cancel: &CancellationToken,
    on_event: impl FnMut(ListReadEvent) + Send,
) -> ListReadExit {
    if cancelled(cancel, &lease.cancellation) {
        return ListReadExit::Cancelled;
    }
    let request = wire::WatchListsRequest {
        scope: Some(match &scope {
            Scope::Catalog => wire::watch_lists_request::Scope::Catalog(Default::default()),
            Scope::Items(list) => wire::watch_lists_request::Scope::ListId(id(list).into()),
        }),
        ..Default::default()
    };
    let stream = tokio::select! { biased;
        () = cancel.cancelled() => return ListReadExit::Cancelled,
        () = lease.cancellation.cancelled() => return ListReadExit::Cancelled,
        result = transport.watch_lists(request, lease.token(), Duration::from_secs(60), &lease.cancellation) => result,
    };
    let mut stream = match stream {
        Ok(stream) => stream,
        Err(failure) => return ListReadExit::Transport(failure),
    };
    drive(
        &NativeApi { transport, lease },
        &mut stream,
        scope,
        cancel,
        &lease.cancellation,
        on_event,
    )
    .await
}

fn cancelled(cancel: &CancellationToken, access: &CancellationToken) -> bool {
    cancel.is_cancelled() || access.is_cancelled()
}

async fn receive(
    stream: &mut impl Stream,
    cancel: &CancellationToken,
    access: &CancellationToken,
) -> Result<wire::WatchListsResponse, ListReadExit> {
    let response = tokio::select! { biased;
        () = cancel.cancelled() => return Err(ListReadExit::Cancelled),
        () = access.cancelled() => return Err(ListReadExit::Cancelled),
        response = stream.next() => response,
    };
    if cancelled(cancel, access) {
        return Err(ListReadExit::Cancelled);
    }
    match response {
        Ok(Some(frame)) => Ok(frame),
        Ok(None) => Err(ListReadExit::Ended),
        Err(failure) => Err(ListReadExit::Transport(failure)),
    }
}

async fn capture(
    api: &impl ReadApi,
    scope: &Scope,
    mut capture: ListCapture,
) -> Result<(ListCapture, u64, ListReadMetadata), ListReadExit> {
    let count = api.count(scope).await?;
    // GetList is part of the same Watch-fenced capture as its items. Fetching
    // a newer revision only when submitting would silently rebase a stale edit.
    let metadata = match (scope, api.metadata(scope).await?) {
        (Scope::Catalog, None) => ListReadMetadata::Catalog,
        (Scope::Items(expected), Some(list)) => {
            home_wire_contract::lists_contract::admit_list_snapshot(&list)
                .map_err(|_| ListReadExit::Contract(ReadFailure::InvalidResponse))?;
            if list.id.value != expected.as_str() {
                return Err(ListReadExit::Contract(ReadFailure::WrongScope));
            }
            ListReadMetadata::List(Arc::new(list))
        }
        (Scope::Catalog, Some(_)) | (Scope::Items(_), None) => {
            return Err(ListReadExit::Contract(ReadFailure::InvalidResponse));
        }
    };
    loop {
        match api.page(scope, capture.next_token().to_owned()).await? {
            Page::Catalog(page) => capture.push_lists(page),
            Page::Items(page) => capture.push_items(page),
        }
        .map_err(ListReadExit::Contract)?;
        if capture.next_token().is_empty() {
            return Ok((capture, count, metadata));
        }
    }
}

async fn drive(
    api: &impl ReadApi,
    stream: &mut impl Stream,
    scope: Scope,
    cancel: &CancellationToken,
    access: &CancellationToken,
    mut on_event: impl FnMut(ListReadEvent) + Send,
) -> ListReadExit {
    let mut watch = ListReadWatch::new(scope.clone());
    let first = match receive(stream, cancel, access).await {
        Ok(frame) => frame,
        Err(exit) => return exit,
    };
    match watch.apply(first) {
        Ok(WatchEffect::Reload) => {}
        Ok(WatchEffect::Resubscribe) => return ListReadExit::Resync,
        Err(error) => return ListReadExit::Contract(error),
    }
    'reload: loop {
        let provisional = match watch.begin_capture() {
            Ok(capture) => capture,
            Err(error) => return ListReadExit::Contract(error),
        };
        // This future is dropped on any invalidation, including while Count or
        // the last page is blocked. No provisional prefix reaches the host.
        let read = capture(api, &scope, provisional);
        tokio::pin!(read);
        let (complete, count, metadata) = tokio::select! { biased;
                frame = receive(stream, cancel, access) => {
                    let frame = match frame { Ok(frame) => frame, Err(exit) => return exit };
                    match watch.apply(frame) {
                        Ok(WatchEffect::Reload) => {
                            on_event(ListReadEvent::Invalidated);
                            continue 'reload;
                        }
                        Ok(WatchEffect::Resubscribe) => return ListReadExit::Resync,
                        Err(error) => return ListReadExit::Contract(error),
                    }
                }
                result = &mut read => match result {
                    Ok(capture) => capture,
                    Err(exit) => return exit,
                }
        };
        // The last page may itself make an already pending Watch read ready
        // during the same poll. Check once more before publishing that result.
        let queued = tokio::select! { biased;
            frame = receive(stream, cancel, access) => Some(frame),
            () = std::future::ready(()) => None,
        };
        if let Some(frame) = queued {
            let frame = match frame {
                Ok(frame) => frame,
                Err(exit) => return exit,
            };
            match watch.apply(frame) {
                Ok(WatchEffect::Reload) => {
                    on_event(ListReadEvent::Invalidated);
                    continue 'reload;
                }
                Ok(WatchEffect::Resubscribe) => return ListReadExit::Resync,
                Err(error) => return ListReadExit::Contract(error),
            }
        }
        if cancelled(cancel, access) {
            return ListReadExit::Cancelled;
        }
        let rows = match watch.finish(complete) {
            Ok(rows) => rows,
            Err(error) => return ListReadExit::Contract(error),
        };
        on_event(ListReadEvent::Published(Arc::new(ListReadSnapshot {
            rows,
            metadata,
            count,
            fetched_at: SystemTime::now(),
        })));
        let frame = match receive(stream, cancel, access).await {
            Ok(frame) => frame,
            Err(exit) => return exit,
        };
        match watch.apply(frame) {
            Ok(WatchEffect::Reload) => on_event(ListReadEvent::Invalidated),
            Ok(WatchEffect::Resubscribe) => return ListReadExit::Resync,
            Err(error) => return ListReadExit::Contract(error),
        }
    }
}

#[cfg(test)]
mod tests;
