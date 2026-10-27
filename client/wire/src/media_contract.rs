//! Bounded admission of the provisional media facade. Observations do not grant
//! commands; route, capabilities and current action access are independent facts.
use crate::{actions_contract, home::media::v1 as wire, home::state::v1 as state};
use buffa::{EnumValue, Message};
use std::collections::BTreeSet;

pub const MAX_SNAPSHOT_BYTES: usize = 64 * 1024;
pub const MAX_ARTWORK_BYTES: usize = 128 * 1024;
pub const MAX_POSITION_MILLIS: u64 = 7 * 24 * 60 * 60 * 1000;
pub const ACTION_SCHEMA: &str = "home.media.v1.MediaAction";

pub fn identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 256 && !value.chars().any(char::is_control)
}
fn label(value: &str) -> bool {
    value.len() <= 1024 && !value.chars().any(|c| c.is_control() && c != '\n')
}
fn timestamp(value: &buffa_types::google::protobuf::Timestamp) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && (-62_135_596_800..=253_402_300_799).contains(&value.seconds)
        && (0..1_000_000_000).contains(&value.nanos)
}
fn quality(value: &state::Quality) -> bool {
    let availability = match value.availability {
        EnumValue::Known(
            state::Availability::Available
            | state::Availability::Unknown
            | state::Availability::Unavailable,
        ) => true,
        EnumValue::Known(state::Availability::Unspecified) | EnumValue::Unknown(_) => false,
    };
    let freshness = match value.freshness {
        EnumValue::Known(state::Freshness::Fresh | state::Freshness::Stale) => true,
        EnumValue::Known(state::Freshness::Unspecified) | EnumValue::Unknown(_) => false,
    };
    let provenance = match value.provenance {
        EnumValue::Known(
            state::Provenance::Observed
            | state::Provenance::Restored
            | state::Provenance::Estimated,
        ) => true,
        EnumValue::Known(state::Provenance::Unspecified) | EnumValue::Unknown(_) => false,
    };
    value.__buffa_unknown_fields.is_empty() && availability && freshness && provenance
}
pub fn operation(value: EnumValue<wire::Operation>) -> Option<wire::Operation> {
    use wire::Operation as O;
    match value {
        EnumValue::Known(value @ (O::Play | O::Pause | O::Stop | O::Previous | O::Next)) => {
            Some(value)
        }
        EnumValue::Known(O::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
pub fn playback(value: EnumValue<wire::PlaybackState>) -> Option<wire::PlaybackState> {
    use wire::PlaybackState as P;
    match value {
        EnumValue::Known(value @ (P::Stopped | P::Paused | P::Playing | P::Buffering)) => {
            Some(value)
        }
        EnumValue::Known(P::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
pub fn route(value: &wire::Route) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && identity(&value.entity_id)
        && identity(&value.endpoint)
        && identity(&value.coordinator_entity_id)
        && value.topology_revision > 0
        && value.config_revision > 0
        && actions_contract::admission_epoch(&value.admission_epoch)
}
pub fn bounds(value: &wire::QuantityBounds, ceiling: u64) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value.minimum <= value.maximum
        && value.maximum <= ceiling
        && value.step > 0
        && value.step <= ceiling
}
pub fn quantity(value: u64, limits: &wire::QuantityBounds) -> bool {
    value >= limits.minimum
        && value <= limits.maximum
        && limits.step > 0
        && (value - limits.minimum).is_multiple_of(limits.step)
}
fn options(values: &[wire::Option]) -> bool {
    let mut seen = BTreeSet::new();
    values.len() <= 16
        && values.iter().all(|value| {
            value.__buffa_unknown_fields.is_empty()
                && identity(&value.id)
                && !value.label.is_empty()
                && label(&value.label)
                && seen.insert(&value.id)
        })
}
pub fn capabilities(value: &wire::Capabilities) -> bool {
    let mut seen = BTreeSet::new();
    value.__buffa_unknown_fields.is_empty()
        && value.operations.len() <= 5
        && value
            .operations
            .iter()
            .all(|op| operation(*op).is_some() && seen.insert(op.to_i32()))
        && value
            .volume_basis_points
            .as_option()
            .is_none_or(|v| bounds(v, 10_000))
        && value
            .position_millis
            .as_option()
            .is_none_or(|v| bounds(v, MAX_POSITION_MILLIS))
        && options(&value.sources)
        && options(&value.groups)
}
pub fn artwork_reference(value: &wire::ArtworkReference) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && !value.reference.is_empty()
        && value.reference.len() <= 512
        && value.expires_at.as_option().is_some_and(timestamp)
}
pub fn snapshot(value: &wire::Snapshot, expected_entity: &str) -> bool {
    let access_known = match value.action_access {
        EnumValue::Known(state::ActionAccess::Allowed | state::ActionAccess::Denied) => true,
        EnumValue::Known(state::ActionAccess::Unspecified) | EnumValue::Unknown(_) => false,
    };
    let mut members = BTreeSet::new();
    value.__buffa_unknown_fields.is_empty()
        && value.encoded_len() as usize <= MAX_SNAPSHOT_BYTES
        && value.route.as_option().is_some_and(|r| route(r) && r.entity_id == expected_entity)
        && value.capabilities.as_option().is_some_and(capabilities)
        && value.capabilities.as_option().and_then(|c| c.position_millis.as_option()).is_none_or(|b| value.duration_millis.is_some_and(|d| b.maximum <= d))
        && value.descriptor_sha256.len() == 32
        && value.descriptor_sha256.iter().any(|b| *b != 0)
        && access_known
        // Unspecified is an explicit unknown observation, never an inferred stop.
        && (value.playback == wire::PlaybackState::Unspecified || playback(value.playback).is_some())
        && [&value.title, &value.artist, &value.collection].into_iter().all(|v| v.as_ref().is_none_or(|s| label(s)))
        && value.volume_basis_points.is_none_or(|v| v <= 10_000)
        && value.position_millis.is_none_or(|v| v <= MAX_POSITION_MILLIS)
        && value.duration_millis.is_none_or(|v| v <= MAX_POSITION_MILLIS)
        && value.position_millis.zip(value.duration_millis).is_none_or(|(p,d)| p <= d)
        && value.source_id.as_ref().is_none_or(|v| identity(v))
        && value.group_id.as_ref().is_none_or(|v| identity(v))
        && value.members.len() <= 16
        && value.members.iter().all(|m| m.__buffa_unknown_fields.is_empty() && identity(&m.entity_id) && label(&m.label) && members.insert(&m.entity_id))
        && value.artwork.as_option().is_none_or(artwork_reference)
        && value.quality.as_option().is_some_and(quality)
        && value.received_at.as_option().is_some_and(timestamp)
        && label(&value.provenance)
}
pub fn action(value: &wire::MediaAction, current: &wire::Snapshot) -> bool {
    use wire::media_action::Command;
    let Some(limits) = current.capabilities.as_option() else {
        return false;
    };
    let Some(current_route) = current.route.as_option() else {
        return false;
    };
    snapshot(current, &current_route.entity_id)
        && current.action_access == state::ActionAccess::Allowed
        && value.__buffa_unknown_fields.is_empty()
        && value
            .route
            .as_option()
            .is_some_and(|r| route(r) && current.route.as_option() == Some(r))
        && match &value.command {
            Some(Command::Operation(op)) => {
                operation(*op).is_some() && limits.operations.contains(op)
            }
            Some(Command::VolumeBasisPoints(v)) => limits
                .volume_basis_points
                .as_option()
                .is_some_and(|b| bounds(b, 10_000) && quantity(u64::from(*v), b)),
            Some(Command::PositionMillis(v)) => {
                limits
                    .position_millis
                    .as_option()
                    .is_some_and(|b| bounds(b, MAX_POSITION_MILLIS) && quantity(*v, b))
                    && current.duration_millis.is_some_and(|d| *v <= d)
            }
            Some(Command::SourceId(id)) => limits.sources.iter().any(|o| o.id == *id),
            Some(Command::GroupId(id)) => limits.groups.iter().any(|o| o.id == *id),
            None => false,
        }
}
