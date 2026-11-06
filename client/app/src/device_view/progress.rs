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
                    "Effect confirmed",
                    "The server recorded confirmation. Observed state remains separate.",
                );
            }
            CommandCompletion::Success {
                outcome: CommandOutcome::AcknowledgedUnverified,
                ..
            } => {
                return (
                    Phase::AcknowledgedUnverified,
                    "Acknowledged · effect unverified",
                    "Tracking has ended. The acknowledgement does not establish the physical result.",
                );
            }
            CommandCompletion::Failure(failure) => {
                if failure.uncertainty().is_some() {
                    return (
                        Phase::Indeterminate,
                        "Couldn’t confirm the result",
                        "The command may have executed. It will not be submitted again automatically.",
                    );
                }
                if failure.reason() == ApiErrorReason::RequestCancelled {
                    return (
                        Phase::Cancelled,
                        "Command ended as cancelled",
                        "Only the server’s recorded result establishes this status.",
                    );
                }
                return (
                    Phase::Failed,
                    "Command failed",
                    "The server recorded a failure. No observed value is used to infer an effect.",
                );
            }
            CommandCompletion::Pending => {}
        }
    }
    match action.tracking {
        CapabilityTracking::Stopped => {
            return (
                Phase::Stopped,
                "Tracking stopped on this device",
                "This does not cancel a command the server has already accepted.",
            );
        }
        CapabilityTracking::Unavailable => {
            return (
                Phase::Unavailable,
                "Live tracking unavailable",
                "The recorded progress is retained. No additional physical effect is inferred.",
            );
        }
        CapabilityTracking::Interrupted => {
            return (
                Phase::Interrupted,
                "Tracking interrupted",
                "The outcome remains unconfirmed. No new submission will be made.",
            );
        }
        CapabilityTracking::Waiting | CapabilityTracking::Live | CapabilityTracking::Complete => {}
    }
    if let Some(operation) = &action.operation {
        return match operation.phase() {
            CommandPhase::Accepted => (
                Phase::Accepted,
                "Command accepted",
                "The durable admission does not establish an effect.",
            ),
            CommandPhase::Dispatched => (
                Phase::Dispatched,
                "Command forwarded",
                "The physical result has not been confirmed.",
            ),
            CommandPhase::Acknowledged => (
                Phase::Acknowledged,
                "Acknowledged · waiting for confirmation",
                "The observed state remains independent of this command.",
            ),
            CommandPhase::Confirmed
            | CommandPhase::Failed
            | CommandPhase::Cancelled
            | CommandPhase::Indeterminate => (
                Phase::Unavailable,
                "Tracking unavailable",
                "No additional confirmation is inferred.",
            ),
        };
    }
    match action.submission {
        CapabilitySubmission::Prepared => (
            Phase::Preparing,
            "Checking command",
            "The gesture is discarded if its view or permissions change.",
        ),
        CapabilitySubmission::Sending => (
            Phase::Sending,
            "Sending",
            "This action is submitted only once.",
        ),
        CapabilitySubmission::Accepted => (
            Phase::Accepted,
            "Command accepted",
            "Tracking the same operation without another submission.",
        ),
        CapabilitySubmission::Rejected => (
            Phase::Refused,
            "Rejected",
            "The server refused this intention. Refresh the view before another gesture.",
        ),
        CapabilitySubmission::Uncertain => (
            Phase::Uncertain,
            "Submission uncertain",
            "The command may have executed. It will not be submitted again automatically.",
        ),
        CapabilitySubmission::NotSent => (
            Phase::Refused,
            "Not sent",
            "The gesture context was no longer valid before submission.",
        ),
        CapabilitySubmission::Closed => (
            Phase::Stopped,
            "Tracking closed",
            "Closing this device’s tracking does not cancel a physical effect.",
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
        assert!(presentation(Some(&terminal)).1.contains("unverified"));
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
        let terminal = snapshot(Some(wire::CommandOutcome::AcknowledgedUnverified));
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
        assert!(
            ElementQuery::from_root(&window)
                .match_predicate(|e| e
                    .accessible_label()
                    .is_some_and(|s| s.contains("effect unverified")))
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
