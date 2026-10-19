use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{auth_contract, home::auth::v1 as auth};

// The same historical wire image is loaded by contracts/nha142_contract_test.go.
fn login_success() -> auth::LoginDecision {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../contracts/auth_golden/login-success.bin"
    );
    let bytes = std::fs::read(path).expect("read shared login-success fixture");
    let value = auth::LoginDecision::decode_from_slice(&bytes).expect("decode login-success");
    assert_eq!(value.encode_to_vec(), bytes, "shared wire image changed");
    value
}
fn now() -> Timestamp {
    Timestamp {
        seconds: 1_792_314_000,
        ..Default::default()
    }
}
fn session_mut(value: &mut auth::LoginDecision) -> &mut auth::Session {
    let Some(auth::login_decision::Outcome::Authenticated(tokens)) = &mut value.outcome else {
        panic!("fixture lost authenticated outcome");
    };
    tokens
        .session
        .as_option_mut()
        .expect("fixture lost session")
}
fn session(value: &auth::LoginDecision) -> &auth::Session {
    let Some(auth::login_decision::Outcome::Authenticated(tokens)) = &value.outcome else {
        panic!("fixture lost authenticated outcome");
    };
    tokens.session.as_option().expect("fixture lost session")
}

#[test]
fn nha142_session_authentication_admission() {
    let base = login_success();
    assert!(
        auth_contract::admit_login(&base, &now()).is_ok(),
        "legacy absent metadata"
    );
    for method in [
        auth::AuthenticationSatisfaction::PasswordOnly,
        auth::AuthenticationSatisfaction::PasswordFido2,
        auth::AuthenticationSatisfaction::PasswordRecoveryCode,
    ] {
        assert_eq!(
            auth_contract::admit_authentication_satisfaction(method.into()),
            Ok(method)
        );
        let mut decision = base.clone();
        let source = session_mut(&mut decision);
        source.authentication_satisfaction = method.into();
        source.authenticated_at = source.created_at.clone();
        let admitted = auth_contract::admit_login(&decision, &now()).expect("valid evidence");
        session_mut(&mut decision).authentication_satisfaction =
            auth::AuthenticationSatisfaction::Unspecified.into();
        session_mut(&mut decision)
            .authenticated_at
            .as_option_mut()
            .unwrap()
            .seconds += 1;
        let kept = session(admitted.wire());
        assert_eq!(kept.authentication_satisfaction, EnumValue::Known(method));
        assert_eq!(
            kept.authenticated_at, kept.created_at,
            "snapshot aliased source"
        );
        for flaw in 0..4 {
            let mut invalid = admitted.wire().clone();
            let value = session_mut(&mut invalid);
            match flaw {
                0 => value.authenticated_at = Default::default(),
                1 => value.authenticated_at.as_option_mut().unwrap().seconds += 1,
                2 => {
                    value.authenticated_at.as_option_mut().unwrap().seconds = now().seconds + 1;
                    value.created_at.as_option_mut().unwrap().seconds = now().seconds + 1;
                }
                3 => value.authenticated_at.as_option_mut().unwrap().nanos = 1_000_000_000,
                _ => unreachable!(),
            }
            assert!(
                auth_contract::admit_login(&invalid, &now()).is_err(),
                "method {method:?}, flaw {flaw}"
            );
        }
    }
    for method in [
        EnumValue::Known(auth::AuthenticationSatisfaction::Unspecified),
        EnumValue::Unknown(99),
    ] {
        assert!(auth_contract::admit_authentication_satisfaction(method).is_err());
        let mut invalid = base.clone();
        let value = session_mut(&mut invalid);
        value.authentication_satisfaction = method;
        value.authenticated_at = value.created_at.clone();
        assert!(auth_contract::admit_login(&invalid, &now()).is_err());
    }
    let mut explicit_zero_time = base;
    session_mut(&mut explicit_zero_time).authenticated_at = Timestamp::default().into();
    assert!(auth_contract::admit_login(&explicit_zero_time, &now()).is_err());
}

fn version(revision: u64) -> auth::PolicyVersion {
    auth::PolicyVersion {
        revision,
        source_sha256: vec![0x42; 32],
        ..Default::default()
    }
}
fn policy() -> auth::SecondFactorPolicy {
    auth::SecondFactorPolicy {
        version: version(1).into(),
        updated_at: Timestamp {
            seconds: 1_792_310_000,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}

#[test]
fn nha142_second_factor_policy_admission() {
    let mut request = auth::SetSecondFactorPolicyRequest {
        require_admin_factor: true,
        expected: version(1).into(),
        ..Default::default()
    };
    let admitted = auth_contract::admit_set_second_factor_policy(&request).expect("valid CAS");
    request.expected.as_option_mut().unwrap().source_sha256[0] = 0;
    assert_eq!(
        admitted.wire().expected.as_option().unwrap().source_sha256[0],
        0x42
    );
    for revision in [1, i64::MAX as u64] {
        request.expected = version(revision).into();
        assert!(auth_contract::admit_set_second_factor_policy(&request).is_ok());
    }
    let invalid_versions = [
        None,
        Some(version(0)),
        Some(version(i64::MAX as u64 + 1)),
        Some(auth::PolicyVersion {
            source_sha256: vec![0x42; 31],
            ..version(1)
        }),
        Some(auth::PolicyVersion {
            source_sha256: vec![0x42; 33],
            ..version(1)
        }),
    ];
    for (index, invalid) in invalid_versions.into_iter().enumerate() {
        let mut candidate = request.clone();
        candidate.expected = invalid.into();
        assert!(
            auth_contract::admit_set_second_factor_policy(&candidate).is_err(),
            "CAS case {index}"
        );
    }
    let valid_policy = policy();
    assert!(
        auth_contract::admit_get_second_factor_policy_response(
            &auth::GetSecondFactorPolicyResponse {
                policy: valid_policy.clone().into(),
                ..Default::default()
            }
        )
        .is_ok()
    );
    for continuation in [
        auth::SecondFactorSessionContinuation::Continues,
        auth::SecondFactorSessionContinuation::ReauthenticationRequired,
    ] {
        assert!(
            auth_contract::admit_set_second_factor_policy_response(
                &auth::SetSecondFactorPolicyResponse {
                    policy: valid_policy.clone().into(),
                    continuation: continuation.into(),
                    ..Default::default()
                }
            )
            .is_ok()
        );
    }
    for continuation in [
        EnumValue::Known(auth::SecondFactorSessionContinuation::Unspecified),
        EnumValue::Unknown(99),
    ] {
        assert!(
            auth_contract::admit_set_second_factor_policy_response(
                &auth::SetSecondFactorPolicyResponse {
                    policy: valid_policy.clone().into(),
                    continuation,
                    ..Default::default()
                }
            )
            .is_err()
        );
    }
    let invalid_policies = [
        None,
        Some(auth::SecondFactorPolicy {
            version: Default::default(),
            ..valid_policy.clone()
        }),
        Some(auth::SecondFactorPolicy {
            updated_at: Default::default(),
            ..valid_policy.clone()
        }),
        Some(auth::SecondFactorPolicy {
            updated_at: Timestamp {
                seconds: 1_792_310_000,
                nanos: 1_000_000_000,
                ..Default::default()
            }
            .into(),
            ..valid_policy
        }),
    ];
    for (index, invalid) in invalid_policies.into_iter().enumerate() {
        let field = invalid.clone().into();
        assert!(
            auth_contract::admit_get_second_factor_policy_response(
                &auth::GetSecondFactorPolicyResponse {
                    policy: field,
                    ..Default::default()
                }
            )
            .is_err(),
            "get policy case {index}"
        );
        assert!(
            auth_contract::admit_set_second_factor_policy_response(
                &auth::SetSecondFactorPolicyResponse {
                    policy: invalid.into(),
                    continuation: auth::SecondFactorSessionContinuation::Continues.into(),
                    ..Default::default()
                }
            )
            .is_err(),
            "set policy case {index}"
        );
    }
}

#[test]
fn nha142_policy_error_and_unix_reset_admission() {
    for (code, field, valid) in [
        (
            EnumValue::Known(auth::SecondFactorPolicyErrorCode::InvalidVersion),
            "expected",
            true,
        ),
        (
            EnumValue::Known(auth::SecondFactorPolicyErrorCode::VersionConflict),
            "expected",
            true,
        ),
        (
            EnumValue::Known(auth::SecondFactorPolicyErrorCode::AuthorityUnavailable),
            "",
            true,
        ),
        (
            EnumValue::Known(auth::SecondFactorPolicyErrorCode::InvalidVersion),
            "",
            false,
        ),
        (
            EnumValue::Known(auth::SecondFactorPolicyErrorCode::VersionConflict),
            "policy",
            false,
        ),
        (
            EnumValue::Known(auth::SecondFactorPolicyErrorCode::AuthorityUnavailable),
            "expected",
            false,
        ),
        (
            EnumValue::Known(auth::SecondFactorPolicyErrorCode::Unspecified),
            "expected",
            false,
        ),
        (EnumValue::Unknown(99), "expected", false),
    ] {
        let detail = auth::SecondFactorPolicyError {
            code,
            field: field.into(),
            ..Default::default()
        };
        assert_eq!(
            auth_contract::admit_second_factor_policy_error(&detail).is_ok(),
            valid,
            "{code:?}/{field}"
        );
    }
    for id in ["account-a".to_owned(), "a".repeat(128)] {
        let request = auth::ResetSecondFactorRequest {
            account_id: auth::AccountId {
                value: id,
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        assert!(auth_contract::admit_reset_second_factor(&request).is_ok());
    }
    for account in [None, Some(String::new()), Some("a".repeat(129))] {
        let request = auth::ResetSecondFactorRequest {
            account_id: account
                .map(|value| auth::AccountId {
                    value,
                    ..Default::default()
                })
                .into(),
            ..Default::default()
        };
        assert!(auth_contract::admit_reset_second_factor(&request).is_err());
    }
}
