//! Pure admission of auth wire values. No password, FIDO2, token, or rights verification.

use crate::home::auth::v1 as auth;
use buffa::EnumValue;
use buffa_types::google::protobuf::{Duration, Timestamp};

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
                if value.is_empty() {
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
identity!(AccountId, auth::AccountId);
identity!(SessionId, auth::SessionId);
identity!(FactorCredentialId, auth::FactorCredentialId);
identity!(InvitationId, auth::InvitationId);
identity!(ChallengeId, auth::ChallengeId);
identity!(McpKeyId, auth::McpKeyId);
identity!(InstanceId, auth::InstanceId);

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

fn account_id(
    value: &buffa::MessageField<auth::AccountId, buffa::Inline<auth::AccountId>>,
) -> bool {
    value
        .as_option()
        .is_some_and(|value| AccountId::try_from(value).is_ok())
}
fn session_id(
    value: &buffa::MessageField<auth::SessionId, buffa::Inline<auth::SessionId>>,
) -> bool {
    value
        .as_option()
        .is_some_and(|value| SessionId::try_from(value).is_ok())
}
fn challenge_id(
    value: &buffa::MessageField<auth::ChallengeId, buffa::Inline<auth::ChallengeId>>,
) -> bool {
    value
        .as_option()
        .is_some_and(|value| ChallengeId::try_from(value).is_ok())
}
fn mcp_key_id(value: &buffa::MessageField<auth::McpKeyId, buffa::Inline<auth::McpKeyId>>) -> bool {
    value
        .as_option()
        .is_some_and(|value| McpKeyId::try_from(value).is_ok())
}
fn instance_id(
    value: &buffa::MessageField<auth::InstanceId, buffa::Inline<auth::InstanceId>>,
) -> bool {
    value
        .as_option()
        .is_some_and(|value| InstanceId::try_from(value).is_ok())
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
fn status(value: EnumValue<auth::AccountStatus>) -> bool {
    match value {
        EnumValue::Known(auth::AccountStatus::Active)
        | EnumValue::Known(auth::AccountStatus::Disabled)
        | EnumValue::Known(auth::AccountStatus::PendingInvitation) => true,
        EnumValue::Known(auth::AccountStatus::Unspecified) | EnumValue::Unknown(_) => false,
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
        || !account_id(&value.id)
        || !text(&value.login_name)
        || admit_role(value.role).is_err()
        || !status(value.status)
    {
        return Err(AdmissionError::Invalid);
    }
    let (Some(created), Some(updated)) =
        (timestamp(&value.created_at), timestamp(&value.updated_at))
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
        (timestamp(&value.created_at), timestamp(&value.expires_at))
    else {
        return Err(AdmissionError::Invalid);
    };
    if !value.__buffa_unknown_fields.is_empty()
        || !session_id(&value.id)
        || !account_id(&value.account_id)
        || (expires.seconds, expires.nanos) <= (created.seconds, created.nanos)
        || !session_authentication(value, expires)
        || value.revoked_at.as_option().is_some_and(|at| {
            !valid_timestamp(at) || (at.seconds, at.nanos) < (created.seconds, created.nanos)
        })
        || value.last_used_at.as_option().is_some_and(|at| {
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
    let Some(created) = timestamp(&value.created_at) else {
        return Err(AdmissionError::Invalid);
    };
    if !value.__buffa_unknown_fields.is_empty()
        || !account_id(&value.account_id)
        || FactorCredentialId::try_from(&*value.id).is_err()
        || !value.id.is_set()
        || value.kind != auth::FactorKind::Fido2
        || value.last_used_at.as_option().is_some_and(|at| {
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
        (timestamp(&value.created_at), timestamp(&value.expires_at))
    else {
        return Err(AdmissionError::Invalid);
    };
    if !value.__buffa_unknown_fields.is_empty()
        || !value.id.is_set()
        || InvitationId::try_from(&*value.id).is_err()
        || !text(&value.login_name)
        || value.login_name.len() > 256
        || admit_role(value.role).is_err()
        || (expires.seconds, expires.nanos) <= (created.seconds, created.nanos)
        || value.accepted_at.as_option().is_some_and(|at| {
            !valid_timestamp(at) || (at.seconds, at.nanos) < (created.seconds, created.nanos)
        })
        || value.revoked_at.as_option().is_some_and(|at| {
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
        (timestamp(&value.created_at), timestamp(&value.expires_at))
    else {
        return Err(AdmissionError::Invalid);
    };
    if !value.__buffa_unknown_fields.is_empty()
        || !mcp_key_id(&value.id)
        || !account_id(&value.account_id)
        || !text(&value.client_name)
        || value.client_name.len() > 256
        || value.audience != auth::CredentialAudience::Mcp
        || (expires.seconds, expires.nanos) <= (created.seconds, created.nanos)
        || (expires.seconds, expires.nanos) > (created.seconds + 365 * 86_400, created.nanos)
        || value.revoked_at.as_option().is_some_and(|at| {
            !valid_timestamp(at) || (at.seconds, at.nanos) < (created.seconds, created.nanos)
        })
        || value.last_used_at.as_option().is_some_and(|at| {
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
        || !session_id(&value.id)
        || !account_id(&value.account_id)
        || !future(&value.expires_at, &clock.latest)
        || value.revoked_at.is_set()
        || !session_authentication(value, &clock.latest)
    {
        return false;
    }
    let (Some(created), Some(expires)) =
        (timestamp(&value.created_at), timestamp(&value.expires_at))
    else {
        return false;
    };
    (expires.seconds, expires.nanos) > (created.seconds, created.nanos)
        && (value.last_used_at.as_option().is_none() || timestamp(&value.last_used_at).is_some())
}
fn session_tokens(value: &auth::SessionTokens, clock: &SessionClockBounds) -> bool {
    // Remaining lifetime only; the issuing service enforces issuance time.
    if !value.__buffa_unknown_fields.is_empty()
        || value.audience != auth::CredentialAudience::Interactive
        || !text(&value.access_token)
        || !text(&value.refresh_token)
        || !future(&value.access_expires_at, &clock.earliest)
    {
        return false;
    }
    let (Some(session), Some(access_expires)) = (
        value.session.as_option(),
        timestamp(&value.access_expires_at),
    ) else {
        return false;
    };
    let Some(session_expires) = timestamp(&session.expires_at) else {
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
        || !challenge_id(&value.id)
        || !future(&value.expires_at, now)
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
                    || !instance_id(&v.instance_id)
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
    let (Some(request_id), Some(challenge_id)) =
        (request.challenge_id.as_option(), challenge.id.as_option())
    else {
        return Err(AdmissionError::Invalid);
    };
    if ChallengeId::try_from(request_id)? != ChallengeId::try_from(challenge_id)? {
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
    if value.__buffa_unknown_fields.is_empty() && kind_valid {
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
                && account_id(&v.account_id)
                && text(&v.enrollment_token)
                && future(&v.expires_at, &clock.latest)
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
    if !valid_timestamp(now)
        || !value.__buffa_unknown_fields.is_empty()
        || !mcp_key_id(&value.id)
        || !account_id(&value.account_id)
        || !text(&value.client_name)
        || value.audience != auth::CredentialAudience::Mcp
        || value.revoked_at.is_set()
        || !future(&value.expires_at, now)
        || value.scopes.iter().any(|scope| admit_scope(scope).is_err())
    {
        return Err(AdmissionError::Invalid);
    }
    let (Some(created), Some(expires)) =
        (timestamp(&value.created_at), timestamp(&value.expires_at))
    else {
        return Err(AdmissionError::Invalid);
    };
    if (expires.seconds, expires.nanos) <= (created.seconds, created.nanos)
        || (expires.seconds, expires.nanos) > (created.seconds + 365 * 86_400, created.nanos)
        || (value.last_used_at.as_option().is_some() && timestamp(&value.last_used_at).is_none())
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

pub fn admit_mcp_create_request(value: &auth::CreateMcpKeyRequest) -> Result<(), AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() || !text(&value.client_name) {
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

fn field(value: EnumValue<auth::AuthField>) -> bool {
    match value {
        EnumValue::Known(auth::AuthField::LoginName)
        | EnumValue::Known(auth::AuthField::Password)
        | EnumValue::Known(auth::AuthField::AccountId)
        | EnumValue::Known(auth::AuthField::Role)
        | EnumValue::Known(auth::AuthField::Status)
        | EnumValue::Known(auth::AuthField::SessionId)
        | EnumValue::Known(auth::AuthField::ChallengeId)
        | EnumValue::Known(auth::AuthField::FactorProof)
        | EnumValue::Known(auth::AuthField::FactorKind)
        | EnumValue::Known(auth::AuthField::FactorId)
        | EnumValue::Known(auth::AuthField::InvitationToken)
        | EnumValue::Known(auth::AuthField::ExpiresAt)
        | EnumValue::Known(auth::AuthField::ClientName)
        | EnumValue::Known(auth::AuthField::Scopes)
        | EnumValue::Known(auth::AuthField::Lifetime)
        | EnumValue::Known(auth::AuthField::McpKeyId) => true,
        EnumValue::Known(auth::AuthField::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
pub fn admit_error(value: &auth::AuthError) -> Result<Admitted<auth::AuthError>, AdmissionError> {
    use auth::auth_error::Detail;
    use auth::{
        AuthErrorCode as Code, AuthenticationFailure as Authentication,
        ConflictFailure as Conflict, ExpiredResource as Expired, PermissionFailure as Permission,
    };
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    let detail = match &value.detail {
        Some(Detail::Authentication(reason)) => {
            let accepted = match reason {
                EnumValue::Known(Authentication::InvalidCredential)
                | EnumValue::Known(Authentication::Revoked)
                | EnumValue::Known(Authentication::FactorRequired) => true,
                EnumValue::Known(Authentication::Unspecified) | EnumValue::Unknown(_) => false,
            };
            Some((Code::Unauthenticated, accepted))
        }
        Some(Detail::Permission(reason)) => {
            let accepted = match reason {
                EnumValue::Known(Permission::LocalOperatorRequired)
                | EnumValue::Known(Permission::ScopeDenied)
                | EnumValue::Known(Permission::AccountDisabled) => true,
                EnumValue::Known(Permission::Unspecified) | EnumValue::Unknown(_) => false,
            };
            Some((Code::PermissionDenied, accepted))
        }
        Some(Detail::Conflict(reason)) => {
            let accepted = match reason {
                EnumValue::Known(Conflict::AlreadyExists)
                | EnumValue::Known(Conflict::BootstrapComplete)
                | EnumValue::Known(Conflict::AlreadyUsed)
                | EnumValue::Known(Conflict::LastAdmin) => true,
                EnumValue::Known(Conflict::Unspecified) | EnumValue::Unknown(_) => false,
            };
            Some((Code::Conflict, accepted))
        }
        Some(Detail::Expired(reason)) => {
            let accepted = match reason {
                EnumValue::Known(Expired::Invitation)
                | EnumValue::Known(Expired::Challenge)
                | EnumValue::Known(Expired::Session)
                | EnumValue::Known(Expired::AccessToken)
                | EnumValue::Known(Expired::McpKey) => true,
                EnumValue::Known(Expired::Unspecified) | EnumValue::Unknown(_) => false,
            };
            Some((Code::Expired, accepted))
        }
        Some(Detail::InvalidField(value)) => Some((Code::InvalidArgument, field(*value))),
        Some(Detail::RetryAt(value)) => Some((Code::RateLimited, valid_timestamp(value))),
        None => None,
    };
    let valid = match value.code {
        EnumValue::Known(Code::Unauthenticated) => detail == Some((Code::Unauthenticated, true)),
        EnumValue::Known(Code::PermissionDenied) => detail == Some((Code::PermissionDenied, true)),
        EnumValue::Known(Code::Conflict) => detail == Some((Code::Conflict, true)),
        EnumValue::Known(Code::Expired) => detail == Some((Code::Expired, true)),
        EnumValue::Known(Code::InvalidArgument) => detail == Some((Code::InvalidArgument, true)),
        EnumValue::Known(Code::RateLimited) => detail == Some((Code::RateLimited, true)),
        EnumValue::Known(Code::Unspecified) | EnumValue::Unknown(_) => false,
    };
    if valid {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}

/// Newly issued methods only; legacy metadata is handled explicitly below.
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
        EnumValue::Known(auth::AuthenticationSatisfaction::Unspecified) | EnumValue::Unknown(_) => {
            Err(AdmissionError::Invalid)
        }
    }
}
fn session_authentication(value: &auth::Session, now: &Timestamp) -> bool {
    match value.authentication_satisfaction {
        EnumValue::Known(auth::AuthenticationSatisfaction::Unspecified) => {
            !value.authenticated_at.is_set()
        }
        EnumValue::Known(auth::AuthenticationSatisfaction::PasswordOnly)
        | EnumValue::Known(auth::AuthenticationSatisfaction::PasswordFido2)
        | EnumValue::Known(auth::AuthenticationSatisfaction::PasswordRecoveryCode) => {
            match (
                timestamp(&value.authenticated_at),
                timestamp(&value.created_at),
            ) {
                (Some(authenticated), Some(created)) => {
                    (authenticated.seconds, authenticated.nanos) == (created.seconds, created.nanos)
                        && (authenticated.seconds, authenticated.nanos) <= (now.seconds, now.nanos)
                }
                _ => false,
            }
        }
        EnumValue::Unknown(_) => false,
    }
}
fn mfa_policy_version(value: &auth::PolicyVersion) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value.revision > 0
        && value.revision <= i64::MAX as u64
        && value.source_sha256.len() == 32
}
pub fn admit_set_second_factor_policy(
    value: &auth::SetSecondFactorPolicyRequest,
) -> Result<Admitted<auth::SetSecondFactorPolicyRequest>, AdmissionError> {
    if value.__buffa_unknown_fields.is_empty()
        && value.expected.as_option().is_some_and(mfa_policy_version)
    {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
fn second_factor_policy(value: &auth::SecondFactorPolicy) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value.version.as_option().is_some_and(mfa_policy_version)
        && timestamp(&value.updated_at).is_some()
}
pub fn admit_get_second_factor_policy_response(
    value: &auth::GetSecondFactorPolicyResponse,
) -> Result<Admitted<auth::GetSecondFactorPolicyResponse>, AdmissionError> {
    if value.__buffa_unknown_fields.is_empty()
        && value.policy.as_option().is_some_and(second_factor_policy)
    {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_set_second_factor_policy_response(
    value: &auth::SetSecondFactorPolicyResponse,
) -> Result<Admitted<auth::SetSecondFactorPolicyResponse>, AdmissionError> {
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
pub fn admit_second_factor_policy_error(
    value: &auth::SecondFactorPolicyError,
) -> Result<Admitted<auth::SecondFactorPolicyError>, AdmissionError> {
    let valid = match value.code {
        EnumValue::Known(auth::SecondFactorPolicyErrorCode::InvalidVersion)
        | EnumValue::Known(auth::SecondFactorPolicyErrorCode::VersionConflict) => {
            value.field == "expected"
        }
        EnumValue::Known(auth::SecondFactorPolicyErrorCode::AuthorityUnavailable) => {
            value.field.is_empty()
        }
        EnumValue::Known(auth::SecondFactorPolicyErrorCode::Unspecified)
        | EnumValue::Unknown(_) => false,
    };
    if valid && value.__buffa_unknown_fields.is_empty() {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_reset_second_factor(
    value: &auth::ResetSecondFactorRequest,
) -> Result<(), AdmissionError> {
    if value.__buffa_unknown_fields.is_empty()
        && value
            .account_id
            .as_option()
            .is_some_and(|id| AccountId::try_from(id).is_ok() && id.value.len() <= 128)
    {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}
