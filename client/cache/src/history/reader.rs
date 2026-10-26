use super::*;
use async_trait::async_trait;

#[async_trait]
pub(super) trait Api: Send + Sync {
    async fn page(&self, query: &Query, token: String) -> Result<Reply, Failure>;
}
struct Native<'a> {
    transport: &'a HistoryTransport,
    lease: &'a AccessLease,
}
#[async_trait]
impl Api for Native<'_> {
    async fn page(&self, query: &Query, token: String) -> Result<Reply, Failure> {
        let timeout = Duration::from_secs(3);
        match query.resolution {
            Resolution::Observations => self
                .transport
                .query_observations(
                    query.observations(token),
                    self.lease.token(),
                    timeout,
                    &self.lease.cancellation,
                )
                .await
                .map(Reply::Observations),
            Resolution::Hourly => self
                .transport
                .query_aggregates(
                    query.aggregates(token),
                    self.lease.token(),
                    timeout,
                    &self.lease.cancellation,
                )
                .await
                .map(Reply::Hourly),
        }
    }
}

/// One bounded attempt. Cancelling the view or access lease interrupts the RPC
/// and drops every provisional page. The owner must also compare its generation
/// immediately before publishing the returned immutable snapshot.
pub async fn read_history(
    transport: &HistoryTransport,
    lease: &AccessLease,
    query: Query,
    cancel: &CancellationToken,
) -> Result<Snapshot, ReadError> {
    read_from(
        &Native { transport, lease },
        query,
        cancel,
        &lease.cancellation,
    )
    .await
}
pub(super) async fn read_from(
    api: &impl Api,
    query: Query,
    cancel: &CancellationToken,
    access: &CancellationToken,
) -> Result<Snapshot, ReadError> {
    tokio::select! { biased;
        () = cancel.cancelled() => Err(ReadError::Cancelled),
        () = access.cancelled() => Err(ReadError::Cancelled),
        () = tokio::time::sleep(READ_TIMEOUT) => Err(ReadError::Deadline),
        result = collect(api, query, cancel, access) => result,
    }
}
async fn collect(
    api: &impl Api,
    query: Query,
    cancel: &CancellationToken,
    access: &CancellationToken,
) -> Result<Snapshot, ReadError> {
    let mut capture = Capture::new(query);
    let mut token = String::new();
    loop {
        if cancel.is_cancelled() || access.is_cancelled() {
            return Err(ReadError::Cancelled);
        }
        let reply = api
            .page(&capture.query, token)
            .await
            .map_err(ReadError::Transport)?;
        if cancel.is_cancelled() || access.is_cancelled() {
            return Err(ReadError::Cancelled);
        }
        match capture.push(reply)? {
            Progress::Next(next) => {
                if capture.count >= MAX_PAGES {
                    return Err(ReadFailure::BudgetExceeded.into());
                }
                token = next;
            }
            Progress::Complete => return Ok(capture.finish(None)),
            Progress::NotRetained(scope) => return Ok(capture.finish(Some(scope))),
        }
    }
}
