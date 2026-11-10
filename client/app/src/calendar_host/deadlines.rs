//! The reader wakes for a real presentation/transport deadline, never an idle
//! repaint interval. Wall-time inputs are resampled on every backend/UI/session
//! event and on foreground re-admission; write jobs are not timer retries.
use super::{State, Status};
use home_wire_contract::{calendars_contract as admit, home::api::v2 as wire};
use std::time::{Duration, SystemTime};
use tokio::time::Instant;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Kind {
    Expiry,
    CivilDate,
    WidgetWindow,
    ReadRetry,
}
#[derive(Clone)]
pub(super) struct Deadline {
    pub at: Instant,
    pub kind: Kind,
    pub snapshot: Option<std::sync::Arc<home_client_cache::calendars::CalendarReadSnapshot>>,
    pub context: u64,
}
#[derive(Default)]
pub(super) struct Scheduler {
    retry: Option<(u64, Instant)>,
    expiry: Option<Deadline>,
}
impl Scheduler {
    pub fn next(
        &mut self,
        state: &State,
        widget: bool,
        wall: SystemTime,
        monotonic: Instant,
    ) -> Option<Deadline> {
        if !state.active {
            self.retry = None;
            self.expiry = None;
            return None;
        }
        let mut deadlines = Vec::with_capacity(4);
        let mut wall_deadline = |at: SystemTime, kind| {
            deadlines.push(Deadline {
                at: monotonic + at.duration_since(wall).unwrap_or_default(),
                kind,
                snapshot: state.snapshot.clone(),
                context: state.context,
            });
        };
        if state.fresh_at(wall)
            && let Some(snapshot) = &state.snapshot
            && let Some(at) = snapshot
                .expiry_bounds()
                .filter_map(|value| chrono::DateTime::parse_from_rfc3339(value).ok())
                .map(|at| SystemTime::from(at.with_timezone(&chrono::Utc)))
                .min()
        {
            wall_deadline(at, Kind::Expiry);
        }
        if widget {
            if let Some(at) = widget_refresh_at(state) {
                wall_deadline(at, Kind::WidgetWindow);
            }
        } else if let Ok(zone) = state
            .zone
            .parse::<home_client_model::local_time::DisplayTimeZone>()
        {
            let now: chrono::DateTime<chrono::Utc> = wall.into();
            if let Some(at) = home_client_model::local_time::UtcInstant::new(
                now.timestamp(),
                now.timestamp_subsec_nanos() as i32,
            )
            .ok()
            .and_then(|now| zone.next_date_change(now))
            .and_then(|at| {
                chrono::DateTime::<chrono::Utc>::from_timestamp(at.seconds(), at.nanos() as u32)
            }) {
                wall_deadline(at.into(), Kind::CivilDate);
            }
        }
        if state.status == Status::Offline && state.read_retry && !state.busy() {
            self.retry
                .get_or_insert((state.context, monotonic + Duration::from_secs(2)));
            if self
                .retry
                .is_some_and(|(context, _)| context != state.context)
            {
                self.retry = Some((state.context, monotonic + Duration::from_secs(2)));
            }
            deadlines.push(Deadline {
                at: self.retry.expect("offline retry").1,
                kind: Kind::ReadRetry,
                snapshot: None,
                context: state.context,
            });
        } else {
            self.retry = None;
        }
        // Keep the earliest monotonic bound of this exact admitted snapshot.
        // A backward wall-clock jump followed by a UI/backend wake must not
        // postpone expiration; a distinct admission starts its own bound.
        if let Some(expiry) = deadlines
            .iter_mut()
            .find(|deadline| deadline.kind == Kind::Expiry)
        {
            if let Some(previous) = &self.expiry
                && previous.context == expiry.context
                && previous
                    .snapshot
                    .as_ref()
                    .zip(expiry.snapshot.as_ref())
                    .is_some_and(|(old, new)| std::sync::Arc::ptr_eq(old, new))
            {
                expiry.at = expiry.at.min(previous.at);
            }
            self.expiry = Some(expiry.clone());
        } else {
            self.expiry = None;
        }
        deadlines.into_iter().min_by_key(|deadline| deadline.at)
    }
    pub fn fired(&mut self, kind: Kind) {
        if kind == Kind::ReadRetry {
            self.retry = None;
        }
    }
}
fn widget_refresh_at(state: &State) -> Option<SystemTime> {
    let query = state.query_override.as_ref()?;
    let window = query.window.as_option()?;
    let Some(wire::calendar_window::Bounds::Times(span)) = &window.bounds else {
        return None;
    };
    let value = admit::timestamp_text(&span.start_time).ok()?;
    let start = chrono::DateTime::parse_from_rfc3339(&value)
        .ok()?
        .with_timezone(&chrono::Utc);
    Some((start + chrono::Duration::seconds(60)).into())
}

pub(super) async fn wait(deadline: Option<&Deadline>) {
    match deadline {
        Some(deadline) => tokio::time::sleep_until(deadline.at).await,
        None => std::future::pending::<()>().await,
    }
}
