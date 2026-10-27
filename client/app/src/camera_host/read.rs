use super::*;
use crate::camera::{self, Frame};
use home_client_model::cameras::CameraId;
use home_client_session::AccessLease;
use home_client_transport::{Failure, FailureKind, RpcCode};
use home_wire_contract::{cameras_contract as admit, home::cameras::v1 as wire};
use std::{collections::BTreeSet, time::SystemTime};
const TIMEOUT: Duration = Duration::from_secs(5);
fn failure(error: &Failure) -> Status {
    if !matches!(error.authorization_termination(), Ok(None)) {
        return Status::Revoked;
    }
    match error.kind() {
        FailureKind::Rpc(RpcCode::PermissionDenied | RpcCode::Unauthenticated) => Status::Revoked,
        FailureKind::Rpc(RpcCode::Unimplemented) => Status::Unsupported,
        FailureKind::Rpc(RpcCode::InvalidArgument | RpcCode::OutOfRange | RpcCode::DataLoss) => {
            Status::Invalid
        }
        FailureKind::Rpc(RpcCode::ResourceExhausted) => Status::Unavailable,
        FailureKind::Rpc(
            RpcCode::Canceled
            | RpcCode::Unknown
            | RpcCode::DeadlineExceeded
            | RpcCode::NotFound
            | RpcCode::AlreadyExists
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
        // Upstream connectrpc status is explicitly non-exhaustive.
        FailureKind::Rpc(_) => Status::Invalid,
    }
}
async fn response<T>(
    session: &SessionCoordinator,
    access: &AccessLease,
    result: Result<T, Failure>,
) -> Result<T, Status> {
    match result {
        Ok(value) => {
            if access.cancellation.is_cancelled() || !session.is_current(access.generation) {
                Err(Status::Revoked)
            } else {
                Ok(value)
            }
        }
        Err(error) => {
            let status = failure(&error);
            let _ = session.observe_failure(access.generation, &error).await;
            Err(status)
        }
    }
}
impl Cameras {
    pub(super) async fn catalog(
        &self,
        session: SessionCoordinator,
        api: CameraTransport,
        context: u64,
        cancel: CancellationToken,
    ) {
        let access = match session.access(&cancel).await {
            Ok(v) => v,
            Err(_) => {
                self.catalog_failed(context, Status::Revoked);
                return;
            }
        };
        let reply = response(
            &session,
            &access,
            api.list_cameras(
                wire::ListCamerasRequest::default(),
                access.token(),
                TIMEOUT,
                &cancel,
            )
            .await,
        )
        .await;
        if cancel.is_cancelled() {
            return;
        }
        let catalog = match reply.and_then(|v| admit::catalog(&v).map_err(|_| Status::Invalid)) {
            Ok(v) => v,
            Err(status) => {
                self.catalog_failed(context, status);
                return;
            }
        };
        let mut state = self.0.state.lock().expect("camera state");
        if state.context != context || !state.active || !session.is_current(access.generation) {
            return;
        }
        *self.0.source.lock().expect("camera source") = Some((session, access.generation));
        state.catalog = Arc::new(catalog);
        state.catalog_status = Status::Ready;
        if state.selected.is_none()
            && let Some(id) = state.catalog.first().map(|c| c.id.clone())
        {
            state.select(id);
        }
        self.retired(&state);
    }
    fn catalog_failed(&self, context: u64, status: Status) {
        let mut state = self.0.state.lock().expect("camera state");
        if state.context != context {
            return;
        }
        state.catalog = Arc::default();
        state.catalog_status = status;
        state.status = status;
        state.bump();
        self.retired(&state);
    }
    pub(super) async fn camera(
        &self,
        session: SessionCoordinator,
        api: CameraTransport,
        context: u64,
        id: CameraId,
        cancel: CancellationToken,
    ) {
        let access = match session.access(&cancel).await {
            Ok(v) => v,
            Err(_) => {
                self.failed(context, Status::Revoked);
                return;
            }
        };
        let access = Arc::new(access);
        let opened = response(
            &session,
            &access,
            api.open_session(
                wire::OpenSessionRequest {
                    entity_id: id.entity().as_str().into(),
                    ..Default::default()
                },
                access.token(),
                TIMEOUT,
                &cancel,
            )
            .await,
        )
        .await;
        let lease = match opened.and_then(|v| {
            admit::lease(&v, &id, SystemTime::now(), Instant::now()).map_err(|_| Status::Invalid)
        }) {
            Ok(admit::Outcome::Value(lease)) => lease,
            Ok(admit::Outcome::Refused(issue)) => {
                if !cancel.is_cancelled() {
                    self.failed(context, Status::refusal(issue))
                };
                return;
            }
            Err(status) => {
                if !cancel.is_cancelled() {
                    self.failed(context, status)
                };
                return;
            }
        };
        let result = self
            .follow(&session, &api, &access, &lease, context, &cancel)
            .await;
        if !cancel.is_cancelled()
            && let Err(status) = result
        {
            self.failed(context, status);
        }
        // Cleanup is an owned, bounded request on the original verified origin.
        // Never carry an old reference or access token into a replacement profile.
        let _ = api
            .close_session(
                wire::CloseSessionRequest {
                    session: lease.wire_reference().into(),
                    ..Default::default()
                },
                access.token(),
                Duration::from_secs(1),
                &CancellationToken::new(),
            )
            .await;
    }
    async fn follow(
        &self,
        session: &SessionCoordinator,
        api: &CameraTransport,
        access: &Arc<AccessLease>,
        lease: &admit::Lease,
        context: u64,
        cancel: &CancellationToken,
    ) -> Result<(), Status> {
        let work_cancel = cancel.child_token();
        let stream_cancel = cancel.child_token();
        let mut watch = response(
            session,
            access,
            api.watch_session(
                wire::WatchSessionRequest {
                    session: lease.wire_reference().into(),
                    ..Default::default()
                },
                access.token(),
                Duration::from_secs(65),
                &stream_cancel,
            )
            .await,
        )
        .await?;
        let ready = tokio::time::timeout(TIMEOUT, watch.next())
            .await
            .map_err(|_| Status::Offline)?;
        let ready = response(session, access, ready)
            .await?
            .ok_or(Status::Offline)?;
        let mut revision = match admit::watch(&ready, lease, 0, 0).map_err(|_| Status::Invalid)? {
            admit::WatchFrame::Ready(revision) => revision,
            admit::WatchFrame::Expired => return Err(Status::Expired),
            admit::WatchFrame::Revoked => return Err(Status::Revoked),
            admit::WatchFrame::ResyncRequired => return Err(Status::Offline),
            admit::WatchFrame::Changed(_) => return Err(Status::Invalid),
        };
        let mut sequence = 1;
        let mut jobs = JoinSet::new();
        let (mut event_busy, mut image_busy) = (false, false);
        let mut events_needed = true;
        let result = loop {
            let changed = self.0.changed.notified();
            tokio::pin!(changed);
            changed.as_mut().enable();
            if cancel.is_cancelled() {
                break Ok(());
            }
            if !session.is_current(access.generation) || access.cancellation.is_cancelled() {
                break Err(Status::Revoked);
            }
            let image = {
                let mut state = self.0.state.lock().expect("camera state");
                if state.context != context || !state.active {
                    break Ok(());
                }
                if !image_busy && state.image_requested && state.preview_visible {
                    let target = state.target.clone();
                    let eligible = target.as_ref().is_some_and(|key| match key {
                        ImageKey::Snapshot(_) => true,
                        ImageKey::EventThumbnail(key) => {
                            state.events.rows.iter().any(|event| &event.key == key)
                        }
                    });
                    if eligible {
                        state.image_requested = false;
                        target.map(|key| (key, state.image_cancel.clone()))
                    } else {
                        None
                    }
                } else {
                    None
                }
            };
            if events_needed && !event_busy {
                event_busy = true;
                events_needed = false;
                let (api, session, access, lease, token) = (
                    api.clone(),
                    session.clone(),
                    access.clone(),
                    lease.clone(),
                    work_cancel.child_token(),
                );
                jobs.spawn(async move {
                    ReadJob::Events(capture(&api, &session, &access, &lease, &token).await)
                });
            }
            if let Some((key, token)) = image {
                image_busy = true;
                let (api, session, access, lease) =
                    (api.clone(), session.clone(), access.clone(), lease.clone());
                jobs.spawn(async move {
                    let result = picture(&api, &session, &access, &lease, &key, &token).await;
                    ReadJob::Image {
                        key,
                        cancel: token,
                        result,
                    }
                });
            }
            tokio::select! {biased;
                ()=cancel.cancelled()=>break Ok(()),
                ()=access.cancellation.cancelled()=>break Err(Status::Revoked),
                _=tokio::time::sleep_until(tokio::time::Instant::from_std(lease.deadline()))=>break Err(Status::Expired),
                ()=&mut changed=>{},
                message=watch.next()=>{
                    let frame=match response(session,access,message).await {Ok(Some(v))=>v,Ok(None)=>break Err(Status::Offline),Err(status)=>break Err(status)};
                    match admit::watch(&frame,lease,sequence,revision).map_err(|_|Status::Invalid) {
                        Ok(admit::WatchFrame::Changed(next))=>{
                            sequence=frame.sequence;revision=next;events_needed=true;
                            let mut state=self.0.state.lock().expect("camera state");if state.context!=context {break Ok(())}
                            state.events=Events::default();state.picture=None;state.image_cancel.cancel();state.image_cancel=state.cancel.child_token();state.image_requested=state.preview_visible;state.status=Status::Loading;state.bump();self.retired(&state);
                        },
                        Ok(admit::WatchFrame::Expired)=>break Err(Status::Expired),
                        Ok(admit::WatchFrame::Revoked)=>break Err(Status::Revoked),
                        Ok(admit::WatchFrame::ResyncRequired)=>break Err(Status::Offline),
                        Ok(admit::WatchFrame::Ready(_))|Err(_)=>break Err(Status::Invalid),
                    }
                },
                result=jobs.join_next(),if !jobs.is_empty()=>{
                    match result {
                        Some(Ok(ReadJob::Events(result)))=>{
                            event_busy=false;
                            match result {
                                Ok(capture)=>{
                                    if capture.revision<revision {events_needed=true;continue}
                                    let mut state=self.0.state.lock().expect("camera state");if state.context!=context {break Ok(())}
                                    state.events=match Events::new(capture.rows,&state.events) {Ok(v)=>v,Err(status)=>break Err(status)};
                                    if let Some(ImageKey::EventThumbnail(key))=&state.target && !state.events.rows.iter().any(|e|&e.key==key) {state.picture=None;state.status=Status::Missing;state.image_requested=false;}
                                    state.bump();self.publish(&state);
                                },
                                Err(Status::Loading)=>events_needed=true,
                                Err(status)=>break Err(status),
                            }
                        },
                        Some(Ok(ReadJob::Image{key,cancel:local,result}))=>{
                            image_busy=false;let mut state=self.0.state.lock().expect("camera state");
                            if !state.accepts(context,Instant::now()) || local.is_cancelled() || !state.preview_visible || state.target.as_ref()!=Some(&key) {continue}
                            match result {
                                Ok(image)=>{if image.deadline>Instant::now(){state.picture=Some(image);state.status=Status::Ready}else{state.picture=None;state.status=Status::Expired}},
                                Err(Status::Missing)=>{state.picture=None;state.status=Status::Missing;},
                                Err(Status::Unavailable)=>{state.picture=None;state.status=Status::Unavailable;},
                                Err(status @ (Status::Expired|Status::Revoked|Status::WrongCamera|Status::Offline|Status::Invalid|Status::Unsupported))=>break Err(status),
                                Err(Status::Hidden)=>break Err(Status::Revoked),
                                Err(Status::Loading)=>break Err(Status::Offline),
                                Err(Status::Ready)=>break Err(Status::Invalid),
                            }
                            state.bump();self.publish(&state);
                        },
                        Some(Err(_))=>break Err(Status::Invalid),
                        None=>{},
                    }
                },
            }
        };
        stream_cancel.cancel();
        drop(watch);
        // Cancellation never abandons a blocking PNG decode or an HTTP owner.
        {
            let state = self.0.state.lock().expect("camera state");
            if state.context == context {
                state.image_cancel.cancel();
            }
        }
        work_cancel.cancel();
        while jobs.join_next().await.is_some() {}
        result
    }
}
struct Capture {
    revision: u64,
    rows: Vec<admit::Event>,
}
enum ReadJob {
    Events(Result<Capture, Status>),
    Image {
        key: ImageKey,
        cancel: CancellationToken,
        result: Result<Arc<Frame>, Status>,
    },
}
async fn capture(
    api: &CameraTransport,
    session: &SessionCoordinator,
    access: &AccessLease,
    lease: &admit::Lease,
    cancel: &CancellationToken,
) -> Result<Capture, Status> {
    tokio::time::timeout(Duration::from_secs(30), async {
        let mut rows = Vec::new();
        let mut identities = BTreeSet::new();
        let mut cursors = BTreeSet::new();
        let mut cursor = Vec::new();
        let mut revision = None;
        let mut total = None;
        loop {
            if cancel.is_cancelled() {
                return Err(Status::Hidden);
            }
            let reply = response(
                session,
                access,
                api.list_events(
                    wire::ListEventsRequest {
                        session: lease.wire_reference().into(),
                        page_size: admit::MAX_PAGE as u32,
                        cursor,
                        ..Default::default()
                    },
                    access.token(),
                    TIMEOUT,
                    cancel,
                )
                .await,
            )
            .await?;
            let page = match admit::events(&reply, lease, SystemTime::now(), Instant::now())
                .map_err(|_| Status::Invalid)?
            {
                admit::Outcome::Value(v) => v,
                admit::Outcome::Refused(issue) => return Err(Status::refusal(issue)),
            };
            if revision.is_some_and(|r| r != page.revision.get())
                || total.is_some_and(|t| t != page.total)
            {
                return Err(Status::Loading);
            }
            revision = Some(page.revision.get());
            total = Some(page.total);
            for event in page.events {
                if rows.last().is_some_and(|prior: &admit::Event| {
                    event.captured_at > prior.captured_at
                        || event.captured_at == prior.captured_at
                            && event.key.event() <= prior.key.event()
                }) || !identities.insert(event.key.clone())
                {
                    return Err(Status::Invalid);
                }
                rows.push(event);
                if rows.len() > page.total || rows.len() > admit::MAX_EVENTS {
                    return Err(Status::Invalid);
                }
            }
            match page.next {
                Some(next) => {
                    if rows.len() >= page.total || !cursors.insert(next.bytes().to_vec()) {
                        return Err(Status::Invalid);
                    };
                    cursor = next.bytes().to_vec();
                }
                None => {
                    if rows.len() != page.total {
                        return Err(Status::Invalid);
                    };
                    return Ok(Capture {
                        revision: page.revision.get(),
                        rows,
                    });
                }
            }
        }
    })
    .await
    .map_err(|_| Status::Offline)?
}
async fn picture(
    api: &CameraTransport,
    session: &SessionCoordinator,
    access: &AccessLease,
    lease: &admit::Lease,
    key: &ImageKey,
    cancel: &CancellationToken,
) -> Result<Arc<Frame>, Status> {
    let request = admit::image_request(lease, key).map_err(|_| Status::WrongCamera)?;
    let reply = response(
        session,
        access,
        api.read_image(request, access.token(), TIMEOUT, cancel)
            .await,
    )
    .await?;
    let image = match admit::image(&reply, lease, key, SystemTime::now(), Instant::now())
        .map_err(|_| Status::Invalid)?
    {
        admit::Outcome::Value(v) => v,
        admit::Outcome::Refused(issue) => return Err(Status::refusal(issue)),
    };
    if cancel.is_cancelled() {
        return Err(Status::Hidden);
    }
    let frame = tokio::task::spawn_blocking(move || camera::decode(image, Instant::now()))
        .await
        .map_err(|_| Status::Invalid)??;
    if cancel.is_cancelled()
        || access.cancellation.is_cancelled()
        || !session.is_current(access.generation)
    {
        return Err(Status::Hidden);
    }
    if frame.deadline <= Instant::now() {
        return Err(Status::Expired);
    }
    Ok(Arc::new(frame))
}
