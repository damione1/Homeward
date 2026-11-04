//! Closed native recorder evidence shared by History and Observability v2.
//! Positions attest ordering only; unknown UTC is never inferred from a gap.
use crate::configuration_contract::timestamp;
use crate::home::api::v2 as obs;
use buffa::EnumValue;
use home_client_model::decimal_u64::DecimalU64;
pub(crate) fn decimal(value: &str) -> bool {
    DecimalU64::try_from(value).is_ok()
}
// Callers first admit every decimal field before comparing native numeric values.
pub(crate) fn number(value: &str) -> u64 {
    DecimalU64::try_from(value)
        .map(DecimalU64::value)
        .unwrap_or(0)
}

use buffa_types::google::protobuf::Empty;
fn identity(value: &[u8], size: usize) -> bool {
    value.len() == size && value.iter().any(|v| *v != 0)
}
fn empty(value: &Empty) -> bool {
    value.__buffa_unknown_fields.is_empty()
}
fn nonzero<E: Copy + PartialEq + buffa::Enumeration>(value: EnumValue<E>, zero: E) -> bool {
    match value {
        EnumValue::Known(value) => value != zero,
        EnumValue::Unknown(_) => false,
    }
}
pub fn position(value: &obs::AdmissionPosition) -> bool {
    identity(&value.epoch, 16)
        && decimal(&value.sequence)
        && value.__buffa_unknown_fields.is_empty()
}
fn recorder_epoch(value: &obs::RecorderEpochIdentity) -> bool {
    identity(&value.value, 16) && value.__buffa_unknown_fields.is_empty()
}
fn known_lost(value: &obs::KnownLost) -> bool {
    use obs::known_lost::Result;
    value.__buffa_unknown_fields.is_empty()
        && match &value.result {
            Some(Result::LowerBound(value)) => *value > 0,
            Some(Result::NotKnown(value)) => empty(value),
            None => false,
        }
}
fn coverage_epoch(value: &obs::RecorderEpochCoverage) -> bool {
    use obs::recorder_epoch_coverage::{CloseTime, Predecessor};
    let closed = match &value.close_time {
        Some(CloseTime::ClosedAt(value)) => timestamp(value),
        Some(CloseTime::NotRecorded(none)) => !value.clean_close && empty(none),
        None => false,
    };
    let previous = match &value.predecessor {
        Some(Predecessor::Previous(value)) => {
            value.epoch.as_option().is_some_and(recorder_epoch)
                && decimal(&value.checkpoint)
                && value.__buffa_unknown_fields.is_empty()
        }
        Some(Predecessor::NoPrevious(value)) => empty(value),
        None => false,
    };
    decimal(&value.checkpoint)
        && decimal(&value.policy_excluded)
        && value.epoch.as_option().is_some_and(recorder_epoch)
        && value.started_at.as_option().is_some_and(timestamp)
        && nonzero(
            value.reconciliation,
            obs::RecorderReconciliationState::Unspecified,
        )
        && closed
        && previous
        && value.__buffa_unknown_fields.is_empty()
}
fn coverage_gap(value: &obs::RecorderCoverageGap) -> bool {
    use obs::{known_lost::Result, recorder_coverage_gap::UtcBounds};
    if !decimal(&value.from_sequence) || !decimal(&value.through_sequence) {
        return false;
    }
    let from = number(&value.from_sequence);
    let through = number(&value.through_sequence);
    if from == 0 || through < from {
        return false;
    }
    let width = through - from + 1;
    let count = value.known_lost.as_option().is_some_and(|count| {
        known_lost(count)
            && match &count.result {
                Some(Result::LowerBound(count)) => u64::try_from(*count).is_ok_and(|count| {
                    count <= width
                        && (value.envelope != obs::GapEnvelope::ExactPositions
                            || count == width.min(i64::MAX as u64))
                }),
                Some(Result::NotKnown(_)) => value.envelope != obs::GapEnvelope::ExactPositions,
                None => false,
            }
    });
    value.epoch.as_option().is_some_and(recorder_epoch)
        && nonzero(value.reason, obs::GapReason::Unspecified)
        && nonzero(value.envelope, obs::GapEnvelope::Unspecified)
        && count
        && value.__buffa_unknown_fields.is_empty()
        && match &value.utc_bounds {
            Some(UtcBounds::UnknownUtc(value)) => empty(value),
            None => false,
        }
}

pub fn coverage(value: &obs::RecorderCoverageSnapshot) -> bool {
    if value.epochs.is_empty() || value.epochs.len() > 64 || value.gaps.len() > 512 {
        return false;
    }
    value.from.as_option().is_some_and(position)
        && value.through.as_option().is_some_and(position)
        && value.from.epoch == value.epochs[0].epoch.value
        && value.through.epoch == value.epochs[value.epochs.len() - 1].epoch.value
        && number(&value.from.sequence) <= number(&value.epochs[0].checkpoint)
        && number(&value.through.sequence)
            <= number(&value.epochs[value.epochs.len() - 1].checkpoint)
        && (value.from.epoch != value.through.epoch
            || number(&value.from.sequence) <= number(&value.through.sequence))
        && value.epochs.iter().enumerate().all(|(i, epoch)| {
            coverage_epoch(epoch)
                && value.epochs[..i]
                    .iter()
                    .all(|previous| previous.epoch.value != epoch.epoch.value)
        })
        && value.gaps.iter().all(|gap| {
            coverage_gap(gap)
                && value.epochs.iter().any(|epoch| {
                    epoch.epoch.value == gap.epoch.value
                        && number(&gap.through_sequence) <= number(&epoch.checkpoint)
                })
        })
        && value.__buffa_unknown_fields.is_empty()
}
