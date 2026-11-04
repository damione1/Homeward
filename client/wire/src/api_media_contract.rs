//! Exact conversion of the current public media intent. Future decoded fields
//! remain non-executable; observation and live instance admission are separate.
use crate::{
    api_contract::{Violation, ViolationKind},
    home::api::v2 as wire,
};
use buffa::EnumValue;
use home_client_model::{
    api::EntityName,
    api_media::{
        self as model, MediaAction, MediaIdentifier, MediaIntent, MediaOperation, MediaRoute,
        PositionMillis, VolumeBasisPoints,
    },
};

fn reject(field: &'static str, kind: ViolationKind) -> Violation {
    Violation { field, kind }
}

pub fn media_unknown_fields(value: &wire::MediaCommand) -> bool {
    !value.__buffa_unknown_fields.is_empty()
        || value
            .route
            .as_option()
            .is_some_and(|route| !route.__buffa_unknown_fields.is_empty())
        || matches!(&value.command, Some(wire::media_command::Command::Position(duration))
            if !duration.__buffa_unknown_fields.is_empty())
}
pub fn parse_media_operation(
    value: EnumValue<wire::MediaOperation>,
) -> Result<MediaOperation, Violation> {
    use wire::MediaOperation as W;
    match value {
        EnumValue::Known(W::Play) => Ok(MediaOperation::Play),
        EnumValue::Known(W::Pause) => Ok(MediaOperation::Pause),
        EnumValue::Known(W::Stop) => Ok(MediaOperation::Stop),
        EnumValue::Known(W::Previous) => Ok(MediaOperation::Previous),
        EnumValue::Known(W::Next) => Ok(MediaOperation::Next),
        EnumValue::Known(W::Unspecified) => {
            Err(reject("media.operation", ViolationKind::MissingIntent))
        }
        EnumValue::Unknown(_) => Err(reject("media.operation", ViolationKind::UnknownValue)),
    }
}
pub fn parse_media_command(value: &wire::MediaCommand) -> Result<MediaIntent, Violation> {
    use wire::media_command::Command as W;
    if media_unknown_fields(value) {
        return Err(reject("media", ViolationKind::UnknownField));
    }
    let route = value
        .route
        .as_option()
        .ok_or_else(|| reject("media.route", ViolationKind::MissingIntent))?;
    let entity = EntityName::try_from(route.entity.as_str())
        .map_err(|_| reject("media.route.entity", ViolationKind::InvalidName))?;
    let coordinator = EntityName::try_from(route.coordinator.as_str())
        .map_err(|_| reject("media.route.coordinator", ViolationKind::InvalidName))?;
    let endpoint = MediaIdentifier::try_from(route.endpoint.as_str())
        .map_err(|_| reject("media.route.endpoint", ViolationKind::OutOfRange))?;
    let route = MediaRoute::new(
        entity,
        endpoint,
        coordinator,
        route.topology_revision,
        &route.admission_epoch,
        route.config_revision,
    )
    .map_err(|_| reject("media.route", ViolationKind::OutOfRange))?;
    let action = match &value.command {
        Some(W::Operation(operation)) => MediaAction::Operation(parse_media_operation(*operation)?),
        Some(W::VolumeBasisPoints(volume)) => MediaAction::Volume(
            VolumeBasisPoints::try_from(*volume)
                .map_err(|_| reject("media.volume_basis_points", ViolationKind::OutOfRange))?,
        ),
        Some(W::Position(duration)) => {
            if !(0..=model::MAX_POSITION_MILLIS / 1000).contains(&duration.seconds)
                || !(0..1_000_000_000).contains(&duration.nanos)
                || duration.nanos % 1_000_000 != 0
            {
                return Err(reject("media.position", ViolationKind::OutOfRange));
            }
            MediaAction::Position(
                PositionMillis::try_from(
                    duration.seconds * 1000 + i64::from(duration.nanos) / 1_000_000,
                )
                .map_err(|_| reject("media.position", ViolationKind::OutOfRange))?,
            )
        }
        Some(W::SourceId(id)) => MediaAction::Source(
            MediaIdentifier::try_from(id.as_str())
                .map_err(|_| reject("media.source_id", ViolationKind::OutOfRange))?,
        ),
        Some(W::GroupId(id)) => MediaAction::Group(
            MediaIdentifier::try_from(id.as_str())
                .map_err(|_| reject("media.group_id", ViolationKind::OutOfRange))?,
        ),
        None => return Err(reject("media", ViolationKind::MissingIntent)),
    };
    Ok(MediaIntent::new(route, action))
}
pub fn encode_media_command(value: &MediaIntent) -> wire::MediaCommand {
    use wire::media_command::Command as W;
    let route = value.route();
    let command = match value.action() {
        MediaAction::Operation(operation) => W::Operation(
            match operation {
                MediaOperation::Play => wire::MediaOperation::Play,
                MediaOperation::Pause => wire::MediaOperation::Pause,
                MediaOperation::Stop => wire::MediaOperation::Stop,
                MediaOperation::Previous => wire::MediaOperation::Previous,
                MediaOperation::Next => wire::MediaOperation::Next,
            }
            .into(),
        ),
        MediaAction::Volume(volume) => W::VolumeBasisPoints(volume.value()),
        MediaAction::Position(position) => {
            W::Position(Box::new(buffa_types::google::protobuf::Duration {
                seconds: position.value() / 1000,
                nanos: ((position.value() % 1000) * 1_000_000) as i32,
                ..Default::default()
            }))
        }
        MediaAction::Source(id) => W::SourceId(id.as_str().to_owned()),
        MediaAction::Group(id) => W::GroupId(id.as_str().to_owned()),
    };
    wire::MediaCommand {
        route: wire::MediaRoute {
            entity: route.entity().as_str().to_owned(),
            endpoint: route.endpoint().as_str().to_owned(),
            coordinator: route.coordinator().as_str().to_owned(),
            topology_revision: route.topology_revision(),
            admission_epoch: route.admission_epoch().to_owned(),
            config_revision: route.config_revision(),
            ..Default::default()
        }
        .into(),
        command: Some(command),
        ..Default::default()
    }
}

/// Source/capability readers must admit this independently of observation
/// quality. Unknown data origins are nonactionable, never silently OBSERVED.
pub fn parse_data_provenance(
    value: EnumValue<wire::MediaDataProvenance>,
) -> Result<model::MediaDataProvenance, model::MediaError> {
    match value {
        EnumValue::Known(wire::MediaDataProvenance::Observed) => {
            Ok(model::MediaDataProvenance::Observed)
        }
        EnumValue::Known(wire::MediaDataProvenance::Synthetic) => {
            Ok(model::MediaDataProvenance::Synthetic)
        }
        EnumValue::Known(wire::MediaDataProvenance::Unspecified) | EnumValue::Unknown(_) => {
            Err(model::MediaError::Provenance)
        }
    }
}

#[cfg(test)]
mod provenance_tests {
    use super::*;
    #[test]
    fn synthetic_data_remains_distinct_and_unknown_is_nonactionable() {
        assert_eq!(
            parse_data_provenance(wire::MediaDataProvenance::Observed.into()),
            Ok(model::MediaDataProvenance::Observed)
        );
        assert_eq!(
            parse_data_provenance(wire::MediaDataProvenance::Synthetic.into()),
            Ok(model::MediaDataProvenance::Synthetic)
        );
        assert!(parse_data_provenance(wire::MediaDataProvenance::Unspecified.into()).is_err());
        assert!(parse_data_provenance(EnumValue::Unknown(999)).is_err());
    }
}
