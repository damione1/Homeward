use buffa::Message;
use connectrpc::{ConnectError, ErrorCode, ErrorDetail};
use home_client_transport::Failure;
use home_wire_contract::google::rpc::Status;

#[test]
fn native_connect_error_details_are_admitted_once_and_remain_redacted() {
    for line in include_str!("../../../contracts/api_error_cases.tsv").lines() {
        if line.starts_with('#') {
            continue;
        }
        let parts: Vec<_> = line.split('\t').collect();
        let bytes: Vec<_> = parts[1]
            .as_bytes()
            .as_chunks::<2>()
            .0
            .iter()
            .map(|digits| u8::from_str_radix(std::str::from_utf8(digits).unwrap(), 16).unwrap())
            .collect();
        let Ok(status) = Status::decode_from_slice(&bytes) else {
            continue;
        };
        let Some(code) = u32::try_from(status.code)
            .ok()
            .and_then(ErrorCode::from_grpc_code)
        else {
            continue;
        };
        let mut raw = ConnectError::new(code, "SYNTHETIC_DIAGNOSTIC_CANARY");
        for detail in status.details {
            use base64::Engine;
            raw.details.push(ErrorDetail {
                type_url: detail.type_url,
                value: Some(base64::engine::general_purpose::STANDARD.encode(detail.value)),
                debug: None,
            });
        }
        let failure = Failure::from(raw);
        assert!(!format!("{failure:?} {failure}").contains("SYNTHETIC_DIAGNOSTIC_CANARY"));
        let has_info = failure.rpc().unwrap().details.iter().any(|d| {
            d.type_url.ends_with("google.rpc.ErrorInfo")
                || d.type_url.ends_with("ExecutionUncertainty")
        });
        if has_info {
            let admitted = failure.api_failure();
            assert_eq!(
                matches!(admitted, Ok(Some(_))),
                parts[2] == "true",
                "{}",
                parts[0]
            );
            if admitted.is_err() {
                assert!(!failure.may_resume_read());
                assert!(!failure.may_recover_renewal());
            }
        } else {
            assert!(failure.api_failure().unwrap().is_none());
        }
    }
}

#[test]
fn malformed_base64_is_never_treated_as_absent() {
    let raw = ConnectError::new(ErrorCode::Unavailable, "ignored").with_detail(ErrorDetail {
        type_url: "google.rpc.ErrorInfo".into(),
        value: Some("%%%".into()),
        debug: None,
    });
    let failure = Failure::from(raw);
    assert!(failure.api_failure().is_err());
    assert!(!failure.may_resume_read());
}
