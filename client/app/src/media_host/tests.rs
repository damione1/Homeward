use super::*;
use home_client_session::{SecureStore, StoreFailure};
use slint::Model;

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
fn ready(owner: &Media, session: &SessionCoordinator) {
    let data = Data::new(crate::media::tests::snapshot(), &"reader".parse().unwrap()).unwrap();
    let mut state = owner.0.state.lock().unwrap();
    state.foreground = true;
    state.interactive = true;
    state.active = true;
    state.entity = Some("reader".parse().unwrap());
    state.apply(Arc::new(data), session.generation(), Picture::Absent);
}

#[test]
fn a_new_view_cohort_of_the_same_entity_cancels_unsent_media_intent() {
    let owner = Media::detached();
    let session = session();
    let view = ProjectionMailbox::<()>::new();
    owner.choose(Some("reader".parse().unwrap()), view.generation());
    ready(&owner, &session);
    let generation = owner.0.state.lock().unwrap().generation;
    owner.operation(1, generation, ProMediaOperation::Play);
    assert!(owner.0.state.lock().unwrap().pending.is_some());
    let next = view.invalidate().unwrap();
    owner.choose(Some("reader".parse().unwrap()), next);
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
    snapshot.volume_basis_points = Some(1000);
    {
        let mut state = owner.0.state.lock().unwrap();
        state.apply(
            Arc::new(Data::new(snapshot.clone(), &"reader".parse().unwrap()).unwrap()),
            session.generation(),
            Picture::Absent,
        );
        assert_eq!(state.generation, generation);
        assert_eq!(state.volume.text, "72.00");
        assert!(state.volume.gesture);
    }
    snapshot
        .capabilities
        .modify(|c| c.volume_basis_points = Default::default());
    {
        let mut state = owner.0.state.lock().unwrap();
        state.apply(
            Arc::new(Data::new(snapshot, &"reader".parse().unwrap()).unwrap()),
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
    let window = HomewardWindow::new().unwrap();
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
