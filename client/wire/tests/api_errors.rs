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

#[test]
fn operation_handle_is_typed_and_only_admitted_with_uncertainty() {
    use buffa_types::google::protobuf::Any;
    use home_wire_contract::home::api::v2 as api;
    fn detail<T: Message>(kind: &str, value: T) -> Any {
        Any {
            type_url: format!("type.googleapis.com/{kind}"),
            value: value.encode_to_vec().into(),
            ..Default::default()
        }
    }
    let resource = rpc::ResourceInfo {
        resource_type: "longrunning.googleapis.com/Operation".into(),
        resource_name: "homes/synthetic/operations/command-one".into(),
        ..Default::default()
    };
    let mut status = Status {
        code: rpc::Code::UNKNOWN as i32,
        details: vec![
            detail(
                "google.rpc.ErrorInfo",
                rpc::ErrorInfo {
                    domain: api_errors::DOMAIN.into(),
                    reason: "COMMIT_OUTCOME_UNKNOWN".into(),
                    ..Default::default()
                },
            ),
            detail(
                "home.api.v2.ExecutionUncertainty",
                api::ExecutionUncertainty {
                    phase: api::UncertaintyPhase::Commit.into(),
                    disposition: api::ReconciliationDisposition::ReconcileOnly.into(),
                    ..Default::default()
                },
            ),
            detail("google.rpc.ResourceInfo", resource.clone()),
        ],
        ..Default::default()
    };
    let admitted = api_errors::admit_status(&status).unwrap();
    assert_eq!(
        admitted.operation().unwrap().as_str(),
        resource.resource_name
    );
    assert_eq!(admitted.support_reference(), None);
    for bad in [
        rpc::ResourceInfo {
            resource_type: "homeward/Operation".into(),
            ..resource.clone()
        },
        rpc::ResourceInfo {
            resource_name: "homes/synthetic/entities/command-one".into(),
            ..resource.clone()
        },
        rpc::ResourceInfo {
            owner: "SYNTHETIC_SECRET".into(),
            ..resource.clone()
        },
        rpc::ResourceInfo {
            description: "SYNTHETIC_SECRET".into(),
            ..resource.clone()
        },
    ] {
        status.details[2] = detail("google.rpc.ResourceInfo", bad);
        assert!(api_errors::admit_status(&status).is_err());
    }
    status.details[2] = detail("google.rpc.ResourceInfo", resource);
    status.details.push(status.details[2].clone());
    assert_eq!(
        api_errors::admit_status(&status),
        Err(InvalidApiError::Duplicate)
    );
    status.details.pop();
    status.details.remove(1);
    status.details[0] = detail(
        "google.rpc.ErrorInfo",
        rpc::ErrorInfo {
            domain: api_errors::DOMAIN.into(),
            reason: "ACCESS_DENIED".into(),
            ..Default::default()
        },
    );
    status.code = rpc::Code::PERMISSION_DENIED as i32;
    assert_eq!(
        api_errors::admit_status(&status),
        Err(InvalidApiError::Inconsistent)
    );
}
