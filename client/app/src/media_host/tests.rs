use super::*;
use crate::media::tests::media;
use home_client_actions::capabilities::CapabilitySubmission;
use home_client_session::{SecureStore, StoreFailure};
use home_wire_contract::api_playback_contract::playback;
use slint::Model;

#[tokio::test]
async fn renewal_media_resumes_in_both_publication_orders_and_expires_only_old_pixels() {
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    for applied_first in [false, true] {
        let credentials = crate::renewal_tests::Credentials::new().await;
        let entity = EntityName::try_from("homes/test/entities/player").unwrap();
        let source = credentials.source(entity.as_str()).await;
        let owner = Media::detached();
        let view = ProjectionMailbox::<()>::new().generation();
        owner.choose_presented(Some(entity.clone()), view, Some(source.clone()));
        ready(&owner, &credentials.session);
        let (old, cancel) = {
            let state = owner.0.state.lock().unwrap();
            render::render(
                &window,
                render::Projection::new(
                    &state,
                    Some((
                        credentials.session.clone(),
                        credentials.session.generation(),
                    )),
                ),
            );
            (state.generation, state.read_cancel.clone())
        };
        assert!(window.get_media_has_data());
        credentials.renew().await;
        assert!(!owner.0.state.lock().unwrap().authorized());
        let fresh = credentials.source(entity.as_str()).await;
        if !applied_first {
            owner
                .0
                .state
                .lock()
                .unwrap()
                .reconcile_session(credentials.session.generation());
        }
        owner.choose_presented(Some(entity.clone()), view, Some(fresh.clone()));
        if applied_first {
            owner
                .0
                .state
                .lock()
                .unwrap()
                .reconcile_session(credentials.session.generation());
        }
        let (generation, in_flight) = {
            let mut state = owner.0.state.lock().unwrap();
            assert!(cancel.is_cancelled());
            assert_ne!(state.generation, old);
            assert!(!state.retention.unwrap().waiting && state.read_pending);
            assert!(state.admits_read(
                state.generation,
                &Some(fresh.clone()),
                credentials.session.generation()
            ));
            assert!(!state.authorized());
            render::render(
                &window,
                render::Projection::new(
                    &state,
                    Some((credentials.session.clone(), source.source().generation())),
                ),
            );
            assert!(window.get_media_has_data() && !window.get_media_editable());
            state.retention.as_mut().unwrap().deadline = (chrono::Utc::now().timestamp() - 1, 0);
            state.renewal = Some(source.bounded(state.retention.unwrap().deadline));
            (state.generation, state.read_cancel.clone())
        };
        owner.expire_retention(generation);
        {
            let state = owner.0.state.lock().unwrap();
            assert!(state.data.is_none() && state.renewal.is_none());
            assert!(
                !in_flight.is_cancelled(),
                "fresh read survives the old pixels' deadline"
            );
            assert!(state.admits_read(generation, &Some(fresh), credentials.session.generation()));
        }
        ready(&owner, &credentials.session);
        assert!(owner.0.state.lock().unwrap().authorized());
        credentials.session.close().await;
    }
}

#[tokio::test]
async fn renewal_media_local_visibility_and_fresh_scope_both_wake_the_reader() {
    let credentials = crate::renewal_tests::Credentials::new().await;
    let entity = EntityName::try_from("homes/test/entities/player").unwrap();
    let source = credentials.source(entity.as_str()).await;
    for choose_first in [false, true] {
        let owner = Media::detached();
        let view = ProjectionMailbox::<()>::new().generation();
        if choose_first {
            owner.choose_presented(Some(entity.clone()), view, Some(source.clone()));
        }
        owner.interactive(true);
        if !choose_first {
            owner.choose_presented(Some(entity.clone()), view, Some(source.clone()));
        }
        for visible in [false, true] {
            owner.interactive(visible);
        }
        for foreground in [false, true] {
            owner.foreground(foreground);
        }
        let mut state = owner.0.state.lock().unwrap();
        state.active = true;
        assert!(state.read_pending && state.data.is_none());
        assert!(state.admits_read(
            state.generation,
            &Some(source.clone()),
            credentials.session.generation()
        ));
        state.presentation = None;
        drop(state);
        // Consume previous wake before installing the missing fresh provenance.
        while tokio::time::timeout(Duration::from_millis(1), owner.0.changed.notified())
            .await
            .is_ok()
        {}
        owner.choose_presented(Some(entity.clone()), view, Some(source.clone()));
        tokio::time::timeout(Duration::from_secs(1), owner.0.changed.notified())
            .await
            .unwrap();
    }
    credentials.session.close().await;
}

#[tokio::test]
async fn renewal_media_access_cannot_send_an_old_capture_read_with_the_new_credential() {
    let credentials = crate::renewal_tests::Credentials::new().await;
    let entity = EntityName::try_from("homes/test/entities/player").unwrap();
    let source = credentials.source(entity.as_str()).await;
    let owner = Media::detached();
    let view = ProjectionMailbox::<()>::new().generation();
    owner.choose_presented(Some(entity.clone()), view, Some(source.clone()));
    ready(&owner, &credentials.session);
    let context = {
        let state = owner.0.state.lock().unwrap();
        state::ReadContext {
            generation: state.generation,
            cancel: state.read_cancel.clone(),
            source: Some(source),
        }
    };
    let (transport, count, server) = crate::renewal_tests::counted_transport().await;
    credentials.expire_access();
    let (reader, session, api, target) = (
        owner.clone(),
        credentials.session.clone(),
        transport.media(),
        entity.clone(),
    );
    let read = tokio::spawn(async move {
        reader.read(session, api, target, context, None).await;
    });
    credentials.wait_refresh().await;
    credentials.release_refresh();
    tokio::time::timeout(Duration::from_secs(2), read)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(count.load(std::sync::atomic::Ordering::SeqCst), 0);
    let fresh = credentials.source(entity.as_str()).await;
    owner.choose_presented(Some(entity.clone()), view, Some(fresh.clone()));
    let context = {
        let state = owner.0.state.lock().unwrap();
        state::ReadContext {
            generation: state.generation,
            cancel: state.read_cancel.clone(),
            source: Some(fresh),
        }
    };
    tokio::time::timeout(
        Duration::from_secs(2),
        owner.read(
            credentials.session.clone(),
            transport.media(),
            entity,
            context,
            None,
        ),
    )
    .await
    .unwrap();
    assert!(
        count.load(std::sync::atomic::Ordering::SeqCst) > 0,
        "fresh capture can start its own read"
    );
    server.abort();
    credentials.session.close().await;
}

struct EmptyStore;
impl SecureStore for EmptyStore {
    fn load(&self) -> Result<Option<zeroize::Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(None)
    }
    fn save(&self, _: &[u8]) -> Result<(), StoreFailure> {
        Ok(())
    }
    fn clear(&self) -> Result<(), StoreFailure> {
        Ok(())
    }
}
fn session() -> SessionCoordinator {
    let connection = home_client_transport::Connection::new(
        "https://127.0.0.1:9",
        include_bytes!("../../tests/fixtures/trust-a.pem"),
    )
    .unwrap();
    SessionCoordinator::new(connection.auth(), Arc::new(EmptyStore))
}
#[test]
fn model_publication_does_not_reenter_state_from_gesture_cancellation() {
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    let owner = Media::bind(&window);
    let (locked, wait_locked) = std::sync::mpsc::channel();
    let (release, wait_release) = std::sync::mpsc::channel();
    let held = owner.clone();
    let blocker = std::thread::spawn(move || {
        let _state = held.0.state.lock().unwrap();
        locked.send(()).unwrap();
        wait_release.recv_timeout(Duration::from_secs(2)).is_ok()
    });
    wait_locked.recv_timeout(Duration::from_secs(2)).unwrap();
    window.set_media_publishing(true);
    window.invoke_media_edit(
        1,
        1,
        Field::Volume.token(),
        ProValueIntent {
            event: ProValueEvent::Cancel,
            thumb: RangeThumb::Lower,
            ..Default::default()
        },
    );
    let _ = release.send(());
    assert!(
        blocker.join().unwrap(),
        "programmatic cancellation waited for the state lock"
    );
    window.set_media_publishing(false);
}
fn ready(owner: &Media, session: &SessionCoordinator) {
    let data = Data::new(
        playback(&crate::media::tests::snapshot()).unwrap().unwrap(),
        &EntityName::try_from("homes/test/entities/player").unwrap(),
    )
    .unwrap();
    let mut state = owner.0.state.lock().unwrap();
    state.foreground = true;
    state.interactive = true;
    state.active = true;
    state.entity = Some(EntityName::try_from("homes/test/entities/player").unwrap());
    state.apply(Arc::new(data), session.generation(), Picture::Absent);
}

#[test]
fn a_new_view_cohort_of_the_same_entity_cancels_unsent_media_intent() {
    let owner = Media::detached();
    let session = session();
    let view = ProjectionMailbox::<()>::new();
    owner.choose(
        Some(EntityName::try_from("homes/test/entities/player").unwrap()),
        view.generation(),
    );
    ready(&owner, &session);
    let generation = owner.0.state.lock().unwrap().generation;
    owner.operation(1, generation, ProMediaOperation::Play);
    assert!(owner.0.state.lock().unwrap().pending.is_some());
    let next = view.invalidate().unwrap();
    owner.choose(
        Some(EntityName::try_from("homes/test/entities/player").unwrap()),
        next,
    );
    owner.operation(1, generation, ProMediaOperation::Play);
    let state = owner.0.state.lock().unwrap();
    assert!(state.pending.is_none() && state.data.is_none());
    assert_ne!(state.generation, generation);
}

#[test]
fn polling_preserves_a_draft_but_capability_withdrawal_cancels_its_gesture() {
    let owner = Media::detached();
    let session = session();
    ready(&owner, &session);
    let generation = owner.0.state.lock().unwrap().generation;
    owner.edit(
        1,
        generation,
        Field::Volume.token(),
        ProValueIntent {
            event: ProValueEvent::Start,
            thumb: RangeThumb::Lower,
            ..Default::default()
        },
    );
    owner.edit(
        1,
        generation,
        Field::Volume.token(),
        ProValueIntent {
            event: ProValueEvent::Position,
            thumb: RangeThumb::Lower,
            position: 0.72,
            ..Default::default()
        },
    );
    let mut snapshot = crate::media::tests::snapshot();
    media(&mut snapshot)
        .observation
        .as_option_mut()
        .unwrap()
        .volume_basis_points = Some(1000);
    {
        let mut state = owner.0.state.lock().unwrap();
        state.apply(
            Arc::new(
                Data::new(
                    playback(&snapshot).unwrap().unwrap(),
                    &EntityName::try_from("homes/test/entities/player").unwrap(),
                )
                .unwrap(),
            ),
            session.generation(),
            Picture::Absent,
        );
        assert_eq!(state.generation, generation);
        assert_eq!(state.volume.text, "72.00");
        assert!(state.volume.gesture);
    }
    media(&mut snapshot)
        .definition
        .modify(|c| c.volume_basis_points = Default::default());
    {
        let mut state = owner.0.state.lock().unwrap();
        state.apply(
            Arc::new(
                Data::new(
                    playback(&snapshot).unwrap().unwrap(),
                    &EntityName::try_from("homes/test/entities/player").unwrap(),
                )
                .unwrap(),
            ),
            session.generation(),
            Picture::Absent,
        );
        assert_ne!(state.generation, generation);
        assert!(!state.volume.gesture);
    }
    owner.edit(
        1,
        generation,
        Field::Volume.token(),
        ProValueIntent {
            event: ProValueEvent::Commit,
            thumb: RangeThumb::Lower,
            ..Default::default()
        },
    );
    assert!(owner.0.state.lock().unwrap().pending.is_none());
}

#[test]
fn backgrounding_discards_the_intention_and_resume_has_no_write_to_replay() {
    let owner = Media::detached();
    let session = session();
    ready(&owner, &session);
    let generation = owner.0.state.lock().unwrap().generation;
    owner.operation(1, generation, ProMediaOperation::Play);
    owner.foreground(false);
    owner.foreground(true);
    let state = owner.0.state.lock().unwrap();
    assert!(state.pending.is_none() && state.data.is_none() && state.action.is_none());
    assert!(state.read_pending);
    assert_ne!(state.generation, generation);
}

#[tokio::test]
async fn queued_media_projection_cannot_publish_private_content_after_session_close() {
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    let owner = Media::detached();
    let session = session();
    ready(&owner, &session);
    let projection = render::Projection::new(
        &owner.0.state.lock().unwrap(),
        Some((session.clone(), session.generation())),
    );
    let mailbox = ProjectionMailbox::new();
    let Publication::Schedule(ticket) = mailbox.publish(mailbox.generation(), projection) else {
        panic!("first projection must schedule")
    };
    session.close().await;
    ticket.deliver(|_, value| render::render(&window, value));
    assert!(!window.get_media_visible() && !window.get_media_has_data());
    assert!(window.get_media_data().title.is_empty());
    assert_eq!(window.get_media_artwork_id(), 0);
    assert_eq!(window.get_media_values().row_count(), 0);
}

#[test]
fn capture_suspension_revokes_commands_retains_media_and_revalidates_before_reopening() {
    let owner = Media::detached();
    let session = session();
    let view = ProjectionMailbox::<()>::new().generation();
    let entity = EntityName::try_from("homes/test/entities/player").unwrap();
    owner.choose(Some(entity.clone()), view);
    ready(&owner, &session);
    let (old, cancel, data) = {
        let state = owner.0.state.lock().unwrap();
        (
            state.generation,
            state.read_cancel.clone(),
            state.data.clone().unwrap(),
        )
    };
    owner.operation(1, old, ProMediaOperation::Play);
    let deadline = (chrono::Utc::now().timestamp() + 30, 0);
    owner.suspend(Some(entity.clone()), view, deadline);
    let paused = {
        let state = owner.0.state.lock().unwrap();
        assert!(cancel.is_cancelled());
        assert!(!state.authorized() && state.pending.is_none() && !state.read_pending);
        assert!(Arc::ptr_eq(state.data.as_ref().unwrap(), &data));
        assert!(state.retention.unwrap().waiting);
        state.generation
    };
    owner.operation(1, old, ProMediaOperation::Play);
    owner.operation(1, paused, ProMediaOperation::Play);
    owner.reload(paused);
    assert!(owner.0.state.lock().unwrap().pending.is_none());
    assert!(!owner.0.state.lock().unwrap().read_pending);
    owner.choose(Some(entity), view);
    {
        let state = owner.0.state.lock().unwrap();
        assert!(!state.retention.unwrap().waiting && state.read_pending);
        assert!(!state.authorized());
        assert!(Arc::ptr_eq(state.data.as_ref().unwrap(), &data));
    }
    ready(&owner, &session);
    assert!(owner.0.state.lock().unwrap().authorized());
    assert!(owner.0.state.lock().unwrap().retention.is_none());
    owner.foreground(false);
    assert!(owner.0.state.lock().unwrap().data.is_none());
}

#[test]
fn retained_media_expires_even_after_dashboard_joined_and_authoritative_failure_clears() {
    let owner = Media::detached();
    let session = session();
    let view = ProjectionMailbox::<()>::new().generation();
    let entity = EntityName::try_from("homes/test/entities/player").unwrap();
    owner.choose(Some(entity.clone()), view);
    ready(&owner, &session);
    owner.suspend(
        Some(entity.clone()),
        view,
        (chrono::Utc::now().timestamp() - 1, 0),
    );
    owner.choose(Some(entity.clone()), view);
    let generation = owner.0.state.lock().unwrap().generation;
    owner.expire_retention(generation);
    {
        let state = owner.0.state.lock().unwrap();
        assert!(state.data.is_none() && state.entity.is_none() && state.retention.is_none());
    }
    owner.choose(Some(entity.clone()), view);
    ready(&owner, &session);
    owner.suspend(Some(entity), view, (chrono::Utc::now().timestamp() + 30, 0));
    let generation = owner.0.state.lock().unwrap().generation;
    owner.read_failed(generation, Status::Denied);
    assert!(owner.0.state.lock().unwrap().data.is_none());
}

#[test]
fn a_second_pending_cohort_revokes_the_inflight_revalidation_without_extending_retention() {
    let owner = Media::detached();
    let session = session();
    let view = ProjectionMailbox::<()>::new().generation();
    let entity = EntityName::try_from("homes/test/entities/player").unwrap();
    owner.choose(Some(entity.clone()), view);
    ready(&owner, &session);
    let deadline = (chrono::Utc::now().timestamp() + 30, 0);
    owner.suspend(Some(entity.clone()), view, deadline);
    owner.choose(Some(entity.clone()), view);
    let (old, cancel) = {
        let state = owner.0.state.lock().unwrap();
        (state.generation, state.read_cancel.clone())
    };
    owner.suspend(Some(entity), view, (deadline.0 + 30, 0));
    owner.read_failed(old, Status::Denied);
    let state = owner.0.state.lock().unwrap();
    assert!(cancel.is_cancelled());
    assert_ne!(state.generation, old);
    assert!(state.retention.unwrap().waiting && !state.read_pending && !state.authorized());
    assert_eq!(state.retention.unwrap().deadline, deadline);
    assert!(state.data.is_some());
}

#[test]
fn expired_retained_media_is_removed_at_delivery_without_waiting_for_a_timer() {
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    let owner = Media::detached();
    let session = session();
    ready(&owner, &session);
    let projection = {
        let state = owner.0.state.lock().unwrap();
        render::Projection::new(&state, Some((session.clone(), session.generation())))
    };
    render::render(&window, projection);
    assert!(window.get_media_has_data());
    let (projection, old, cancelled) = {
        let mut state = owner.0.state.lock().unwrap();
        state.picture = Picture::Loading;
        state.retention = Some(state::Retention {
            deadline: (chrono::Utc::now().timestamp() - 1, 0),
            waiting: false,
        });
        (
            render::Projection::new(&state, Some((session.clone(), session.generation()))),
            state.generation,
            state.read_cancel.clone(),
        )
    };
    let Publication::Schedule(ticket) = owner
        .0
        .mailbox
        .publish(owner.0.mailbox.generation(), projection)
    else {
        panic!("first scheduled publication");
    };
    Media::deliver(&Arc::downgrade(&owner.0), &window, ticket);
    assert!(!window.get_media_has_data() && !window.get_media_visible());
    assert_eq!(window.get_media_retention_ms(), 0);
    assert!(cancelled.is_cancelled());
    owner.read_failed(old, Status::Offline);
    let state = owner.0.state.lock().unwrap();
    assert!(state.data.is_none() && matches!(state.picture, Picture::Absent));
    assert!(state.retention.is_none() && state.entity.is_none());
    assert_ne!(state.generation, old);
    assert_eq!(
        state.status,
        Status::Hidden,
        "the old read result remains rejected"
    );
}

#[test]
fn media_success_and_pending_are_silent_while_unverified_and_failures_remain_visible() {
    use crate::device_view::progress::tests::snapshot;
    use home_wire_contract::home::api::v2 as wire;
    let confirmed = snapshot(Some(wire::CommandOutcome::Confirmed));
    assert_eq!(
        progress::presentation(Some(&confirmed)),
        (crate::ProActionPhase::Confirmed, "", "")
    );
    let pending = snapshot(None);
    assert_eq!(
        progress::presentation(Some(&pending)),
        (crate::ProActionPhase::Acknowledged, "", "")
    );
    let terminal = snapshot(Some(wire::CommandOutcome::AcknowledgedUnverified));
    assert_eq!(
        progress::presentation(Some(&terminal)).0,
        crate::ProActionPhase::AcknowledgedUnverified
    );
    assert!(!progress::presentation(Some(&terminal)).1.is_empty());
    let mut refused = pending;
    refused.operation = None;
    refused.submission = CapabilitySubmission::Rejected;
    assert!(!progress::presentation(Some(&refused)).1.is_empty());
}

#[test]
fn retained_media_revalidation_keeps_observed_content_without_a_loading_banner() {
    use i_slint_backend_testing::ElementQuery;
    use slint::ComponentHandle;
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window.set_show_endpoint(false);
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_view_loaded(true);
    window.set_connection_status(crate::ConnectionStatus::Live);
    window.set_page(crate::ViewPage::Detail);
    window.show().unwrap();
    let owner = Media::detached();
    let session = session();
    ready(&owner, &session);
    for waiting in [true, false] {
        let projection = {
            let mut state = owner.0.state.lock().unwrap();
            state.status = Status::Loading;
            state.retention = Some(state::Retention {
                deadline: (chrono::Utc::now().timestamp() + 30, 0),
                waiting,
            });
            render::Projection::new(&state, Some((session.clone(), session.generation())))
        };
        render::render(&window, projection);
        assert!(window.get_media_has_data() && window.get_media_visible());
        assert!(!window.get_media_editable());
        assert!(window.get_media_notice().is_empty());
    }
    let generation = owner.0.state.lock().unwrap().generation;
    owner.read_failed(generation, Status::Denied);
    let projection = {
        let state = owner.0.state.lock().unwrap();
        render::Projection::new(&state, Some((session.clone(), session.generation())))
    };
    render::render(&window, projection);
    assert!(!window.get_media_has_data());
    assert!(!window.get_media_notice().is_empty());
    i_slint_backend_testing::mock_elapsed_time(Duration::from_millis(1));
    assert_eq!(
        ElementQuery::from_root(&window)
            .match_inherits("HomewardMedia")
            .find_all()
            .len(),
        1,
        "a denied reading must remain visible with history closed"
    );
    assert!(
        ElementQuery::from_root(&window)
            .match_predicate(|element| element.accessible_label().as_deref()
                == Some("Media access denied. Private data was removed."))
            .find_first()
            .is_some()
    );
}
