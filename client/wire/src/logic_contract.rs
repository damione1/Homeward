//! Provisional synthetic logic representation, never execution or authority.
use crate::home::{
    configuration::v1 as config, configurationplan::v1 as plan, history::v1 as history,
    logic::v1 as wire, observability::v1 as obs,
};
use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::{Empty, Timestamp};
use std::collections::BTreeSet;

pub const MAX_PAGE: usize = 100;
pub const MAX_CAPTURE_ROWS: usize = 512;
pub const MAX_BYTES: u32 = 1_048_576;
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Invalid {
    Representation,
}
fn text(value: &str, max: usize) -> bool {
    !value.is_empty()
        && value.len() <= max
        && value.trim() == value
        && !value.contains(['\0', '\r', '\n'])
}
macro_rules! identity {
    ($name:ident, $max:literal) => {
        #[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
        pub struct $name(String);
        impl $name {
            pub fn parse(value: &str) -> Result<Self, Invalid> {
                if text(value, $max) {
                    Ok(Self(value.into()))
                } else {
                    Err(Invalid::Representation)
                }
            }
            pub fn as_str(&self) -> &str {
                &self.0
            }
        }
    };
}
identity!(DefinitionId, 128);
identity!(RunId, 128);
identity!(RequestId, 128);
identity!(CaptureId, 128);
identity!(PageToken, 512);
macro_rules! known {
    ($name:ident, $ty:path, $($variant:ident),+ $(,)?) => {
        fn $name(value: EnumValue<$ty>) -> bool {
            use $ty as V;
            match value {
                EnumValue::Known($(V::$variant)|+) => true,
                EnumValue::Known(V::Unspecified) | EnumValue::Unknown(_) => false,
            }
        }
    };
}
known!(deployment, wire::Deployment, SyntheticOnly);
known!(
    compatibility,
    wire::Compatibility,
    Supported,
    Partial,
    Unsupported
);
known!(
    diagnostic,
    wire::Diagnostic,
    InvalidSchema,
    ExcludedReference,
    UnsupportedExtension,
    SourceUnavailable
);
known!(mode, wire::RunMode, Single, Restart, Queued, Parallel);
known!(
    status,
    wire::RunStatus,
    Queued,
    Running,
    Completed,
    Canceled,
    Interrupted,
    Failed
);
known!(
    reason,
    wire::RunReason,
    ExplicitRequest,
    SingleBusy,
    RestartReplaced,
    WaitingForSlot,
    ExplicitCancellation,
    ServerRestart,
    SyntheticFailure
);
known!(
    cancellation,
    wire::Cancellation,
    FutureStepsStopped,
    AlreadyTerminal
);
known!(gap_reason, wire::GapReason, Evicted, Redacted, NotRetained);
known!(
    action,
    obs::TraceActionKind,
    Variables,
    Sequence,
    Choose,
    If,
    Repeat,
    Parallel,
    Wait,
    Stop,
    Call,
    Delay,
    WaitTemplate
);
known!(
    stage,
    obs::TraceNodeStage,
    Entered,
    Completed,
    Stopped,
    Cancelled,
    Failed,
    Waiting
);
known!(
    command_outcome,
    history::CommandOutcome,
    Accepted,
    Dispatched,
    Acknowledged,
    Confirmed,
    Indeterminate,
    Refused,
    NotAttempted
);
fn family(value: EnumValue<plan::SourceFamily>) -> bool {
    match value {
        EnumValue::Known(
            plan::SourceFamily::Automation | plan::SourceFamily::Script | plan::SourceFamily::Scene,
        ) => true,
        EnumValue::Known(
            plan::SourceFamily::Unspecified
            | plan::SourceFamily::Operational
            | plan::SourceFamily::DeclarativeV1
            | plan::SourceFamily::DeclarativeV2
            | plan::SourceFamily::Authorization
            | plan::SourceFamily::Ownership
            | plan::SourceFamily::Dashboard,
        )
        | EnumValue::Unknown(_) => false,
    }
}
fn sized(value: &impl Message) -> bool {
    value.compute_size(&mut Default::default()) <= MAX_BYTES
}
fn digest(value: &[u8]) -> bool {
    value.len() == 32 && value.iter().any(|value| *value != 0)
}
fn empty(value: &Empty) -> bool {
    value.__buffa_unknown_fields.is_empty()
}
fn timestamp(value: &Timestamp) -> bool {
    (-62_135_596_800..=253_402_300_799).contains(&value.seconds)
        && (0..1_000_000_000).contains(&value.nanos)
        && value.__buffa_unknown_fields.is_empty()
}
fn generation(value: &config::ActiveGeneration) -> bool {
    value.revision > 0 && digest(&value.manifest_sha256) && value.__buffa_unknown_fields.is_empty()
}
fn reference(value: &wire::DefinitionRef) -> bool {
    DefinitionId::parse(&value.id).is_ok()
        && value.__buffa_unknown_fields.is_empty()
        && value.version.as_option().is_some_and(|version| {
            version.revision > 0
                && digest(&version.source_sha256)
                && version.__buffa_unknown_fields.is_empty()
        })
}
fn context(value: &wire::Context) -> bool {
    deployment(value.deployment)
        && text(&value.epoch, 128)
        && value.catalog_revision > 0
        && value.generation.as_option().is_some_and(generation)
        && value.__buffa_unknown_fields.is_empty()
}
fn definition(value: &wire::Definition) -> bool {
    value.reference.as_option().is_some_and(reference)
        && text(&value.title, 256)
        && family(value.family)
        && mode(value.mode)
        && compatibility(value.compatibility)
        && value.source_document.as_option().is_some_and(|document| {
            text(&document.value, 128) && document.__buffa_unknown_fields.is_empty()
        })
        && (digest(&value.protected_source_reference)
            || (value.protected_source_reference.is_empty()
                && value
                    .diagnostics
                    .contains(&wire::Diagnostic::SourceUnavailable.into())))
        && text(&value.schema_version, 128)
        && value.diagnostics.len() <= 16
        && value.diagnostics.iter().all(|value| diagnostic(*value))
        && value
            .diagnostics
            .iter()
            .map(|value| value.to_i32())
            .collect::<BTreeSet<_>>()
            .len()
            == value.diagnostics.len()
        && (!value.can_run || value.compatibility != wire::Compatibility::Unsupported)
        && value.__buffa_unknown_fields.is_empty()
}
fn actor(value: &wire::Actor) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && match &value.observation {
            Some(wire::actor::Observation::Account(value)) => {
                text(&value.value, 128) && value.__buffa_unknown_fields.is_empty()
            }
            Some(
                wire::actor::Observation::Redacted(value)
                | wire::actor::Observation::NotRecorded(value),
            ) => empty(value),
            None => false,
        }
}
fn run(value: &wire::Run) -> bool {
    RunId::parse(&value.id).is_ok()
        && value.definition.as_option().is_some_and(reference)
        && value.generation.as_option().is_some_and(generation)
        && value.actor.as_option().is_some_and(actor)
        && mode(value.mode)
        && status(value.status)
        && reason(value.reason)
        && value.created_at.as_option().is_some_and(timestamp)
        && RequestId::parse(&value.start_request_id).is_ok()
        && value.simulated
        && !value.hardware_validated
        && (!value.can_cancel
            || matches!(
                value.status,
                EnumValue::Known(wire::RunStatus::Running | wire::RunStatus::Queued)
            ))
        && value.__buffa_unknown_fields.is_empty()
}
fn page(size: u32, token: &Option<String>) -> bool {
    (1..=MAX_PAGE as u32).contains(&size)
        && token
            .as_ref()
            .is_none_or(|value| PageToken::parse(value).is_ok())
}
fn continuation(value: &wire::Continuation, count: usize) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(wire::continuation::Result::NextPageToken(value)) => {
                count > 0 && PageToken::parse(value).is_ok()
            }
            Some(wire::continuation::Result::Complete(value)) => empty(value),
            None => false,
        }
}
fn node(value: &wire::Node) -> bool {
    action(value.kind)
        && stage(value.stage)
        && value.__buffa_unknown_fields.is_empty()
        && match &value.source {
            Some(wire::node::Source::Position(value)) => {
                text(&value.file, 1024)
                    && text(&value.path, 1024)
                    && value.line > 0
                    && value.column > 0
                    && value.__buffa_unknown_fields.is_empty()
            }
            Some(wire::node::Source::Redacted(value) | wire::node::Source::NotRecorded(value)) => {
                empty(value)
            }
            None => false,
        }
}
fn row(value: &wire::TraceRow) -> bool {
    value.ordinal > 0
        && value.at.as_option().is_some_and(timestamp)
        && value.__buffa_unknown_fields.is_empty()
        && match &value.event {
            Some(wire::trace_row::Event::Node(value)) => node(value),
            Some(wire::trace_row::Event::Command(value)) => {
                text(&value.command_id, 128)
                    && command_outcome(value.outcome)
                    && value.__buffa_unknown_fields.is_empty()
            }
            Some(wire::trace_row::Event::Gap(gap)) => {
                gap_reason(gap.reason)
                    && (gap.count_unknown && gap.known_missing_count == 0
                        || !gap.count_unknown && gap.known_missing_count > 0)
                    && (gap.count_unknown
                        || value.ordinal.checked_add(gap.known_missing_count).is_some())
                    && gap.__buffa_unknown_fields.is_empty()
            }
            Some(wire::trace_row::Event::Lifecycle(value)) => reason(*value),
            None => false,
        }
}
/// A known gap occupies its first missing ordinal. Its count determines the
/// next ordinal; an unknown gap permits a jump but never asserts continuity.
pub fn next_ordinal(value: &wire::TraceRow) -> Option<u64> {
    match &value.event {
        Some(wire::trace_row::Event::Gap(value_gap)) => {
            if value_gap.count_unknown {
                None
            } else {
                value.ordinal.checked_add(value_gap.known_missing_count)
            }
        }
        Some(
            wire::trace_row::Event::Node(_)
            | wire::trace_row::Event::Command(_)
            | wire::trace_row::Event::Lifecycle(_),
        ) => value.ordinal.checked_add(1),
        None => None,
    }
}
pub enum Request<'a> {
    Definitions(&'a wire::ListDefinitionsRequest),
    Runs(&'a wire::ListRunsRequest),
    Start(&'a wire::StartRunRequest),
    Get(&'a wire::GetRunRequest),
    Cancel(&'a wire::CancelRunRequest),
    Trace(&'a wire::ReadTraceRequest),
}
pub fn admit_request(value: Request<'_>) -> Result<(), Invalid> {
    let valid = match value {
        Request::Definitions(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && page(value.page_size, &value.page_token)
        }
        Request::Runs(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && value.context.as_option().is_some_and(context)
                && DefinitionId::parse(&value.definition_id).is_ok()
                && page(value.page_size, &value.page_token)
        }
        Request::Start(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && value.expected.as_option().is_some_and(context)
                && value.definition.as_option().is_some_and(reference)
                && RequestId::parse(&value.request_id).is_ok()
        }
        Request::Get(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && value.context.as_option().is_some_and(context)
                && match &value.selector {
                    Some(wire::get_run_request::Selector::RunId(value)) => {
                        RunId::parse(value).is_ok()
                    }
                    Some(wire::get_run_request::Selector::StartRequestId(value)) => {
                        RequestId::parse(value).is_ok()
                    }
                    None => false,
                }
        }
        Request::Cancel(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && value.expected.as_option().is_some_and(context)
                && RunId::parse(&value.run_id).is_ok()
                && RequestId::parse(&value.request_id).is_ok()
        }
        Request::Trace(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && value.context.as_option().is_some_and(context)
                && RunId::parse(&value.run_id).is_ok()
                && page(value.page_size, &value.page_token)
        }
    };
    if valid {
        Ok(())
    } else {
        Err(Invalid::Representation)
    }
}
pub enum Response<'a> {
    Definitions(&'a wire::ListDefinitionsResponse),
    Runs(&'a wire::ListRunsResponse),
    Start(&'a wire::StartRunResponse),
    Get(&'a wire::GetRunResponse),
    Cancel(&'a wire::CancelRunResponse),
    Trace(&'a wire::ReadTraceResponse),
}
pub fn admit_response(value: Response<'_>) -> Result<(), Invalid> {
    let valid = match value {
        Response::Definitions(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && value.context.as_option().is_some_and(context)
                && value.definitions.len() <= MAX_PAGE
                && value.definitions.iter().all(definition)
                && value
                    .definitions
                    .iter()
                    .map(|value| value.reference.id.as_str())
                    .collect::<BTreeSet<_>>()
                    .len()
                    == value.definitions.len()
                && value.retained_count as usize >= value.definitions.len()
                && value
                    .continuation
                    .as_option()
                    .is_some_and(|next| continuation(next, value.definitions.len()))
        }
        Response::Runs(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && value.context.as_option().is_some_and(context)
                && value.runs.len() <= MAX_PAGE
                && value.runs.iter().all(run)
                && value
                    .runs
                    .iter()
                    .map(|value| value.id.as_str())
                    .collect::<BTreeSet<_>>()
                    .len()
                    == value.runs.len()
                && value
                    .continuation
                    .as_option()
                    .is_some_and(|next| continuation(next, value.runs.len()))
        }
        Response::Start(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && value.context.as_option().is_some_and(context)
                && value.run.as_option().is_some_and(run)
        }
        Response::Get(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && value.context.as_option().is_some_and(context)
                && value.run.as_option().is_some_and(run)
        }
        Response::Cancel(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && value.context.as_option().is_some_and(context)
                && value.run.as_option().is_some_and(run)
                && cancellation(value.cancellation)
                && !value.dispatched_effects_reversed
                && match value.cancellation {
                    EnumValue::Known(wire::Cancellation::FutureStepsStopped) => {
                        value.run.status == wire::RunStatus::Canceled
                    }
                    EnumValue::Known(wire::Cancellation::AlreadyTerminal) => matches!(
                        value.run.status,
                        EnumValue::Known(
                            wire::RunStatus::Completed
                                | wire::RunStatus::Canceled
                                | wire::RunStatus::Interrupted
                                | wire::RunStatus::Failed
                        )
                    ),
                    EnumValue::Known(wire::Cancellation::Unspecified) | EnumValue::Unknown(_) => {
                        false
                    }
                }
        }
        Response::Trace(value) => {
            sized(value)
                && value.__buffa_unknown_fields.is_empty()
                && value.context.as_option().is_some_and(context)
                && value.run.as_option().is_some_and(run)
                && CaptureId::parse(&value.capture_id).is_ok()
                && value.rows.len() <= MAX_PAGE
                && value.rows.iter().all(row)
                && value.rows.windows(2).all(|pair| {
                    pair[1].ordinal > pair[0].ordinal
                        && next_ordinal(&pair[0]).is_none_or(|next| next == pair[1].ordinal)
                })
                && value
                    .continuation
                    .as_option()
                    .is_some_and(|next| continuation(next, value.rows.len()))
        }
    };
    if valid {
        Ok(())
    } else {
        Err(Invalid::Representation)
    }
}
