use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{auth_contract, authorization_contract, home::api::v2 as auth};

fn time(seconds: i64) -> Timestamp {
    Timestamp {
        seconds,
        ..Default::default()
    }
}
#[test]
fn unrecorded_evidence_is_explicit_and_never_invents_a_timestamp() {
    let mut session = auth::Session {
        name: "homes/h/accounts/a/sessions/s".into(),
        create_time: time(100).into(),
        expire_time: time(1000).into(),
        authentication_satisfaction: auth::AuthenticationSatisfaction::NotRecorded.into(),
        ..Default::default()
    };
    assert!(auth_contract::admit_session_metadata(&session).is_ok());
    session.authentication_time = time(100).into();
    assert!(auth_contract::admit_session_metadata(&session).is_err());
    session.authentication_satisfaction = auth::AuthenticationSatisfaction::Unspecified.into();
    session.authentication_time = Default::default();
    assert!(auth_contract::admit_session_metadata(&session).is_err());
}
#[test]
fn policy_default_has_no_fabricated_cas_or_update_time() {
    let mut policy = auth::AuthorizationPolicy {
        name: "homes/h/accounts/a/authorizationPolicy".into(),
        yaml_source: b"schema: homeward.authorization/1\naccount_id: opaque-native\ngrants: []\n"
            .to_vec(),
        schema: authorization_contract::POLICY_SCHEMA.into(),
        syntax_profile: authorization_contract::POLICY_SYNTAX_PROFILE.into(),
        origin: auth::AuthorizationPolicyOrigin::Default.into(),
        ..Default::default()
    };
    assert!(authorization_contract::admit_get_response(&policy).is_ok());
    policy.etag = "invented".into();
    assert!(authorization_contract::admit_get_response(&policy).is_err());
    policy.etag.clear();
    policy.update_time = time(100).into();
    assert!(authorization_contract::admit_get_response(&policy).is_err());
    policy.origin = auth::AuthorizationPolicyOrigin::Stored.into();
    policy.etag = "opaque-server-cas".into();
    assert!(authorization_contract::admit_get_response(&policy).is_ok());
}
