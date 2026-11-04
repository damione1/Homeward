//! Pure admission of current content resources. Protected projections remain
//! bound to a source/kind/session; no native ID is recovered from a public name.
use crate::{
    api_media_contract::parse_data_provenance, api_observations::parse_quality,
    home::api::v2 as wire,
};
use buffa::{EnumValue, Message};
use home_client_model::{api_content::*, local_time::UtcInstant};
pub const MAX_METADATA_BYTES: usize = 64 * 1024;
pub const MAX_SOURCE_PAGE: usize = 256;
pub fn instant(
    value: &buffa_types::google::protobuf::Timestamp,
) -> Result<UtcInstant, ContentError> {
    if !value.__buffa_unknown_fields.is_empty()
        || (value.seconds == -62_135_596_800 && value.nanos == 0)
    {
        return Err(ContentError::Time);
    }
    UtcInstant::new(value.seconds, value.nanos).map_err(|_| ContentError::Time)
}
pub fn kind(value: EnumValue<wire::MediaContentKind>) -> Result<ContentKind, ContentError> {
    match value {
        EnumValue::Known(wire::MediaContentKind::Artwork) => Ok(ContentKind::Artwork),
        EnumValue::Known(wire::MediaContentKind::Camera) => Ok(ContentKind::Camera),
        EnumValue::Known(wire::MediaContentKind::Unspecified) | EnumValue::Unknown(_) => {
            Err(ContentError::Kind)
        }
    }
}
pub fn encode_kind(value: ContentKind) -> EnumValue<wire::MediaContentKind> {
    match value {
        ContentKind::Artwork => wire::MediaContentKind::Artwork.into(),
        ContentKind::Camera => wire::MediaContentKind::Camera.into(),
    }
}
pub fn source(value: &wire::MediaSource) -> Result<MediaSource, ContentError> {
    if !value.__buffa_unknown_fields.is_empty() || value.encoded_len() as usize > MAX_METADATA_BYTES
    {
        return Err(ContentError::Limit);
    }
    let name = MediaSourceName::try_from(value.name.as_str())?;
    let entity = home_client_model::api::EntityName::try_from(value.source_entity.as_str())
        .map_err(|_| ContentError::Name)?;
    let kinds = value
        .content_kinds
        .iter()
        .copied()
        .map(kind)
        .collect::<Result<Vec<_>, _>>()?;
    let quality = parse_quality(value.quality.as_option().ok_or(ContentError::Kind)?)
        .map_err(|_| ContentError::Kind)?;
    let provenance =
        parse_data_provenance(value.data_provenance).map_err(|_| ContentError::Kind)?;
    MediaSource::new(
        name,
        entity,
        &value.display_name,
        kinds,
        quality,
        provenance,
    )
}
pub fn source_page(
    value: &wire::ListMediaSourcesResponse,
    home: &home_client_model::api::HomeName,
) -> Result<Vec<MediaSource>, ContentError> {
    if !value.__buffa_unknown_fields.is_empty()
        || value.encoded_len() as usize > MAX_METADATA_BYTES
        || value.media_sources.len() > MAX_SOURCE_PAGE
        || value.next_page_token.len() > 4096
    {
        return Err(ContentError::Limit);
    }
    let mut result = Vec::with_capacity(value.media_sources.len());
    for raw in &value.media_sources {
        let source = source(raw)?;
        if source.name().home() != home
            || result
                .last()
                .is_some_and(|previous: &MediaSource| previous.name() >= source.name())
        {
            return Err(ContentError::ForeignSource);
        }
        result.push(source);
    }
    Ok(result)
}
pub fn session(value: &wire::MediaSession) -> Result<MediaSession, ContentError> {
    if !value.__buffa_unknown_fields.is_empty()
        || !value.artwork_reference.is_empty()
        || value.encoded_len() as usize > MAX_METADATA_BYTES
    {
        return Err(ContentError::Limit);
    }
    let name = MediaSessionName::try_from(value.name.as_str())?;
    let source = MediaSourceName::try_from(value.media_source.as_str())?;
    let kind = kind(value.content_kind)?;
    let created = instant(value.create_time.as_option().ok_or(ContentError::Time)?)?;
    let expires = instant(value.expire_time.as_option().ok_or(ContentError::Time)?)?;
    let etag = if value.events_etag.is_empty() {
        None
    } else {
        Some(EventsEtag::try_from(value.events_etag.as_str())?)
    };
    MediaSession::new(name, source, kind, created, expires, etag)
}
pub fn create_session_request(
    source: &MediaSourceName,
    kind: ContentKind,
    artwork_reference: Option<&[u8]>,
    request_id: home_client_model::api::RequestId,
) -> Result<wire::CreateMediaSessionRequest, ContentError> {
    match (kind, artwork_reference) {
        (ContentKind::Artwork, Some(reference))
            if !reference.is_empty() && reference.len() <= 512 => {}
        (ContentKind::Camera, None) => {}
        (ContentKind::Artwork, Some(_) | None) | (ContentKind::Camera, Some(_)) => {
            return Err(ContentError::Kind);
        }
    }
    Ok(wire::CreateMediaSessionRequest {
        parent: source.home().as_str().to_owned(),
        media_session: wire::MediaSession {
            media_source: source.as_str().to_owned(),
            content_kind: encode_kind(kind),
            artwork_reference: artwork_reference.unwrap_or_default().to_vec(),
            ..Default::default()
        }
        .into(),
        request_id: request_id.as_str().to_owned(),
        ..Default::default()
    })
}
