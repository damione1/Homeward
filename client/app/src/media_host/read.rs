use super::*;
use crate::media;
use buffa::Message;
use home_client_model::{
    api::{CapabilityName, EntityName, HomeName},
    api_content::ContentKind,
    api_playback::{ArtworkReference, Playback},
    cameras::ImageKey,
};
use home_client_session::AccessLease;
use home_client_transport::{Failure, FailureKind, RpcCode};
use home_wire_contract::{
    api_playback_contract as playback, camera_content, content_contract, home::api::v2 as wire,
};
use std::{
    collections::BTreeSet,
    future::Future,
    time::{Instant, SystemTime},
};
fn failure(error: &Failure) -> Status {
    match error.api_failure() {
        Err(_) => Status::Contract,
        Ok(Some(error)) => {
            use wire::ApiErrorReason as R;
            match error.reason() {
                R::AccessDenied | R::AuthenticationRequired => Status::Denied,
                R::MethodNotSupported | R::SourceReadOnly => Status::Unsupported,
                R::ServiceUnavailable | R::RequestDeadlineExceeded | R::RequestCancelled => {
                    Status::Offline
                }
                R::AuthorityUnavailable
                | R::ValidationFailed
                | R::ResourceNotFound
                | R::ResourceAlreadyExists
                | R::RequestIdReused
                | R::EtagMismatch
                | R::ConfigChanged
                | R::TopologyChanged
                | R::CapabilityUnavailable
                | R::StatePreconditionFailed
                | R::ConfigRestartRequired
                | R::RateLimited
                | R::RequestTooLarge
                | R::StreamCapacity
                | R::QueryBudgetExceeded
                | R::ResyncRequired
                | R::CommitOutcomeUnknown
                | R::EffectOutcomeUnknown
                | R::InternalFailure
                | R::StoredDataIntegrity
                | R::Unspecified => Status::Contract,
            }
        }
        Ok(None) => {
            if matches!(
                error.kind(),
                FailureKind::Rpc(RpcCode::PermissionDenied | RpcCode::Unauthenticated)
            ) {
                Status::Denied
            } else if error.may_resume_read() {
                Status::Offline
            } else {
                Status::Contract
            }
        }
    }
}
async fn call<T>(
    session: &SessionCoordinator,
    lease: &AccessLease,
    cancel: &CancellationToken,
    future: impl Future<Output = Result<T, Failure>>,
) -> Result<T, Status> {
    let result = tokio::select! {biased;()=cancel.cancelled()=>return Err(Status::Denied),()=lease.cancellation.cancelled()=>return Err(Status::Denied),result=future=>result};
    match result {
        Ok(value) => {
            if session.is_current(lease.generation)
                && !lease.cancellation.is_cancelled()
                && !cancel.is_cancelled()
            {
                Ok(value)
            } else {
                Err(Status::Denied)
            }
        }
        Err(error) => {
            let status = failure(&error);
            let _ = session.observe_failure(lease.generation, &error).await;
            Err(status)
        }
    }
}
async fn current_playback(
    session: &SessionCoordinator,
    transport: &MediaTransport,
    lease: &AccessLease,
    entity: &EntityName,
    cancel: &CancellationToken,
) -> Result<Playback, Status> {
    let homes = call(
        session,
        lease,
        cancel,
        transport.home().list_homes(
            wire::ListHomesRequest {
                page_size: 2,
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(5),
            cancel,
        ),
    )
    .await?;
    if !homes.__buffa_unknown_fields.is_empty()
        || homes.homes.len() != 1
        || !homes.next_page_token.is_empty()
        || !homes.homes[0].__buffa_unknown_fields.is_empty()
    {
        return Err(Status::Contract);
    }
    let home = HomeName::try_from(homes.homes[0].name.as_str()).map_err(|_| Status::Contract)?;
    if entity
        .as_str()
        .split('/')
        .take(2)
        .ne(home.as_str().split('/'))
    {
        return Err(Status::Denied);
    }
    let mut token = String::new();
    let mut tokens = BTreeSet::new();
    let mut previous = None;
    let mut media = None;
    let mut bytes = 0usize;
    for _ in 0..128 {
        let page = call(
            session,
            lease,
            cancel,
            transport.home().list_capabilities(
                wire::ListCapabilitiesRequest {
                    parent: entity.as_str().to_owned(),
                    page_size: 256,
                    page_token: token,
                    ..Default::default()
                },
                lease.token(),
                Duration::from_secs(5),
                cancel,
            ),
        )
        .await?;
        bytes = bytes.saturating_add(page.encoded_len() as usize);
        if !page.__buffa_unknown_fields.is_empty()
            || page.capabilities.len() > 256
            || page.next_page_token.len() > 4096
            || bytes > 16 * 1024 * 1024
        {
            return Err(Status::Contract);
        }
        for capability in &page.capabilities {
            let name =
                CapabilityName::try_from(capability.name.as_str()).map_err(|_| Status::Contract)?;
            if name
                .as_str()
                .rsplit_once("/capabilities/")
                .map(|(parent, _)| parent)
                != Some(entity.as_str())
                || previous.as_ref().is_some_and(|previous| previous >= &name)
            {
                return Err(Status::Contract);
            }
            previous = Some(name);
            if let Some(value) = playback::playback(capability).map_err(|_| Status::Contract)? {
                // The current player view needs an exact slot; it never chooses
                // an arbitrary one when an entity advertises multiple players.
                if media.replace(value).is_some() {
                    return Err(Status::Contract);
                }
            }
        }
        if page.next_page_token.is_empty() {
            return media.ok_or(Status::Unsupported);
        }
        if !tokens.insert(page.next_page_token.clone()) {
            return Err(Status::Contract);
        }
        token = page.next_page_token;
    }
    Err(Status::Contract)
}
impl Media {
    pub(super) async fn read(
        &self,
        session: SessionCoordinator,
        transport: MediaTransport,
        entity: EntityName,
        generation: i32,
        cancel: CancellationToken,
        previous: Option<(ArtworkReference, Picture)>,
    ) {
        let expected = entity.clone();
        let lease = match session.access(&cancel).await {
            Ok(lease) => lease,
            Err(_) => {
                if !cancel.is_cancelled() {
                    self.read_failed(generation, Status::Denied)
                }
                return;
            }
        };
        let value = current_playback(&session, &transport, &lease, &expected, &cancel)
            .await
            .and_then(|value| {
                Data::new(value, &expected)
                    .map(Arc::new)
                    .map_err(|_| Status::Contract)
            });
        if cancel.is_cancelled()
            || lease.cancellation.is_cancelled()
            || !session.is_current(lease.generation)
        {
            return;
        }
        let data = match value {
            Ok(data) => data,
            Err(status) => {
                self.read_failed(generation, status);
                return;
            }
        };
        let picture = picture(
            &session, &transport, &lease, &expected, &data, &cancel, previous,
        )
        .await;
        let mut state = self.0.state.lock().expect("media state");
        if state.generation != generation
            || state.entity.as_ref() != Some(&entity)
            || cancel.is_cancelled()
            || lease.cancellation.is_cancelled()
            || !session.is_current(lease.generation)
        {
            return;
        }
        state.apply(data, lease.generation, picture);
        self.publish(&state);
    }
    fn read_failed(&self, generation: i32, status: Status) {
        let mut state = self.0.state.lock().expect("media state");
        if state.generation != generation {
            return;
        }
        match status {
            Status::Offline => {
                state.invalidate(false);
                state.status = Status::Offline;
                state.read_pending = false;
            }
            Status::Denied | Status::Contract | Status::Unsupported => {
                state.clear_private();
                state.status = status;
                let _ = self.0.mailbox.invalidate();
            }
            Status::Hidden | Status::Loading | Status::Ready => {
                state.clear_private();
                state.status = Status::Contract;
                let _ = self.0.mailbox.invalidate();
            }
        }
        self.publish(&state);
    }
}
async fn picture(
    session: &SessionCoordinator,
    transport: &MediaTransport,
    access: &AccessLease,
    entity: &EntityName,
    data: &Data,
    cancel: &CancellationToken,
    previous: Option<(ArtworkReference, Picture)>,
) -> Picture {
    let Some(reference) = data
        .snapshot()
        .observation()
        .and_then(|reading| reading.artwork())
    else {
        return Picture::Absent;
    };
    let Some(source_name) = reference.media_source() else {
        return Picture::Failed;
    };
    let wall = SystemTime::now();
    let now = Instant::now();
    let ref_expiry = match media::expiry(reference.expires()) {
        Ok(expiry) => expiry,
        Err(_) => return Picture::Expired,
    };
    let remaining = match ref_expiry.duration_since(wall) {
        Ok(remaining) if !remaining.is_zero() => remaining.min(Duration::from_secs(30)),
        Ok(_) | Err(_) => return Picture::Expired,
    };
    let Some(reference_deadline) = now.checked_add(remaining) else {
        return Picture::Failed;
    };
    if let Some((prior, picture)) = previous
        && prior == *reference
    {
        return match picture {
            Picture::Ready(image) => {
                if image.deadline() > now && image.expires() > wall {
                    Picture::Ready(image)
                } else {
                    Picture::Expired
                }
            }
            Picture::Denied => Picture::Denied,
            Picture::Failed => Picture::Failed,
            Picture::Expired => Picture::Expired,
            Picture::Absent | Picture::Loading => {
                acquire_picture(
                    session,
                    transport,
                    access,
                    ArtworkTarget {
                        entity,
                        reference,
                        source_name,
                        ref_expiry,
                        reference_deadline,
                        started: wall,
                    },
                    cancel,
                )
                .await
            }
        };
    }
    acquire_picture(
        session,
        transport,
        access,
        ArtworkTarget {
            entity,
            reference,
            source_name,
            ref_expiry,
            reference_deadline,
            started: wall,
        },
        cancel,
    )
    .await
}
struct ArtworkTarget<'a> {
    entity: &'a EntityName,
    reference: &'a ArtworkReference,
    source_name: &'a home_client_model::api_content::MediaSourceName,
    ref_expiry: SystemTime,
    reference_deadline: Instant,
    started: SystemTime,
}
async fn acquire_picture(
    session: &SessionCoordinator,
    transport: &MediaTransport,
    access: &AccessLease,
    target: ArtworkTarget<'_>,
    cancel: &CancellationToken,
) -> Picture {
    let ArtworkTarget {
        entity,
        reference,
        source_name,
        ref_expiry,
        reference_deadline,
        started,
    } = target;
    let result = call(
        session,
        access,
        cancel,
        transport.get_media_source(
            wire::GetMediaSourceRequest {
                name: source_name.as_str().to_owned(),
                ..Default::default()
            },
            access.token(),
            Duration::from_secs(5),
            cancel,
        ),
    )
    .await;
    let source =
        match result.and_then(|raw| content_contract::source(&raw).map_err(|_| Status::Contract)) {
            Ok(source) => source,
            Err(Status::Denied) => return Picture::Denied,
            Err(
                Status::Hidden
                | Status::Loading
                | Status::Ready
                | Status::Unsupported
                | Status::Offline
                | Status::Contract,
            ) => return Picture::Failed,
        };
    if source.name() != source_name
        || source.entity() != entity
        || !source.kinds().contains(&ContentKind::Artwork)
    {
        return Picture::Failed;
    }
    let delete_request_id = match home_client_actions::new_request_id() {
        Ok(id) => id,
        Err(_) => return Picture::Failed,
    };
    let request = match home_client_actions::new_request_id().ok().and_then(|id| {
        content_contract::create_session_request(
            source_name,
            ContentKind::Artwork,
            Some(reference.bytes()),
            id,
        )
        .ok()
    }) {
        Some(request) => request,
        None => return Picture::Failed,
    };
    let opened = call(
        session,
        access,
        cancel,
        transport.create_media_session(request, access.token(), Duration::from_secs(5), cancel),
    )
    .await;
    let lease = match opened {
        Ok(raw) => match camera_content::lease(
            &raw,
            source_name,
            ContentKind::Artwork,
            SystemTime::now(),
            Instant::now(),
        ) {
            Ok(lease) => lease,
            Err(_) => return Picture::Failed,
        },
        Err(Status::Denied) => return Picture::Denied,
        Err(
            Status::Hidden
            | Status::Loading
            | Status::Ready
            | Status::Unsupported
            | Status::Offline
            | Status::Contract,
        ) => return Picture::Failed,
    };
    let deadline = lease.deadline().min(reference_deadline);
    let result = async {
        let request =
            camera_content::image_request(&lease, &ImageKey::Snapshot(source_name.clone()))
                .map_err(|_| Picture::Failed)?;
        let raw = call(
            session,
            access,
            cancel,
            transport.read_media_session(request, access.token(), Duration::from_secs(5), cancel),
        )
        .await
        .map_err(|status| {
            if status == Status::Denied {
                Picture::Denied
            } else {
                Picture::Failed
            }
        })?;
        let image = camera_content::image(
            &raw,
            &lease,
            &ImageKey::Snapshot(source_name.clone()),
            SystemTime::now(),
            Instant::now(),
        )
        .map_err(|_| Picture::Failed)?;
        let expires = lease.expires_at().min(ref_expiry).min(
            started
                .checked_add(Duration::from_secs(30))
                .ok_or(Picture::Failed)?,
        );
        // Even cancellation joins the bounded native decoder; it cannot publish
        // independently. Both caller generation and monotonic expiry fence use.
        let decoded = tokio::task::spawn_blocking(move || {
            media::decode_artwork(
                &image.png,
                expires,
                SystemTime::now(),
                deadline,
                source.data_provenance(),
            )
        })
        .await;
        if cancel.is_cancelled()
            || access.cancellation.is_cancelled()
            || !session.is_current(access.generation)
        {
            return Err(Picture::Failed);
        }
        if deadline <= Instant::now() {
            return Err(Picture::Expired);
        }
        match decoded {
            Ok(Ok(image)) => Ok(Picture::Ready(Arc::new(image))),
            Ok(Err(media::ArtworkFailure::Expired)) => Err(Picture::Expired),
            Ok(Err(media::ArtworkFailure::Invalid | media::ArtworkFailure::Capacity)) | Err(_) => {
                Err(Picture::Failed)
            }
        }
    }
    .await;
    let _ = transport
        .delete_media_session(
            wire::DeleteMediaSessionRequest {
                name: lease.name().as_str().to_owned(),
                request_id: delete_request_id.as_str().to_owned(),
                ..Default::default()
            },
            access.token(),
            Duration::from_secs(1),
            &CancellationToken::new(),
        )
        .await;
    match result {
        Ok(picture) | Err(picture) => picture,
    }
}
