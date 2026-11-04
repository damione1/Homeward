//! Pure admission of auth wire values. No password, FIDO2, token, or rights verification.

use crate::home::{api::v2 as auth, auth::v1 as native};
use buffa::EnumValue;
use buffa_types::google::protobuf::{Duration, Timestamp};
pub use home_client_model::auth::{
    AccountName, FactorCredentialName, InvitationName, McpKeyName, SecondFactorPolicyName,
    SessionName,
};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AdmissionError {
    Invalid,
}

macro_rules! identity {
    ($name:ident, $wire:ty) => {
        #[derive(Debug, Clone, PartialEq, Eq)]
        pub struct $name(String);
        impl $name {
            pub fn parse(value: &str) -> Result<Self, AdmissionError> {
                if value.is_empty() || value.len() > 128 || value.contains('\0') {
                    return Err(AdmissionError::Invalid);
                }
                Ok(Self(value.to_owned()))
            }
            pub fn as_str(&self) -> &str {
                &self.0
            }
        }
        impl TryFrom<&$wire> for $name {
            type Error = AdmissionError;
            fn try_from(value: &$wire) -> Result<Self, Self::Error> {
                if !value.__buffa_unknown_fields.is_empty() {
                    return Err(AdmissionError::Invalid);
                }
                Self::parse(&value.value)
            }
        }
    };
}
identity!(AccountId, native::AccountId);
identity!(SessionId, native::SessionId);
identity!(ChallengeId, native::ChallengeId);
identity!(InstanceId, native::InstanceId);

/// Owns an immutable admitted snapshot. The source wire value may be mutated later.
pub struct Admitted<T> {
    wire: T,
}
impl<T> Admitted<T> {
    pub fn wire(&self) -> &T {
        &self.wire
    }
}

fn valid_timestamp(value: &Timestamp) -> bool {
    (-62_135_596_800..=253_402_300_799).contains(&value.seconds)
        && (0..1_000_000_000).contains(&value.nanos)
        && value.__buffa_unknown_fields.is_empty()
}
fn timestamp(
    value: &buffa::MessageField<Timestamp, buffa::Inline<Timestamp>>,
) -> Option<&Timestamp> {
    value.as_option().filter(|value| valid_timestamp(value))
}
fn future(
    value: &buffa::MessageField<Timestamp, buffa::Inline<Timestamp>>,
    now: &Timestamp,
) -> bool {
    timestamp(value).is_some_and(|value| (value.seconds, value.nanos) > (now.seconds, now.nanos))
}
fn text(value: &str) -> bool {
    !value.is_empty()
}

pub fn admit_role(role: EnumValue<auth::AccountRole>) -> Result<auth::AccountRole, AdmissionError> {
    match role {
        EnumValue::Known(auth::AccountRole::Admin) => Ok(auth::AccountRole::Admin),
        EnumValue::Known(auth::AccountRole::User) => Ok(auth::AccountRole::User),
        EnumValue::Known(auth::AccountRole::Unspecified) | EnumValue::Unknown(_) => {
            Err(AdmissionError::Invalid)
        }
    }
}
fn status(value: EnumValue<auth::AccountAccess>) -> bool {
    match value {
        EnumValue::Known(auth::AccountAccess::Enabled)
        | EnumValue::Known(auth::AccountAccess::Disabled) => true,
        EnumValue::Known(auth::AccountAccess::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
fn family(value: EnumValue<auth::ScopeFamily>) -> bool {
    match value {
        EnumValue::Known(auth::ScopeFamily::StateRegistry)
        | EnumValue::Known(auth::ScopeFamily::History)
        | EnumValue::Known(auth::ScopeFamily::Media)
        | EnumValue::Known(auth::ScopeFamily::Location)
        | EnumValue::Known(auth::ScopeFamily::Actions)
        | EnumValue::Known(auth::ScopeFamily::Dashboards)
        | EnumValue::Known(auth::ScopeFamily::Calendars)
        | EnumValue::Known(auth::ScopeFamily::Lists)
        | EnumValue::Known(auth::ScopeFamily::Logic)
        | EnumValue::Known(auth::ScopeFamily::Integrations)
        | EnumValue::Known(auth::ScopeFamily::GlobalConfiguration)
        | EnumValue::Known(auth::ScopeFamily::Accounts)
        | EnumValue::Known(auth::ScopeFamily::Backups) => true,
        EnumValue::Known(auth::ScopeFamily::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
fn operation(value: EnumValue<auth::ScopeOperation>) -> bool {
    match value {
        EnumValue::Known(auth::ScopeOperation::Read)
        | EnumValue::Known(auth::ScopeOperation::Write)
        | EnumValue::Known(auth::ScopeOperation::Administer) => true,
        EnumValue::Known(auth::ScopeOperation::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
pub fn admit_scope(value: &auth::Scope) -> Result<Admitted<auth::Scope>, AdmissionError> {
    if value.__buffa_unknown_fields.is_empty() && family(value.family) && operation(value.operation)
    {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}

pub fn admit_account(value: &auth::Account) -> Result<Admitted<auth::Account>, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || AccountName::try_from(value.name.as_str()).is_err()
        || !value.initial_password.is_empty()
        || value.etag.is_empty()
        || value.etag.len() > 128
        || value.login_name.len() > 256
        || !text(&value.login_name)
        || admit_role(value.role).is_err()
        || !status(value.access)
    {
        return Err(AdmissionError::Invalid);
    }
    let (Some(created), Some(updated)) =
        (timestamp(&value.create_time), timestamp(&value.update_time))
    else {
        return Err(AdmissionError::Invalid);
    };
    if (updated.seconds, updated.nanos) < (created.seconds, created.nanos) {
        return Err(AdmissionError::Invalid);
    }
    Ok(Admitted {
        wire: value.clone(),
    })
}

/// Settings list historical metadata too. Expired/revoked rows are not usable
/// credentials, but remain valid rows; their timestamps and ownership are exact.
pub fn admit_session_metadata(
    value: &auth::Session,
) -> Result<Admitted<auth::Session>, AdmissionError> {
    let (Some(created), Some(expires)) =
        (timestamp(&value.create_time), timestamp(&value.expire_time))
    else {
        return Err(AdmissionError::Invalid);
    };
    if !value.__buffa_unknown_fields.is_empty()
        || SessionName::try_from(value.name.as_str()).is_err()
        || (expires.seconds, expires.nanos) <= (created.seconds, created.nanos)
        || !session_authentication(value, expires)
        || value.revoke_time.as_option().is_some_and(|at| {
            !valid_timestamp(at) || (at.seconds, at.nanos) < (created.seconds, created.nanos)
        })
        || value.last_use_time.as_option().is_some_and(|at| {
            !valid_timestamp(at) || (at.seconds, at.nanos) < (created.seconds, created.nanos)
        })
    {
        return Err(AdmissionError::Invalid);
    }
    Ok(Admitted {
        wire: value.clone(),
    })
}

pub fn admit_factor_metadata(
    value: &auth::FactorCredential,
) -> Result<Admitted<auth::FactorCredential>, AdmissionError> {
    let Some(created) = timestamp(&value.create_time) else {
        return Err(AdmissionError::Invalid);
    };
    if !value.__buffa_unknown_fields.is_empty()
        || FactorCredentialName::try_from(value.name.as_str()).is_err()
        || value.kind != auth::FactorKind::Fido2
        || value.last_use_time.as_option().is_some_and(|at| {
            !valid_timestamp(at) || (at.seconds, at.nanos) < (created.seconds, created.nanos)
        })
    {
        return Err(AdmissionError::Invalid);
    }
    Ok(Admitted {
        wire: value.clone(),
    })
}

pub fn admit_invitation_metadata(
    value: &auth::Invitation,
) -> Result<Admitted<auth::Invitation>, AdmissionError> {
    let (Some(created), Some(expires)) =
        (timestamp(&value.create_time), timestamp(&value.expire_time))
    else {
        return Err(AdmissionError::Invalid);
    };
    if !value.__buffa_unknown_fields.is_empty()
        || InvitationName::try_from(value.name.as_str()).is_err()
        || !text(&value.login_name)
        || value.login_name.len() > 256
        || admit_role(value.role).is_err()
        || (expires.seconds, expires.nanos) <= (created.seconds, created.nanos)
        || value.accept_time.as_option().is_some_and(|at| {
            !valid_timestamp(at) || (at.seconds, at.nanos) < (created.seconds, created.nanos)
        })
        || value.revoke_time.as_option().is_some_and(|at| {
            !valid_timestamp(at) || (at.seconds, at.nanos) < (created.seconds, created.nanos)
        })
    {
        return Err(AdmissionError::Invalid);
    }
    Ok(Admitted {
        wire: value.clone(),
    })
}

pub fn admit_mcp_key_metadata(
    value: &auth::McpKey,
) -> Result<Admitted<auth::McpKey>, AdmissionError> {
    let (Some(created), Some(expires)) =
        (timestamp(&value.create_time), timestamp(&value.expire_time))
    else {
        return Err(AdmissionError::Invalid);
    };
    if !value.__buffa_unknown_fields.is_empty()
        || McpKeyName::try_from(value.name.as_str()).is_err()
        || !text(&value.client_name)
        || value.client_name.len() > 256
        || value.audience != auth::CredentialAudience::Mcp
        || (expires.seconds, expires.nanos) <= (created.seconds, created.nanos)
        || (expires.seconds, expires.nanos) > (created.seconds + 365 * 86_400, created.nanos)
        || value.revoke_time.as_option().is_some_and(|at| {
            !valid_timestamp(at) || (at.seconds, at.nanos) < (created.seconds, created.nanos)
        })
        || value.last_use_time.as_option().is_some_and(|at| {
            !valid_timestamp(at) || (at.seconds, at.nanos) < (created.seconds, created.nanos)
        })
        || value.scopes.len() > 39
        || value.scopes.iter().any(|scope| admit_scope(scope).is_err())
    {
        return Err(AdmissionError::Invalid);
    }
    for (index, scope) in value.scopes.iter().enumerate() {
        if value.scopes[..index]
            .iter()
            .any(|prior| prior.family == scope.family && prior.operation == scope.operation)
        {
            return Err(AdmissionError::Invalid);
        }
    }
    Ok(Admitted {
        wire: value.clone(),
    })
}

// Local and server wall clocks need not be identical. Raw contract admission
// remains exact; the interactive client admits a bounded 30-second uncertainty
// and uses its upper bound for every credential expiration decision.
const INTERACTIVE_CLOCK_UNCERTAINTY_SECONDS: i64 = 30;
struct SessionClockBounds {
    earliest: Timestamp,
    latest: Timestamp,
}
impl SessionClockBounds {
    fn exact(now: &Timestamp) -> Result<Self, AdmissionError> {
        if !valid_timestamp(now) {
            return Err(AdmissionError::Invalid);
        }
        Ok(Self {
            earliest: now.clone(),
            latest: now.clone(),
        })
    }
    fn interactive(now: &Timestamp) -> Result<Self, AdmissionError> {
        let mut bounds = Self::exact(now)?;
        bounds.earliest.seconds -= INTERACTIVE_CLOCK_UNCERTAINTY_SECONDS;
        bounds.latest.seconds += INTERACTIVE_CLOCK_UNCERTAINTY_SECONDS;
        if !valid_timestamp(&bounds.earliest) || !valid_timestamp(&bounds.latest) {
            return Err(AdmissionError::Invalid);
        }
        Ok(bounds)
    }
}

/// Conservative time for interactive expiration checks. This can expire a
/// credential early; it never adds leeway after the returned server deadline.
pub fn interactive_expiration_now(now: &Timestamp) -> Result<Timestamp, AdmissionError> {
    Ok(SessionClockBounds::interactive(now)?.latest)
}

fn session(value: &auth::Session, clock: &SessionClockBounds) -> bool {
    if !value.__buffa_unknown_fields.is_empty()
        || SessionName::try_from(value.name.as_str()).is_err()
        || !future(&value.expire_time, &clock.latest)
        || value.revoke_time.is_set()
        || !session_authentication(value, &clock.latest)
    {
        return false;
    }
    let (Some(created), Some(expires)) =
        (timestamp(&value.create_time), timestamp(&value.expire_time))
    else {
        return false;
    };
    (expires.seconds, expires.nanos) > (created.seconds, created.nanos)
        && (created.seconds, created.nanos) <= (clock.latest.seconds, clock.latest.nanos)
        && value.last_use_time.as_option().is_none_or(|at| {
            valid_timestamp(at)
                && (at.seconds, at.nanos) >= (created.seconds, created.nanos)
                && (at.seconds, at.nanos) <= (clock.latest.seconds, clock.latest.nanos)
        })
}
fn session_tokens(value: &auth::SessionTokens, clock: &SessionClockBounds) -> bool {
    // Remaining lifetime only; the issuing service enforces issuance time.
    if !value.__buffa_unknown_fields.is_empty()
        || value.audience != auth::CredentialAudience::Interactive
        || !text(&value.access_token)
        || !text(&value.refresh_token)
        || value.access_token.len() > 4096
        || value.refresh_token.len() > 4096
        || !future(&value.access_expire_time, &clock.earliest)
    {
        return false;
    }
    let (Some(session), Some(access_expires)) = (
        value.session.as_option(),
        timestamp(&value.access_expire_time),
    ) else {
        return false;
    };
    let Some(session_expires) = timestamp(&session.expire_time) else {
        return false;
    };
    self::session(session, clock)
        && (access_expires.seconds, access_expires.nanos)
            <= (session_expires.seconds, session_expires.nanos)
        && (access_expires.seconds, access_expires.nanos)
            <= (clock.latest.seconds + 900, clock.latest.nanos)
}
fn factor_challenge(value: &auth::FactorChallenge, now: &Timestamp) -> bool {
    if !value.__buffa_unknown_fields.is_empty()
        || ChallengeId::parse(&value.challenge_id).is_err()
        || !future(&value.expire_time, now)
        || value.options.is_empty()
    {
        return false;
    }
    let (mut fido, mut recovery) = (false, false);
    for option in &value.options {
        if !option.__buffa_unknown_fields.is_empty() {
            return false;
        }
        match &option.method {
            Some(auth::factor_option::Method::Fido2(v)) => {
                if fido
                    || !v.__buffa_unknown_fields.is_empty()
                    || v.challenge.is_empty()
                    || !text(&v.rp_id)
                    || !text(&v.origin)
                    || InstanceId::parse(&v.instance_id).is_err()
                {
                    return false;
                }
                fido = true;
            }
            Some(auth::factor_option::Method::RecoveryCode(v)) => {
                if recovery || !v.__buffa_unknown_fields.is_empty() {
                    return false;
                }
                recovery = true;
            }
            None => return false,
        }
    }
    true
}

/// Shape and offered-option admission only; no FIDO2 or recovery proof is verified.
pub fn admit_factor_proof_for_challenge(
    proof: &auth::FactorProof,
    challenge: &auth::FactorChallenge,
    now: &Timestamp,
) -> Result<(), AdmissionError> {
    if !proof.__buffa_unknown_fields.is_empty() || !factor_challenge(challenge, now) {
        return Err(AdmissionError::Invalid);
    }
    let valid =
        match &proof.proof {
            Some(auth::factor_proof::Proof::Fido2(value)) => {
                challenge.options.iter().any(|option| {
                    matches!(option.method, Some(auth::factor_option::Method::Fido2(_)))
                }) && value.__buffa_unknown_fields.is_empty()
                    && !value.credential_id.is_empty()
                    && !value.authenticator_data.is_empty()
                    && !value.client_data_json.is_empty()
                    && !value.signature.is_empty()
                    && value.client_data_hash.len() == 32
            }
            Some(auth::factor_proof::Proof::RecoveryCode(value)) => {
                challenge.options.iter().any(|option| {
                    matches!(
                        option.method,
                        Some(auth::factor_option::Method::RecoveryCode(_))
                    )
                }) && text(value)
            }
            None => false,
        };
    if valid {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}

pub fn admit_complete_login_request(
    request: &auth::CompleteLoginRequest,
    challenge: &auth::FactorChallenge,
    now: &Timestamp,
) -> Result<(), AdmissionError> {
    if !request.__buffa_unknown_fields.is_empty() || !factor_challenge(challenge, now) {
        return Err(AdmissionError::Invalid);
    }
    if ChallengeId::parse(&request.challenge_id)? != ChallengeId::parse(&challenge.challenge_id)? {
        return Err(AdmissionError::Invalid);
    }
    let Some(proof) = request.proof.as_option() else {
        return Err(AdmissionError::Invalid);
    };
    admit_factor_proof_for_challenge(proof, challenge, now)
}

pub fn admit_begin_factor_enrollment(
    value: &auth::BeginFactorEnrollmentRequest,
) -> Result<(), AdmissionError> {
    let kind_valid = match value.kind {
        EnumValue::Known(auth::FactorKind::Fido2) => true,
        EnumValue::Known(auth::FactorKind::Unspecified) | EnumValue::Unknown(_) => false,
    };
    if value.__buffa_unknown_fields.is_empty()
        && kind_valid
        && AccountName::try_from(value.parent.as_str()).is_ok()
    {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}

/// Validate a token bundle without making another owned copy of its secrets.
/// Persistence revalidates at its recorded admission time, then separately
/// checks the current absolute/access expirations before exposing a session.
pub fn validate_session_tokens(
    value: &auth::SessionTokens,
    now: &Timestamp,
) -> Result<(), AdmissionError> {
    if session_tokens(value, &SessionClockBounds::exact(now)?) {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}

pub fn validate_login(value: &auth::LoginDecision, now: &Timestamp) -> Result<(), AdmissionError> {
    validate_login_with_clock(value, &SessionClockBounds::exact(now)?)
}

/// Interactive responses cross independent clocks. Shape, audience, identity,
/// authentication ordering and all unknown-field checks remain identical.
pub fn validate_received_login(
    value: &auth::LoginDecision,
    now: &Timestamp,
) -> Result<(), AdmissionError> {
    validate_login_with_clock(value, &SessionClockBounds::interactive(now)?)
}

/// A possibly expired short access credential is admitted only within the
/// uncertainty interval, then immediately refreshed by the owner. The absolute
/// session itself must still be valid at the interval's conservative upper bound.
pub fn validate_received_session_tokens(
    value: &auth::SessionTokens,
    now: &Timestamp,
) -> Result<(), AdmissionError> {
    if session_tokens(value, &SessionClockBounds::interactive(now)?) {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}

fn validate_login_with_clock(
    value: &auth::LoginDecision,
    clock: &SessionClockBounds,
) -> Result<(), AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    let valid = match &value.outcome {
        Some(auth::login_decision::Outcome::Authenticated(v)) => session_tokens(v, clock),
        Some(auth::login_decision::Outcome::Challenge(v)) => factor_challenge(v, &clock.latest),
        Some(auth::login_decision::Outcome::EnrollmentRequired(v)) => {
            v.__buffa_unknown_fields.is_empty()
                && AccountName::try_from(v.account.as_str()).is_ok()
                && text(&v.enrollment_token)
                && future(&v.expire_time, &clock.latest)
        }
        None => false,
    };
    if !valid {
        return Err(AdmissionError::Invalid);
    }
    Ok(())
}

pub fn admit_login(
    value: &auth::LoginDecision,
    now: &Timestamp,
) -> Result<Admitted<auth::LoginDecision>, AdmissionError> {
    validate_login(value, now)?;
    Ok(Admitted {
        wire: value.clone(),
    })
}

pub fn admit_mcp_key(
    value: &auth::McpKey,
    now: &Timestamp,
) -> Result<Admitted<auth::McpKey>, AdmissionError> {
    if !valid_timestamp(now) || value.revoke_time.is_set() || !future(&value.expire_time, now) {
        return Err(AdmissionError::Invalid);
    }
    admit_mcp_key_metadata(value)
}

pub fn admit_mcp_lifetime(value: Option<&Duration>) -> Result<std::time::Duration, AdmissionError> {
    let Some(value) = value else {
        return Ok(std::time::Duration::from_secs(30 * 86_400));
    };
    if !value.__buffa_unknown_fields.is_empty()
        || value.seconds < 0
        || value.seconds > 365 * 86_400
        || !(0..1_000_000_000).contains(&value.nanos)
        || (value.seconds == 0 && value.nanos == 0)
        || (value.seconds == 365 * 86_400 && value.nanos > 0)
    {
        return Err(AdmissionError::Invalid);
    }
    Ok(std::time::Duration::new(
        value.seconds as u64,
        value.nanos as u32,
    ))
}

pub fn admit_mcp_issue_request(value: &auth::IssueMcpKeyRequest) -> Result<(), AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || !text(&value.client_name)
        || value.client_name.len() > 256
        || value.scopes.len() > 39
        || AccountName::try_from(value.parent.as_str()).is_err()
    {
        return Err(AdmissionError::Invalid);
    }
    admit_mcp_lifetime(value.lifetime.as_option())?;
    for (index, scope) in value.scopes.iter().enumerate() {
        admit_scope(scope)?;
        if value.scopes[..index]
            .iter()
            .any(|prior| prior.family == scope.family && prior.operation == scope.operation)
        {
            return Err(AdmissionError::Invalid);
        }
    }
    Ok(())
}

/// Admission of explicitly recorded original login evidence.
pub fn admit_authentication_satisfaction(
    value: EnumValue<auth::AuthenticationSatisfaction>,
) -> Result<auth::AuthenticationSatisfaction, AdmissionError> {
    match value {
        EnumValue::Known(auth::AuthenticationSatisfaction::PasswordOnly) => {
            Ok(auth::AuthenticationSatisfaction::PasswordOnly)
        }
        EnumValue::Known(auth::AuthenticationSatisfaction::PasswordFido2) => {
            Ok(auth::AuthenticationSatisfaction::PasswordFido2)
        }
        EnumValue::Known(auth::AuthenticationSatisfaction::PasswordRecoveryCode) => {
            Ok(auth::AuthenticationSatisfaction::PasswordRecoveryCode)
        }
        EnumValue::Known(auth::AuthenticationSatisfaction::Unspecified)
        | EnumValue::Known(auth::AuthenticationSatisfaction::NotRecorded)
        | EnumValue::Unknown(_) => Err(AdmissionError::Invalid),
    }
}
fn session_authentication(value: &auth::Session, now: &Timestamp) -> bool {
    match value.authentication_satisfaction {
        EnumValue::Known(auth::AuthenticationSatisfaction::NotRecorded) => {
            !value.authentication_time.is_set()
        }
        EnumValue::Known(auth::AuthenticationSatisfaction::PasswordOnly)
        | EnumValue::Known(auth::AuthenticationSatisfaction::PasswordFido2)
        | EnumValue::Known(auth::AuthenticationSatisfaction::PasswordRecoveryCode) => {
            match (
                timestamp(&value.authentication_time),
                timestamp(&value.create_time),
            ) {
                (Some(authenticated), Some(created)) => {
                    (authenticated.seconds, authenticated.nanos) == (created.seconds, created.nanos)
                        && (authenticated.seconds, authenticated.nanos) <= (now.seconds, now.nanos)
                }
                _ => false,
            }
        }
        EnumValue::Known(auth::AuthenticationSatisfaction::Unspecified) | EnumValue::Unknown(_) => {
            false
        }
    }
}
fn second_factor_policy(value: &auth::SecondFactorPolicy) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && SecondFactorPolicyName::try_from(value.name.as_str()).is_ok()
        && !value.etag.is_empty()
        && value.etag.len() <= 128
        && timestamp(&value.update_time).is_some()
}
pub fn admit_change_second_factor_policy(
    value: &auth::ChangeSecondFactorPolicyRequest,
) -> Result<Admitted<auth::ChangeSecondFactorPolicyRequest>, AdmissionError> {
    if value.__buffa_unknown_fields.is_empty()
        && SecondFactorPolicyName::try_from(value.name.as_str()).is_ok()
        && !value.etag.is_empty()
        && value.etag.len() <= 128
    {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_second_factor_policy(
    value: &auth::SecondFactorPolicy,
) -> Result<Admitted<auth::SecondFactorPolicy>, AdmissionError> {
    if second_factor_policy(value) {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_change_second_factor_policy_response(
    value: &auth::ChangeSecondFactorPolicyResponse,
) -> Result<Admitted<auth::ChangeSecondFactorPolicyResponse>, AdmissionError> {
    let continuation = match value.continuation {
        EnumValue::Known(auth::SecondFactorSessionContinuation::Continues)
        | EnumValue::Known(auth::SecondFactorSessionContinuation::ReauthenticationRequired) => true,
        EnumValue::Known(auth::SecondFactorSessionContinuation::Unspecified)
        | EnumValue::Unknown(_) => false,
    };
    if continuation
        && value.__buffa_unknown_fields.is_empty()
        && value.policy.as_option().is_some_and(second_factor_policy)
    {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
