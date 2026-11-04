use super::*;
use home_client_model::api_content::MediaSourceName;
use home_client_session::AccessLease;
use home_client_transport::{Failure, FailureKind, RpcCode};
use home_wire_contract::{
    camera_content as camera, content_contract,
    home::api::v2 as wire,
    media_video_contract::{self as video, VideoFrame},
};
use std::time::SystemTime;

#[derive(Clone, Copy)]
enum ReadFailure {
    Expired,
    Revoked,
    Offline,
    Unavailable,
    Unsupported,
    Invalid,
    Cancelled,
}
impl ReadFailure {
    fn phase(self) -> Option<Playback> {
        match self {
            Self::Expired => Some(Playback::Expired),
            Self::Revoked => Some(Playback::Revoked),
            Self::Offline => Some(Playback::Offline),
            Self::Unavailable => Some(Playback::Unavailable),
            Self::Unsupported => Some(Playback::Unsupported),
            Self::Invalid => Some(Playback::Invalid),
            Self::Cancelled => None,
        }
    }
}
fn transport_failure(error: &Failure) -> ReadFailure {
    if !matches!(error.authorization_termination(), Ok(None)) {
        return ReadFailure::Revoked;
    }
    match error.api_failure() {
        Err(_) => ReadFailure::Invalid,
        Ok(Some(error)) => {
            use wire::ApiErrorReason as R;
            match error.reason() {
                R::AccessDenied | R::AuthenticationRequired => ReadFailure::Revoked,
                R::MethodNotSupported => ReadFailure::Unsupported,
                R::ServiceUnavailable | R::RequestDeadlineExceeded | R::RequestCancelled => {
                    ReadFailure::Offline
                }
                R::ResourceNotFound
                | R::CapabilityUnavailable
                | R::ResyncRequired
                | R::ConfigChanged
                | R::TopologyChanged
                | R::StatePreconditionFailed
                | R::RateLimited
                | R::RequestTooLarge
                | R::StreamCapacity
                | R::QueryBudgetExceeded => ReadFailure::Unavailable,
                R::ValidationFailed
                | R::ResourceAlreadyExists
                | R::RequestIdReused
                | R::EtagMismatch
                | R::SourceReadOnly
                | R::ConfigRestartRequired
                | R::CommitOutcomeUnknown
                | R::EffectOutcomeUnknown
                | R::InternalFailure
                | R::StoredDataIntegrity
                | R::Unspecified => ReadFailure::Invalid,
            }
        }
        Ok(None) => {
            if matches!(
                error.kind(),
                FailureKind::Rpc(RpcCode::PermissionDenied | RpcCode::Unauthenticated)
            ) {
                ReadFailure::Revoked
            } else if error.may_resume_read() {
                ReadFailure::Offline
            } else {
                ReadFailure::Invalid
            }
        }
    }
}
enum Input {
    Header(video::Header),
    Sample(home_client_model::video::EncodedFrame),
}
struct WorkerLifetime(Videos);
impl Drop for WorkerLifetime {
    fn drop(&mut self) {
        self.0
            .0
            .counters
            .workers_live
            .fetch_sub(1, Ordering::SeqCst);
        self.0
            .0
            .counters
            .workers_joined
            .fetch_add(1, Ordering::SeqCst);
    }
}
impl Videos {
    async fn response<T>(
        &self,
        session: &SessionCoordinator,
        access: &AccessLease,
        context: u64,
        result: Result<T, Failure>,
    ) -> Result<T, ReadFailure> {
        match result {
            Ok(value) => {
                if access.cancellation.is_cancelled() || !session.is_current(access.generation) {
                    Err(ReadFailure::Revoked)
                } else {
                    Ok(value)
                }
            }
            Err(error) => {
                let failure = transport_failure(&error);
                if let Some(phase) = failure.phase() {
                    self.failed(context, phase);
                }
                let _ = session.observe_failure(access.generation, &error).await;
                Err(failure)
            }
        }
    }
    pub(super) async fn play(
        &self,
        session: SessionCoordinator,
        api: MediaTransport,
        context: u64,
        id: MediaSourceName,
        cancel: CancellationToken,
    ) {
        {
            let access = match session.access(&cancel).await {
                Ok(access) => access,
                Err(_) => {
                    self.failed(context, Playback::Revoked);
                    return;
                }
            };
            if !self
                .0
                .state
                .lock()
                .expect("video state")
                .accepts(context, access.generation)
            {
                return;
            }
            let delete_request_id = match home_client_actions::new_request_id() {
                Ok(id) => id,
                Err(_) => {
                    self.failed(context, Playback::Invalid);
                    return;
                }
            };
            let request = match home_client_actions::new_request_id()
                .ok()
                .and_then(|request_id| {
                    content_contract::create_session_request(
                        &id,
                        home_client_model::api_content::ContentKind::Camera,
                        None,
                        request_id,
                    )
                    .ok()
                }) {
                Some(request) => request,
                None => {
                    self.failed(context, Playback::Invalid);
                    return;
                }
            };
            let result = self
                .response(
                    &session,
                    &access,
                    context,
                    api.create_media_session(
                        request,
                        access.token(),
                        Duration::from_secs(5),
                        &cancel,
                    )
                    .await,
                )
                .await;
            let lease = match result.and_then(|raw| {
                camera::lease(
                    &raw,
                    &id,
                    home_client_model::api_content::ContentKind::Camera,
                    SystemTime::now(),
                    Instant::now(),
                )
                .map_err(|_| ReadFailure::Invalid)
            }) {
                Ok(lease) => lease,
                Err(error) => {
                    if let Some(phase) = error.phase() {
                        self.failed(context, phase);
                    }
                    return;
                }
            };
            self.follow(&session, &api, &access, &lease, context, &cancel)
                .await;
            // One bounded cleanup on the original origin/token; no retries and no
            // transfer of a camera lease to another profile or account.
            let _ = api
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
        }
    }
    async fn follow(
        &self,
        session: &SessionCoordinator,
        api: &MediaTransport,
        access: &AccessLease,
        lease: &camera::Lease,
        context: u64,
        cancel: &CancellationToken,
    ) {
        let stream_cancel = cancel.child_token();
        let opened = self
            .response(
                session,
                access,
                context,
                api.stream_media_session(
                    wire::StreamMediaSessionRequest {
                        name: lease.name().as_str().to_owned(),
                        ..Default::default()
                    },
                    access.token(),
                    Duration::from_secs(60),
                    &stream_cancel,
                )
                .await,
            )
            .await;
        let mut stream = match opened {
            Ok(stream) => stream,
            Err(error) => {
                if let Some(phase) = error.phase() {
                    self.failed(context, phase);
                }
                return;
            }
        };
        let mut admission = video::VideoStream::new(lease.clone());
        let work_cancel = cancel.child_token();
        let (send, receive) = tokio::sync::mpsc::channel(1);
        let (owner, work, generation, deadline) = (
            self.clone(),
            work_cancel.clone(),
            access.generation,
            lease.deadline(),
        );
        let mut worker = tokio::task::spawn_blocking(move || {
            owner.decode(context, generation, deadline, receive, work)
        });
        let mut completed = None;
        let failure = loop {
            let next = tokio::select! {biased;
                ()=cancel.cancelled()=>break ReadFailure::Cancelled,
                ()=access.cancellation.cancelled()=>break ReadFailure::Revoked,
                _=tokio::time::sleep_until(tokio::time::Instant::from_std(lease.deadline()))=>break ReadFailure::Expired,
                result=&mut worker=>{let failure=match &result{Ok(Err(error))=>*error,Ok(Ok(()))|Err(_)=>ReadFailure::Invalid};completed=Some(result);break failure},
                result=stream.next()=>result,
            };
            let raw = match next {
                Ok(Some(raw)) => raw,
                Ok(None) => break ReadFailure::Offline,
                Err(error) => {
                    let failure = transport_failure(&error);
                    work_cancel.cancel();
                    if let Some(phase) = failure.phase() {
                        self.failed(context, phase);
                    }
                    let _ = session.observe_failure(access.generation, &error).await;
                    break failure;
                }
            };
            if !session.is_current(access.generation) {
                break ReadFailure::Revoked;
            }
            let frame = match admission.accept(raw, Instant::now()) {
                Ok(frame) => frame,
                Err(camera::AdmissionError::Expired) => break ReadFailure::Expired,
                Err(
                    camera::AdmissionError::Invalid
                    | camera::AdmissionError::Capacity
                    | camera::AdmissionError::ForeignResource
                    | camera::AdmissionError::Sequence
                    | camera::AdmissionError::StaleRevision,
                ) => break ReadFailure::Invalid,
            };
            let input = match frame {
                VideoFrame::Header(header) => Input::Header(header),
                VideoFrame::Sample(sample) => Input::Sample(sample),
            };
            let sent = tokio::select! {biased;
                ()=cancel.cancelled()=>break ReadFailure::Cancelled,
                ()=access.cancellation.cancelled()=>break ReadFailure::Revoked,
                _=tokio::time::sleep_until(tokio::time::Instant::from_std(lease.deadline()))=>break ReadFailure::Expired,
                result=send.send(input)=>result,
            };
            if sent.is_err() {
                // Receiver closure may race the completed worker branch. Keep
                // its actual codec/format failure instead of inventing Invalid.
                let result = (&mut worker).await;
                let failure = match &result {
                    Ok(Err(error)) => *error,
                    Ok(Ok(())) | Err(_) => ReadFailure::Invalid,
                };
                completed = Some(result);
                break failure;
            }
        };
        // Withdraw pixels/publication NOW, before waiting for native destruction
        // or HTTP cleanup. A completed stale worker can never republish them.
        if let Some(phase) = failure.phase() {
            self.failed(context, phase);
        }
        work_cancel.cancel();
        stream_cancel.cancel();
        drop(send);
        drop(stream);
        if completed.is_none() {
            let _ = worker.await;
        }
    }

    fn decode(
        &self,
        context: u64,
        generation: Generation,
        deadline: Instant,
        mut input: tokio::sync::mpsc::Receiver<Input>,
        cancel: CancellationToken,
    ) -> Result<(), ReadFailure> {
        use home_client_model::video::VideoDimensions;
        use home_client_platform::video::{DecodeFailure, DecoderBackend, H264Decoder};
        enum Decoder {
            AwaitingHeader,
            Active {
                decoder: H264Decoder,
                dimensions: VideoDimensions,
            },
        }
        self.0
            .counters
            .workers_started
            .fetch_add(1, Ordering::SeqCst);
        let live = self.0.counters.workers_live.fetch_add(1, Ordering::SeqCst) + 1;
        self.0
            .counters
            .max_live_workers
            .fetch_max(live, Ordering::SeqCst);
        let _lifetime = WorkerLifetime(self.clone());
        let mut decoder = Decoder::AwaitingHeader;
        while let Some(input) = input.blocking_recv() {
            if cancel.is_cancelled() {
                return Err(ReadFailure::Cancelled);
            }
            if Instant::now() >= deadline {
                return Err(ReadFailure::Expired);
            }
            match input {
                Input::Header(header) => {
                    match decoder {
                        Decoder::AwaitingHeader => {}
                        Decoder::Active { .. } => return Err(ReadFailure::Invalid),
                    }
                    let native = H264Decoder::new(&header.format).map_err(|error| match error {
                        DecodeFailure::HardwareUnavailable | DecodeFailure::Native(_) => {
                            ReadFailure::Unsupported
                        }
                        DecodeFailure::Format(_)
                        | DecodeFailure::Input(_)
                        | DecodeFailure::DimensionLimit
                        | DecodeFailure::KeyframeRequired
                        | DecodeFailure::MissingOutput
                        | DecodeFailure::InvalidOutput => ReadFailure::Invalid,
                    })?;
                    match native.backend() {
                        DecoderBackend::VideoToolboxHardware => {}
                    }
                    if native.dimensions()
                        != (header.dimensions.width(), header.dimensions.height())
                    {
                        return Err(ReadFailure::Invalid);
                    }
                    decoder = Decoder::Active {
                        decoder: native,
                        dimensions: header.dimensions,
                    };
                }
                Input::Sample(sample) => {
                    let (decoder, dimensions) = match &mut decoder {
                        Decoder::AwaitingHeader => return Err(ReadFailure::Invalid),
                        Decoder::Active {
                            decoder,
                            dimensions,
                        } => (decoder, dimensions),
                    };
                    let started = Instant::now();
                    let frame = decoder.decode(&sample).map_err(|error| match error {
                        DecodeFailure::HardwareUnavailable => ReadFailure::Unsupported,
                        DecodeFailure::Format(_)
                        | DecodeFailure::Native(_)
                        | DecodeFailure::Input(_)
                        | DecodeFailure::DimensionLimit
                        | DecodeFailure::KeyframeRequired
                        | DecodeFailure::MissingOutput
                        | DecodeFailure::InvalidOutput => ReadFailure::Invalid,
                    })?;
                    if frame.dimensions() != (dimensions.width(), dimensions.height()) {
                        return Err(ReadFailure::Invalid);
                    }
                    // SharedPixelBuffer is Send+Sync. Allocate/copy on this worker;
                    // the UI only wraps its immutable reference in an Image.
                    let pixels = slint::SharedPixelBuffer::<slint::Rgba8Pixel>::clone_from_slice(
                        frame.pixels(),
                        dimensions.width(),
                        dimensions.height(),
                    );
                    let nanos = started.elapsed().as_nanos().min(u64::MAX as u128) as u64;
                    self.0
                        .counters
                        .frames_decoded
                        .fetch_add(1, Ordering::SeqCst);
                    self.0
                        .counters
                        .decode_nanos
                        .fetch_add(nanos, Ordering::SeqCst);
                    self.0
                        .counters
                        .max_decode_nanos
                        .fetch_max(nanos, Ordering::SeqCst);
                    if !cancel.is_cancelled() {
                        self.frame(
                            context,
                            generation,
                            Picture {
                                pixels,
                                deadline,
                                presentation_micros: sample.presentation_micros(),
                            },
                        );
                    }
                }
            }
        }
        Ok(())
    }
}
