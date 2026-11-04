use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{auth_contract, home::api::v2 as auth};
fn at(seconds: i64) -> Timestamp {
    Timestamp {
        seconds,
        ..Default::default()
    }
}
fn session() -> auth::Session {
    auth::Session {
        name: "homes/h/accounts/account-history/sessions/session-history".into(),
        create_time: at(1_700_000_000).into(),
        expire_time: at(1_700_000_900).into(),
        revoke_time: at(1_700_000_100).into(),
        authentication_satisfaction: auth::AuthenticationSatisfaction::PasswordOnly.into(),
        authentication_time: at(1_700_000_000).into(),
        ..Default::default()
    }
}
#[test]
fn historical_session_metadata_does_not_become_an_active_credential() {
    let value = session();
    assert!(auth_contract::admit_session_metadata(&value).is_ok());
    let tokens = auth::SessionTokens {
        session: value.into(),
        audience: auth::CredentialAudience::Interactive.into(),
        access_token: "private-access".into(),
        refresh_token: "private-refresh".into(),
        access_expire_time: at(1_700_000_800).into(),
        ..Default::default()
    };
    assert!(auth_contract::validate_received_session_tokens(&tokens, &at(1_700_001_000)).is_err());
}
#[test]
fn factor_and_invitation_metadata_refuse_future_variants_and_invalid_chronology() {
    let mut factor = auth::FactorCredential {
        name: "homes/h/accounts/account-synthetic/factorCredentials/factor-synthetic".into(),
        kind: auth::FactorKind::Fido2.into(),
        create_time: at(1_700_000_000).into(),
        ..Default::default()
    };
    assert!(auth_contract::admit_factor_metadata(&factor).is_ok());
    factor.kind = 999.into();
    assert!(auth_contract::admit_factor_metadata(&factor).is_err());
    let mut invitation = auth::Invitation {
        name: "homes/h/invitations/invite-synthetic".into(),
        login_name: "Invited member".into(),
        role: auth::AccountRole::User.into(),
        create_time: at(1_700_000_000).into(),
        expire_time: at(1_700_000_900).into(),
        revoke_time: at(1_700_000_100).into(),
        ..Default::default()
    };
    assert!(auth_contract::admit_invitation_metadata(&invitation).is_ok());
    invitation.expire_time = at(1_699_999_000).into();
    assert!(auth_contract::admit_invitation_metadata(&invitation).is_err());
}
#[test]
fn revoked_mcp_metadata_stays_distinct_from_interactive_session_audience() {
    let mut key = auth::McpKey {
        name: "homes/h/accounts/account-history/mcpKeys/mcp-history".into(),
        client_name: "Synthetic client".into(),
        audience: auth::CredentialAudience::Mcp.into(),
        create_time: at(1_700_000_000).into(),
        expire_time: at(1_700_000_900).into(),
        revoke_time: at(1_700_000_100).into(),
        ..Default::default()
    };
    assert!(auth_contract::admit_mcp_key_metadata(&key).is_ok());
    assert!(auth_contract::admit_mcp_key(&key, &at(1_700_000_200)).is_err());
    key.audience = auth::CredentialAudience::Interactive.into();
    assert!(auth_contract::admit_mcp_key_metadata(&key).is_err());
}
