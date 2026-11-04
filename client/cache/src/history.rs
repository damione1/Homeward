//! Bounded, all-or-nothing history reads. Wire values (including decimal money,
//! nanoseconds, logical positions and coverage) remain the source of truth.
//! No provisional page is published, no stale cursor is retried automatically.
use buffa::Message;
use home_client_model::api::EntityName;
use home_client_session::AccessLease;
use home_client_transport::{Failure, HistoryTransport};
use home_wire_contract::{history_contract as contract, home::api::v2 as wire};
use std::{collections::BTreeSet, time::Duration};
use tokio_util::sync::CancellationToken;

mod reader;
pub use reader::read_history;

pub const MAX_POINTS: usize = 32_768;
pub const MAX_PAGES: usize = 1024;
pub const MAX_ENCODED_BYTES: usize = 32 << 20;
const PAGE_SIZE: i32 = 100;
const READ_TIMEOUT: Duration = Duration::from_secs(45);

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Resolution {
    Observations,
    Hourly,
}
impl Resolution {
    fn wire(self) -> wire::HistoryResolution {
        match self {
            Self::Observations => wire::HistoryResolution::Raw,
            Self::Hourly => wire::HistoryResolution::Hour,
        }
    }
}

#[derive(Clone, Debug, PartialEq)]
pub struct Query {
    entity: EntityName,
    period: wire::HistoryPeriod,
    resolution: Resolution,
}
impl Query {
    /// Uses the delivered server contract: raw <=7 days, hourly <=500 hours
    /// with UTC hour boundaries. A rejected window requires an explicit edit.
    pub fn new(
        entity: EntityName,
        period: wire::HistoryPeriod,
        resolution: Resolution,
    ) -> Result<Self, ReadFailure> {
        let query = Self {
            entity,
            period,
            resolution,
        };
        let valid = match resolution {
            Resolution::Observations => {
                contract::admit_observations_request(&query.observations(String::new())).is_ok()
            }
            Resolution::Hourly => {
                contract::admit_aggregates_request(&query.aggregates(String::new())).is_ok()
            }
        };
        if !valid {
            return Err(ReadFailure::InvalidQuery);
        }
        Ok(query)
    }
    pub fn entity(&self) -> &EntityName {
        &self.entity
    }
    pub fn period(&self) -> &wire::HistoryPeriod {
        &self.period
    }
    pub fn resolution(&self) -> Resolution {
        self.resolution
    }
    fn observations(&self, token: String) -> wire::QueryObservationsRequest {
        wire::QueryObservationsRequest {
            parent: self.entity.as_str().into(),
            period: self.period.clone().into(),
            page_size: PAGE_SIZE,
            page_token: token,
            ..Default::default()
        }
    }
    fn aggregates(&self, token: String) -> wire::QueryAggregatesRequest {
        wire::QueryAggregatesRequest {
            parent: self.entity.as_str().into(),
            period: self.period.clone().into(),
            page_size: PAGE_SIZE,
            page_token: token,
            ..Default::default()
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ReadFailure {
    InvalidQuery,
    InvalidResponse,
    ChangedScope,
    DuplicateOrOutOfOrder,
    BudgetExceeded,
}
#[derive(Debug)]
pub enum ReadError {
    Contract(ReadFailure),
    Transport(Failure),
    Cancelled,
    Deadline,
    StaleCursor,
    ServerBudget,
}
impl From<ReadFailure> for ReadError {
    fn from(value: ReadFailure) -> Self {
        Self::Contract(value)
    }
}

/// Coverage is retained per page; unknown UTC evidence never becomes a guessed
/// time interval. The renderer must label incompleteness from every page.
pub enum Pages {
    Observations(Vec<wire::QueryObservationsResponse>),
    Hourly(Vec<wire::QueryAggregatesResponse>),
}
pub enum Content {
    Retained(Pages),
    NotRetained(Box<wire::HistoryReadScope>),
}
pub struct Snapshot {
    pub query: Query,
    pub content: Content,
    pub encoded_bytes: usize,
    pub points: usize,
}

enum Reply {
    Observations(wire::QueryObservationsResponse),
    Hourly(wire::QueryAggregatesResponse),
}
enum Page {
    Observations(wire::QueryObservationsResponse),
    Hourly(wire::QueryAggregatesResponse),
}
impl Page {
    fn scope(&self) -> &wire::HistoryReadScope {
        match self {
            Self::Observations(p) => &p.scope,
            Self::Hourly(p) => &p.scope,
        }
    }
    fn next_token(&self) -> &str {
        match self {
            Self::Observations(p) => &p.next_page_token,
            Self::Hourly(p) => &p.next_page_token,
        }
    }
    fn len(&self) -> usize {
        match self {
            Self::Observations(p) => p.observations.len(),
            Self::Hourly(p) => p.aggregates.len(),
        }
    }
}

struct Capture {
    query: Query,
    pages: Pages,
    scope: Option<wire::HistoryReadScope>,
    tokens: BTreeSet<String>,
    observation_ids: BTreeSet<String>,
    aggregate_keys: BTreeSet<Vec<u8>>,
    last_bucket: Option<(i64, i32)>,
    bytes: usize,
    points: usize,
    count: usize,
}
impl Capture {
    fn new(query: Query) -> Self {
        let pages = match query.resolution {
            Resolution::Observations => Pages::Observations(Vec::new()),
            Resolution::Hourly => Pages::Hourly(Vec::new()),
        };
        Self {
            query,
            pages,
            scope: None,
            tokens: BTreeSet::new(),
            observation_ids: BTreeSet::new(),
            aggregate_keys: BTreeSet::new(),
            last_bucket: None,
            bytes: 0,
            points: 0,
            count: 0,
        }
    }
    fn admit_scope(&mut self, scope: &wire::HistoryReadScope) -> Result<(), ReadFailure> {
        let hours = match self.query.resolution {
            Resolution::Observations => 168,
            Resolution::Hourly => 500,
        };
        if scope.requested.as_option() != Some(&self.query.period)
            || scope.resolution != self.query.resolution.wire()
            || scope.limits.max_points != PAGE_SIZE
            || scope.limits.max_period_hours != hours
        {
            return Err(ReadFailure::ChangedScope);
        }
        if let Some(expected) = &self.scope {
            if scope != expected {
                return Err(ReadFailure::ChangedScope);
            }
        } else {
            self.scope = Some(scope.clone());
        }
        Ok(())
    }
    fn push(&mut self, reply: Reply) -> Result<Progress, ReadError> {
        self.count += 1;
        if self.count > MAX_PAGES {
            return Err(ReadFailure::BudgetExceeded.into());
        }
        let (page, bytes) = match reply {
            Reply::Observations(response) => {
                contract::admit_observations_response(&response)
                    .map_err(|_| ReadFailure::InvalidResponse)?;
                let bytes = response.compute_size(&mut Default::default()) as usize;
                if !response.scope.retained.is_set() {
                    return self.not_retained(
                        response
                            .scope
                            .into_option()
                            .ok_or(ReadFailure::InvalidResponse)?,
                    );
                }
                let page = Page::Observations(response);
                (page, bytes)
            }
            Reply::Hourly(response) => {
                contract::admit_aggregates_response(&response)
                    .map_err(|_| ReadFailure::InvalidResponse)?;
                let bytes = response.compute_size(&mut Default::default()) as usize;
                if !response.scope.retained.is_set() {
                    return self.not_retained(
                        response
                            .scope
                            .into_option()
                            .ok_or(ReadFailure::InvalidResponse)?,
                    );
                }
                let page = Page::Hourly(response);
                (page, bytes)
            }
        };
        self.admit_scope(page.scope())?;
        self.bytes = self
            .bytes
            .checked_add(bytes)
            .ok_or(ReadFailure::BudgetExceeded)?;
        self.points = self
            .points
            .checked_add(page.len())
            .ok_or(ReadFailure::BudgetExceeded)?;
        if self.bytes > MAX_ENCODED_BYTES || self.points > MAX_POINTS {
            return Err(ReadFailure::BudgetExceeded.into());
        }
        let progress = if page.next_token().is_empty() {
            Progress::Complete
        } else {
            let token = page.next_token().to_owned();
            if !self.tokens.insert(token.clone()) {
                return Err(ReadFailure::DuplicateOrOutOfOrder.into());
            }
            Progress::Next(token)
        };
        match (&mut self.pages, page) {
            (Pages::Observations(pages), Page::Observations(page)) => {
                for point in &page.observations {
                    if !within(&self.query.period, &point.state.receive_time) {
                        return Err(ReadFailure::InvalidResponse.into());
                    }
                    if !self.observation_ids.insert(point.observation_id.clone()) {
                        return Err(ReadFailure::DuplicateOrOutOfOrder.into());
                    }
                }
                pages.push(page);
            }
            (Pages::Hourly(pages), Page::Hourly(page)) => {
                for point in &page.aggregates {
                    let at = (point.bucket.start.seconds, point.bucket.start.nanos);
                    if self.last_bucket.is_some_and(|last| at < last)
                        || !within(&self.query.period, &point.bucket.start)
                        || (point.bucket.end.seconds, point.bucket.end.nanos)
                            > (self.query.period.end.seconds, self.query.period.end.nanos)
                    {
                        return Err(ReadFailure::DuplicateOrOutOfOrder.into());
                    }
                    // Several descriptor/quality groups in the same hour are
                    // distinct points, not duplicates or values to average.
                    let key = point.aggregate_id.as_bytes().to_vec();
                    if !self.aggregate_keys.insert(key) {
                        return Err(ReadFailure::DuplicateOrOutOfOrder.into());
                    }
                    self.last_bucket = Some(at);
                }
                pages.push(page);
            }
            (Pages::Observations(_), Page::Hourly(_))
            | (Pages::Hourly(_), Page::Observations(_)) => {
                return Err(ReadFailure::InvalidResponse.into());
            }
        }
        Ok(progress)
    }
    fn not_retained(&mut self, scope: wire::HistoryReadScope) -> Result<Progress, ReadError> {
        if self.count != 1 {
            return Err(ReadFailure::ChangedScope.into());
        }
        self.admit_scope(&scope)?;
        if scope.retained.is_set() {
            return Err(ReadFailure::InvalidResponse.into());
        }
        Ok(Progress::NotRetained(Box::new(scope)))
    }
    fn finish(self, content: Option<Box<wire::HistoryReadScope>>) -> Snapshot {
        Snapshot {
            query: self.query,
            content: match content {
                Some(scope) => Content::NotRetained(scope),
                None => Content::Retained(self.pages),
            },
            encoded_bytes: self.bytes,
            points: self.points,
        }
    }
}
fn within(period: &wire::HistoryPeriod, time: &buffa_types::google::protobuf::Timestamp) -> bool {
    let key = (time.seconds, time.nanos);
    (period.start.seconds, period.start.nanos) <= key
        && key < (period.end.seconds, period.end.nanos)
}
fn transport_error(failure: Failure) -> ReadError {
    match failure.api_failure() {
        Ok(Some(api)) if api.reason() == wire::ApiErrorReason::ResyncRequired => {
            ReadError::StaleCursor
        }
        Ok(Some(api)) if api.reason() == wire::ApiErrorReason::QueryBudgetExceeded => {
            ReadError::ServerBudget
        }
        Err(_) => ReadFailure::InvalidResponse.into(),
        Ok(Some(_)) | Ok(None) => ReadError::Transport(failure),
    }
}
enum Progress {
    Next(String),
    Complete,
    NotRetained(Box<wire::HistoryReadScope>),
}

#[cfg(test)]
mod tests;
