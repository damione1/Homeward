use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{auth_contract, home::api::v2 as auth};

// The same current public wire image is loaded by contracts/nha203_auth_resources_test.go.
fn login_success() -> auth::LoginDecision {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../contracts/auth_resource_golden/login-success.bin"
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
        "explicit NOT_RECORDED metadata"
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
        source.authentication_time = source.create_time.clone();
        let admitted = auth_contract::admit_login(&decision, &now()).expect("valid evidence");
        session_mut(&mut decision).authentication_satisfaction =
            auth::AuthenticationSatisfaction::Unspecified.into();
        session_mut(&mut decision)
            .authentication_time
            .as_option_mut()
            .unwrap()
            .seconds += 1;
        let kept = session(admitted.wire());
        assert_eq!(kept.authentication_satisfaction, EnumValue::Known(method));
        assert_eq!(
            kept.authentication_time, kept.create_time,
            "snapshot aliased source"
        );
        for flaw in 0..4 {
            let mut invalid = admitted.wire().clone();
            let value = session_mut(&mut invalid);
            match flaw {
                0 => value.authentication_time = Default::default(),
                1 => value.authentication_time.as_option_mut().unwrap().seconds += 1,
                2 => {
                    value.authentication_time.as_option_mut().unwrap().seconds = now().seconds + 1;
                    value.create_time.as_option_mut().unwrap().seconds = now().seconds + 1;
                }
                3 => value.authentication_time.as_option_mut().unwrap().nanos = 1_000_000_000,
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
        value.authentication_time = value.create_time.clone();
        assert!(auth_contract::admit_login(&invalid, &now()).is_err());
    }
    let mut explicit_zero_time = base;
    session_mut(&mut explicit_zero_time).authentication_time = Timestamp::default().into();
    assert!(auth_contract::admit_login(&explicit_zero_time, &now()).is_err());
}

fn policy() -> auth::SecondFactorPolicy {
    auth::SecondFactorPolicy {
        name: "homes/home-a/secondFactorPolicy".into(),
        etag: "opaque-cas".into(),
        update_time: Timestamp {
            seconds: 1_792_310_000,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
#[test]
fn nha142_second_factor_policy_admission() {
    let mut request = auth::ChangeSecondFactorPolicyRequest {
        name: "homes/home-a/secondFactorPolicy".into(),
        etag: "opaque-cas".into(),
        require_admin_factor: true,
        ..Default::default()
    };
    let admitted = auth_contract::admit_change_second_factor_policy(&request).expect("valid CAS");
    request.etag = "changed-after-admission".into();
    assert_eq!(admitted.wire().etag, "opaque-cas");
    for flaw in 0..4 {
        let mut invalid = request.clone();
        match flaw {
            0 => invalid.name.clear(),
            1 => invalid.name = "homes/home-a/accounts/a/authorizationPolicy".into(),
            2 => invalid.etag.clear(),
            3 => invalid.etag = "a".repeat(129),
            _ => unreachable!(),
        }
        assert!(auth_contract::admit_change_second_factor_policy(&invalid).is_err());
    }
    let valid = policy();
    assert!(auth_contract::admit_second_factor_policy(&valid).is_ok());
    for continuation in [
        auth::SecondFactorSessionContinuation::Continues,
        auth::SecondFactorSessionContinuation::ReauthenticationRequired,
    ] {
        assert!(
            auth_contract::admit_change_second_factor_policy_response(
                &auth::ChangeSecondFactorPolicyResponse {
                    policy: valid.clone().into(),
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
            auth_contract::admit_change_second_factor_policy_response(
                &auth::ChangeSecondFactorPolicyResponse {
                    policy: valid.clone().into(),
                    continuation,
                    ..Default::default()
                }
            )
            .is_err()
        );
    }
    for flaw in 0..5 {
        let mut invalid = valid.clone();
        match flaw {
            0 => invalid.name.clear(),
            1 => invalid.etag.clear(),
            2 => invalid.update_time = Default::default(),
            3 => invalid.update_time.as_option_mut().unwrap().nanos = 1_000_000_000,
            4 => invalid.etag = "a".repeat(129),
            _ => unreachable!(),
        }
        assert!(auth_contract::admit_second_factor_policy(&invalid).is_err());
        assert!(
            auth_contract::admit_change_second_factor_policy_response(
                &auth::ChangeSecondFactorPolicyResponse {
                    policy: invalid.into(),
                    continuation: auth::SecondFactorSessionContinuation::Continues.into(),
                    ..Default::default()
                }
            )
            .is_err()
        );
    }
    assert!(
        auth_contract::admit_change_second_factor_policy_response(
            &auth::ChangeSecondFactorPolicyResponse {
                continuation: auth::SecondFactorSessionContinuation::Continues.into(),
                ..Default::default()
            }
        )
        .is_err()
    );
}
