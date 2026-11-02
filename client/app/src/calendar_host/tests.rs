//! Local owner invariants only. Network/authorization evidence uses the isolated
//! Go TLS fixture; these tests never log in or contact an installation.
use super::*;
use home_client_cache::calendars::CalendarCapture;
use home_client_session::{SecureStore, StoreFailure};
use slint::Model;

#[test]
fn a_superseded_read_releases_its_own_busy_slot_without_unlocking_another_context() {
    let mut state = State {
        busy: true,
        ..Default::default()
    };
    let (context, generation) = (state.context, state.generation);
    state.bump();
    assert!(!state.finish_read_job(context, generation));
    assert!(!state.busy);
    state.busy = true;
    state.reset_reads();
    assert!(!state.finish_read_job(context, generation));
    assert!(state.busy);
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
fn ready(state: &mut State, session: &SessionCoordinator) {
    let id = wire::CalendarId {
        entity_id: "synthetic-calendar".into(),
        ..Default::default()
    };
    let calendar = wire::Calendar {
        id: id.clone().into(),
        origin: wire::Origin::Local.into(),
        name: Some("Synthetic calendar".into()),
        revision: wire::Revision {
            resource: wire::EventKey {
                calendar: id.clone().into(),
                ..Default::default()
            }
            .into(),
            presence: wire::Presence::Present.into(),
            number: 1,
            ..Default::default()
        }
        .into(),
        quality: wire::Quality::Fresh.into(),
        provenance: wire::DataProvenance::Synthetic.into(),
        mutation_access: wire::MutationAccess::Allowed.into(),
        ..Default::default()
    };
    state.catalog = vec![calendar.clone()];
    state.selected = admit::calendar_id(&id);
    state.active = true;
    state.interactive = true;
    state.foreground = true;
    state.status = Status::Ready;
    state.source = Some(session.generation());
    state.start = "2026-11-01".into();
    state.end = "2026-11-08".into();
    state.zone = "America/Toronto".into();
    let mut capture = CalendarCapture::new(&calendar, &state.query().unwrap()).unwrap();
    capture
        .push(&wire::QueryOccurrencesResponse {
            revision: calendar.revision.clone(),
            quality: wire::Quality::Fresh.into(),
            provenance: wire::DataProvenance::Synthetic.into(),
            ..Default::default()
        })
        .unwrap();
    state.snapshot(
        Arc::new(capture.finish(&calendar).unwrap()),
        session.generation(),
    );
}
#[test]
fn an_uncertain_calendar_write_stays_disabled_until_an_explicit_read() {
    let owner = Calendars::detached();
    let session = session();
    let generation = {
        let mut state = owner.0.state.lock().unwrap();
        ready(&mut state, &session);
        state.uncertain = true;
        assert!(!state.editable());
        state.generation
    };
    owner.command(generation, CalendarCommand::Rename, "Do not retry", "", "");
    assert!(owner.0.state.lock().unwrap().job.is_none());
    owner.command(generation, CalendarCommand::Reload, "", "", "");
    let state = owner.0.state.lock().unwrap();
    assert!(!state.uncertain && !state.ready);
    assert!(state.job.is_none());
    assert!(!state.editable());
}
#[test]
fn remote_and_calculated_sources_never_create_mutations_even_with_allowed_access() {
    let owner = Calendars::detached();
    let session = session();
    for origin in [wire::Origin::RemoteIcs, wire::Origin::Calculated] {
        let generation = {
            let mut state = owner.0.state.lock().unwrap();
            ready(&mut state, &session);
            let mut calendar = state.calendar().unwrap().clone();
            calendar.origin = origin.into();
            let query = state.query().unwrap();
            let mut capture = CalendarCapture::new(&calendar, &query).unwrap();
            capture
                .push(&wire::QueryOccurrencesResponse {
                    revision: calendar.revision.clone(),
                    quality: wire::Quality::Fresh.into(),
                    provenance: wire::DataProvenance::Synthetic.into(),
                    ..Default::default()
                })
                .unwrap();
            state.snapshot(
                Arc::new(capture.finish(&calendar).unwrap()),
                session.generation(),
            );
            assert!(!state.editable());
            state.generation
        };
        owner.command(generation, CalendarCommand::Remove, "", "", "");
        owner.command(generation, CalendarCommand::NewEvent, "", "", "");
        let state = owner.0.state.lock().unwrap();
        assert!(state.job.is_none() && state.draft.is_none());
    }
}
#[test]
fn coalescing_a_private_reset_with_a_fresh_same_calendar_capture_erases_editor_buffers() {
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    let session = session();
    let mut state = State::default();
    ready(&mut state, &session);
    let key = EventKey::new(
        state.selected.clone().unwrap(),
        "private-draft".parse().unwrap(),
    );
    let mut draft = Draft::new(key, &state.start.parse().unwrap());
    draft.edit(Field::Summary, "Private old summary".into());
    draft.edit(Field::Description, "Private old description".into());
    state.draft = Some(draft);
    render::render(
        &window,
        render::Projection::new(&state, Some(session.clone())),
    );
    window.set_calendars_name("Private old calendar name".into());
    assert_eq!(window.get_calendars_edit_summary(), "Private old summary");
    state.clear_private();
    ready(&mut state, &session);
    // Deliver only the fresh frame. The intermediate empty frame was coalesced.
    render::render(&window, render::Projection::new(&state, Some(session)));
    assert!(window.get_calendars_edit_summary().is_empty());
    assert!(window.get_calendars_edit_description().is_empty());
    assert_ne!(window.get_calendars_name(), "Private old calendar name");
    assert!(!window.get_calendars().editor.selected);
}
#[test]
fn missing_authority_proof_clears_scalar_editor_fields_as_well_as_the_projection() {
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window.set_calendars_edit_summary("Private stale summary".into());
    window.set_calendars_edit_zone("Private old zone".into());
    let session = session();
    let mut state = State::default();
    ready(&mut state, &session);
    render::render(&window, render::Projection::new(&state, None));
    assert!(window.get_calendars_edit_summary().is_empty());
    assert!(window.get_calendars_edit_zone().is_empty());
    assert_eq!(window.get_calendars().rows.row_count(), 0);
}

#[test]
fn an_inflight_write_owns_its_receipt_across_watch_updates_but_not_context_changes() {
    let mut state = State::default();
    ready(&mut state, &session());
    state.busy = true;
    let context = state.context;
    assert!(state.owns_write(context));
    state.ready = false;
    state.bump();
    assert!(state.owns_write(context));
    state.reset_reads();
    assert!(!state.owns_write(context));
}

#[test]
fn week_mode_reads_exactly_seven_civil_days_and_custom_ranges_return_to_list() {
    let owner = Calendars::detached();
    {
        let mut state = owner.0.state.lock().unwrap();
        ready(&mut state, &session());
        state.end = "2026-12-01".into();
    }
    owner.mode(ProAgendaMode::Week);
    {
        let state = owner.0.state.lock().unwrap();
        assert!(state.week && !state.ready);
        assert_eq!(state.start, "2026-11-01");
        assert_eq!(state.end, "2026-11-08");
    }
    let generation = owner.0.state.lock().unwrap().generation;
    owner.command(
        generation,
        CalendarCommand::ApplyRange,
        "2026-11-01",
        "2026-12-01",
        "America/Toronto",
    );
    let state = owner.0.state.lock().unwrap();
    assert!(!state.week);
    assert_eq!(state.end, "2026-12-01");
}

#[test]
fn a_confirmed_receipt_blocks_older_captures_and_refreshes_catalog_metadata() {
    let session = session();
    let mut state = State::default();
    ready(&mut state, &session);
    let old = state.snapshot.as_ref().unwrap().clone();
    let mut calendar = old.calendar().clone();
    let mut revision = calendar.revision.as_option().unwrap().clone();
    revision.number = 2;
    calendar.revision = revision.into();
    calendar.name = Some("Authoritative renamed calendar".into());
    state.minimum_revision =
        admit::calendar_revision(&calendar.revision, state.selected.as_ref().unwrap());
    state.ready = false;
    state.snapshot(old, session.generation());
    assert!(!state.ready);
    assert_eq!(state.catalog[0].revision.number, 1);
    let mut capture = CalendarCapture::new(&calendar, &state.query().unwrap()).unwrap();
    capture
        .push(&wire::QueryOccurrencesResponse {
            revision: calendar.revision.clone(),
            quality: wire::Quality::Fresh.into(),
            provenance: wire::DataProvenance::Synthetic.into(),
            ..Default::default()
        })
        .unwrap();
    state.snapshot(
        Arc::new(capture.finish(&calendar).unwrap()),
        session.generation(),
    );
    assert!(state.ready && state.minimum_revision.is_none());
    assert_eq!(
        state.catalog[0].name.as_deref(),
        Some("Authoritative renamed calendar")
    );
}

fn widget_target() -> crate::dashboard_widget_host::AgendaTarget {
    crate::dashboard_widget_host::AgendaTarget {
        token: 17,
        generation: 19,
        calendar: CalendarId::new("exact-calendar-second".parse().unwrap()),
        zone: "America/Toronto".parse().unwrap(),
        window_seconds: 7200,
        limit: 3,
    }
}
#[test]
fn widget_calendar_window_preserves_server_duration_and_zone_across_a_fold() {
    let target = widget_target();
    let now = chrono::DateTime::parse_from_rfc3339("2026-11-01T05:30:00Z")
        .unwrap()
        .with_timezone(&chrono::Utc);
    let query = state::widget_query(&target, now).unwrap();
    assert_eq!(query.id.entity_id, "exact-calendar-second");
    assert_eq!(query.page_size, 3);
    assert_eq!(query.window.zone, "America/Toronto");
    let Some(wire::window::Bounds::Times(span)) = &query.window.bounds else {
        panic!("absolute configured window")
    };
    assert_eq!(span.start_instant, "2026-11-01T05:30:00Z");
    assert_eq!(span.end_instant, "2026-11-01T07:30:00Z");
    assert_eq!(span.zone, "America/Toronto");
    assert!(admit::admit_query(&query).is_ok());
    let mut invalid = target;
    invalid.window_seconds = 31_622_401;
    assert!(state::widget_query(&invalid, now).is_none());
}
#[test]
fn widget_calendar_never_selects_first_catalogue_or_substitutes_missing_source() {
    let target = widget_target();
    let owner = Calendars::for_widget(None, target.clone());
    {
        let state = owner.0.state.lock().unwrap();
        assert_eq!(state.selected.as_ref(), Some(&target.calendar));
        assert!(state.catalog.is_empty());
        assert!(!state.catalog_needed);
        assert_eq!(state.query().unwrap().id.entity_id, "exact-calendar-second");
        assert!(!state.ready);
    }
    let context = owner.0.state.lock().unwrap().context;
    owner.failed(context, Status::Unsupported);
    {
        let mut state = owner.0.state.lock().unwrap();
        assert_eq!(state.status, Status::Unsupported);
        assert!(state.snapshot.is_none());
        assert!(!state.ready);
        owner.pin_widget(&mut state);
        assert_eq!(state.selected.as_ref(), Some(&target.calendar));
        assert!(!state.catalog_needed);
        assert!(state.snapshot.is_none());
    }
}
