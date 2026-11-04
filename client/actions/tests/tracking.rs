//! Current command tracking contract: UUID correlation, monotonic standard LRO
//! reads and qualified terminal evidence. Submission/stop ownership is exercised
//! by the CapabilityController unit tests, not a second tracking state machine.
use buffa::Message;
use buffa_types::google::protobuf::{Any, Timestamp};
use home_client_actions::new_request_id;
use home_client_model::api::{CapabilityName, Command, Deadline, Intent, Power};
use home_wire_contract::{
    api_command_operations::{CommandCompletion, CommandOutcome, admit_command_operation},
    google::longrunning::{Operation, operation},
    home::api::v2 as wire,
};

fn command() -> Command {
    Command::new(
        CapabilityName::try_from("homes/test/entities/relay/capabilities/power").unwrap(),
        Intent::Power(Power::Off),
        Some(new_request_id().unwrap()),
        Deadline::new(2_000_000_000, 0).unwrap(),
    )
}
fn pending(command: &Command, phase: wire::CommandPhase) -> Operation {
    Operation {
        name: "homes/test/operations/cmd-one".into(),
        metadata: Any {
            type_url: wire::CommandMetadata::TYPE_URL.into(),
            value: wire::CommandMetadata {
                capability: command.target().as_str().into(),
                request_id: command.request_id().unwrap().as_str().into(),
                create_time: Timestamp::from_unix_secs(1_700_000_000).into(),
                phase: phase.into(),
                ..Default::default()
            }
            .encode_to_vec()
            .into(),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn terminal(
    command: &Command,
    phase: wire::CommandPhase,
    outcome: wire::CommandOutcome,
) -> Operation {
    let mut raw = pending(command, phase);
    raw.done = true;
    raw.result = Some(operation::Result::Response(Box::new(Any {
        type_url: wire::ExecuteCapabilityResponse::TYPE_URL.into(),
        value: wire::ExecuteCapabilityResponse {
            capability: command.target().as_str().into(),
            outcome: outcome.into(),
            ..Default::default()
        }
        .encode_to_vec()
        .into(),
        ..Default::default()
    })));
    raw
}

#[test]
fn acknowledgement_remains_unverified_and_confirmation_requires_consistent_terminal_evidence() {
    let command = command();
    let ack = admit_command_operation(
        &pending(&command, wire::CommandPhase::Acknowledged),
        &command,
        None,
    )
    .unwrap();
    assert_eq!(ack.completion(), &CommandCompletion::Pending);
    let raw = terminal(
        &command,
        wire::CommandPhase::Acknowledged,
        wire::CommandOutcome::AcknowledgedUnverified,
    );
    let durable = admit_command_operation(&raw, &command, Some(&ack)).unwrap();
    assert!(matches!(
        durable.completion(),
        CommandCompletion::Success {
            outcome: CommandOutcome::AcknowledgedUnverified,
            ..
        }
    ));
    let wrong = terminal(
        &command,
        wire::CommandPhase::Acknowledged,
        wire::CommandOutcome::Confirmed,
    );
    assert!(admit_command_operation(&wrong, &command, Some(&ack)).is_err());
    assert!(
        admit_command_operation(
            &pending(&command, wire::CommandPhase::Confirmed),
            &command,
            Some(&ack)
        )
        .is_err()
    );
    let confirmed = terminal(
        &command,
        wire::CommandPhase::Confirmed,
        wire::CommandOutcome::Confirmed,
    );
    assert!(matches!(
        admit_command_operation(&confirmed, &command, Some(&ack))
            .unwrap()
            .completion(),
        CommandCompletion::Success {
            outcome: CommandOutcome::Confirmed,
            ..
        }
    ));
}

#[test]
fn reconnect_reads_cannot_regress_or_replace_an_immutable_terminal() {
    let command = command();
    let ack = admit_command_operation(
        &pending(&command, wire::CommandPhase::Acknowledged),
        &command,
        None,
    )
    .unwrap();
    assert!(
        admit_command_operation(
            &pending(&command, wire::CommandPhase::Dispatched),
            &command,
            Some(&ack)
        )
        .is_err()
    );
    let raw = terminal(
        &command,
        wire::CommandPhase::Confirmed,
        wire::CommandOutcome::Confirmed,
    );
    let confirmed = admit_command_operation(&raw, &command, Some(&ack)).unwrap();
    assert_eq!(
        admit_command_operation(&raw, &command, Some(&confirmed)).unwrap(),
        confirmed
    );
    assert!(
        admit_command_operation(
            &terminal(
                &command,
                wire::CommandPhase::Acknowledged,
                wire::CommandOutcome::AcknowledgedUnverified
            ),
            &command,
            Some(&confirmed)
        )
        .is_err()
    );
}

#[test]
fn foreign_identity_and_future_metadata_leave_the_previous_operation_untouched() {
    let command = command();
    let raw = pending(&command, wire::CommandPhase::Accepted);
    let prior = admit_command_operation(&raw, &command, None).unwrap();
    let mut foreign = raw.clone();
    foreign.name = "homes/test/operations/cmd-other".into();
    assert!(admit_command_operation(&foreign, &command, Some(&prior)).is_err());
    assert!(admit_command_operation(&raw, &self::command(), None).is_err());
    let mut future = raw.clone();
    let mut encoded = future.metadata.value.to_vec();
    encoded.extend([0xf8, 0x07, 0x01]);
    future.metadata.as_option_mut().unwrap().value = encoded.into();
    assert!(admit_command_operation(&future, &command, Some(&prior)).is_err());
    assert_eq!(
        admit_command_operation(&raw, &command, Some(&prior)).unwrap(),
        prior
    );
}

#[test]
fn future_phase_and_unexpected_result_type_are_rejected() {
    let command = command();
    let mut raw = pending(&command, wire::CommandPhase::Accepted);
    let mut metadata = wire::CommandMetadata::decode_from_slice(&raw.metadata.value).unwrap();
    metadata.phase = 999.into();
    raw.metadata.as_option_mut().unwrap().value = metadata.encode_to_vec().into();
    assert!(admit_command_operation(&raw, &command, None).is_err());
    let mut raw = terminal(
        &command,
        wire::CommandPhase::Confirmed,
        wire::CommandOutcome::Confirmed,
    );
    let Some(operation::Result::Response(value)) = &mut raw.result else {
        panic!("response fixture")
    };
    value.type_url = "type.googleapis.com/home.api.v2.Entity".into();
    assert!(admit_command_operation(&raw, &command, None).is_err());
}

#[test]
fn every_explicit_intention_gets_a_distinct_uuid4() {
    let first = new_request_id().unwrap();
    let second = new_request_id().unwrap();
    assert_ne!(first, second);
    assert_eq!(first.as_str().as_bytes()[14], b'4');
    assert!(matches!(
        first.as_str().as_bytes()[19],
        b'8' | b'9' | b'a' | b'b'
    ));
}
