use super::{Media, Picture, Status};
use crate::media::{self, Data};
use buffa::Message;
use home_client_model::EntityId;
use home_client_session::{AccessLease, SessionCoordinator};
use home_client_transport::{FailureKind, MediaTransport, RpcCode};
use home_wire_contract::{home::media::v1 as wire, media_contract as admit};
use std::{
    sync::Arc,
    time::{Duration, SystemTime},
};
use tokio_util::sync::CancellationToken;

fn failure(kind: FailureKind) -> Status {
    match kind {
        FailureKind::Rpc(RpcCode::Unimplemented | RpcCode::NotFound) => Status::Unsupported,
        FailureKind::Rpc(RpcCode::Unauthenticated | RpcCode::PermissionDenied) => Status::Denied,
        FailureKind::Rpc(RpcCode::InvalidArgument | RpcCode::OutOfRange | RpcCode::DataLoss) => {
            Status::Contract
        }
        FailureKind::Rpc(
            RpcCode::Canceled
            | RpcCode::Unknown
            | RpcCode::DeadlineExceeded
            | RpcCode::AlreadyExists
            | RpcCode::ResourceExhausted
            | RpcCode::FailedPrecondition
            | RpcCode::Aborted
            | RpcCode::Internal
            | RpcCode::Unavailable,
        )
        | FailureKind::InvalidConfiguration
        | FailureKind::UntrustedCertificate
        | FailureKind::WrongServerName
        | FailureKind::TlsRejected
        | FailureKind::NetworkInterrupted
        | FailureKind::Deadline
        | FailureKind::Cancelled => Status::Offline,
        // The upstream RPC code enum is explicitly non-exhaustive. Future
        // statuses fail closed; they cannot leave a commandable projection.
        FailureKind::Rpc(_) => Status::Contract,
    }
}
impl Media {
    pub(super) async fn read(
        &self,
        session: SessionCoordinator,
        transport: MediaTransport,
        entity: EntityId,
        generation: i32,
        cancel: CancellationToken,
        previous: Option<(wire::ArtworkReference, Picture)>,
    ) {
        let lease = match session.access(&cancel).await {
            Ok(lease) => lease,
            Err(_) => {
                if !cancel.is_cancelled() {
                    self.read_failed(generation, Status::Denied);
                }
                return;
            }
        };
        let response = transport
            .read_snapshot(
                wire::ReadSnapshotRequest {
                    entity_id: entity.as_str().into(),
                    ..Default::default()
                },
                lease.token(),
                Duration::from_secs(2),
                &cancel,
            )
            .await;
        let result = match response {
            Ok(response) => {
                if !response.__buffa_unknown_fields.is_empty()
                    || response.encoded_len() as usize > admit::MAX_SNAPSHOT_BYTES
                {
                    Err(Status::Contract)
                } else {
                    match response.result {
                        Some(wire::read_snapshot_response::Result::Snapshot(value)) => {
                            Data::new(*value, &entity)
                                .map(Arc::new)
                                .map_err(|_| Status::Contract)
                        }
                        Some(wire::read_snapshot_response::Result::Unsupported(value)) => {
                            if value.__buffa_unknown_fields.is_empty() {
                                Err(Status::Unsupported)
                            } else {
                                Err(Status::Contract)
                            }
                        }
                        None => Err(Status::Contract),
                    }
                }
            }
            Err(error) => {
                let status = failure(error.kind());
                let _ = session.observe_failure(lease.generation, &error).await;
                Err(status)
            }
        };
        if cancel.is_cancelled()
            || lease.cancellation.is_cancelled()
            || !session.is_current(lease.generation)
        {
            return;
        }
        let (data, picture) = match result {
            Ok(data) => {
                let picture = picture(
                    &session, &transport, &lease, &entity, &data, &cancel, previous,
                )
                .await;
                (data, picture)
            }
            Err(status) => {
                self.read_failed(generation, status);
                return;
            }
        };
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
    lease: &AccessLease,
    entity: &EntityId,
    data: &Data,
    cancel: &CancellationToken,
    previous: Option<(wire::ArtworkReference, Picture)>,
) -> Picture {
    let Some(reference) = data.snapshot().artwork.as_option() else {
        return Picture::Absent;
    };
    let Some(deadline) = reference
        .expires_at
        .as_option()
        .and_then(|v| media::expiry(v).ok())
    else {
        return Picture::Failed;
    };
    if deadline <= SystemTime::now() {
        return Picture::Expired;
    }
    if let Some((prior, picture)) = previous
        && prior == *reference
    {
        match picture {
            Picture::Ready(image) => {
                if image.expires() > SystemTime::now() {
                    return Picture::Ready(image);
                }
            }
            Picture::Denied => return Picture::Denied,
            Picture::Failed => return Picture::Failed,
            Picture::Absent | Picture::Loading | Picture::Expired => {}
        }
    }
    let reply = transport
        .read_artwork(
            wire::ReadArtworkRequest {
                entity_id: entity.as_str().into(),
                reference: reference.reference.clone(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(2),
            cancel,
        )
        .await;
    let reply = match reply {
        Ok(reply) => reply,
        Err(error) => {
            let status = failure(error.kind());
            let _ = session.observe_failure(lease.generation, &error).await;
            return match status {
                Status::Denied => Picture::Denied,
                Status::Hidden
                | Status::Loading
                | Status::Ready
                | Status::Unsupported
                | Status::Offline
                | Status::Contract => {
                    if deadline <= SystemTime::now() {
                        Picture::Expired
                    } else {
                        Picture::Failed
                    }
                }
            };
        }
    };
    if cancel.is_cancelled()
        || lease.cancellation.is_cancelled()
        || !reply.__buffa_unknown_fields.is_empty()
        || reply.png.len() > admit::MAX_ARTWORK_BYTES
    {
        return Picture::Failed;
    }
    let Some(reply_expiry) = reply
        .expires_at
        .as_option()
        .filter(|t| t.__buffa_unknown_fields.is_empty())
        .and_then(|t| media::expiry(t).ok())
    else {
        return Picture::Failed;
    };
    let Some(local_limit) = SystemTime::now().checked_add(Duration::from_secs(30)) else {
        return Picture::Failed;
    };
    let deadline = deadline.min(reply_expiry).min(local_limit);
    if deadline <= SystemTime::now() {
        return Picture::Expired;
    }
    // The bounded decoder is always joined, even when the view is cancelled
    // while decoding; it can never publish by itself.
    let result = tokio::task::spawn_blocking(move || {
        media::decode_artwork(&reply.png, deadline, SystemTime::now())
    })
    .await;
    match result {
        Ok(Ok(image)) => Picture::Ready(Arc::new(image)),
        Ok(Err(media::ArtworkFailure::Expired)) => Picture::Expired,
        Ok(Err(media::ArtworkFailure::Invalid | media::ArtworkFailure::Capacity)) | Err(_) => {
            Picture::Failed
        }
    }
}
