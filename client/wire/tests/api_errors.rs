use buffa::{Enumeration, Message};
use home_wire_contract::{
    api_errors::{self, InvalidApiError},
    google::rpc::{self, Status},
};

#[test]
fn shared_canonical_errors_admit_without_human_messages() {
    for line in include_str!("../../../contracts/api_error_cases.tsv").lines() {
        if line.starts_with('#') {
            continue;
        }
        let parts: Vec<_> = line.split('\t').collect();
        assert_eq!(parts.len(), 5);
        let bytes: Vec<_> = parts[1]
            .as_bytes()
            .as_chunks::<2>()
            .0
            .iter()
            .map(|digits| u8::from_str_radix(std::str::from_utf8(digits).unwrap(), 16).unwrap())
            .collect();
        let status = Status::decode_from_slice(&bytes);
        let admitted = status
            .as_ref()
            .ok()
            .and_then(|status| api_errors::admit_status(status).ok());
        assert_eq!(admitted.is_some(), parts[2] == "true", "{}", parts[0]);
        if let Some(failure) = admitted {
            assert_eq!(
                failure.reason().proto_name(),
                format!("API_ERROR_REASON_{}", parts[3])
            );
            assert_eq!(failure.code() as i32, parts[4].parse::<i32>().unwrap());
            assert!(!format!("{failure:?}").contains("SYNTHETIC_DIAGNOSTIC_CANARY"));
        }
    }
}

#[test]
fn oversized_native_errors_are_not_admitted() {
    let status = Status {
        code: rpc::Code::INTERNAL as i32,
        message: "x".repeat(api_errors::MAX_DETAILS_BYTES + 1),
        ..Default::default()
    };
    assert_eq!(
        api_errors::admit_status(&status),
        Err(InvalidApiError::Limit)
    );
}
