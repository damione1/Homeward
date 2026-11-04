//! Closed public playback projection. No provider/native v1 message enters this
//! graph, and observations never manufacture support, routing or write grants.
use crate::{
    api_media_contract::{parse_data_provenance, parse_media_operation},
    api_observations::{parse_action_access, parse_quality},
    content_contract::instant,
    home::api::v2 as wire,
};
use buffa::{EnumValue, Message};
use home_client_model::{
    api::{CapabilityName, DefinitionEtag, EntityName},
    api_media::{
        MAX_POSITION_MILLIS, MediaIdentifier, MediaRoute, PositionMillis, VolumeBasisPoints,
    },
    api_playback::*,
};
pub const MAX_PLAYBACK_BYTES: usize = 64 * 1024;
fn invalid<T, E>(value: Result<T, E>) -> Result<T, PlaybackError> {
    value.map_err(|_| PlaybackError::Observation)
}
fn duration(
    value: &buffa_types::google::protobuf::Duration,
) -> Result<PositionMillis, PlaybackError> {
    if !value.__buffa_unknown_fields.is_empty()
        || !(0..=MAX_POSITION_MILLIS / 1000).contains(&value.seconds)
        || !(0..1_000_000_000).contains(&value.nanos)
        || value.nanos % 1_000_000 != 0
    {
        return Err(PlaybackError::Bounds);
    }
    invalid(PositionMillis::try_from(
        value.seconds * 1000 + i64::from(value.nanos) / 1_000_000,
    ))
}
pub fn parse_route(value: &wire::MediaRoute) -> Result<MediaRoute, PlaybackError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(PlaybackError::Route);
    }
    invalid(MediaRoute::new(
        invalid(EntityName::try_from(value.entity.as_str()))?,
        invalid(MediaIdentifier::try_from(value.endpoint.as_str()))?,
        invalid(EntityName::try_from(value.coordinator.as_str()))?,
        value.topology_revision,
        &value.admission_epoch,
        value.config_revision,
    ))
}
fn choices(values: &[wire::MediaOption]) -> Result<Vec<MediaChoice>, PlaybackError> {
    values
        .iter()
        .map(|value| {
            if !value.__buffa_unknown_fields.is_empty() {
                return Err(PlaybackError::Observation);
            }
            MediaChoice::new(
                invalid(MediaIdentifier::try_from(value.id.as_str()))?,
                &value.display_name,
            )
        })
        .collect()
}
fn definition(value: &wire::MediaPlaybackDefinition) -> Result<PlaybackDefinition, PlaybackError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(PlaybackError::Observation);
    }
    let operations = value
        .operations
        .iter()
        .copied()
        .map(|operation| invalid(parse_media_operation(operation)))
        .collect::<Result<Vec<_>, _>>()?;
    let volume = value
        .volume_basis_points
        .as_option()
        .map(|bounds| {
            if !bounds.__buffa_unknown_fields.is_empty() {
                return Err(PlaybackError::Observation);
            }
            VolumeBounds::new(
                invalid(VolumeBasisPoints::try_from(bounds.minimum))?,
                invalid(VolumeBasisPoints::try_from(bounds.maximum))?,
                invalid(VolumeBasisPoints::try_from(bounds.step))?,
            )
        })
        .transpose()?;
    let position = value
        .position
        .as_option()
        .map(|bounds| {
            if !bounds.__buffa_unknown_fields.is_empty() {
                return Err(PlaybackError::Observation);
            }
            PositionBounds::new(
                duration(bounds.minimum.as_option().ok_or(PlaybackError::Bounds)?)?,
                duration(bounds.maximum.as_option().ok_or(PlaybackError::Bounds)?)?,
                duration(bounds.step.as_option().ok_or(PlaybackError::Bounds)?)?,
            )
        })
        .transpose()?;
    PlaybackDefinition::new(
        operations,
        volume,
        position,
        choices(&value.sources)?,
        choices(&value.groups)?,
    )
}
fn observation(
    value: &wire::MediaPlaybackObservation,
) -> Result<PlaybackObservation, PlaybackError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(PlaybackError::Observation);
    }
    let playback = match value.playback {
        EnumValue::Known(wire::MediaPlaybackState::Unspecified) => PlaybackState::Unknown,
        EnumValue::Known(wire::MediaPlaybackState::Stopped) => PlaybackState::Stopped,
        EnumValue::Known(wire::MediaPlaybackState::Paused) => PlaybackState::Paused,
        EnumValue::Known(wire::MediaPlaybackState::Playing) => PlaybackState::Playing,
        EnumValue::Known(wire::MediaPlaybackState::Buffering) => PlaybackState::Buffering,
        EnumValue::Unknown(_) => return Err(PlaybackError::Observation),
    };
    let route = value.route.as_option().map(parse_route).transpose()?;
    let volume = value
        .volume_basis_points
        .map(|value| invalid(VolumeBasisPoints::try_from(value)))
        .transpose()?;
    let position = value.position.as_option().map(duration).transpose()?;
    let duration = value.duration.as_option().map(duration).transpose()?;
    let members = value
        .members
        .iter()
        .map(|value| {
            if !value.__buffa_unknown_fields.is_empty() {
                return Err(PlaybackError::Observation);
            }
            MediaMember::new(
                invalid(EntityName::try_from(value.entity.as_str()))?,
                &value.display_name,
            )
        })
        .collect::<Result<Vec<_>, _>>()?;
    let artwork = value
        .artwork
        .as_option()
        .map(|value| {
            if !value.__buffa_unknown_fields.is_empty() {
                return Err(PlaybackError::Observation);
            }
            ArtworkReference::new(
                value.reference.clone(),
                invalid(instant(
                    value
                        .expire_time
                        .as_option()
                        .ok_or(PlaybackError::Observation)?,
                ))?,
                if value.media_source.is_empty() {
                    None
                } else {
                    Some(invalid(
                        home_client_model::api_content::MediaSourceName::try_from(
                            value.media_source.as_str(),
                        ),
                    )?)
                },
            )
        })
        .transpose()?;
    PlaybackObservation::new(PlaybackReadingInput {
        route,
        playback,
        title: value.title.clone(),
        artist: value.artist.clone(),
        collection: value.collection.clone(),
        volume,
        position,
        duration,
        source: value
            .source_id
            .as_deref()
            .map(|value| invalid(MediaIdentifier::try_from(value)))
            .transpose()?,
        group: value
            .group_id
            .as_deref()
            .map(|value| invalid(MediaIdentifier::try_from(value)))
            .transpose()?,
        members,
        artwork,
        quality: invalid(parse_quality(
            value
                .quality
                .as_option()
                .ok_or(PlaybackError::Observation)?,
        ))?,
        observed_time: value
            .observed_time
            .as_option()
            .map(|value| invalid(instant(value)))
            .transpose()?,
        powered_on: value.powered_on,
    })
}
pub fn playback(value: &wire::Capability) -> Result<Option<Playback>, PlaybackError> {
    if !value.__buffa_unknown_fields.is_empty() || value.encoded_len() as usize > MAX_PLAYBACK_BYTES
    {
        return Err(PlaybackError::Limit);
    }
    use wire::capability::Trait as T;
    let media = match &value.r#trait {
        Some(T::MediaPlayback(media)) => media,
        Some(
            T::Power(_)
            | T::Level(_)
            | T::TemperatureControl(_)
            | T::NumericMeasurement(_)
            | T::BinaryObservation(_)
            | T::LightControl(_)
            | T::FanControl(_)
            | T::Cover(_)
            | T::Lock(_)
            | T::Alarm(_)
            | T::Helper(_)
            | T::Solar(_)
            | T::SolarEvent(_)
            | T::Weather(_)
            | T::Presence(_)
            | T::PresenceDetails(_)
            | T::Opaque(_),
        ) => return Ok(None),
        None => return Err(PlaybackError::Observation),
    };
    if !media.__buffa_unknown_fields.is_empty() {
        return Err(PlaybackError::Observation);
    }
    let name = invalid(CapabilityName::try_from(value.name.as_str()))?;
    let etag = if value.etag.is_empty() {
        None
    } else {
        Some(invalid(DefinitionEtag::try_from(value.etag.as_str()))?)
    };
    let definition = media.definition.as_option().map(definition).transpose()?;
    let observation = media.observation.as_option().map(observation).transpose()?;
    Playback::new(
        name,
        etag,
        invalid(parse_action_access(value.action_access))?,
        invalid(parse_action_access(media.action_access))?,
        definition,
        observation,
        invalid(parse_data_provenance(media.data_provenance))?,
    )
    .map(Some)
}
