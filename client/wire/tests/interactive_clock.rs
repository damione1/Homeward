use buffa::Message;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{auth_contract, home::auth::v1 as auth};

const NOW: i64 = 1_800_000_000;
fn timestamp(seconds: i64, nanos: i32) -> Timestamp {
    Timestamp {
        seconds,
        nanos,
        ..Default::default()
    }
}
fn tokens(offset_seconds: i64, offset_nanos: i32) -> auth::SessionTokens {
    let cwd = std::env::current_dir().unwrap();
    let bytes = std::fs::read(cwd.join("../../contracts/auth_golden/login-success.bin")).unwrap();
    let decision = auth::LoginDecision::decode_from_slice(&bytes).unwrap();
    let Some(auth::login_decision::Outcome::Authenticated(mut tokens)) = decision.outcome else {
        panic!("session fixture required")
    };
    tokens.session.as_option_mut().unwrap().created_at =
        timestamp(NOW + offset_seconds, offset_nanos).into();
    tokens.session.as_option_mut().unwrap().authenticated_at = tokens.session.created_at.clone();
    tokens
        .session
        .as_option_mut()
        .unwrap()
        .authentication_satisfaction = auth::AuthenticationSatisfaction::PasswordOnly.into();
    tokens.session.as_option_mut().unwrap().expires_at =
        timestamp(NOW + 3600 + offset_seconds, offset_nanos).into();
    tokens.access_expires_at = timestamp(NOW + 900 + offset_seconds, offset_nanos).into();
    *tokens
}

#[test]
fn independent_clocks_admit_small_skew_without_changing_exact_contract_admission() {
    let now = timestamp(NOW, 0);
    for (seconds, nanos) in [(-30, 0), (-1, 855_000_000), (0, 145_000_000), (30, 0)] {
        let value = tokens(seconds, nanos);
        assert!(auth_contract::validate_received_session_tokens(&value, &now).is_ok());
        if seconds >= 0 {
            assert!(
                auth_contract::validate_session_tokens(&value, &now).is_err(),
                "exact-clock admission must remain exact"
            );
        }
    }
    for value in [tokens(30, 1), tokens(31, 0), tokens(3600, 0)] {
        assert!(auth_contract::validate_received_session_tokens(&value, &now).is_err());
    }
}

#[test]
fn received_access_may_need_immediate_refresh_but_absolute_expiry_is_conservative() {
    let now = timestamp(NOW, 0);
    let mut value = tokens(0, 0);
    value.access_expires_at = timestamp(NOW - 1, 0).into();
    assert!(auth_contract::validate_received_session_tokens(&value, &now).is_ok());
    value.access_expires_at = timestamp(NOW - 30, 0).into();
    assert!(auth_contract::validate_received_session_tokens(&value, &now).is_err());
    value.access_expires_at = timestamp(NOW + 3, 0).into();
    value.session.as_option_mut().unwrap().expires_at = timestamp(NOW + 30, 0).into();
    assert!(auth_contract::validate_received_session_tokens(&value, &now).is_err());
    value.session.as_option_mut().unwrap().expires_at = timestamp(NOW + 30, 1).into();
    assert!(auth_contract::validate_received_session_tokens(&value, &now).is_ok());
    assert_eq!(
        auth_contract::interactive_expiration_now(&now).unwrap(),
        timestamp(NOW + 30, 0)
    );
}

#[test]
fn clock_uncertainty_does_not_accept_wrong_audience_authentication_order_or_unbounded_ttl() {
    let now = timestamp(NOW, 0);
    let mut value = tokens(0, 0);
    value.audience = auth::CredentialAudience::Mcp.into();
    assert!(auth_contract::validate_received_session_tokens(&value, &now).is_err());
    value.audience = auth::CredentialAudience::Interactive.into();
    value
        .session
        .as_option_mut()
        .unwrap()
        .authenticated_at
        .as_option_mut()
        .unwrap()
        .nanos = 1;
    assert!(auth_contract::validate_received_session_tokens(&value, &now).is_err());
    value.session.as_option_mut().unwrap().authenticated_at = value.session.created_at.clone();
    value.access_expires_at = timestamp(NOW + 930, 1).into();
    assert!(auth_contract::validate_received_session_tokens(&value, &now).is_err());
    for now in [
        timestamp(i64::MAX, 0),
        timestamp(NOW, 1_000_000_000),
        timestamp(253_402_300_799, 0),
    ] {
        assert!(auth_contract::interactive_expiration_now(&now).is_err());
    }
}
