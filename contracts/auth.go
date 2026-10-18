package contracts

import (
	"errors"
	"reflect"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These opaque IDs only establish a nonempty UTF-8 representation. Ownership,
// lookup, authentication and authorization belong to later services. Their
// private fields and checked Value methods keep Go zero values invalid.
var ErrInvalidAuthID = errors.New("invalid auth identity")

type AccountID struct{ account string }
type SessionID struct{ session string }
type FactorCredentialID struct{ factor string }
type InvitationID struct{ invitation string }
type ChallengeID struct{ challenge string }
type McpKeyID struct{ mcpKey string }
type InstanceID struct{ instance string }

func parseAuthID(value string) (string, error) {
	if value == "" || !utf8.ValidString(value) {
		return "", ErrInvalidAuthID
	}
	return value, nil
}
func ParseAccountID(value string) (AccountID, error) {
	v, err := parseAuthID(value)
	return AccountID{v}, err
}
func ParseSessionID(value string) (SessionID, error) {
	v, err := parseAuthID(value)
	return SessionID{v}, err
}
func ParseFactorCredentialID(value string) (FactorCredentialID, error) {
	v, err := parseAuthID(value)
	return FactorCredentialID{v}, err
}
func ParseInvitationID(value string) (InvitationID, error) {
	v, err := parseAuthID(value)
	return InvitationID{v}, err
}
func ParseChallengeID(value string) (ChallengeID, error) {
	v, err := parseAuthID(value)
	return ChallengeID{v}, err
}
func ParseMcpKeyID(value string) (McpKeyID, error) {
	v, err := parseAuthID(value)
	return McpKeyID{v}, err
}
func ParseInstanceID(value string) (InstanceID, error) {
	v, err := parseAuthID(value)
	return InstanceID{v}, err
}
func (id AccountID) Value() (string, error)          { return parseAuthID(id.account) }
func (id SessionID) Value() (string, error)          { return parseAuthID(id.session) }
func (id FactorCredentialID) Value() (string, error) { return parseAuthID(id.factor) }
func (id InvitationID) Value() (string, error)       { return parseAuthID(id.invitation) }
func (id ChallengeID) Value() (string, error)        { return parseAuthID(id.challenge) }
func (id McpKeyID) Value() (string, error)           { return parseAuthID(id.mcpKey) }
func (id InstanceID) Value() (string, error)         { return parseAuthID(id.instance) }

var ErrInvalidAuthContract = errors.New("invalid auth contract")

func validAccountID(id *auth.AccountId) bool {
	if id == nil {
		return false
	}
	_, err := ParseAccountID(id.Value)
	return err == nil
}
func validSessionID(id *auth.SessionId) bool {
	if id == nil {
		return false
	}
	_, err := ParseSessionID(id.Value)
	return err == nil
}
func validChallengeID(id *auth.ChallengeId) bool {
	if id == nil {
		return false
	}
	_, err := ParseChallengeID(id.Value)
	return err == nil
}
func validMcpKeyID(id *auth.McpKeyId) bool {
	if id == nil {
		return false
	}
	_, err := ParseMcpKeyID(id.Value)
	return err == nil
}
func validInstanceID(id *auth.InstanceId) bool {
	if id == nil {
		return false
	}
	_, err := ParseInstanceID(id.Value)
	return err == nil
}
func validTimestamp(ts *timestamppb.Timestamp) bool {
	return ts != nil && ts.CheckValid() == nil
}
func validFuture(ts *timestamppb.Timestamp, now time.Time) bool {
	return validTimestamp(ts) && ts.AsTime().After(now)
}
func validText(value string) bool { return value != "" && utf8.ValidString(value) }

// Role and scope sets come solely from the generated proto enums. These
// switches deliberately list every admitted value; descriptor-driven tests
// fail if a newly generated value has not been considered here.
func AdmitAccountRole(role auth.AccountRole) error {
	switch role {
	case auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.AccountRole_ACCOUNT_ROLE_USER:
		return nil
	case auth.AccountRole_ACCOUNT_ROLE_UNSPECIFIED:
		return ErrInvalidAuthContract
	default:
		return ErrInvalidAuthContract
	}
}
func admitAccountStatus(status auth.AccountStatus) bool {
	switch status {
	case auth.AccountStatus_ACCOUNT_STATUS_ACTIVE, auth.AccountStatus_ACCOUNT_STATUS_DISABLED, auth.AccountStatus_ACCOUNT_STATUS_PENDING_INVITATION:
		return true
	case auth.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func admitScopeFamily(family auth.ScopeFamily) bool {
	switch family {
	case auth.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, auth.ScopeFamily_SCOPE_FAMILY_HISTORY,
		auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeFamily_SCOPE_FAMILY_LOCATION,
		auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeFamily_SCOPE_FAMILY_DASHBOARDS,
		auth.ScopeFamily_SCOPE_FAMILY_CALENDARS, auth.ScopeFamily_SCOPE_FAMILY_LISTS,
		auth.ScopeFamily_SCOPE_FAMILY_LOGIC, auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS,
		auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeFamily_SCOPE_FAMILY_ACCOUNTS,
		auth.ScopeFamily_SCOPE_FAMILY_BACKUPS:
		return true
	case auth.ScopeFamily_SCOPE_FAMILY_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func admitScopeOperation(operation auth.ScopeOperation) bool {
	switch operation {
	case auth.ScopeOperation_SCOPE_OPERATION_READ, auth.ScopeOperation_SCOPE_OPERATION_WRITE,
		auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER:
		return true
	case auth.ScopeOperation_SCOPE_OPERATION_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func AdmitScope(scope *auth.Scope) error {
	if scope == nil || !knownFieldsOnly(scope.ProtoReflect()) || !admitScopeFamily(scope.Family) || !admitScopeOperation(scope.Operation) {
		return ErrInvalidAuthContract
	}
	return nil
}

// AdmittedAuthWire owns a deep snapshot. Snapshot returns a separate copy, so
// later mutation of the decoded wire message cannot change the admitted value.
type AdmittedAuthWire[T proto.Message] struct{ value T }

func (a AdmittedAuthWire[T]) Valid() bool {
	v := reflect.ValueOf(a.value)
	return v.IsValid() && v.Kind() == reflect.Pointer && !v.IsNil()
}

func (a AdmittedAuthWire[T]) Snapshot() (T, error) {
	if !a.Valid() {
		var zero T
		return zero, ErrInvalidAuthContract
	}
	return proto.Clone(a.value).(T), nil
}

func AdmitAccount(account *auth.Account) (AdmittedAuthWire[*auth.Account], error) {
	if account == nil || !knownFieldsOnly(account.ProtoReflect()) || !validAccountID(account.Id) ||
		!validText(account.LoginName) || AdmitAccountRole(account.Role) != nil || !admitAccountStatus(account.Status) ||
		!validTimestamp(account.CreatedAt) || !validTimestamp(account.UpdatedAt) ||
		account.UpdatedAt.AsTime().Before(account.CreatedAt.AsTime()) {
		return AdmittedAuthWire[*auth.Account]{}, ErrInvalidAuthContract
	}
	return AdmittedAuthWire[*auth.Account]{value: proto.Clone(account).(*auth.Account)}, nil
}

func validSession(session *auth.Session, now time.Time) bool {
	return session != nil && validSessionID(session.Id) && validAccountID(session.AccountId) &&
		validTimestamp(session.CreatedAt) && validFuture(session.ExpiresAt, now) &&
		session.ExpiresAt.AsTime().After(session.CreatedAt.AsTime()) &&
		(session.RevokedAt == nil || validTimestamp(session.RevokedAt)) &&
		(session.LastUsedAt == nil || validTimestamp(session.LastUsedAt)) && session.RevokedAt == nil
}
func validSessionTokens(tokens *auth.SessionTokens, now time.Time) bool {
	// This bounds remaining lifetime at admission, not issuance duration. The
	// issuing service must enforce its own clock and 15-minute token policy.
	return tokens != nil && validSession(tokens.Session, now) &&
		tokens.Audience == auth.CredentialAudience_CREDENTIAL_AUDIENCE_INTERACTIVE &&
		validText(tokens.AccessToken) && validText(tokens.RefreshToken) && validFuture(tokens.AccessExpiresAt, now) &&
		!tokens.AccessExpiresAt.AsTime().After(now.Add(15*time.Minute)) &&
		!tokens.AccessExpiresAt.AsTime().After(tokens.Session.ExpiresAt.AsTime())
}
func validFactorChallenge(challenge *auth.FactorChallenge, now time.Time) bool {
	if challenge == nil || !knownFieldsOnly(challenge.ProtoReflect()) || !validChallengeID(challenge.Id) || !validFuture(challenge.ExpiresAt, now) || len(challenge.Options) == 0 {
		return false
	}
	seenFido, seenRecovery := false, false
	for _, option := range challenge.Options {
		if option == nil {
			return false
		}
		switch method := option.Method.(type) {
		case *auth.FactorOption_Fido2:
			if seenFido || method == nil || method.Fido2 == nil {
				return false
			}
			f := method.Fido2
			if len(f.Challenge) == 0 || !validText(f.RpId) || !validText(f.Origin) || !validInstanceID(f.InstanceId) {
				return false
			}
			seenFido = true
		case *auth.FactorOption_RecoveryCode:
			if seenRecovery || method == nil || method.RecoveryCode == nil {
				return false
			}
			seenRecovery = true
		default:
			return false
		}
	}
	return true
}

// AdmitFactorProofForChallenge only checks the offered wire shape. It never
// verifies a recovery code, FIDO signature, challenge binding, RP ID or UV.
func AdmitFactorProofForChallenge(proof *auth.FactorProof, challenge *auth.FactorChallenge, now time.Time) error {
	if proof == nil || !knownFieldsOnly(proof.ProtoReflect()) || !validFactorChallenge(challenge, now) {
		return ErrInvalidAuthContract
	}
	for _, option := range challenge.Options {
		switch chosen := proof.Proof.(type) {
		case *auth.FactorProof_Fido2:
			if _, offered := option.Method.(*auth.FactorOption_Fido2); offered && chosen != nil && chosen.Fido2 != nil {
				f := chosen.Fido2
				if len(f.CredentialId) != 0 && len(f.AuthenticatorData) != 0 && len(f.ClientDataJson) != 0 && len(f.Signature) != 0 && len(f.ClientDataHash) == 32 {
					return nil
				}
			}
		case *auth.FactorProof_RecoveryCode:
			if _, offered := option.Method.(*auth.FactorOption_RecoveryCode); offered && chosen != nil && validText(chosen.RecoveryCode) {
				return nil
			}
		default:
			return ErrInvalidAuthContract
		}
	}
	return ErrInvalidAuthContract
}

func AdmitCompleteLoginRequest(request *auth.CompleteLoginRequest, challenge *auth.FactorChallenge, now time.Time) error {
	if request == nil || !knownFieldsOnly(request.ProtoReflect()) || !validFactorChallenge(challenge, now) ||
		!validChallengeID(request.ChallengeId) || request.ChallengeId.Value != challenge.Id.Value {
		return ErrInvalidAuthContract
	}
	return AdmitFactorProofForChallenge(request.Proof, challenge, now)
}

func AdmitBeginFactorEnrollment(request *auth.BeginFactorEnrollmentRequest) error {
	if request == nil || !knownFieldsOnly(request.ProtoReflect()) || request.Kind != auth.FactorKind_FACTOR_KIND_FIDO2 || !utf8.ValidString(request.EnrollmentToken) {
		return ErrInvalidAuthContract
	}
	return nil
}

func AdmitLoginDecision(decision *auth.LoginDecision, now time.Time) (AdmittedAuthWire[*auth.LoginDecision], error) {
	if decision == nil || !knownFieldsOnly(decision.ProtoReflect()) {
		return AdmittedAuthWire[*auth.LoginDecision]{}, ErrInvalidAuthContract
	}
	valid := false
	switch outcome := decision.Outcome.(type) {
	case *auth.LoginDecision_Authenticated:
		valid = outcome != nil && validSessionTokens(outcome.Authenticated, now)
	case *auth.LoginDecision_Challenge:
		valid = outcome != nil && validFactorChallenge(outcome.Challenge, now)
	case *auth.LoginDecision_EnrollmentRequired:
		valid = outcome != nil && outcome.EnrollmentRequired != nil &&
			validAccountID(outcome.EnrollmentRequired.AccountId) &&
			validText(outcome.EnrollmentRequired.EnrollmentToken) &&
			validFuture(outcome.EnrollmentRequired.ExpiresAt, now)
	default:
		// Includes absent and unknown future oneof branches.
		valid = false
	}
	if !valid {
		return AdmittedAuthWire[*auth.LoginDecision]{}, ErrInvalidAuthContract
	}
	return AdmittedAuthWire[*auth.LoginDecision]{value: proto.Clone(decision).(*auth.LoginDecision)}, nil
}

func AdmitMcpKey(key *auth.McpKey, now time.Time) (AdmittedAuthWire[*auth.McpKey], error) {
	if key == nil || !knownFieldsOnly(key.ProtoReflect()) || !validMcpKeyID(key.Id) || !validAccountID(key.AccountId) ||
		!validText(key.ClientName) || key.Audience != auth.CredentialAudience_CREDENTIAL_AUDIENCE_MCP ||
		!validTimestamp(key.CreatedAt) || !validFuture(key.ExpiresAt, now) ||
		!key.ExpiresAt.AsTime().After(key.CreatedAt.AsTime()) || key.RevokedAt != nil ||
		key.ExpiresAt.AsTime().Sub(key.CreatedAt.AsTime()) > 365*24*time.Hour ||
		(key.LastUsedAt != nil && !validTimestamp(key.LastUsedAt)) {
		return AdmittedAuthWire[*auth.McpKey]{}, ErrInvalidAuthContract
	}
	seen := make(map[[2]int32]struct{}, len(key.Scopes))
	for _, scope := range key.Scopes {
		if AdmitScope(scope) != nil {
			return AdmittedAuthWire[*auth.McpKey]{}, ErrInvalidAuthContract
		}
		pair := [2]int32{int32(scope.Family), int32(scope.Operation)}
		if _, duplicate := seen[pair]; duplicate {
			return AdmittedAuthWire[*auth.McpKey]{}, ErrInvalidAuthContract
		}
		seen[pair] = struct{}{}
	}
	return AdmittedAuthWire[*auth.McpKey]{value: proto.Clone(key).(*auth.McpKey)}, nil
}

// Absence selects the 30-day default. A present zero is invalid, including a
// non-nil empty Duration message; the ceiling is 365 days.
func AdmitMcpKeyLifetime(lifetime *durationpb.Duration) (time.Duration, error) {
	if lifetime == nil {
		return 30 * 24 * time.Hour, nil
	}
	if !knownFieldsOnly(lifetime.ProtoReflect()) || lifetime.CheckValid() != nil {
		return 0, ErrInvalidAuthContract
	}
	d := lifetime.AsDuration()
	if d <= 0 || d > 365*24*time.Hour {
		return 0, ErrInvalidAuthContract
	}
	return d, nil
}

func AdmitMcpKeyCreateRequest(request *auth.CreateMcpKeyRequest) error {
	if request == nil || !knownFieldsOnly(request.ProtoReflect()) || !validText(request.ClientName) {
		return ErrInvalidAuthContract
	}
	if _, err := AdmitMcpKeyLifetime(request.Lifetime); err != nil {
		return err
	}
	seen := make(map[[2]int32]struct{}, len(request.Scopes))
	for _, scope := range request.Scopes {
		if AdmitScope(scope) != nil {
			return ErrInvalidAuthContract
		}
		pair := [2]int32{int32(scope.Family), int32(scope.Operation)}
		if _, duplicate := seen[pair]; duplicate {
			return ErrInvalidAuthContract
		}
		seen[pair] = struct{}{}
	}
	return nil
}

func AdmitAuthError(detail *auth.AuthError) (AdmittedAuthWire[*auth.AuthError], error) {
	if detail == nil || !knownFieldsOnly(detail.ProtoReflect()) {
		return AdmittedAuthWire[*auth.AuthError]{}, ErrInvalidAuthContract
	}
	valid := false
	switch detail.Code {
	case auth.AuthErrorCode_AUTH_ERROR_CODE_UNAUTHENTICATED:
		reason, ok := detail.Detail.(*auth.AuthError_Authentication)
		valid = ok && reason != nil && (reason.Authentication == auth.AuthenticationFailure_AUTHENTICATION_FAILURE_INVALID_CREDENTIAL || reason.Authentication == auth.AuthenticationFailure_AUTHENTICATION_FAILURE_REVOKED || reason.Authentication == auth.AuthenticationFailure_AUTHENTICATION_FAILURE_FACTOR_REQUIRED)
	case auth.AuthErrorCode_AUTH_ERROR_CODE_PERMISSION_DENIED:
		reason, ok := detail.Detail.(*auth.AuthError_Permission)
		valid = ok && reason != nil && (reason.Permission == auth.PermissionFailure_PERMISSION_FAILURE_LOCAL_OPERATOR_REQUIRED || reason.Permission == auth.PermissionFailure_PERMISSION_FAILURE_SCOPE_DENIED || reason.Permission == auth.PermissionFailure_PERMISSION_FAILURE_ACCOUNT_DISABLED)
	case auth.AuthErrorCode_AUTH_ERROR_CODE_CONFLICT:
		reason, ok := detail.Detail.(*auth.AuthError_Conflict)
		valid = ok && reason != nil && (reason.Conflict == auth.ConflictFailure_CONFLICT_FAILURE_ALREADY_EXISTS || reason.Conflict == auth.ConflictFailure_CONFLICT_FAILURE_BOOTSTRAP_COMPLETE || reason.Conflict == auth.ConflictFailure_CONFLICT_FAILURE_ALREADY_USED || reason.Conflict == auth.ConflictFailure_CONFLICT_FAILURE_LAST_ADMIN)
	case auth.AuthErrorCode_AUTH_ERROR_CODE_EXPIRED:
		reason, ok := detail.Detail.(*auth.AuthError_Expired)
		valid = ok && reason != nil && (reason.Expired == auth.ExpiredResource_EXPIRED_RESOURCE_INVITATION || reason.Expired == auth.ExpiredResource_EXPIRED_RESOURCE_CHALLENGE || reason.Expired == auth.ExpiredResource_EXPIRED_RESOURCE_SESSION || reason.Expired == auth.ExpiredResource_EXPIRED_RESOURCE_ACCESS_TOKEN || reason.Expired == auth.ExpiredResource_EXPIRED_RESOURCE_MCP_KEY)
	case auth.AuthErrorCode_AUTH_ERROR_CODE_INVALID_ARGUMENT:
		field, ok := detail.Detail.(*auth.AuthError_InvalidField)
		valid = ok && field != nil && knownAuthField(field.InvalidField)
	case auth.AuthErrorCode_AUTH_ERROR_CODE_RATE_LIMITED:
		retry, ok := detail.Detail.(*auth.AuthError_RetryAt)
		valid = ok && retry != nil && validTimestamp(retry.RetryAt)
	case auth.AuthErrorCode_AUTH_ERROR_CODE_UNSPECIFIED:
		valid = false
	default:
		valid = false
	}
	if !valid {
		return AdmittedAuthWire[*auth.AuthError]{}, ErrInvalidAuthContract
	}
	return AdmittedAuthWire[*auth.AuthError]{value: proto.Clone(detail).(*auth.AuthError)}, nil
}

func knownAuthField(field auth.AuthField) bool {
	switch field {
	case auth.AuthField_AUTH_FIELD_LOGIN_NAME, auth.AuthField_AUTH_FIELD_PASSWORD, auth.AuthField_AUTH_FIELD_ACCOUNT_ID,
		auth.AuthField_AUTH_FIELD_ROLE, auth.AuthField_AUTH_FIELD_STATUS, auth.AuthField_AUTH_FIELD_SESSION_ID,
		auth.AuthField_AUTH_FIELD_CHALLENGE_ID, auth.AuthField_AUTH_FIELD_FACTOR_PROOF,
		auth.AuthField_AUTH_FIELD_FACTOR_KIND, auth.AuthField_AUTH_FIELD_FACTOR_ID,
		auth.AuthField_AUTH_FIELD_INVITATION_TOKEN, auth.AuthField_AUTH_FIELD_EXPIRES_AT,
		auth.AuthField_AUTH_FIELD_CLIENT_NAME, auth.AuthField_AUTH_FIELD_SCOPES,
		auth.AuthField_AUTH_FIELD_LIFETIME, auth.AuthField_AUTH_FIELD_MCP_KEY_ID:
		return true
	case auth.AuthField_AUTH_FIELD_UNSPECIFIED:
		return false
	default:
		return false
	}
}

// Connect mapping is exhaustive over admitted auth categories. It does not
// serialize any credential or construct a public message from untrusted text.
func AuthErrorConnectCode(detail *auth.AuthError) (connect.Code, error) {
	if _, err := AdmitAuthError(detail); err != nil {
		return 0, err
	}
	switch detail.Code {
	case auth.AuthErrorCode_AUTH_ERROR_CODE_UNAUTHENTICATED:
		return connect.CodeUnauthenticated, nil
	case auth.AuthErrorCode_AUTH_ERROR_CODE_PERMISSION_DENIED:
		return connect.CodePermissionDenied, nil
	case auth.AuthErrorCode_AUTH_ERROR_CODE_CONFLICT:
		return connect.CodeFailedPrecondition, nil
	case auth.AuthErrorCode_AUTH_ERROR_CODE_EXPIRED:
		reason := detail.GetExpired()
		switch reason {
		case auth.ExpiredResource_EXPIRED_RESOURCE_SESSION, auth.ExpiredResource_EXPIRED_RESOURCE_ACCESS_TOKEN,
			auth.ExpiredResource_EXPIRED_RESOURCE_MCP_KEY:
			return connect.CodeUnauthenticated, nil
		case auth.ExpiredResource_EXPIRED_RESOURCE_INVITATION, auth.ExpiredResource_EXPIRED_RESOURCE_CHALLENGE:
			return connect.CodeFailedPrecondition, nil
		case auth.ExpiredResource_EXPIRED_RESOURCE_UNSPECIFIED:
			return 0, ErrInvalidAuthContract
		default:
			return 0, ErrInvalidAuthContract
		}
	case auth.AuthErrorCode_AUTH_ERROR_CODE_INVALID_ARGUMENT:
		return connect.CodeInvalidArgument, nil
	case auth.AuthErrorCode_AUTH_ERROR_CODE_RATE_LIMITED:
		return connect.CodeResourceExhausted, nil
	case auth.AuthErrorCode_AUTH_ERROR_CODE_UNSPECIFIED:
		return 0, ErrInvalidAuthContract
	default:
		return 0, ErrInvalidAuthContract
	}
}
