use buffa::Message;
use buffa_types::google::protobuf::{Duration, Timestamp};
use home_wire_contract::{auth_contract, home::auth::v1 as auth};

fn fixture(name: &str) -> Vec<u8> {
    std::fs::read(format!(
        "{}/../../contracts/auth_golden/{name}.bin",
        env!("CARGO_MANIFEST_DIR")
    ))
    .unwrap()
}
fn roundtrip<M: Message + Default>(name: &str) -> M {
    let bytes = fixture(name);
    let value = M::decode_from_slice(&bytes).unwrap();
    assert_eq!(value.encode_to_vec(), bytes, "{name} wire roundtrip");
    value
}
fn now() -> Timestamp {
    Timestamp {
        seconds: 1_792_314_000,
        ..Default::default()
    }
}

#[test]
fn nha146_cross_language_fixtures() {
    let index = include_str!("../../../contracts/auth_golden/index.tsv");
    let mut count = 0;
    for line in index
        .lines()
        .filter(|line| !line.is_empty() && !line.starts_with('#'))
    {
        let fields: Vec<_> = line.split('\t').collect();
        assert_eq!(fields.len(), 3);
        let [name, kind, accepted] = [fields[0], fields[1], fields[2]];
        let admitted = match kind {
            "Account" => auth_contract::admit_account(&roundtrip::<auth::Account>(name)).is_ok(),
            "LoginDecision" => {
                auth_contract::admit_login(&roundtrip::<auth::LoginDecision>(name), &now()).is_ok()
            }
            "AuthError" => auth_contract::admit_error(&roundtrip::<auth::AuthError>(name)).is_ok(),
            "McpKey" => {
                auth_contract::admit_mcp_key(&roundtrip::<auth::McpKey>(name), &now()).is_ok()
            }
            "CreateMcpKeyRequest" => auth_contract::admit_mcp_create_request(&roundtrip::<
                auth::CreateMcpKeyRequest,
            >(name))
            .is_ok(),
            "BeginFactorEnrollmentRequest" => {
                auth_contract::admit_begin_factor_enrollment(&roundtrip::<
                    auth::BeginFactorEnrollmentRequest,
                >(name))
                .is_ok()
            }
            "FactorProof" => {
                let login: auth::LoginDecision = roundtrip("login-challenge");
                let Some(auth::login_decision::Outcome::Challenge(challenge)) = &login.outcome
                else {
                    panic!("fixture challenge missing");
                };
                auth_contract::admit_factor_proof_for_challenge(
                    &roundtrip::<auth::FactorProof>(name),
                    challenge,
                    &now(),
                )
                .is_ok()
            }
            "CompleteLoginRequest" => {
                let login: auth::LoginDecision = roundtrip("login-challenge");
                let Some(auth::login_decision::Outcome::Challenge(challenge)) = &login.outcome
                else {
                    panic!("fixture challenge missing");
                };
                auth_contract::admit_complete_login_request(
                    &roundtrip::<auth::CompleteLoginRequest>(name),
                    challenge,
                    &now(),
                )
                .is_ok()
            }
            other => panic!("unhandled fixture type {other}"),
        };
        assert_eq!(admitted, accepted == "true", "{name}");
        count += 1;
    }
    assert_eq!(count, 37);
}

#[test]
fn nha146_absence_zero_and_immutable_snapshot() {
    let default: auth::CreateMcpKeyRequest = roundtrip("mcp-create-default");
    assert!(default.lifetime.as_option().is_none());
    assert_eq!(
        auth_contract::admit_mcp_lifetime(default.lifetime.as_option()),
        Ok(std::time::Duration::from_secs(30 * 86_400))
    );
    let zero: auth::CreateMcpKeyRequest = roundtrip("mcp-create-zero");
    assert!(zero.lifetime.as_option().is_some());
    assert!(auth_contract::admit_mcp_lifetime(zero.lifetime.as_option()).is_err());
    for (name, want) in [
        (
            "mcp-create-half-second",
            std::time::Duration::from_millis(500),
        ),
        (
            "mcp-create-one-half-second",
            std::time::Duration::from_millis(1500),
        ),
    ] {
        let request: auth::CreateMcpKeyRequest = roundtrip(name);
        assert_eq!(
            auth_contract::admit_mcp_lifetime(request.lifetime.as_option()),
            Ok(want)
        );
    }
    let over: auth::CreateMcpKeyRequest = roundtrip("mcp-create-overmax");
    assert!(auth_contract::admit_mcp_lifetime(over.lifetime.as_option()).is_err());
    let mut unknown = Duration {
        seconds: 1,
        nanos: 500_000_000,
        ..Default::default()
    };
    assert!(auth_contract::admit_mcp_lifetime(Some(&unknown)).is_ok());
    let mut encoded = unknown.encode_to_vec();
    encoded.extend_from_slice(&[0x98, 0x06, 0x01]);
    unknown = Duration::decode_from_slice(&encoded).unwrap();
    assert_eq!(unknown.encode_to_vec(), encoded);
    assert!(auth_contract::admit_mcp_lifetime(Some(&unknown)).is_err());

    let mut source: auth::LoginDecision = roundtrip("login-challenge");
    let admitted = auth_contract::admit_login(&source, &now()).unwrap();
    if let Some(auth::login_decision::Outcome::Challenge(challenge)) = &mut source.outcome {
        challenge.id.as_option_mut().unwrap().value = "changed-after-admission".into();
    }
    let Some(auth::login_decision::Outcome::Challenge(challenge)) = &admitted.wire().outcome else {
        panic!("admission lost challenge");
    };
    assert_eq!(challenge.id.value, "challenge-a");
    let unknown_oneof: auth::LoginDecision = roundtrip("login-future-oneof");
    assert!(unknown_oneof.outcome.is_none());
    assert!(!unknown_oneof.__buffa_unknown_fields.is_empty());
    assert!(auth_contract::admit_login(&unknown_oneof, &now()).is_err());
}

#[test]
fn nha146_recovery_is_only_an_offered_option() {
    let login: auth::LoginDecision = roundtrip("login-recovery-only");
    let Some(auth::login_decision::Outcome::Challenge(challenge)) = &login.outcome else {
        panic!("fixture challenge missing");
    };
    let fido: auth::FactorProof = roundtrip("factor-proof-fido");
    assert!(auth_contract::admit_factor_proof_for_challenge(&fido, challenge, &now()).is_err());
    let recovery: auth::FactorProof = roundtrip("factor-proof-recovery");
    assert!(auth_contract::admit_factor_proof_for_challenge(&recovery, challenge, &now()).is_ok());
    let absent: auth::FactorProof = roundtrip("factor-proof-absent");
    assert!(auth_contract::admit_factor_proof_for_challenge(&absent, challenge, &now()).is_err());
    let fido_only: auth::LoginDecision = roundtrip("login-fido-only");
    let Some(auth::login_decision::Outcome::Challenge(fido_challenge)) = &fido_only.outcome else {
        panic!("fixture FIDO challenge missing");
    };
    assert!(
        auth_contract::admit_factor_proof_for_challenge(&recovery, fido_challenge, &now()).is_err()
    );
    let duplicate: auth::LoginDecision = roundtrip("login-duplicate-options");
    assert!(auth_contract::admit_login(&duplicate, &now()).is_err());
}
