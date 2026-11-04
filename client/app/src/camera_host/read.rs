use super::*;
use crate::camera::{self, Frame};
use buffa::Message;
use home_client_model::api_content::MediaSourceName;
use home_client_session::AccessLease;
use home_client_transport::{Failure, FailureKind, RpcCode};
use home_wire_contract::{camera_content as admit, content_contract, home::api::v2 as wire};
use std::{collections::BTreeSet, time::SystemTime};
const TIMEOUT: Duration = Duration::from_secs(5);
fn failure(error: &Failure) -> Status {
    match error.api_failure() {
        Err(_) => Status::Invalid,
        Ok(Some(error)) => {
            use wire::ApiErrorReason as R;
            match error.reason() {
                R::AccessDenied | R::AuthenticationRequired => Status::Revoked,
                R::MethodNotSupported => Status::Unsupported,
                R::ServiceUnavailable | R::RequestDeadlineExceeded | R::RequestCancelled => {
                    Status::Offline
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
                | R::QueryBudgetExceeded => Status::Unavailable,
                R::AuthorityUnavailable
                | R::ValidationFailed
                | R::ResourceAlreadyExists
                | R::RequestIdReused
                | R::EtagMismatch
                | R::SourceReadOnly
                | R::ConfigRestartRequired
                | R::CommitOutcomeUnknown
                | R::EffectOutcomeUnknown
                | R::InternalFailure
                | R::StoredDataIntegrity
                | R::Unspecified => Status::Invalid,
            }
        }
        Ok(None) => {
            if matches!(
                error.kind(),
                FailureKind::Rpc(RpcCode::PermissionDenied | RpcCode::Unauthenticated)
            ) {
                Status::Revoked
            } else if error.may_resume_read() {
                Status::Offline
            } else {
                Status::Invalid
            }
        }
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
        api: MediaTransport,
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
        let result = async {
            let homes = response(
                &session,
                &access,
                api.home()
                    .list_homes(
                        wire::ListHomesRequest {
                            page_size: 2,
                            ..Default::default()
                        },
                        access.token(),
                        TIMEOUT,
                        &cancel,
                    )
                    .await,
            )
            .await?;
            if !homes.__buffa_unknown_fields.is_empty()
                || homes.homes.len() != 1
                || !homes.next_page_token.is_empty()
                || !homes.homes[0].__buffa_unknown_fields.is_empty()
            {
                return Err(Status::Invalid);
            }
            let home = home_client_model::api::HomeName::try_from(homes.homes[0].name.as_str())
                .map_err(|_| Status::Invalid)?;
            let mut catalog = Vec::new();
            let mut tokens = BTreeSet::new();
            let mut token = String::new();
            let mut previous = None;
            let mut pages = 0;
            loop {
                let page = response(
                    &session,
                    &access,
                    api.list_media_sources(
                        wire::ListMediaSourcesRequest {
                            parent: home.as_str().to_owned(),
                            page_size: 256,
                            page_token: token,
                            ..Default::default()
                        },
                        access.token(),
                        TIMEOUT,
                        &cancel,
                    )
                    .await,
                )
                .await?;
                let sources =
                    content_contract::source_page(&page, &home).map_err(|_| Status::Invalid)?;
                for source in &sources {
                    if previous.as_ref().is_some_and(|name| name >= source.name()) {
                        return Err(Status::Invalid);
                    }
                    previous = Some(source.name().clone());
                }
                catalog.extend(admit::catalog(&page, &home).map_err(|_| Status::Invalid)?);
                pages += 1;
                if catalog.len() > admit::MAX_CATALOG || pages > 128 {
                    return Err(Status::Invalid);
                }
                if page.next_page_token.is_empty() {
                    return Ok(catalog);
                }
                if !tokens.insert(page.next_page_token.clone()) {
                    return Err(Status::Invalid);
                }
                token = page.next_page_token;
            }
        }
        .await;
        if cancel.is_cancelled() {
            return;
        }
        let catalog = match result {
            Ok(catalog) => catalog,
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
        api: MediaTransport,
        context: u64,
        id: MediaSourceName,
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
        let delete_request_id = match home_client_actions::new_request_id() {
            Ok(id) => id,
            Err(_) => {
                self.failed(context, Status::Invalid);
                return;
            }
        };
        let request = match content_contract::create_session_request(
            &id,
            home_client_model::api_content::ContentKind::Camera,
            None,
            match home_client_actions::new_request_id() {
                Ok(id) => id,
                Err(_) => {
                    self.failed(context, Status::Invalid);
                    return;
                }
            },
        ) {
            Ok(request) => request,
            Err(_) => {
                self.failed(context, Status::Invalid);
                return;
            }
        };
        let opened = response(
            &session,
            &access,
            api.create_media_session(request, access.token(), TIMEOUT, &cancel)
                .await,
        )
        .await;
        let lease = match opened.and_then(|v| {
            admit::lease(
                &v,
                &id,
                home_client_model::api_content::ContentKind::Camera,
                SystemTime::now(),
                Instant::now(),
            )
            .map_err(|_| Status::Invalid)
        }) {
            Ok(lease) => lease,
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
    async fn follow(
        &self,
        session: &SessionCoordinator,
        api: &MediaTransport,
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
            api.watch_media_session(
                wire::WatchMediaSessionRequest {
                    name: lease.name().as_str().to_owned(),
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
        let mut revision =
            match admit::watch(&ready, lease, 0, None).map_err(|_| Status::Invalid)? {
                admit::WatchFrame::Ready(revision) => revision,
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
                        ImageKey::EventThumbnail(key) => state
                            .events
                            .rows
                            .iter()
                            .any(|event| &event.key == key && event.has_image),
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
                    // Local stream sequence fences in-flight pages; opaque etags have no ordering.
                    ReadJob::Events {
                        sequence,
                        result: capture(&api, &session, &access, &lease, &token).await,
                    }
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
                    match admit::watch(&frame,lease,sequence,Some(&revision)).map_err(|_|Status::Invalid) {
                        Ok(admit::WatchFrame::Changed(next))=>{
                            sequence=match u64::try_from(frame.sequence){Ok(sequence)=>sequence,Err(_)=>break Err(Status::Invalid)};revision=next;events_needed=true;
                            let mut state=self.0.state.lock().expect("camera state");if state.context!=context {break Ok(())}
                            state.events=Events::default();state.picture=None;state.image_cancel.cancel();state.image_cancel=state.cancel.child_token();state.image_requested=state.preview_visible;state.status=Status::Loading;state.bump();self.retired(&state);
                        },
                        Ok(admit::WatchFrame::Ready(_))|Err(_)=>break Err(Status::Invalid),
                    }
                },
                result=jobs.join_next(),if !jobs.is_empty()=>{
                    match result {
                        Some(Ok(ReadJob::Events{sequence:captured_sequence,result}))=>{
                            event_busy=false;
                            match result {
                                Ok(capture)=>{
                                    if captured_sequence!=sequence {events_needed=true;continue}
                                    let mut state=self.0.state.lock().expect("camera state");if state.context!=context {break Ok(())}
                                    if let Err(status) = state.replace_events(capture.rows) { break Err(status); }
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
                                Ok(image)=>{if image.session!=*lease.name(){break Err(Status::Invalid)};if image.deadline>Instant::now(){state.picture=Some(image);state.status=Status::Ready}else{state.picture=None;state.status=Status::Expired}},
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
    rows: Vec<admit::Event>,
}
enum ReadJob {
    Events {
        sequence: u64,
        result: Result<Capture, Status>,
    },
    Image {
        key: ImageKey,
        cancel: CancellationToken,
        result: Result<Arc<Frame>, Status>,
    },
}
async fn capture(
    api: &MediaTransport,
    session: &SessionCoordinator,
    access: &AccessLease,
    lease: &admit::Lease,
    cancel: &CancellationToken,
) -> Result<Capture, Status> {
    tokio::time::timeout(Duration::from_secs(30), async {
        let mut rows = Vec::new();
        let mut identities = BTreeSet::new();
        let mut cursors = BTreeSet::new();
        let mut cursor = String::new();
        let mut revision = None;
        let mut total = None;
        let mut pages = 0usize;
        let mut bytes = 0usize;
        loop {
            if cancel.is_cancelled() {
                return Err(Status::Hidden);
            }
            let reply = response(
                session,
                access,
                api.query_media_session_events(
                    wire::QueryMediaSessionEventsRequest {
                        name: lease.name().as_str().to_owned(),
                        page_size: admit::MAX_PAGE as i32,
                        page_token: cursor,
                        ..Default::default()
                    },
                    access.token(),
                    TIMEOUT,
                    cancel,
                )
                .await,
            )
            .await?;
            pages += 1;
            bytes = bytes
                .saturating_add(reply.encoded_len() as usize * 8)
                .saturating_add(reply.media_events.len() * 512);
            if pages > admit::MAX_EVENTS + 1 || bytes > 32 * 1024 * 1024 {
                return Err(Status::Invalid);
            }
            let page = admit::events(&reply, lease, SystemTime::now(), Instant::now())
                .map_err(|_| Status::Invalid)?;
            if revision.as_ref().is_some_and(|r| r != &page.etag)
                || total.is_some_and(|t| t != page.total)
            {
                return Err(Status::Loading);
            }
            revision = Some(page.etag.clone());
            total = Some(page.total);
            for event in page.events {
                if rows
                    .last()
                    .is_some_and(|prior: &admit::Event| event.key.event() <= prior.key.event())
                    || !identities.insert(event.key.clone())
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
                    if !cursors.insert(next.as_str().to_owned()) {
                        return Err(Status::Invalid);
                    };
                    cursor = next.as_str().to_owned();
                }
                None => {
                    if rows.len() != page.total {
                        return Err(Status::Invalid);
                    };
                    return Ok(Capture { rows });
                }
            }
        }
    })
    .await
    .map_err(|_| Status::Offline)?
}
async fn picture(
    api: &MediaTransport,
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
        api.read_media_session(request, access.token(), TIMEOUT, cancel)
            .await,
    )
    .await?;
    let image = admit::image(&reply, lease, key, SystemTime::now(), Instant::now())
        .map_err(|_| Status::Invalid)?;
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
