use crate::ProActionPhase as Phase;
use home_client_actions::capabilities::{
    CapabilitySnapshot, CapabilitySubmission, CapabilityTracking,
};
use home_wire_contract::api_command_operations::{CommandCompletion, CommandOutcome, CommandPhase};
use home_wire_contract::home::api::v2::ApiErrorReason;

type Presentation = (Phase, &'static str, &'static str);

// Keep command facts and admission phases intact. Normal progress is shown by
// the control's busy state; observed values are the only successful visual result.
pub(crate) fn quiet(
    presentation: Presentation,
    snapshot: Option<&CapabilitySnapshot>,
) -> Presentation {
    let routine = match presentation.0 {
        Phase::Preparing
        | Phase::Sending
        | Phase::Accepted
        | Phase::Dispatched
        | Phase::Confirmed => true,
        Phase::Acknowledged => snapshot
            .and_then(|snapshot| snapshot.operation.as_ref())
            .is_some_and(|operation| !operation.done()),
        Phase::Idle
        | Phase::AcknowledgedUnverified
        | Phase::Refused
        | Phase::Uncertain
        | Phase::Stopped
        | Phase::Unavailable
        | Phase::Interrupted
        | Phase::Failed
        | Phase::Cancelled
        | Phase::Indeterminate => false,
    };
    if routine {
        (presentation.0, "", "")
    } else {
        presentation
    }
}
pub(crate) fn presentation(snapshot: Option<&CapabilitySnapshot>) -> Presentation {
    quiet(facts(snapshot), snapshot)
}
// An unavailable live feed does not finish an accepted operation. Keep its
// control closed until the same operation supplies a terminal result; a
// standalone unavailable status without a pending operation adds no lock.
pub(super) fn blocks_input(snapshot: Option<&CapabilitySnapshot>) -> bool {
    snapshot.is_some_and(|snapshot| {
        snapshot.tracking == CapabilityTracking::Unavailable
            && snapshot
                .operation
                .as_ref()
                .is_some_and(|operation| !operation.done())
    })
}
fn facts(snapshot: Option<&CapabilitySnapshot>) -> Presentation {
    let Some(action) = snapshot else {
        return (Phase::Idle, "", "");
    };
    if let Some(operation) = &action.operation {
        match operation.completion() {
            CommandCompletion::Success {
                outcome: CommandOutcome::Confirmed,
                ..
            } => {
                return (
                    Phase::Confirmed,
                    "Change confirmed",
                    "The device confirmed the change.",
                );
            }
            CommandCompletion::Success {
                outcome: CommandOutcome::SentUnverified,
                ..
            } => {
                // Dispatch completion is terminal for input, never a physical ACK.
                return (Phase::Idle, "", "");
            }
            CommandCompletion::Success {
                outcome: CommandOutcome::AcknowledgedUnverified,
                ..
            } => {
                return (
                    Phase::AcknowledgedUnverified,
                    "Change not confirmed",
                    "Check the device before trying again.",
                );
            }
            CommandCompletion::Failure(failure) => {
                if failure.uncertainty().is_some() {
                    return (
                        Phase::Indeterminate,
                        "Change not confirmed",
                        "Check the device before trying again.",
                    );
                }
                if failure.reason() == ApiErrorReason::RequestCancelled {
                    return (
                        Phase::Cancelled,
                        "Change cancelled",
                        "Check the device before trying again.",
                    );
                }
                return (
                    Phase::Failed,
                    "Change failed",
                    "Check the device and try again.",
                );
            }
            CommandCompletion::Pending => {}
        }
    }
    match action.tracking {
        CapabilityTracking::Stopped => {
            return (
                Phase::Stopped,
                "Command tracking stopped",
                "Check the device before trying again.",
            );
        }
        CapabilityTracking::Unavailable => {
            return (
                Phase::Unavailable,
                "Command status unavailable",
                "The change has not been confirmed yet.",
            );
        }
        CapabilityTracking::Interrupted => {
            return (
                Phase::Interrupted,
                "Connection interrupted",
                "Check the device before trying again.",
            );
        }
        CapabilityTracking::Waiting | CapabilityTracking::Live | CapabilityTracking::Complete => {}
    }
    if let Some(operation) = &action.operation {
        return match operation.phase() {
            CommandPhase::Accepted => (
                Phase::Accepted,
                "Change requested",
                "Waiting for the device.",
            ),
            CommandPhase::Dispatched => {
                (Phase::Dispatched, "Change sent", "Waiting for the device.")
            }
            CommandPhase::Acknowledged => (
                Phase::Acknowledged,
                "Waiting for the device",
                "The change has not been confirmed yet.",
            ),
            CommandPhase::Confirmed
            | CommandPhase::Failed
            | CommandPhase::Cancelled
            | CommandPhase::Indeterminate => (
                Phase::Unavailable,
                "Command status unavailable",
                "Check the device before trying again.",
            ),
        };
    }
    match action.submission {
        CapabilitySubmission::Prepared => (Phase::Preparing, "Preparing change", ""),
        CapabilitySubmission::Sending => (Phase::Sending, "Sending change", ""),
        CapabilitySubmission::Accepted => (
            Phase::Accepted,
            "Change requested",
            "Waiting for the device.",
        ),
        CapabilitySubmission::Rejected => {
            (Phase::Refused, "Change refused", "Refresh and try again.")
        }
        CapabilitySubmission::Uncertain => (
            Phase::Uncertain,
            "Change not confirmed",
            "Check the device before trying again.",
        ),
        CapabilitySubmission::NotSent => (Phase::Refused, "Change not sent", "Please try again."),
        CapabilitySubmission::Closed => (
            Phase::Stopped,
            "Command tracking stopped",
            "Check the device before trying again.",
        ),
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use buffa::Message;
    use buffa_types::google::protobuf::{Any, Timestamp};
    use home_client_model::api::{CapabilityName, Command, Deadline, Intent, Power, RequestId};
    use home_wire_contract::{
        api_command_operations::admit_command_operation, google::longrunning, home::api::v2 as wire,
    };

    pub(crate) fn snapshot(outcome: Option<wire::CommandOutcome>) -> CapabilitySnapshot {
        let id = RequestId::try_from("12345678-1234-4123-8123-123456789abc").unwrap();
        let target =
            CapabilityName::try_from("homes/synthetic/entities/relay/capabilities/power").unwrap();
        let command = Command::new(
            target.clone(),
            Intent::Power(Power::On),
            Some(id.clone()),
            Deadline::new(1800000000, 0).unwrap(),
        );
        let phase = if outcome == Some(wire::CommandOutcome::Confirmed) {
            wire::CommandPhase::Confirmed
        } else if outcome == Some(wire::CommandOutcome::SentUnverified) {
            wire::CommandPhase::Dispatched
        } else {
            wire::CommandPhase::Acknowledged
        };
        let metadata = wire::CommandMetadata {
            capability: target.as_str().into(),
            request_id: id.as_str().into(),
            create_time: Timestamp::from_unix_secs(1700000000).into(),
            phase: phase.into(),
            live_progress_available: outcome.is_none(),
            ..Default::default()
        };
        let raw = longrunning::Operation {
            name: "homes/synthetic/operations/cmd-test".into(),
            done: outcome.is_some(),
            metadata: Any {
                type_url: "type.googleapis.com/home.api.v2.CommandMetadata".into(),
                value: metadata.encode_to_vec().into(),
                ..Default::default()
            }
            .into(),
            result: outcome.map(|outcome| {
                longrunning::operation::Result::Response(Box::new(Any {
                    type_url: "type.googleapis.com/home.api.v2.ExecuteCapabilityResponse".into(),
                    value: wire::ExecuteCapabilityResponse {
                        capability: target.as_str().into(),
                        outcome: outcome.into(),
                        ..Default::default()
                    }
                    .encode_to_vec()
                    .into(),
                    ..Default::default()
                }))
            }),
            ..Default::default()
        };
        CapabilitySnapshot {
            request_id: id,
            submission: CapabilitySubmission::Accepted,
            tracking: CapabilityTracking::Live,
            operation: Some(admit_command_operation(&raw, &command, None).unwrap()),
            failure: None,
            issue: None,
        }
    }
    #[test]
    fn pending_and_terminal_ack_never_render_confirmation() {
        let pending = snapshot(None);
        assert_eq!(presentation(Some(&pending)).0, Phase::Acknowledged);
        assert_eq!(presentation(Some(&pending)), (Phase::Acknowledged, "", ""));
        let terminal = snapshot(Some(wire::CommandOutcome::AcknowledgedUnverified));
        assert_eq!(
            presentation(Some(&terminal)).0,
            Phase::AcknowledgedUnverified
        );
        assert!(presentation(Some(&terminal)).1.contains("not confirmed"));
        let confirmed = snapshot(Some(wire::CommandOutcome::Confirmed));
        assert_eq!(presentation(Some(&confirmed)), (Phase::Confirmed, "", ""));
    }
    #[test]
    fn terminal_ack_releases_the_real_detail_slider_but_keeps_uncertainty_visible() {
        use i_slint_backend_testing::ElementQuery;
        use slint::{ComponentHandle, Model, ModelRc};
        use std::{cell::RefCell, rc::Rc};
        slint::platform::set_platform(Box::new(i_slint_backend_testing::TestingBackend::new(
            i_slint_backend_testing::TestingBackendOptions {
                mock_time: true,
                renderer_name: Some("software".into()),
                ..Default::default()
            },
        )))
        .unwrap();
        let window = crate::create_window().unwrap();
        window.set_show_endpoint(false);
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_view_loaded(true);
        window.set_connection_status(crate::ConnectionStatus::Live);
        window.set_commands_ready(true);
        window.set_page(crate::ViewPage::Detail);
        window
            .window()
            .set_size(slint::LogicalSize::new(900., 1400.));
        window.show().unwrap();
        let render = |name: &str| {
            if let Some(directory) = std::env::var_os("HOMEWARD_POSTQA_RENDER_DIR") {
                let directory = std::path::PathBuf::from(directory);
                std::fs::create_dir_all(&directory).unwrap();
                let pixels = window.window().take_snapshot().unwrap();
                let file = std::fs::File::create(directory.join(format!("{name}.png"))).unwrap();
                let mut encoder = png::Encoder::new(file, pixels.width(), pixels.height());
                encoder.set_color(png::ColorType::Rgba);
                encoder.set_depth(png::BitDepth::Eight);
                encoder
                    .write_header()
                    .unwrap()
                    .write_image_data(pixels.as_bytes())
                    .unwrap();
            }
        };
        let calls = Rc::new(RefCell::new(Vec::new()));
        let output = calls.clone();
        window.on_device_brightness(move |t, g, v| output.borrow_mut().push((t, g, v)));
        let publish = |snapshot: &CapabilitySnapshot, access| {
            let (phase, label, description) = presentation(Some(snapshot));
            let mut row = super::super::presentation::CardRow {
                token: 0,
                generation: 1,
                title: "Synthetic light".into(),
                kind: crate::ProDeviceKind::Light,
                power_known: true,
                power_on: true,
                brightness_known: true,
                brightness_percent: 62.037033,
                access,
                phase,
                action_label: label.into(),
                action_detail: description.into(),
                command_unavailable: blocks_input(Some(snapshot)),
                ..Default::default()
            };
            super::super::brightness_bounds(
                &mut row,
                super::super::number::Bounds::percentage_definition(
                    &home_wire_contract::home::api::v2::PercentageBounds {
                        minimum: 0.,
                        maximum: 100.,
                        ..Default::default()
                    },
                ),
            );
            let panel =
                super::super::detail::project(&row, row.ui(None, &Default::default()), None);
            window.set_detail_panels(ModelRc::from([panel]));
            slint::platform::update_timers_and_animations();
        };
        let slider = || {
            ElementQuery::from_root(&window)
                .match_inherits("KitSlider")
                .find_first()
                .expect("real detail slider")
        };
        publish(&snapshot(None), crate::ProControlAccess::Allowed);
        assert_eq!(slider().accessible_value().as_deref(), Some("62%"));
        render("ack-pending");
        slider().invoke_accessible_increment_action();
        assert!(calls.borrow().is_empty(), "pending ACK blocks input");
        let mut unavailable = snapshot(None);
        unavailable.tracking = CapabilityTracking::Unavailable;
        publish(&unavailable, crate::ProControlAccess::Allowed);
        assert_eq!(presentation(Some(&unavailable)).0, Phase::Unavailable);
        assert!(blocks_input(Some(&unavailable)));
        slider().invoke_accessible_increment_action();
        assert!(
            calls.borrow().is_empty(),
            "unavailable progress is still pending"
        );
        render("ack-pending-unavailable");
        let terminal = snapshot(Some(wire::CommandOutcome::AcknowledgedUnverified));
        let mut terminal_unavailable = terminal.clone();
        terminal_unavailable.tracking = CapabilityTracking::Unavailable;
        assert!(!blocks_input(Some(&terminal_unavailable)));
        let mut no_operation = unavailable.clone();
        no_operation.operation = None;
        assert!(!blocks_input(Some(&no_operation)));
        publish(&terminal, crate::ProControlAccess::Allowed);
        render("ack-terminal");
        slider().invoke_accessible_increment_action();
        assert_eq!(&*calls.borrow(), &[(0, 1, 63.)]);
        assert_eq!(
            window
                .get_detail_panels()
                .row_data(0)
                .unwrap()
                .light
                .brightness,
            62.037033,
            "no optimistic observation"
        );
        ElementQuery::from_root(&window)
            .match_predicate(|e| e.accessible_label().as_deref() == Some("Last command"))
            .find_first()
            .expect("historical command remains consultable")
            .invoke_accessible_default_action();
        slint::platform::update_timers_and_animations();
        assert!(
            ElementQuery::from_root(&window)
                .match_predicate(|e| e
                    .accessible_label()
                    .is_some_and(|s| s.contains("Change not confirmed")))
                .find_first()
                .is_some(),
            "uncertainty remains visible"
        );
        // Exercise actual pointer gestures, including a second gesture after
        // a pending/terminal transition. Accessibility alone missed this path.
        use slint::platform::{PointerEventButton, WindowEvent};
        let tap = |fraction: f32| {
            let element = slider();
            let origin = element.absolute_position();
            let size = element.size();
            let position = slint::LogicalPosition::new(
                origin.x + size.width * 0.5,
                origin.y + size.height * fraction,
            );
            window
                .window()
                .dispatch_event(WindowEvent::PointerMoved { position });
            window.window().dispatch_event(WindowEvent::PointerPressed {
                position,
                button: PointerEventButton::Left,
            });
            window
                .window()
                .dispatch_event(WindowEvent::PointerReleased {
                    position,
                    button: PointerEventButton::Left,
                });
            slint::platform::update_timers_and_animations();
        };
        tap(0.213);
        assert_eq!(calls.borrow().len(), 2);
        publish(&snapshot(None), crate::ProControlAccess::Allowed);
        tap(0.743);
        assert_eq!(calls.borrow().len(), 2, "pending blocks pointer input");
        publish(&terminal, crate::ProControlAccess::Allowed);
        tap(0.743);
        assert_eq!(calls.borrow().len(), 3);
        for (_, _, value) in calls.borrow().iter() {
            assert_eq!(
                value.fract(),
                0.,
                "pointer and accessible intents are integers"
            );
            assert!((0.0..=100.0).contains(value));
        }
        assert_ne!(calls.borrow()[1].2, calls.borrow()[2].2);
        render("ack-terminal-second-pointer");
        publish(&terminal, crate::ProControlAccess::Denied);
        tap(0.421);
        slider().invoke_accessible_increment_action();
        assert_eq!(
            calls.borrow().len(),
            3,
            "terminal ACK does not grant authority"
        );
    }
    #[test]
    fn stopping_tracking_cannot_cancel_an_effect_or_erase_a_recorded_confirmation() {
        let mut pending = snapshot(None);
        pending.tracking = CapabilityTracking::Stopped;
        assert_eq!(presentation(Some(&pending)).0, Phase::Stopped);
        let mut confirmed = snapshot(Some(wire::CommandOutcome::Confirmed));
        confirmed.tracking = CapabilityTracking::Stopped;
        assert_eq!(presentation(Some(&confirmed)), (Phase::Confirmed, "", ""));
    }
    #[test]
    fn routine_progress_is_silent_but_problems_keep_their_feedback() {
        let mut progress = snapshot(None);
        progress.operation = None;
        for submission in [
            CapabilitySubmission::Prepared,
            CapabilitySubmission::Sending,
            CapabilitySubmission::Accepted,
        ] {
            progress.submission = submission;
            let (_, label, detail) = presentation(Some(&progress));
            assert!(label.is_empty() && detail.is_empty());
        }
        for submission in [
            CapabilitySubmission::Rejected,
            CapabilitySubmission::NotSent,
            CapabilitySubmission::Uncertain,
            CapabilitySubmission::Closed,
        ] {
            progress.submission = submission;
            let (_, label, detail) = presentation(Some(&progress));
            assert!(!label.is_empty() && !detail.is_empty());
        }
    }
}

#[cfg(test)]
mod dispatch_only_tests {
    use super::*;
    #[test]
    fn sent_without_ack_releases_input_without_claiming_confirmation() {
        let snapshot = super::tests::snapshot(Some(
            home_wire_contract::home::api::v2::CommandOutcome::SentUnverified,
        ));
        assert_eq!(presentation(Some(&snapshot)), (Phase::Idle, "", ""));
        assert!(!blocks_input(Some(&snapshot)));
    }
}
