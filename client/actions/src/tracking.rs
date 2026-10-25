use crate::CommandId;
use home_wire_contract::{actions_contract as admit, home::actions::v1 as wire};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TrackingError {
    InvalidResponse,
    ForeignCommand,
    ForeignContext,
    OutOfOrder,
    AlreadySubmitted,
    NotSubmitted,
    Stopped,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SubmissionState {
    Prepared,
    InFlight,
    Replied,
    Uncertain,
    NotSent,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum LiveState {
    NotRequested,
    Available,
    Unavailable,
    Interrupted,
    Stopped,
}

enum WatchCursor {
    Next(u64),
    Ended,
}

/// The three sources of knowledge deliberately stay separate. In particular a
/// Submit/ledger ACK never becomes confirmation by matching a cached value.
pub struct ActionRecord {
    id: CommandId,
    epoch: String,
    config_revision: u64,
    submission: SubmissionState,
    response: Option<wire::CommandResponse>,
    failure: Option<wire::ActionOperationFailure>,
    durable: Option<wire::DurableActionResult>,
    live: LiveState,
    progress: Option<wire::ActionProgress>,
    watch_cursor: WatchCursor,
}

impl ActionRecord {
    pub fn new(id: CommandId, epoch: &str, config_revision: u64) -> Result<Self, TrackingError> {
        if !admit::admission_epoch(epoch) || config_revision == 0 {
            return Err(TrackingError::ForeignContext);
        }
        Ok(Self {
            id,
            epoch: epoch.to_owned(),
            config_revision,
            submission: SubmissionState::Prepared,
            response: None,
            failure: None,
            durable: None,
            live: LiveState::NotRequested,
            progress: None,
            watch_cursor: WatchCursor::Ended,
        })
    }
    pub fn id(&self) -> &CommandId {
        &self.id
    }
    /// Read-only recovery never creates a fresh dispatch opportunity.
    pub fn recover(
        id: CommandId,
        epoch: &str,
        config_revision: u64,
    ) -> Result<Self, TrackingError> {
        let mut record = Self::new(id, epoch, config_revision)?;
        record.submission = SubmissionState::Uncertain;
        Ok(record)
    }
    pub fn submission(&self) -> SubmissionState {
        self.submission
    }
    pub fn response(&self) -> Option<&wire::CommandResponse> {
        self.response.as_ref()
    }
    pub fn operation_failure(&self) -> Option<&wire::ActionOperationFailure> {
        self.failure.as_ref()
    }
    pub fn durable(&self) -> Option<&wire::DurableActionResult> {
        self.durable.as_ref()
    }
    pub fn live(&self) -> LiveState {
        self.live
    }
    pub fn progress(&self) -> Option<&wire::ActionProgress> {
        self.progress.as_ref()
    }

    /// Consumes the only local dispatch opportunity, before polling the RPC.
    pub fn begin_submission(&mut self) -> Result<(), TrackingError> {
        if self.submission != SubmissionState::Prepared {
            return Err(TrackingError::AlreadySubmitted);
        }
        self.submission = SubmissionState::InFlight;
        Ok(())
    }
    pub fn not_sent(&mut self) -> Result<(), TrackingError> {
        if self.submission != SubmissionState::Prepared {
            return Err(TrackingError::AlreadySubmitted);
        }
        self.submission = SubmissionState::NotSent;
        Ok(())
    }
    fn check_id(&self, response: &wire::CommandResponse) -> Result<(), TrackingError> {
        admit::command_response(response).map_err(|_| TrackingError::InvalidResponse)?;
        if response.command_id == self.id.as_str() {
            Ok(())
        } else {
            Err(TrackingError::ForeignCommand)
        }
    }
    pub fn submitted(&mut self, response: wire::SubmitActionResponse) -> Result<(), TrackingError> {
        if self.submission != SubmissionState::InFlight {
            return Err(TrackingError::AlreadySubmitted);
        }
        if !response.__buffa_unknown_fields.is_empty() {
            return Err(TrackingError::InvalidResponse);
        }
        let result = response
            .result
            .into_option()
            .ok_or(TrackingError::InvalidResponse)?;
        self.check_id(&result)?;
        self.response = Some(result);
        self.submission = SubmissionState::Replied;
        Ok(())
    }
    /// Absence of a typed detail preserves uncertainty too. There is no method
    /// that changes this record back into a fresh submission opportunity.
    pub fn submission_uncertain(
        &mut self,
        detail: Option<wire::ActionOperationFailure>,
    ) -> Result<(), TrackingError> {
        if self.submission != SubmissionState::InFlight {
            return Err(TrackingError::AlreadySubmitted);
        }
        if let Some(detail) = &detail {
            admit::operation_failure(detail).map_err(|_| TrackingError::InvalidResponse)?;
            if let Some(response) = detail.response.as_option() {
                self.check_id(response)?;
            }
        }
        self.response = detail
            .as_ref()
            .and_then(|detail| detail.response.as_option())
            .cloned();
        self.failure = detail;
        self.submission = SubmissionState::Uncertain;
        Ok(())
    }
    fn check_progress(&self, point: &wire::ActionProgress) -> Result<(), TrackingError> {
        admit::progress(point).map_err(|_| TrackingError::InvalidResponse)?;
        if point.admission_epoch != self.epoch || point.config_revision != self.config_revision {
            return Err(TrackingError::ForeignContext);
        }
        if let Some(previous) = &self.progress {
            if point.sequence < previous.sequence
                || (point.sequence == previous.sequence && point != previous)
            {
                return Err(TrackingError::OutOfOrder);
            }
            if terminal(previous.status) && point.status != previous.status {
                return Err(TrackingError::OutOfOrder);
            }
            if point.status == wire::ActionStatus::Accepted
                && previous.status != wire::ActionStatus::Accepted
            {
                return Err(TrackingError::OutOfOrder);
            }
            if point.status == wire::ActionStatus::Dispatched
                && previous.status == wire::ActionStatus::Acknowledged
            {
                return Err(TrackingError::OutOfOrder);
            }
        }
        Ok(())
    }
    pub fn got(&mut self, response: wire::GetActionResponse) -> Result<(), TrackingError> {
        use wire::get_action_response::Live;
        self.readable()?;
        admit::get_response(&response).map_err(|_| TrackingError::InvalidResponse)?;
        let durable = response
            .durable
            .into_option()
            .ok_or(TrackingError::InvalidResponse)?;
        self.check_id(
            durable
                .response
                .as_option()
                .ok_or(TrackingError::InvalidResponse)?,
        )?;
        match &response.live {
            Some(Live::Progress(point)) => self.check_progress(point)?,
            Some(Live::Unavailable(_)) => {}
            None => return Err(TrackingError::InvalidResponse),
        }
        self.durable = Some(durable);
        match response.live {
            Some(Live::Progress(point)) => {
                self.progress = Some(*point);
                self.live = LiveState::Available;
            }
            Some(Live::Unavailable(_)) => {
                self.live = LiveState::Unavailable;
            }
            None => return Err(TrackingError::InvalidResponse),
        }
        Ok(())
    }
    /// A new Watch has no replay cursor. Its stream sequence restarts, while
    /// the action's last admitted point and physical knowledge are retained.
    pub fn begin_watch(&mut self) -> Result<(), TrackingError> {
        self.readable()?;
        self.watch_cursor = WatchCursor::Next(1);
        Ok(())
    }
    pub fn watched(&mut self, response: wire::WatchActionResponse) -> Result<(), TrackingError> {
        use wire::watch_action_response::Frame;
        self.readable()?;
        admit::watch_response(&response).map_err(|_| TrackingError::InvalidResponse)?;
        let expected = match self.watch_cursor {
            WatchCursor::Next(sequence) => sequence,
            WatchCursor::Ended => return Err(TrackingError::OutOfOrder),
        };
        if expected != response.stream_sequence {
            return Err(TrackingError::OutOfOrder);
        }
        match &response.frame {
            Some(Frame::Progress(point)) => self.check_progress(point)?,
            Some(Frame::Unavailable(_)) => {}
            None => return Err(TrackingError::InvalidResponse),
        }
        match response.frame {
            Some(Frame::Progress(point)) => {
                self.progress = Some(*point);
                self.live = LiveState::Available;
                self.watch_cursor = match response.stream_sequence.checked_add(1) {
                    Some(next) => WatchCursor::Next(next),
                    None => WatchCursor::Ended,
                };
            }
            Some(Frame::Unavailable(_)) => {
                self.live = LiveState::Unavailable;
                self.watch_cursor = WatchCursor::Ended;
            }
            None => return Err(TrackingError::InvalidResponse),
        }
        Ok(())
    }
    pub fn interrupted(&mut self) {
        if !matches!(self.live, LiveState::Stopped | LiveState::Unavailable) {
            self.live = LiveState::Interrupted;
        }
        self.watch_cursor = WatchCursor::Ended;
    }
    pub fn stop_tracking(&mut self) {
        self.live = LiveState::Stopped;
        self.watch_cursor = WatchCursor::Ended;
    }
    fn readable(&self) -> Result<(), TrackingError> {
        if self.live == LiveState::Stopped {
            return Err(TrackingError::Stopped);
        }
        match self.submission {
            SubmissionState::Prepared | SubmissionState::NotSent => {
                Err(TrackingError::NotSubmitted)
            }
            SubmissionState::InFlight | SubmissionState::Replied | SubmissionState::Uncertain => {
                Ok(())
            }
        }
    }
    pub fn confirmed(&self) -> bool {
        self.progress
            .as_ref()
            .is_some_and(|point| point.status == wire::ActionStatus::Confirmed)
    }
    pub fn terminal(&self) -> bool {
        self.progress
            .as_ref()
            .is_some_and(|point| terminal(point.status))
    }
}

fn terminal(status: buffa::EnumValue<wire::ActionStatus>) -> bool {
    use buffa::EnumValue;
    use wire::ActionStatus as S;
    match status {
        EnumValue::Known(S::Confirmed | S::Failed | S::Cancelled | S::Indeterminate) => true,
        EnumValue::Known(S::Unspecified | S::Accepted | S::Dispatched | S::Acknowledged)
        | EnumValue::Unknown(_) => false,
    }
}
