use buffa::Message;
use home_client_model::api::{CapabilityName, Intent, Percentage, Power};
use home_wire_contract::{
    api_contract::{encode_command, parse_command},
    home::api::v2::ExecuteCapabilityRequest,
};

#[test]
fn shared_protobuf_command_corpus() {
    for line in include_str!("../../../contracts/api_v2_commands.tsv").lines() {
        if line.starts_with('#') {
            continue;
        }
        let parts: Vec<_> = line.split('\t').collect();
        assert_eq!(parts.len(), 4);
        let bytes: Vec<_> = parts[1]
            .as_bytes()
            .as_chunks::<2>()
            .0
            .iter()
            .map(|digits| u8::from_str_radix(std::str::from_utf8(digits).unwrap(), 16).unwrap())
            .collect();
        let request = ExecuteCapabilityRequest::decode_from_slice(&bytes).unwrap();
        let result = parse_command(&request);
        if parts[2] == "ok" {
            let command = result.unwrap_or_else(|error| panic!("{}: {error:?}", parts[0]));
            assert_eq!(
                parse_command(&encode_command(&command)),
                Ok(command.clone())
            );
            if parts[0] == "definition-etag" {
                assert_eq!(
                    command.definition_etag().unwrap().as_str(),
                    "definition-abc"
                );
            }
            if parts[0] == "off" {
                assert_eq!(command.intent(), Intent::Power(Power::Off));
            }
            if parts[0] == "zero" {
                assert_eq!(
                    command.intent(),
                    Intent::Level(Percentage::try_from(0.0).unwrap())
                );
            }
        } else {
            let error = result.expect_err(parts[0]);
            if matches!(
                parts[0],
                "light-off-brightness-transition" | "helper-toggle-value"
            ) {
                assert_eq!(
                    error.kind,
                    home_wire_contract::home::api::v2::ValidationReason::InconsistentFields
                );
            }
            assert_eq!(
                error.field,
                if parts[3] == "-" { "" } else { parts[3] },
                "{}",
                parts[0]
            );
        }
    }
}

#[test]
fn shared_resource_name_corpus() {
    for line in include_str!("../../../contracts/api_v2_names.tsv").lines() {
        if line.starts_with('#') {
            continue;
        }
        let (expected, input) = line.split_once('\t').unwrap();
        let input = if input == "-" { "" } else { input };
        let result = CapabilityName::try_from(input);
        assert_eq!(result.is_ok(), expected == "ok", "{input}");
        if let Ok(name) = result {
            assert_eq!(name.as_str(), input);
        }
    }
}

#[test]
fn percentage_rejects_nonfinite_without_losing_zero() {
    for value in [f64::NAN, f64::INFINITY, f64::NEG_INFINITY, -1.0, 101.0] {
        assert!(Percentage::try_from(value).is_err());
    }
    assert_eq!(Percentage::try_from(0.0).unwrap().value(), 0.0);
}
