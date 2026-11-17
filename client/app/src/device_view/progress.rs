use crate::ProActionPhase as Phase;
use home_client_actions::capabilities::{
    CapabilitySnapshot, JobState, PendingStage, TerminalCause, job_state,
};
use home_wire_contract::api_command_operations::{CommandCompletion, CommandOutcome};
use home_wire_contract::home::api::v2::ApiErrorReason;

type Presentation = (Phase, &'static str, &'static str);

/// How a control shows its value around one gesture. The control is always
/// editable: a pending gesture shows the intended value, the observed state
/// wins as soon as it arrives, and an unconfirmed gesture snaps back silently.
/// Only a real refusal or failure leaves a notice.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum ControlDisplay {
    Observed,
    Pending,
    Reverted(RevertCause),
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum RevertCause {
    /// Dispatched or acknowledged without a qualified observation, or the
    /// outcome is unknown. The observed value is shown without a notice.
    Unverified,
    NotSent,
    Refused,
    Failed,
    Cancelled,
}

pub(crate) fn display(snapshot: Option<&CapabilitySnapshot>) -> ControlDisplay {
    let Some(action) = snapshot else {
        return ControlDisplay::Observed;
    };
    if let Some(operation) = &action.operation {
        match operation.completion() {
            CommandCompletion::Success {
                outcome: CommandOutcome::Confirmed,
                ..
            } => return ControlDisplay::Observed,
            CommandCompletion::Success {
                outcome: CommandOutcome::SentUnverified | CommandOutcome::AcknowledgedUnverified,
                ..
            } => return ControlDisplay::Reverted(RevertCause::Unverified),
            CommandCompletion::Failure(failure) => {
                if failure.uncertainty().is_some() {
                    return ControlDisplay::Reverted(RevertCause::Unverified);
                }
                if failure.reason() == ApiErrorReason::RequestCancelled {
                    return ControlDisplay::Reverted(RevertCause::Cancelled);
                }
                return ControlDisplay::Reverted(RevertCause::Failed);
            }
            CommandCompletion::Pending => {}
        }
    }
    match job_state(action) {
        JobState::Pending(
            PendingStage::Preparing
            | PendingStage::Sending
            | PendingStage::Deferred
            | PendingStage::Tracking,
        ) => ControlDisplay::Pending,
        JobState::Terminal(TerminalCause::NotSent) => {
            ControlDisplay::Reverted(RevertCause::NotSent)
        }
        JobState::Terminal(TerminalCause::Rejected) => {
            ControlDisplay::Reverted(RevertCause::Refused)
        }
        JobState::Terminal(
            TerminalCause::Uncertain | TerminalCause::Interrupted | TerminalCause::Unavailable,
        ) => ControlDisplay::Reverted(RevertCause::Unverified),
        // A replaced, stopped or closed job is not this control's story any more;
        // a completed one without an operation has nothing to show.
        JobState::Terminal(
            TerminalCause::Superseded
            | TerminalCause::Closed
            | TerminalCause::Stopped
            | TerminalCause::Complete,
        ) => ControlDisplay::Observed,
    }
}

pub(crate) fn pending(snapshot: Option<&CapabilitySnapshot>) -> bool {
    display(snapshot) == ControlDisplay::Pending
}

// Tracking phases stay internal. The row carries a phase and a notice only for
// a confirmed result (silent, success tone) or a real refusal/failure.
pub(crate) fn presentation(snapshot: Option<&CapabilitySnapshot>) -> Presentation {
    let confirmed = snapshot
        .and_then(|action| action.operation.as_ref())
        .is_some_and(|operation| {
            matches!(
                operation.completion(),
                CommandCompletion::Success {
                    outcome: CommandOutcome::Confirmed,
                    ..
                }
            )
        });
    match display(snapshot) {
        ControlDisplay::Observed => {
            if confirmed {
                (Phase::Confirmed, "", "")
            } else {
                (Phase::Idle, "", "")
            }
        }
        ControlDisplay::Pending | ControlDisplay::Reverted(RevertCause::Unverified) => {
            (Phase::Idle, "", "")
        }
        ControlDisplay::Reverted(RevertCause::NotSent) => {
            (Phase::Refused, "Change not sent", "Please try again.")
        }
        ControlDisplay::Reverted(RevertCause::Refused) => {
            (Phase::Refused, "Change refused", "Refresh and try again.")
        }
        ControlDisplay::Reverted(RevertCause::Failed) => (
            Phase::Failed,
            "Change failed",
            "Check the device and try again.",
        ),
        ControlDisplay::Reverted(RevertCause::Cancelled) => (
            Phase::Cancelled,
            "Change cancelled",
            "Check the device before trying again.",
        ),
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use buffa::Message;
    use buffa_types::google::protobuf::{Any, Timestamp};
    use home_client_actions::capabilities::{CapabilitySubmission, CapabilityTracking};
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
    fn pending_and_unverified_results_are_silent_and_only_confirmation_or_failure_speak() {
        let pending = snapshot(None);
        assert_eq!(display(Some(&pending)), ControlDisplay::Pending);
        assert_eq!(presentation(Some(&pending)), (Phase::Idle, "", ""));
        let terminal = snapshot(Some(wire::CommandOutcome::AcknowledgedUnverified));
        assert_eq!(
            display(Some(&terminal)),
            ControlDisplay::Reverted(RevertCause::Unverified)
        );
        assert_eq!(
            presentation(Some(&terminal)),
            (Phase::Idle, "", ""),
            "an unverified acknowledgement snaps back without a notice"
        );
        let confirmed = snapshot(Some(wire::CommandOutcome::Confirmed));
        assert_eq!(display(Some(&confirmed)), ControlDisplay::Observed);
        assert_eq!(presentation(Some(&confirmed)), (Phase::Confirmed, "", ""));
        assert_eq!(presentation(None), (Phase::Idle, "", ""));
    }
    #[test]
    fn every_admission_outcome_maps_to_a_display_without_locking() {
        use CapabilitySubmission as S;
        use CapabilityTracking as T;
        let mut progress = snapshot(None);
        progress.operation = None;
        let cases = [
            (
                S::Prepared,
                T::Waiting,
                ControlDisplay::Pending,
                Phase::Idle,
            ),
            (S::Sending, T::Waiting, ControlDisplay::Pending, Phase::Idle),
            (
                S::Deferred,
                T::Waiting,
                ControlDisplay::Pending,
                Phase::Idle,
            ),
            (S::Accepted, T::Live, ControlDisplay::Pending, Phase::Idle),
            (
                S::Uncertain,
                T::Unavailable,
                ControlDisplay::Reverted(RevertCause::Unverified),
                Phase::Idle,
            ),
            (
                S::Accepted,
                T::Interrupted,
                ControlDisplay::Reverted(RevertCause::Unverified),
                Phase::Idle,
            ),
            (
                S::Accepted,
                T::Unavailable,
                ControlDisplay::Reverted(RevertCause::Unverified),
                Phase::Idle,
            ),
            (
                S::Rejected,
                T::Unavailable,
                ControlDisplay::Reverted(RevertCause::Refused),
                Phase::Refused,
            ),
            (
                S::NotSent,
                T::Stopped,
                ControlDisplay::Reverted(RevertCause::NotSent),
                Phase::Refused,
            ),
            (
                S::Superseded,
                T::Stopped,
                ControlDisplay::Observed,
                Phase::Idle,
            ),
            (S::Closed, T::Stopped, ControlDisplay::Observed, Phase::Idle),
            (
                S::Accepted,
                T::Stopped,
                ControlDisplay::Observed,
                Phase::Idle,
            ),
        ];
        for (submission, tracking, expected, phase) in cases {
            progress.submission = submission;
            progress.tracking = tracking;
            assert_eq!(
                display(Some(&progress)),
                expected,
                "{submission:?} {tracking:?}"
            );
            let (actual, label, _) = presentation(Some(&progress));
            assert_eq!(actual, phase, "{submission:?} {tracking:?}");
            assert_eq!(label.is_empty(), phase == Phase::Idle);
        }
    }
    #[test]
    fn a_pending_gesture_never_blocks_the_real_detail_slider_and_the_observed_value_returns() {
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
        let publish = |snapshot: &CapabilitySnapshot, intended: Option<f32>, access| {
            let (phase, label, description) = presentation(Some(snapshot));
            let mut row = super::super::presentation::CardRow {
                token: 0,
                generation: 1,
                title: "Synthetic light".into(),
                kind: crate::ProDeviceKind::Light,
                power_known: true,
                power_on: true,
                brightness_known: true,
                brightness_percent: intended.unwrap_or(62.037033),
                access,
                phase,
                action_label: label.into(),
                action_detail: description.into(),
                pending: pending(Some(snapshot)),
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
        // The pending gesture shows its intended value and stays editable.
        publish(&snapshot(None), Some(80.), crate::ProControlAccess::Allowed);
        // Kit 0.8.0: the accessible value is numeric; the readout is the description.
        assert_eq!(slider().accessible_value().as_deref(), Some("80"));
        render("pending-optimistic");
        slider().invoke_accessible_increment_action();
        assert_eq!(
            &*calls.borrow(),
            &[(0, 1, 81.)],
            "a pending gesture never blocks input"
        );
        // An unverified terminal snaps back to the observed value, silently.
        let terminal = snapshot(Some(wire::CommandOutcome::AcknowledgedUnverified));
        publish(&terminal, None, crate::ProControlAccess::Allowed);
        render("unverified-reverted");
        assert_eq!(slider().accessible_value().as_deref(), Some("62%"));
        assert_eq!(
            window
                .get_detail_panels()
                .row_data(0)
                .unwrap()
                .light
                .brightness,
            62.037033,
            "the observed value wins once the gesture is settled"
        );
        assert!(
            ElementQuery::from_root(&window)
                .match_predicate(|e| e
                    .accessible_label()
                    .is_some_and(|s| s.contains("not confirmed") || s.contains("Stop Tracking")))
                .find_first()
                .is_none(),
            "no tracking notice and no stop control"
        );
        slider().invoke_accessible_increment_action();
        assert_eq!(calls.borrow().len(), 2);
        // Exercise actual pointer gestures across pending and settled states.
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
        assert_eq!(calls.borrow().len(), 3);
        publish(&snapshot(None), Some(40.), crate::ProControlAccess::Allowed);
        tap(0.743);
        assert_eq!(calls.borrow().len(), 4, "pending keeps pointer input open");
        for (_, _, value) in calls.borrow().iter() {
            assert_eq!(
                value.fract(),
                0.,
                "pointer and accessible intents are integers"
            );
            assert!((0.0..=100.0).contains(value));
        }
        render("pending-second-pointer");
        let mut refused = snapshot(None);
        refused.operation = None;
        refused.submission = CapabilitySubmission::Rejected;
        refused.tracking = CapabilityTracking::Unavailable;
        publish(&refused, None, crate::ProControlAccess::Allowed);
        assert!(
            ElementQuery::from_root(&window)
                .match_predicate(|e| e
                    .accessible_label()
                    .is_some_and(|s| s.contains("Change refused")))
                .find_first()
                .is_some(),
            "a real refusal is noticed"
        );
        publish(&terminal, None, crate::ProControlAccess::Denied);
        tap(0.421);
        slider().invoke_accessible_increment_action();
        assert_eq!(
            calls.borrow().len(),
            4,
            "a settled gesture does not grant authority"
        );
    }
    #[test]
    fn stopping_tracking_cannot_cancel_an_effect_or_erase_a_recorded_confirmation() {
        let mut pending = snapshot(None);
        pending.tracking = CapabilityTracking::Stopped;
        assert_eq!(display(Some(&pending)), ControlDisplay::Observed);
        let mut confirmed = snapshot(Some(wire::CommandOutcome::Confirmed));
        confirmed.tracking = CapabilityTracking::Stopped;
        assert_eq!(presentation(Some(&confirmed)), (Phase::Confirmed, "", ""));
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
        assert_eq!(
            display(Some(&snapshot)),
            ControlDisplay::Reverted(RevertCause::Unverified)
        );
        assert!(!pending(Some(&snapshot)));
    }
}
