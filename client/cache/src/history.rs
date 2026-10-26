//! Bounded, all-or-nothing history reads. Wire values (including decimal money,
//! nanoseconds, logical positions and coverage) remain the source of truth.
//! No provisional page is published, no stale cursor is retried automatically.
use buffa::Message;
use home_client_model::EntityId;
use home_client_session::AccessLease;
use home_client_transport::{Failure, HistoryTransport};
use home_wire_contract::{history_contract as contract, home::history::v1 as wire};
use std::{collections::BTreeSet, time::Duration};
use tokio_util::sync::CancellationToken;

mod reader;
pub use reader::read_history;

pub const MAX_POINTS: usize = 32_768;
pub const MAX_PAGES: usize = 1024;
pub const MAX_ENCODED_BYTES: usize = 32 << 20;
const PAGE_SIZE: u32 = 100;
const READ_TIMEOUT: Duration = Duration::from_secs(45);

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Resolution {
    Observations,
    Hourly,
}
impl Resolution {
    fn wire(self) -> wire::Resolution {
        match self {
            Self::Observations => wire::Resolution::Raw,
            Self::Hourly => wire::Resolution::Hour,
        }
    }
}

#[derive(Clone, Debug, PartialEq)]
pub struct Query {
    entity: EntityId,
    period: wire::Period,
    resolution: Resolution,
}
impl Query {
    /// Uses the delivered server contract: raw <=7 days, hourly <=500 hours
    /// with UTC hour boundaries. A rejected window requires an explicit edit.
    pub fn new(
        entity: EntityId,
        period: wire::Period,
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
    pub fn entity(&self) -> &EntityId {
        &self.entity
    }
    pub fn period(&self) -> &wire::Period {
        &self.period
    }
    pub fn resolution(&self) -> Resolution {
        self.resolution
    }
    fn observations(&self, token: String) -> wire::QueryObservationsRequest {
        wire::QueryObservationsRequest {
            entity_id: self.entity.as_str().into(),
            period: self.period.clone().into(),
            resolution: self.resolution.wire().into(),
            page_size: Some(PAGE_SIZE),
            page_token: token,
            ..Default::default()
        }
    }
    fn aggregates(&self, token: String) -> wire::QueryAggregatesRequest {
        wire::QueryAggregatesRequest {
            entity_id: self.entity.as_str().into(),
            period: self.period.clone().into(),
            resolution: self.resolution.wire().into(),
            page_size: Some(PAGE_SIZE),
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
    ServerBudget(wire::BudgetReason),
}
impl From<ReadFailure> for ReadError {
    fn from(value: ReadFailure) -> Self {
        Self::Contract(value)
    }
}

/// Coverage is retained per page; unknown UTC evidence never becomes a guessed
/// time interval. The renderer must label incompleteness from every page.
pub enum Pages {
    Observations(Vec<wire::ObservationPage>),
    Hourly(Vec<wire::AggregatePage>),
}
pub enum Content {
    Retained(Pages),
    NotRetained(Box<wire::ReadScope>),
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
    Observations(wire::ObservationPage),
    Hourly(wire::AggregatePage),
}
impl Page {
    fn scope(&self) -> &wire::ReadScope {
        match self {
            Self::Observations(p) => &p.scope,
            Self::Hourly(p) => &p.scope,
        }
    }
    fn continuation(&self) -> &wire::Continuation {
        match self {
            Self::Observations(p) => &p.continuation,
            Self::Hourly(p) => &p.continuation,
        }
    }
    fn len(&self) -> usize {
        match self {
            Self::Observations(p) => p.points.len(),
            Self::Hourly(p) => p.points.len(),
        }
    }
}

struct Capture {
    query: Query,
    pages: Pages,
    scope: Option<wire::ReadScope>,
    tokens: BTreeSet<String>,
    epochs: BTreeSet<Vec<u8>>,
    last_position: Option<(Vec<u8>, u64)>,
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
            epochs: BTreeSet::new(),
            last_position: None,
            aggregate_keys: BTreeSet::new(),
            last_bucket: None,
            bytes: 0,
            points: 0,
            count: 0,
        }
    }
    fn admit_scope(&mut self, scope: &wire::ReadScope) -> Result<(), ReadFailure> {
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
                use wire::query_observations_response::Result;
                let page = match response.result {
                    Some(Result::Page(p)) => Page::Observations(*p),
                    Some(Result::NotRetained(scope)) => return self.not_retained(*scope),
                    Some(Result::StaleCursor(_)) => return Err(ReadError::StaleCursor),
                    Some(Result::Budget(b)) => return Err(server_budget(&b)),
                    None => return Err(ReadFailure::InvalidResponse.into()),
                };
                (page, bytes)
            }
            Reply::Hourly(response) => {
                contract::admit_aggregates_response(&response)
                    .map_err(|_| ReadFailure::InvalidResponse)?;
                let bytes = response.compute_size(&mut Default::default()) as usize;
                use wire::query_aggregates_response::Result;
                let page = match response.result {
                    Some(Result::Page(p)) => Page::Hourly(*p),
                    Some(Result::NotRetained(scope)) => return self.not_retained(*scope),
                    Some(Result::StaleCursor(_)) => return Err(ReadError::StaleCursor),
                    Some(Result::Budget(b)) => return Err(server_budget(&b)),
                    None => return Err(ReadFailure::InvalidResponse.into()),
                };
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
        let progress = match &page.continuation().result {
            Some(wire::continuation::Result::End(_)) => Progress::Complete,
            Some(wire::continuation::Result::NextPageToken(token)) => {
                if !self.tokens.insert(token.clone()) {
                    return Err(ReadFailure::DuplicateOrOutOfOrder.into());
                }
                Progress::Next(token.clone())
            }
            None => return Err(ReadFailure::InvalidResponse.into()),
        };
        match (&mut self.pages, page) {
            (Pages::Observations(pages), Page::Observations(page)) => {
                for point in &page.points {
                    use home_wire_contract::home::registry::v1::entity_ref::Reference;
                    if !matches!(&point.state.entity.reference, Some(Reference::EntityId(id)) if id == self.query.entity.as_str())
                        || !within(&self.query.period, &point.state.received_at)
                    {
                        return Err(ReadFailure::InvalidResponse.into());
                    }
                    let position = &point.position;
                    if let Some((epoch, sequence)) = &self.last_position
                        && ((epoch == &position.epoch && *sequence >= position.sequence)
                            || (epoch != &position.epoch && self.epochs.contains(&position.epoch)))
                    {
                        return Err(ReadFailure::DuplicateOrOutOfOrder.into());
                    }
                    self.epochs.insert(position.epoch.clone());
                    self.last_position = Some((position.epoch.clone(), position.sequence));
                }
                pages.push(page);
            }
            (Pages::Hourly(pages), Page::Hourly(page)) => {
                for point in &page.points {
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
                    let mut key = point.bucket.encode_to_vec();
                    let descriptor = point.descriptor.encode_to_vec();
                    key.extend_from_slice(&(descriptor.len() as u32).to_be_bytes());
                    key.extend(descriptor);
                    key.extend(point.quality.encode_to_vec());
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
    fn not_retained(&mut self, scope: wire::ReadScope) -> Result<Progress, ReadError> {
        if self.count != 1 {
            return Err(ReadFailure::ChangedScope.into());
        }
        self.admit_scope(&scope)?;
        if scope.retained.is_set() {
            return Err(ReadFailure::InvalidResponse.into());
        }
        Ok(Progress::NotRetained(Box::new(scope)))
    }
    fn finish(self, content: Option<Box<wire::ReadScope>>) -> Snapshot {
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
fn within(period: &wire::Period, time: &buffa_types::google::protobuf::Timestamp) -> bool {
    let key = (time.seconds, time.nanos);
    (period.start.seconds, period.start.nanos) <= key
        && key < (period.end.seconds, period.end.nanos)
}
fn server_budget(budget: &wire::BudgetExceeded) -> ReadError {
    match budget.reason {
        buffa::EnumValue::Known(reason) => ReadError::ServerBudget(reason),
        buffa::EnumValue::Unknown(_) => ReadFailure::InvalidResponse.into(),
    }
}
enum Progress {
    Next(String),
    Complete,
    NotRetained(Box<wire::ReadScope>),
}

#[cfg(test)]
mod tests;
