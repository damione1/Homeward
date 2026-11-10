//! Local owner invariants only. Network/authorization evidence uses the isolated
//! Go TLS fixture; these tests never log in or contact an installation.
use super::*;
use home_client_cache::calendars::CalendarCapture;
use home_client_session::{SecureStore, StoreFailure};
use slint::Model;

#[test]
fn a_superseded_read_releases_its_own_busy_slot_without_unlocking_another_context() {
    let mut state = State {
        activity: Activity::Read,
        ..Default::default()
    };
    let (context, generation) = (state.context, state.generation);
    state.bump();
    assert!(!state.finish_read_job(context, generation));
    assert!(!state.busy());
    state.activity = Activity::Write;
    state.reset_reads();
    assert!(!state.finish_read_job(context, generation));
    assert!(state.busy());
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
    let calendar = wire::Calendar {
        name: "homes/synthetic/calendars/id-63616c".into(),
        source_entity: "homes/synthetic/entities/source".into(),
        display_name: "Synthetic calendar".into(),
        etag: "W/\"calendar-one\"".into(),
        origin: wire::CalendarOrigin::Local.into(),
        freshness: wire::CalendarFreshness::Fresh.into(),
        provenance: wire::CalendarProvenance::Synthetic.into(),
        mutation_access: wire::CalendarMutationAccess::Allowed.into(),
        ..Default::default()
    };
    state.catalog = vec![wire::CalendarSource {
        calendar: calendar.name.clone(),
        source_entity: calendar.source_entity.clone(),
        display_name: calendar.display_name.clone(),
        origin: calendar.origin,
        provenance: calendar.provenance,
        mutation_access: calendar.mutation_access,
        materialized: true,
        ..Default::default()
    }];
    state.selected = Some(
        home_client_model::api_calendars::CalendarName::try_from(calendar.name.as_str())
            .unwrap()
            .native_id()
            .clone(),
    );
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
            calendar_etag: calendar.etag.clone(),
            freshness: wire::CalendarFreshness::Fresh.into(),
            provenance: wire::CalendarProvenance::Synthetic.into(),
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
    for origin in [
        wire::CalendarOrigin::RemoteIcs,
        wire::CalendarOrigin::Calculated,
    ] {
        let generation = {
            let mut state = owner.0.state.lock().unwrap();
            ready(&mut state, &session);
            let mut calendar = state.calendar().unwrap().clone();
            calendar.origin = origin.into();
            let query = state.query().unwrap();
            assert!(
                CalendarCapture::new(&calendar, &query).is_err(),
                "remote allowed access is rejected at admission"
            );
            calendar.mutation_access = wire::CalendarMutationAccess::Denied.into();
            let mut capture = CalendarCapture::new(&calendar, &query).unwrap();
            capture
                .push(&wire::QueryOccurrencesResponse {
                    calendar_etag: calendar.etag.clone(),
                    freshness: wire::CalendarFreshness::Fresh.into(),
                    provenance: wire::CalendarProvenance::Synthetic.into(),
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
    let mut draft = Draft::new(
        state.selected_name().unwrap(),
        &state.start.parse().unwrap(),
    );
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
    state.activity = Activity::Write;
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
        assert!(state.mode == ProAgendaMode::Week && !state.ready);
        assert_eq!(state.start, "2026-10-26");
        assert_eq!(state.end, "2026-11-02");
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
    assert_eq!(state.mode, ProAgendaMode::List);
    assert_eq!(state.end, "2026-12-01");
}

#[test]
fn a_new_opaque_etag_marks_an_open_draft_conflicted_and_refreshes_label() {
    let session = session();
    let mut state = State::default();
    ready(&mut state, &session);
    state.draft = Some(Draft::new(
        state.selected_name().unwrap(),
        &state.start.parse().unwrap(),
    ));
    let mut calendar = state.calendar().unwrap().clone();
    calendar.etag = "W/\"unrelated-opaque-token\"".into();
    calendar.display_name = "Authoritative renamed calendar".into();
    let mut capture = CalendarCapture::new(&calendar, &state.query().unwrap()).unwrap();
    capture
        .push(&wire::QueryOccurrencesResponse {
            calendar_etag: calendar.etag.clone(),
            freshness: wire::CalendarFreshness::Fresh.into(),
            provenance: wire::CalendarProvenance::Synthetic.into(),
            ..Default::default()
        })
        .unwrap();
    state.snapshot(
        Arc::new(capture.finish(&calendar).unwrap()),
        session.generation(),
    );
    assert!(state.ready);
    assert_eq!(state.draft.as_ref().unwrap().phase, EditPhase::Conflict);
    assert_eq!(
        state.catalog[0].display_name,
        "Authoritative renamed calendar"
    );
}

fn widget_target() -> crate::dashboard_widget_host::AgendaTarget {
    crate::dashboard_widget_host::AgendaTarget {
        token: 17,
        generation: 19,
        calendar: home_client_model::api_calendars::CalendarName::try_from(
            "homes/synthetic/calendars/id-65786163742d63616c656e6461722d7365636f6e64",
        )
        .unwrap(),
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
    let name = home_client_model::api_calendars::CalendarName::try_from(
        "homes/synthetic/calendars/id-65786163742d63616c656e6461722d7365636f6e64",
    )
    .unwrap();
    let query = state::widget_query(&target, &name, now).unwrap();
    assert_eq!(query.parent, format!("{}/events/-", name.as_str()));
    assert_eq!(query.page_size, 3);
    assert_eq!(query.window.time_zone, "America/Toronto");
    let Some(wire::calendar_window::Bounds::Times(span)) = &query.window.bounds else {
        panic!("absolute configured window")
    };
    assert_eq!(
        span.start_time.as_option().unwrap(),
        &admit::parse_timestamp("2026-11-01T05:30:00Z", true).unwrap()
    );
    assert_eq!(
        span.end_time.as_option().unwrap(),
        &admit::parse_timestamp("2026-11-01T07:30:00Z", true).unwrap()
    );
    assert!(admit::admit_query(&query).is_ok());
    let mut invalid = target;
    invalid.window_seconds = 31_622_401;
    assert!(state::widget_query(&invalid, &name, now).is_none());
}
#[test]
fn widget_calendar_never_selects_first_catalogue_or_substitutes_missing_source() {
    let target = widget_target();
    let owner = Calendars::for_widget(None, target.clone());
    {
        let state = owner.0.state.lock().unwrap();
        assert_eq!(state.selected.as_ref(), Some(target.calendar.native_id()));
        assert!(state.catalog.is_empty());
        assert!(state.catalog_needed);
        assert!(state.query().is_none());
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
        assert_eq!(state.selected.as_ref(), Some(target.calendar.native_id()));
        assert!(state.catalog_needed);
        assert!(state.snapshot.is_none());
    }
}

#[tokio::test]
async fn nhf037_retained_calendar_projection_does_not_own_session_storage() {
    let connection = home_client_transport::Connection::new("https://127.0.0.1:1", &[]).unwrap();
    let store = Arc::new(EmptyStore);
    let storage = Arc::downgrade(&store);
    let session = SessionCoordinator::new(connection.auth(), store);
    let mut state = State::default();
    ready(&mut state, &session);
    let active = render::Projection::new(&state, Some(session.clone()));
    state.clear_private();
    let cleared = render::Projection::new(&state, Some(session.clone()));
    session.close().await;
    drop(session);
    assert!(
        storage.upgrade().is_none(),
        "calendar projection retained storage"
    );
    // Both the populated and final clear payload may outlive the Activity.
    drop(active);
    drop(cleared);
}

#[test]
fn profile_zone_initializes_empty_week_and_weak_publisher_does_not_retain_owner() {
    let owner = Calendars::detached();
    owner.display_profile(
        session().generation(),
        crate::environment_view::ProfileZone::from_time_zone(Some("Pacific/Kiritimati")),
    );
    {
        let state = owner.0.state.lock().unwrap();
        assert_eq!(state.zone, "Pacific/Kiritimati");
        assert_eq!(state.mode, ProAgendaMode::Week);
        let start = chrono::NaiveDate::parse_from_str(&state.start, "%Y-%m-%d").unwrap();
        use chrono::Datelike;
        assert_eq!(start.weekday(), chrono::Weekday::Mon);
        assert_eq!(
            state.end,
            start
                .checked_add_days(chrono::Days::new(7))
                .unwrap()
                .to_string()
        );
    }
    let weak = Arc::downgrade(&owner.0);
    let publish = owner.display_profile_publisher();
    drop(owner);
    assert!(weak.upgrade().is_none());
    publish(
        session().generation(),
        crate::environment_view::ProfileZone::Missing,
    );
}

#[test]
fn empty_calendar_week_has_seven_visible_columns_and_period_navigation() {
    use i_slint_backend_testing::{ElementQuery, TestingBackend, TestingBackendOptions};
    slint::platform::set_platform(Box::new(TestingBackend::new(TestingBackendOptions {
        mock_time: true,
        renderer_name: Some("software".into()),
        ..Default::default()
    })))
    .unwrap();
    let window = crate::create_window().unwrap();
    window.set_show_endpoint(false);
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_view_loaded(true);
    window.set_commands_ready(true);
    window.set_connection_status(crate::ConnectionStatus::Live);
    window.set_page(crate::ViewPage::Calendars);
    window.set_page_title("Calendar".into());
    let mut state = State {
        active: true,
        interactive: true,
        status: Status::Ready,
        catalog_needed: false,
        notice: "The server provided no authorized calendars.".into(),
        start: "2026-11-02".into(),
        end: "2026-11-09".into(),
        zone: "Europe/Paris".into(),
        ..Default::default()
    };
    render::render(&window, render::Projection::new(&state, None));
    assert_eq!(window.get_calendars().columns.row_count(), 7);
    window.show().unwrap();
    for (name, width, height) in [("desktop", 1280., 900.), ("phone", 390., 844.)] {
        window
            .window()
            .set_size(slint::LogicalSize::new(width, height));
        for dark in [false, true] {
            for large in [false, true] {
                window.set_dark_mode(dark);
                window.set_large_text(large);
                window.invoke_calendars_scroll_to(if name == "phone" && large {
                    -300.
                } else {
                    0.
                });
                i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(300));
                let headers = ElementQuery::from_root(&window)
                    .match_predicate(|el| el.accessible_label().as_deref() == Some("Mon 2"))
                    .find_all();
                assert!(!headers.is_empty(), "empty week retains its Monday header");
                if let Some(directory) = std::env::var_os("HOMEWARD_CALENDAR_RENDER_DIR") {
                    let directory = std::path::PathBuf::from(directory);
                    std::fs::create_dir_all(&directory).unwrap();
                    let save = |suffix: &str| {
                        i_slint_backend_testing::mock_elapsed_time(
                            std::time::Duration::from_millis(300),
                        );
                        let pixels = window.window().take_snapshot().unwrap();
                        let mut encoder = png::Encoder::new(
                            std::fs::File::create(
                                directory.join(format!("{name}-{dark}-{large}-{suffix}.png")),
                            )
                            .unwrap(),
                            pixels.width(),
                            pixels.height(),
                        );
                        encoder.set_color(png::ColorType::Rgba);
                        encoder.set_depth(png::BitDepth::Eight);
                        encoder
                            .write_header()
                            .unwrap()
                            .write_image_data(pixels.as_bytes())
                            .unwrap();
                    };
                    if name == "phone" && large {
                        save("empty-scrolled");
                        window.invoke_calendars_scroll_to(0.);
                    }
                    save("empty");
                    let empty = window.get_calendars();
                    let mut populated = empty.clone();
                    populated.notice = "".into();
                    populated.title = "Studio calendar".into();
                    populated.selected = true;
                    populated.present = true;
                    populated.catalog = slint::ModelRc::from(
                        [
                            crate::ContentItem {
                                token: 1,
                                title: "Studio calendar".into(),
                                selected: true,
                                ..Default::default()
                            },
                            crate::ContentItem {
                                token: 2,
                                title: "Training".into(),
                                ..Default::default()
                            },
                        ]
                        .as_slice(),
                    );
                    let columns = populated
                        .columns
                        .iter()
                        .enumerate()
                        .map(|(index, mut column)| {
                            column.today = index == 2;
                            column.rows = slint::ModelRc::from(
                                [crate::ProAgendaRow {
                                    token: index as i32 + 1,
                                    generation: 1,
                                    date: column.day.date.clone(),
                                    day_label: column.day.label.clone(),
                                    title: [
                                        "Design review",
                                        "Workshop",
                                        "Team meeting",
                                        "Reservation",
                                        "Planning",
                                        "Open studio",
                                        "Annual event",
                                    ][index]
                                        .into(),
                                    time_label: "09:00–10:00".into(),
                                    kind: crate::ProAgendaTemporalKind::Timed,
                                    ..Default::default()
                                }]
                                .as_slice(),
                            );
                            column
                        })
                        .collect::<Vec<_>>();
                    populated.columns = slint::ModelRc::from(columns.as_slice());
                    window.set_calendars(populated);
                    save("populated");
                    if name == "phone" && large {
                        window.invoke_calendars_scroll_to(-300.);
                        save("populated-scrolled");
                        window.invoke_calendars_scroll_to(0.);
                    }
                    window.set_calendars(empty);
                }
            }
        }
    }
    let owner = Calendars::detached();
    std::mem::swap(&mut *owner.0.state.lock().unwrap(), &mut state);
    let generation = owner.0.state.lock().unwrap().generation;
    owner.command(generation, CalendarCommand::NextWeek, "", "", "");
    assert_eq!(owner.0.state.lock().unwrap().start, "2026-11-09");
    owner.mode(ProAgendaMode::Day);
    assert_eq!(owner.0.state.lock().unwrap().mode, ProAgendaMode::Day);
}

#[test]
fn profile_change_revokes_pending_context_and_preserves_uncertain_write_without_replay() {
    let owner = Calendars::detached();
    owner.display_profile(
        session().generation(),
        crate::environment_view::ProfileZone::from_time_zone(Some("Europe/Paris")),
    );
    let before = {
        let mut state = owner.0.state.lock().unwrap();
        state.activity = Activity::Write;
        state.active = true;
        state.interactive = true;
        state.context
    };
    owner.display_profile(
        session().generation(),
        crate::environment_view::ProfileZone::from_time_zone(Some("America/New_York")),
    );
    {
        let state = owner.0.state.lock().unwrap();
        assert!(state.uncertain);
        assert!(!state.busy());
        assert!(state.job.is_none());
        assert!(!state.owns_write(before));
        assert!(!state.notice.is_empty());
    }
    owner.clear();
    let state = owner.0.state.lock().unwrap();
    assert!(state.zone.is_empty());
    assert!(state.start.is_empty());
    assert!(state.end.is_empty());
    assert!(!state.profile_received);
    assert!(state.snapshot.is_none());
}

#[test]
fn absent_profile_renders_placeholder_columns_without_device_dates() {
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    let owner = Calendars::detached();
    owner.display_profile(
        session().generation(),
        crate::environment_view::ProfileZone::Missing,
    );
    let state = owner.0.state.lock().unwrap();
    assert!(state.start.is_empty());
    assert!(!state.notice.is_empty());
    render::render(&window, render::Projection::new(&state, None));
    let frame = window.get_calendars();
    assert_eq!(frame.columns.row_count(), 7);
    assert!(
        frame
            .columns
            .iter()
            .all(|column| column.day.label == "—" && column.day.date.year == 0)
    );
}

#[test]
fn day_mode_queries_one_civil_day_across_both_dst_changes() {
    let mut state = State::default();
    ready(&mut state, &session());
    for (start, end) in [("2026-03-08", "2026-03-09"), ("2026-11-01", "2026-11-02")] {
        state.focus_period(
            chrono::NaiveDate::parse_from_str(start, "%Y-%m-%d").unwrap(),
            ProAgendaMode::Day,
        );
        let query = state.query().unwrap();
        assert_eq!(query.window.time_zone, "America/Toronto");
        let Some(wire::calendar_window::Bounds::Dates(span)) = &query.window.bounds else {
            panic!("civil window required")
        };
        assert_eq!(admit::date(&span.start_date).unwrap().to_string(), start);
        assert_eq!(admit::date(&span.end_date).unwrap().to_string(), end);
        assert_eq!(state.mode, ProAgendaMode::Day);
    }
}

#[test]
fn stale_remote_query_is_visible_without_opening_calendar_settings() {
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window.set_show_endpoint(false);
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_view_loaded(true);
    window.set_page(crate::ViewPage::Calendars);
    let session = session();
    let mut state = State::default();
    ready(&mut state, &session);
    let mut calendar = state.calendar().unwrap().clone();
    calendar.origin = wire::CalendarOrigin::RemoteIcs.into();
    calendar.mutation_access = wire::CalendarMutationAccess::Denied.into();
    let mut capture = CalendarCapture::new(&calendar, &state.query().unwrap()).unwrap();
    capture
        .push(&wire::QueryOccurrencesResponse {
            calendar_etag: calendar.etag.clone(),
            freshness: wire::CalendarFreshness::Stale.into(),
            provenance: wire::CalendarProvenance::Synthetic.into(),
            occurrences: vec![wire::Occurrence {
                calendar_event: format!("{}/events/id-7374616c65", calendar.name),
                summary: "Previous source image".into(),
                time_zone: state.zone.clone(),
                recurrence_point: wire::CalendarRecurrencePoint {
                    value: Some(wire::calendar_recurrence_point::Value::Date(Box::new(
                        admit::date_wire("2026-11-02".parse().unwrap()),
                    ))),
                    ..Default::default()
                }
                .into(),
                span: wire::CalendarSpan {
                    value: Some(wire::calendar_span::Value::Dates(Box::new(
                        wire::CalendarDateSpan {
                            start_date: admit::date_wire("2026-11-02".parse().unwrap()).into(),
                            end_date: admit::date_wire("2026-11-03".parse().unwrap()).into(),
                            ..Default::default()
                        },
                    ))),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }],
            ..Default::default()
        })
        .unwrap();
    state.snapshot(
        Arc::new(capture.finish(&calendar).unwrap()),
        session.generation(),
    );
    render::render(
        &window,
        render::Projection::new(&state, Some(session.clone())),
    );
    assert_eq!(window.get_calendars().notice, "Outdated data");
    assert_eq!(window.get_calendars().rows.row_count(), 1);
    window.show().unwrap();
    assert!(
        i_slint_backend_testing::ElementQuery::from_root(&window)
            .match_predicate(
                |element| element.accessible_label().as_deref() == Some("Outdated data")
            )
            .find_first()
            .is_some()
    );
    state.notice = "Event revision reloaded; mutation outcome still uncertain.".into();
    state.uncertain = true;
    render::render(
        &window,
        render::Projection::new(&state, Some(session.clone())),
    );
    let notice = window.get_calendars().notice;
    assert!(notice.contains("mutation outcome still uncertain"));
    assert!(notice.contains("Outdated data"));
}

#[test]
fn read_only_recurrence_details_use_the_selected_server_occurrence_not_the_series() {
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    let session = session();
    let owner = Calendars::detached();
    let master = wire::CalendarEvent {
        name: "homes/synthetic/calendars/id-63616c/events/id-736572696573".into(),
        summary: "Series title".into(),
        recurrence_rule: Some("FREQ=WEEKLY".into()),
        span: wire::CalendarSpan {
            value: Some(wire::calendar_span::Value::Dates(Box::new(
                wire::CalendarDateSpan {
                    start_date: admit::date_wire("2026-10-05".parse().unwrap()).into(),
                    end_date: admit::date_wire("2026-10-06".parse().unwrap()).into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let recurrence_point = wire::CalendarRecurrencePoint {
        value: Some(wire::calendar_recurrence_point::Value::Date(Box::new(
            admit::date_wire("2026-11-02".parse().unwrap()),
        ))),
        ..Default::default()
    };
    let generation = {
        let mut state = owner.0.state.lock().unwrap();
        ready(&mut state, &session);
        let mut calendar = state.calendar().unwrap().clone();
        calendar.origin = wire::CalendarOrigin::RemoteIcs.into();
        calendar.mutation_access = wire::CalendarMutationAccess::Denied.into();
        state.catalog[0].origin = calendar.origin;
        state.catalog[0].mutation_access = calendar.mutation_access;
        let mut capture = CalendarCapture::new(&calendar, &state.query().unwrap()).unwrap();
        capture
            .push(&wire::QueryOccurrencesResponse {
                calendar_etag: calendar.etag.clone(),
                freshness: wire::CalendarFreshness::Fresh.into(),
                provenance: wire::CalendarProvenance::Synthetic.into(),
                occurrences: vec![wire::Occurrence {
                    calendar_event: master.name.clone(),
                    recurrence_point: recurrence_point.clone().into(),
                    summary: "Moved occurrence override".into(),
                    description: "Occurrence description".into(),
                    location: "Studio B".into(),
                    time_zone: state.zone.clone(),
                    span: wire::CalendarSpan {
                        value: Some(wire::calendar_span::Value::Dates(Box::new(
                            wire::CalendarDateSpan {
                                start_date: admit::date_wire("2026-11-03".parse().unwrap()).into(),
                                end_date: admit::date_wire("2026-11-04".parse().unwrap()).into(),
                                ..Default::default()
                            },
                        ))),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                }],
                ..Default::default()
            })
            .unwrap();
        state.snapshot(
            Arc::new(capture.finish(&calendar).unwrap()),
            session.generation(),
        );
        state.generation
    };
    owner.select(1, generation);
    {
        let state = owner.0.state.lock().unwrap();
        assert!(
            state.job.is_none(),
            "read-only occurrence must not fetch its series definition"
        );
        assert_eq!(
            *state.occurrence.as_ref().unwrap().recurrence_point,
            recurrence_point
        );
        render::render(
            &window,
            render::Projection::new(&state, Some(session.clone())),
        );
    }
    let view = window.get_calendars().editor;
    assert_eq!(view.title, "Occurrence");
    assert_eq!(view.access, crate::ProAgendaAccess::ReadOnly);
    assert_eq!(view.summary, "Moved occurrence override");
    assert_eq!(view.description, "Occurrence description");
    assert_eq!(view.location, "Studio B");
    assert_eq!(view.start_date, "2026-11-03");
    assert_eq!(view.end_date, "2026-11-04");
    assert_ne!(view.summary.as_str(), master.summary);
    owner.action(1, generation, ProAgendaAction::Save);
    assert!(owner.0.state.lock().unwrap().job.is_none());
    {
        let mut state = owner.0.state.lock().unwrap();
        let calendar = state.calendar().unwrap().clone();
        let selected = state.occurrence.as_ref().unwrap().clone();
        let mut inserted = selected.clone();
        inserted.calendar_event = format!("{}/events/id-696e736572746564", calendar.name);
        inserted.summary = "Inserted before selection".into();
        let mut capture = CalendarCapture::new(&calendar, &state.query().unwrap()).unwrap();
        capture
            .push(&wire::QueryOccurrencesResponse {
                calendar_etag: calendar.etag.clone(),
                freshness: wire::CalendarFreshness::Fresh.into(),
                provenance: wire::CalendarProvenance::Synthetic.into(),
                occurrences: vec![inserted, selected.clone()],
                ..Default::default()
            })
            .unwrap();
        let source = state.source.unwrap();
        state.snapshot(Arc::new(capture.finish(&calendar).unwrap()), source);
        assert_eq!(state.active_index, Some(1));
        assert_eq!(state.draft_token, 2);
        assert_eq!(state.occurrence.as_ref().unwrap().summary, selected.summary);
        let mut empty = CalendarCapture::new(&calendar, &state.query().unwrap()).unwrap();
        empty
            .push(&wire::QueryOccurrencesResponse {
                calendar_etag: calendar.etag.clone(),
                freshness: wire::CalendarFreshness::Fresh.into(),
                provenance: wire::CalendarProvenance::Synthetic.into(),
                ..Default::default()
            })
            .unwrap();
        state.snapshot(Arc::new(empty.finish(&calendar).unwrap()), source);
        assert!(state.occurrence.is_none() && state.active_index.is_none());
        assert_eq!(state.draft_token, 0);
        state.occurrence = Some(selected);
    }

    owner.0.state.lock().unwrap().clear_private();
    assert!(owner.0.state.lock().unwrap().occurrence.is_none());
}

#[tokio::test]
async fn server_profile_survives_both_worker_publication_orders_only_for_its_generation() {
    use crate::environment_view::ProfileZone;
    let session = session();
    let old = session.generation();
    let profile = ProfileZone::from_time_zone(Some("Europe/Paris"));
    let mut first = State::default();
    first.apply_profile(old, profile);
    session.logout_local().await.unwrap();
    let current = session.generation();
    assert_ne!(old, current);
    // Same-zone publication still tags its new source before the early return.
    first.apply_profile(current, profile);
    first.zone = "Pacific/Honolulu".into(); // User range override is not a server profile.
    first.change_generation(current, &session);
    assert_eq!(first.zone, "Europe/Paris");
    assert!(first.profile_received);
    let mut second = State::default();
    second.apply_profile(old, profile);
    second.change_generation(current, &session);
    assert!(!second.profile_received && second.zone.is_empty());
    second.apply_profile(current, profile);
    assert_eq!(
        (&first.zone, &first.start, &first.end),
        (&second.zone, &second.start, &second.end)
    );
    second.apply_profile(old, profile);
    second.change_generation(old, &session);
    assert!(second.zone.is_empty() && second.server_profile.is_none());
    second.apply_profile(current, ProfileZone::Missing);
    second.change_generation(current, &session);
    assert!(second.profile_received && second.zone.is_empty() && !second.notice.is_empty());
    second.clear_private();
    assert!(second.server_profile.is_none() && !second.profile_received);
    let owner = Calendars::detached();
    *owner.0.session.lock().unwrap() = Some(session.clone());
    owner.display_profile(old, profile);
    assert!(owner.0.state.lock().unwrap().server_profile.is_none());
    let publish = owner.display_profile_publisher();
    session.close().await;
    publish(current, profile);
    assert!(owner.0.state.lock().unwrap().server_profile.is_none());
}

#[tokio::test]
async fn invalid_calendar_reload_retains_only_the_current_server_profile() {
    use crate::environment_view::ProfileZone;
    for status in [Status::Invalid, Status::Unsupported] {
        let session = session();
        let owner = Calendars::detached();
        *owner.0.session.lock().unwrap() = Some(session.clone());
        let (catalog, selected) = {
            let mut state = owner.0.state.lock().unwrap();
            ready(&mut state, &session);
            (state.catalog.clone(), state.selected.clone())
        };
        owner.display_profile(
            session.generation(),
            ProfileZone::from_time_zone(Some("Europe/Paris")),
        );
        let context = owner.0.state.lock().unwrap().context;
        owner.failed(context, status);
        let generation = {
            let mut state = owner.0.state.lock().unwrap();
            assert_eq!(state.zone, "Europe/Paris");
            assert!(
                state.catalog.is_empty() && state.snapshot.is_none() && state.occurrence.is_none()
            );
            // The next successful catalogue response restores metadata only.
            state.catalog = catalog.clone();
            state.selected = selected.clone();
            state.generation
        };
        owner.command(generation, CalendarCommand::Reload, "", "", "");
        assert!(owner.0.state.lock().unwrap().query().is_some());
        session.logout_local().await.unwrap();
        let context = owner.0.state.lock().unwrap().context;
        owner.failed(context, status);
        {
            let mut state = owner.0.state.lock().unwrap();
            state.catalog = catalog;
            state.selected = selected;
            assert!(state.query().is_none() && state.server_profile.is_none());
        }
        owner.display_profile(session.generation(), ProfileZone::Missing);
        let context = owner.0.state.lock().unwrap().context;
        owner.failed(context, status);
        assert!(owner.0.state.lock().unwrap().query().is_none());
    }
}

#[test]
fn idle_publications_are_noops_but_mailbox_invalidation_is_never_swallowed() {
    let owner = Calendars::detached();
    let session = session();
    *owner.0.session.lock().unwrap() = Some(session.clone());
    let mut state = owner.0.state.lock().unwrap();
    ready(&mut state, &session);
    owner.publish(&state);
    let before = owner.0.mailbox.metrics();
    for _ in 0..1000 {
        owner.publish(&state);
    }
    assert_eq!(owner.0.mailbox.metrics(), before);
    owner.0.mailbox.invalidate().unwrap();
    owner.publish(&state);
    assert!(owner.0.mailbox.metrics().scheduled > before.scheduled);
}

#[derive(Default)]
struct Notifications(std::cell::Cell<usize>);
impl i_slint_core::model::ModelChangeListener for Notifications {
    fn row_changed(self: std::pin::Pin<&Self>, _: usize) {
        self.0.set(self.0.get() + 1);
    }
    fn row_added(self: std::pin::Pin<&Self>, _: usize, _: usize) {
        self.0.set(self.0.get() + 1);
    }
    fn row_removed(self: std::pin::Pin<&Self>, _: usize, _: usize) {
        self.0.set(self.0.get() + 1);
    }
    fn reset(self: std::pin::Pin<&Self>) {
        self.0.set(self.0.get() + 1);
    }
}
#[test]
fn repeated_projection_keeps_calendar_models_and_equal_rows_of_new_admissions() {
    i_slint_backend_testing::init_no_event_loop();
    for count in [0, 1, 250] {
        let window = crate::create_window().unwrap();
        let session = session();
        let mut state = State::default();
        ready(&mut state, &session);
        let calendar = state.calendar().unwrap().clone();
        let occurrences = (0..count)
            .map(|index| wire::Occurrence {
                calendar_event: format!(
                    "{}/events/id-{}",
                    calendar.name,
                    format!("synthetic-{index}")
                        .bytes()
                        .map(|byte| format!("{byte:02x}"))
                        .collect::<String>()
                ),
                recurrence_point: wire::CalendarRecurrencePoint {
                    value: Some(wire::calendar_recurrence_point::Value::Date(Box::new(
                        admit::date_wire("2026-11-02".parse().unwrap()),
                    ))),
                    ..Default::default()
                }
                .into(),
                summary: format!("Synthetic event {index}"),
                time_zone: state.zone.clone(),
                span: wire::CalendarSpan {
                    value: Some(wire::calendar_span::Value::Dates(Box::new(
                        wire::CalendarDateSpan {
                            start_date: admit::date_wire("2026-11-02".parse().unwrap()).into(),
                            end_date: admit::date_wire("2026-11-03".parse().unwrap()).into(),
                            ..Default::default()
                        },
                    ))),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            })
            .collect::<Vec<_>>();
        let page = wire::QueryOccurrencesResponse {
            calendar_etag: calendar.etag.clone(),
            freshness: wire::CalendarFreshness::Fresh.into(),
            provenance: wire::CalendarProvenance::Synthetic.into(),
            occurrences,
            ..Default::default()
        };
        let mut capture = CalendarCapture::new(&calendar, &state.query().unwrap()).unwrap();
        capture.push(&page).unwrap();
        state.snapshot(
            Arc::new(capture.finish(&calendar).unwrap()),
            session.generation(),
        );
        render::render(
            &window,
            render::Projection::new(&state, Some(session.clone())),
        );
        let first = window.get_calendars();
        let children = first
            .columns
            .iter()
            .map(|column| column.rows)
            .collect::<Vec<_>>();
        assert_eq!(
            children.iter().map(|rows| rows.row_count()).sum::<usize>(),
            count
        );
        let notifications = Box::pin(i_slint_core::model::ModelChangeListenerContainer::<
            Notifications,
        >::default());
        first
            .columns
            .model_tracker()
            .attach_peer(notifications.as_ref().model_peer());
        for _ in 0..10 {
            render::render(
                &window,
                render::Projection::new(&state, Some(session.clone())),
            );
            let next = window.get_calendars();
            assert!(first.columns == next.columns);
            assert!(first.rows == next.rows);
            assert!(first.days == next.days);
            assert!(first.catalog == next.catalog);
            assert!(first.calendar_days == next.calendar_days);
            for (index, child) in children.iter().enumerate() {
                assert!(*child == next.columns.row_data(index).unwrap().rows);
            }
        }
        // A distinct admitted capture retains authority even when its rows match.
        let old = state.snapshot.clone().unwrap();
        let mut capture = CalendarCapture::new(old.calendar(), old.query()).unwrap();
        capture.push(&page).unwrap();
        state.snapshot(
            Arc::new(capture.finish(old.calendar()).unwrap()),
            session.generation(),
        );
        assert!(!Arc::ptr_eq(&old, state.snapshot.as_ref().unwrap()));
        render::render(
            &window,
            render::Projection::new(&state, Some(session.clone())),
        );
        assert!(first.columns == window.get_calendars().columns);
        assert!(first.rows == window.get_calendars().rows);
        assert_eq!(
            notifications.0.get(),
            0,
            "unchanged data must not notify column delegates"
        );
        if count > 0 {
            state.active_index = Some(0);
            render::render(
                &window,
                render::Projection::new(&state, Some(session.clone())),
            );
            assert_eq!(
                notifications.0.get(),
                1,
                "only the newly active column changes"
            );
            for (index, rows) in children.iter().enumerate() {
                assert!(*rows == window.get_calendars().columns.row_data(index).unwrap().rows);
            }
        }
    }
}

#[test]
fn deadlines_use_all_expiry_bounds_latch_replays_and_retry_reads_only() {
    use super::deadlines::{Kind, Scheduler};
    use std::time::{Duration, SystemTime};
    let session = session();
    let mut state = State::default();
    ready(&mut state, &session);
    let at = |value: &str| -> SystemTime {
        chrono::DateTime::parse_from_rfc3339(value)
            .unwrap()
            .with_timezone(&chrono::Utc)
            .into()
    };
    let mut calendar = state.calendar().unwrap().clone();
    calendar.fresh_until_time = admit::parse_timestamp("2026-11-09T17:02:00Z", true)
        .unwrap()
        .into();
    let capture = || {
        let mut capture = CalendarCapture::new(&calendar, &state.query().unwrap()).unwrap();
        capture
            .push(&wire::QueryOccurrencesResponse {
                calendar_etag: calendar.etag.clone(),
                freshness: wire::CalendarFreshness::Fresh.into(),
                provenance: wire::CalendarProvenance::Synthetic.into(),
                fresh_until_time: admit::parse_timestamp("2026-11-09T17:00:30Z", true)
                    .unwrap()
                    .into(),
                ..Default::default()
            })
            .unwrap();
        Arc::new(capture.finish(&calendar).unwrap())
    };
    let old = capture();
    let renewed = capture();
    state.snapshot(old.clone(), session.generation());
    let mut scheduler = Scheduler::default();
    let monotonic = tokio::time::Instant::now();
    let wall = at("2026-11-09T17:00:00Z");
    let deadline = scheduler.next(&state, false, wall, monotonic).unwrap();
    assert_eq!(deadline.kind, Kind::Expiry);
    assert_eq!(deadline.at - monotonic, Duration::from_secs(30));
    assert_eq!(
        scheduler
            .next(
                &state,
                false,
                wall - Duration::from_secs(3600),
                monotonic + Duration::from_secs(1)
            )
            .unwrap()
            .at,
        deadline.at,
        "clockback plus an event cannot postpone this admission"
    );
    assert!(!state.fresh_at(wall + Duration::from_secs(30)));
    assert!(!state.fresh_at(wall));
    state.snapshot(old, session.generation());
    assert!(
        !state.fresh_at(wall),
        "replaying the same admission cannot undo expiration"
    );
    assert_eq!(
        scheduler.next(&state, false, wall, monotonic).unwrap().kind,
        Kind::CivilDate
    );
    state.snapshot(renewed, session.generation());
    assert!(
        state.fresh_at(wall),
        "a new backend admission may restore freshness"
    );
    state.status = Status::Offline;
    state.read_retry = true;
    let retry = scheduler.next(&state, false, wall, monotonic).unwrap();
    assert_eq!(retry.kind, Kind::ReadRetry);
    assert_eq!(retry.at - monotonic, Duration::from_secs(2));
    assert_eq!(
        scheduler
            .next(&state, false, wall, monotonic + Duration::from_secs(1))
            .unwrap()
            .at,
        retry.at
    );
    state.activity = Activity::Write;
    assert_eq!(
        scheduler.next(&state, false, wall, monotonic).unwrap().kind,
        Kind::Expiry
    );
    state.active = false;
    assert!(scheduler.next(&state, false, wall, monotonic).is_none());
}

#[tokio::test]
async fn open_watch_publication_wakes_the_owner_and_rearms_its_expiry_without_stream_completion() {
    use super::deadlines::{Kind, Scheduler};
    use home_client_cache::calendars::CalendarReadEvent;
    use std::time::{Duration, SystemTime};
    let owner = Calendars::detached();
    let session = session();
    let mut scheduler = Scheduler::default();
    let monotonic = tokio::time::Instant::now();
    let wall: SystemTime = chrono::DateTime::parse_from_rfc3339("2090-11-09T17:00:00Z")
        .unwrap()
        .with_timezone(&chrono::Utc)
        .into();
    let (context, snapshot) = {
        let mut state = owner.0.state.lock().unwrap();
        ready(&mut state, &session);
        assert_eq!(
            scheduler.next(&state, false, wall, monotonic).unwrap().kind,
            Kind::CivilDate
        );
        let mut calendar = state.calendar().unwrap().clone();
        calendar.fresh_until_time = admit::parse_timestamp("2090-11-09T17:00:30Z", true)
            .unwrap()
            .into();
        let mut capture = CalendarCapture::new(&calendar, &state.query().unwrap()).unwrap();
        capture
            .push(&wire::QueryOccurrencesResponse {
                calendar_etag: calendar.etag.clone(),
                freshness: wire::CalendarFreshness::Fresh.into(),
                provenance: wire::CalendarProvenance::Synthetic.into(),
                ..Default::default()
            })
            .unwrap();
        (state.context, Arc::new(capture.finish(&calendar).unwrap()))
    };
    let cancel = CancellationToken::new();
    let (sender, mut stream) = tokio::sync::mpsc::channel(1);
    sender
        .send(CalendarReadEvent::Published(snapshot.clone()))
        .await
        .unwrap();
    let event = stream.recv().await.unwrap();
    owner.read_event(
        context,
        &session,
        session.generation(),
        (&cancel, &cancel),
        event,
    );
    tokio::time::timeout(Duration::from_millis(100), owner.0.changed.notified())
        .await
        .unwrap();
    assert!(!stream.is_closed(), "the Watch task has not completed");
    {
        let state = owner.0.state.lock().unwrap();
        let deadline = scheduler.next(&state, false, wall, monotonic).unwrap();
        assert_eq!(deadline.kind, Kind::Expiry);
        assert_eq!(deadline.at - monotonic, Duration::from_secs(30));
    }
    // An unrelated unary read fails while this stream remains open: the owner
    // still schedules read recovery without preparing or replaying a write.
    owner.failed(context, Status::Offline);
    {
        let state = owner.0.state.lock().unwrap();
        assert_eq!(
            scheduler.next(&state, false, wall, monotonic).unwrap().kind,
            Kind::ReadRetry
        );
        assert!(state.job.is_none());
    }
    cancel.cancel();
    let before = owner.0.mailbox.metrics();
    owner.read_event(
        context,
        &session,
        session.generation(),
        (&cancel, &cancel),
        CalendarReadEvent::Invalidated,
    );
    assert_eq!(owner.0.mailbox.metrics(), before);
}

#[test]
fn a_disappeared_calendar_readmits_lifecycle_capabilities_without_resurrection_or_replay() {
    for activity in [Activity::Idle, Activity::Read, Activity::Write] {
        let session = session();
        let mut state = State::default();
        ready(&mut state, &session);
        state.catalog_needed = false;
        state.activity = activity;
        let old_context = state.context;
        state.not_materialized(session.generation());
        assert!(!state.editable());
        assert!(!state.ready);
        assert!(state.catalog_needed);
        assert_ne!(state.context, old_context);
        assert!(state.snapshot.is_none() && state.draft.is_none() && state.occurrence.is_none());
        assert!(state.job.is_none());
        assert!(!state.busy());
        assert_eq!(state.uncertain, activity == Activity::Write);
        assert!(!state.owns_write(old_context));
        // Only the server's new catalogue may change materialization/capability.
        assert!(state.catalog[0].materialized);
        state.catalog[0].materialized = false;
        state.catalog[0].mutation_access = wire::CalendarMutationAccess::Denied.into();
        state.catalog_needed = false;
        let readmitted_context = state.context;
        state.not_materialized(session.generation());
        assert!(state.ready);
        assert_eq!(
            state.context, readmitted_context,
            "no empty-source retry loop"
        );
        assert!(!state.catalog_needed);
        assert!(!state.editable());
        assert_eq!(state.uncertain, activity == Activity::Write);
        if activity == Activity::Write {
            assert!(!state.notice.is_empty());
        }
    }
    let session = session();
    let mut empty = State::default();
    ready(&mut empty, &session);
    empty.snapshot = None;
    empty.catalog[0].materialized = false;
    empty.catalog_needed = false;
    let context = empty.context;
    empty.not_materialized(session.generation());
    assert!(
        empty.editable(),
        "server-admitted new source remains creatable"
    );
    assert_eq!(empty.context, context);
    assert!(!empty.catalog_needed);
}
