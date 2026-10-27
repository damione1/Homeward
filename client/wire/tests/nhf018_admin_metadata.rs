use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{auth_contract, home::auth::v1 as auth};
fn at(seconds: i64) -> Timestamp {
    Timestamp {
        seconds,
        ..Default::default()
    }
}
fn session() -> auth::Session {
    auth::Session {
        id: auth::SessionId {
            value: "session-history".into(),
            ..Default::default()
        }
        .into(),
        account_id: auth::AccountId {
            value: "account-history".into(),
            ..Default::default()
        }
        .into(),
        created_at: at(1_700_000_000).into(),
        expires_at: at(1_700_000_900).into(),
        revoked_at: at(1_700_000_100).into(),
        authentication_satisfaction: auth::AuthenticationSatisfaction::PasswordOnly.into(),
        authenticated_at: at(1_700_000_000).into(),
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
        access_expires_at: at(1_700_000_800).into(),
        ..Default::default()
    };
    assert!(auth_contract::validate_received_session_tokens(&tokens, &at(1_700_001_000)).is_err());
}
#[test]
fn factor_and_invitation_metadata_refuse_future_variants_and_invalid_chronology() {
    let mut factor = auth::FactorCredential {
        id: auth::FactorCredentialId {
            value: "factor-synthetic".into(),
            ..Default::default()
        }
        .into(),
        account_id: auth::AccountId {
            value: "account-synthetic".into(),
            ..Default::default()
        }
        .into(),
        kind: auth::FactorKind::Fido2.into(),
        created_at: at(1_700_000_000).into(),
        ..Default::default()
    };
    assert!(auth_contract::admit_factor_metadata(&factor).is_ok());
    factor.kind = 999.into();
    assert!(auth_contract::admit_factor_metadata(&factor).is_err());
    let mut invitation = auth::Invitation {
        id: auth::InvitationId {
            value: "invite-synthetic".into(),
            ..Default::default()
        }
        .into(),
        login_name: "Invited member".into(),
        role: auth::AccountRole::User.into(),
        created_at: at(1_700_000_000).into(),
        expires_at: at(1_700_000_900).into(),
        revoked_at: at(1_700_000_100).into(),
        ..Default::default()
    };
    assert!(auth_contract::admit_invitation_metadata(&invitation).is_ok());
    invitation.expires_at = at(1_699_999_000).into();
    assert!(auth_contract::admit_invitation_metadata(&invitation).is_err());
}
#[test]
fn revoked_mcp_metadata_stays_distinct_from_interactive_session_audience() {
    let mut key = auth::McpKey {
        id: auth::McpKeyId {
            value: "mcp-history".into(),
            ..Default::default()
        }
        .into(),
        account_id: auth::AccountId {
            value: "account-history".into(),
            ..Default::default()
        }
        .into(),
        client_name: "Synthetic client".into(),
        audience: auth::CredentialAudience::Mcp.into(),
        created_at: at(1_700_000_000).into(),
        expires_at: at(1_700_000_900).into(),
        revoked_at: at(1_700_000_100).into(),
        ..Default::default()
    };
    assert!(auth_contract::admit_mcp_key_metadata(&key).is_ok());
    assert!(auth_contract::admit_mcp_key(&key, &at(1_700_000_200)).is_err());
    key.audience = auth::CredentialAudience::Interactive.into();
    assert!(auth_contract::admit_mcp_key_metadata(&key).is_err());
}
