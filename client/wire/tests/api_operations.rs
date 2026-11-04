use buffa::Message;
use buffa_types::google::protobuf::Any;
use home_client_model::api::OperationName;
use home_wire_contract::{api_operations::admit_envelope, google::longrunning as wire};

#[test]
fn operation_envelope_distinguishes_pending_terminal_and_foreign_result() {
    let name = OperationName::try_from("homes/synthetic/operations/cmd-synthetic").unwrap();
    let metadata = Any {
        type_url: "type.googleapis.com/home.api.v2.CommandMetadata".into(),
        ..Default::default()
    };
    let pending = wire::Operation {
        name: name.as_str().into(),
        metadata: metadata.into(),
        ..Default::default()
    };
    assert!(admit_envelope(&pending, &name).is_ok());
    let mut changed = pending.clone();
    changed.done = true;
    assert!(admit_envelope(&changed, &name).is_err());
    let result = Any {
        type_url: "type.googleapis.com/home.api.v2.ExecuteCapabilityResponse".into(),
        ..Default::default()
    };
    changed.result = Some(wire::operation::Result::Response(Box::new(result)));
    assert!(admit_envelope(&changed, &name).is_ok());
    changed.done = false;
    assert!(admit_envelope(&changed, &name).is_err());
    changed = pending.clone();
    changed.name = "homes/foreign/operations/cmd-synthetic".into();
    assert!(admit_envelope(&changed, &name).is_err());
    changed = pending.clone();
    let mut encoded = changed.encode_to_vec();
    encoded.extend([0xf8, 0x07, 0x01]);
    changed = wire::Operation::decode_from_slice(&encoded).unwrap();
    assert!(admit_envelope(&changed, &name).is_err());
    changed = pending;
    changed.metadata = Any {
        type_url: "type.googleapis.com/home.api.v2.CommandMetadata".into(),
        value: vec![0; 65 * 1024].into(),
        ..Default::default()
    }
    .into();
    assert!(admit_envelope(&changed, &name).is_err());
}
