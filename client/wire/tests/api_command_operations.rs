use buffa::Message;
use buffa_types::google::protobuf::{Any, Timestamp};
use home_client_model::api::{CapabilityName, Command, Deadline, Intent, Power, RequestId};
use home_wire_contract::{
    api_command_operations::admit_command_operation, api_operations::InvalidOperation,
    google::longrunning::Operation, home::api::v2 as wire,
};

fn command(request: Option<RequestId>) -> Command {
    Command::new(
        CapabilityName::try_from("homes/synthetic/entities/relay/capabilities/power").unwrap(),
        Intent::Power(Power::Off),
        request,
        Deadline::new(2000000000, 0).unwrap(),
    )
}
fn operation(command: &Command, request_id: &str) -> Operation {
    Operation {
        name: "homes/synthetic/operations/cmd-server-occurrence".into(),
        metadata: Any {
            type_url: "type.googleapis.com/home.api.v2.CommandMetadata".into(),
            value: wire::CommandMetadata {
                capability: command.target().as_str().into(),
                request_id: request_id.into(),
                create_time: Timestamp::from_unix_secs(1700000000).into(),
                phase: wire::CommandPhase::Accepted.into(),
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
#[test]
fn server_occurrence_never_substitutes_for_the_optional_client_request_identity() {
    let omitted = command(None);
    assert!(admit_command_operation(&operation(&omitted, ""), &omitted, None).is_ok());
    assert_eq!(
        admit_command_operation(
            &operation(&omitted, "12345678-1234-4123-8123-123456789abc"),
            &omitted,
            None
        ),
        Err(InvalidOperation::Inconsistent)
    );
    let id = RequestId::try_from("12345678-1234-4123-8123-123456789abc").unwrap();
    let supplied = command(Some(id.clone()));
    assert!(admit_command_operation(&operation(&supplied, id.as_str()), &supplied, None).is_ok());
    for changed in ["", "22345678-1234-4123-8123-123456789abc"] {
        assert_eq!(
            admit_command_operation(&operation(&supplied, changed), &supplied, None),
            Err(InvalidOperation::Inconsistent)
        );
    }
}
