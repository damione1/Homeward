use super::{CapturedRows, ListCapture, ListReadWatch, ReadFailure, Scope, WatchEffect, wire};
use async_trait::async_trait;
use home_client_session::AccessLease;
use home_client_transport::{Failure, TaskTransport, TaskWatch};
use std::{
    sync::Arc,
    time::{Duration, SystemTime},
};
use tokio_util::sync::CancellationToken;

/// Read data only. Count is the server total_size, not inferred from the
/// number of delegates or the number of currently visible UI rows.
pub struct ListReadSnapshot {
    pub home: home_client_model::api::HomeName,
    pub rows: CapturedRows,
    pub metadata: ListReadMetadata,
    pub count: u64,
    pub fetched_at: SystemTime,
}

pub enum ListReadMetadata {
    Catalog,
    List(Arc<home_client_model::api_tasks::TaskListResource>),
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
    Catalog(wire::ListTaskListsResponse),
    Items(wire::ListTasksResponse),
}

#[async_trait]
trait ReadApi: Send + Sync {
    async fn metadata(
        &self,
        scope: &Scope,
    ) -> Result<Option<home_client_model::api_tasks::TaskListResource>, ListReadExit>;
    async fn page(&self, scope: &Scope, token: String) -> Result<Page, ListReadExit>;
}
#[async_trait]
trait Stream: Send {
    async fn next(&mut self) -> Result<Option<wire::WatchTaskListsResponse>, Failure>;
}
#[async_trait]
impl Stream for TaskWatch {
    async fn next(&mut self) -> Result<Option<wire::WatchTaskListsResponse>, Failure> {
        TaskWatch::next(self).await
    }
}

struct NativeApi<'a> {
    transport: &'a TaskTransport,
    lease: &'a AccessLease,
    home: home_client_model::api::HomeName,
}

#[async_trait]
impl ReadApi for NativeApi<'_> {
    async fn metadata(
        &self,
        scope: &Scope,
    ) -> Result<Option<home_client_model::api_tasks::TaskListResource>, ListReadExit> {
        let Scope::Items(id) = scope else {
            return Ok(None);
        };
        let name = home_client_model::api_tasks::TaskListName::new(self.home.clone(), id.clone());
        let response = self
            .transport
            .get_task_list(
                wire::GetTaskListRequest {
                    name: name.as_str().to_owned(),
                    ..Default::default()
                },
                self.lease.token(),
                Duration::from_secs(10),
                &self.lease.cancellation,
            )
            .await
            .map_err(ListReadExit::Transport)?;
        let resource = home_wire_contract::tasks_contract::parse_task_list(&response)
            .map_err(|_| ListReadExit::Contract(ReadFailure::InvalidResponse))?;
        if resource.name() != &name {
            return Err(ListReadExit::Contract(ReadFailure::WrongScope));
        }
        Ok(Some(resource))
    }
    async fn page(&self, scope: &Scope, token: String) -> Result<Page, ListReadExit> {
        let timeout = Duration::from_secs(10);
        match scope {
            Scope::Catalog => self
                .transport
                .list_task_lists(
                    wire::ListTaskListsRequest {
                        parent: self.home.as_str().to_owned(),
                        page_size: super::PAGE_SIZE as i32,
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
                .list_tasks(
                    wire::ListTasksRequest {
                        parent: home_client_model::api_tasks::TaskListName::new(
                            self.home.clone(),
                            list.clone(),
                        )
                        .as_str()
                        .to_owned(),
                        page_size: super::PAGE_SIZE as i32,
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
/// Ready precedes metadata and page RPCs; every queued invalidation is consumed
/// before a ready page result. Reconnect opens a fresh stream and rereads;
/// this protocol has no resumable cursor and this function never sends writes.
pub async fn read_lists_once(
    transport: &TaskTransport,
    lease: &AccessLease,
    scope: Scope,
    cancel: &CancellationToken,
    on_event: impl FnMut(ListReadEvent) + Send,
) -> ListReadExit {
    if cancelled(cancel, &lease.cancellation) {
        return ListReadExit::Cancelled;
    }
    let discovery = tokio::select! { biased;
        ()=cancel.cancelled()=>return ListReadExit::Cancelled,
        ()=lease.cancellation.cancelled()=>return ListReadExit::Cancelled,
        result=transport.home().list_homes(wire::ListHomesRequest{page_size:2,..Default::default()},lease.token(),Duration::from_secs(10),&lease.cancellation)=>result,
    };
    let homes = match discovery {
        Ok(value) => value,
        Err(error) => return ListReadExit::Transport(error),
    };
    if !homes.__buffa_unknown_fields.is_empty()
        || !homes.next_page_token.is_empty()
        || homes.homes.len() != 1
    {
        return ListReadExit::Contract(ReadFailure::WrongScope);
    }
    let resource = &homes.homes[0];
    if !resource.__buffa_unknown_fields.is_empty() {
        return ListReadExit::Contract(ReadFailure::InvalidResponse);
    }
    let home = match home_client_model::api::HomeName::try_from(resource.name.as_str()) {
        Ok(home) => home,
        Err(_) => return ListReadExit::Contract(ReadFailure::InvalidResponse),
    };
    let request = wire::WatchTaskListsRequest {
        parent: home.as_str().to_owned(),
        task_list: match &scope {
            Scope::Catalog => None,
            Scope::Items(list) => Some(
                home_client_model::api_tasks::TaskListName::new(home.clone(), list.clone())
                    .as_str()
                    .to_owned(),
            ),
        },
        ..Default::default()
    };
    let stream = tokio::select! { biased;
        () = cancel.cancelled() => return ListReadExit::Cancelled,
        () = lease.cancellation.cancelled() => return ListReadExit::Cancelled,
        result = transport.watch_task_lists(request, lease.token(), Duration::from_secs(60), &lease.cancellation) => result,
    };
    let mut stream = match stream {
        Ok(stream) => stream,
        Err(failure) => return ListReadExit::Transport(failure),
    };
    drive(
        &NativeApi {
            transport,
            lease,
            home: home.clone(),
        },
        &mut stream,
        scope,
        home,
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
) -> Result<wire::WatchTaskListsResponse, ListReadExit> {
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
    home: &home_client_model::api::HomeName,
    mut capture: ListCapture,
) -> Result<(ListCapture, u64, ListReadMetadata), ListReadExit> {
    // GetTaskList is part of the same Watch-fenced capture as its items. Fetching
    // a newer revision only when submitting would silently rebase a stale edit.
    let metadata = match (scope, api.metadata(scope).await?) {
        (Scope::Catalog, None) => ListReadMetadata::Catalog,
        (Scope::Items(expected), Some(list)) => {
            if list.name().home() != home || list.native_id() != expected {
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
            let count = capture.total().map_err(ListReadExit::Contract)?;
            return Ok((capture, count, metadata));
        }
    }
}

async fn drive(
    api: &impl ReadApi,
    stream: &mut impl Stream,
    scope: Scope,
    home: home_client_model::api::HomeName,
    cancel: &CancellationToken,
    access: &CancellationToken,
    mut on_event: impl FnMut(ListReadEvent) + Send,
) -> ListReadExit {
    let mut watch = ListReadWatch::new(scope.clone(), home.clone());
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
        // This future is dropped on any invalidation, including while metadata or
        // the last page is blocked. No provisional prefix reaches the host.
        let read = capture(api, &scope, &home, provisional);
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
            home: home.clone(),
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
