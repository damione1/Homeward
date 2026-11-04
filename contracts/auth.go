package contracts

import (
	"errors"
	"reflect"
	"time"
	"unicode/utf8"

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
func validChallengeID(id *auth.ChallengeId) bool {
	if id == nil {
		return false
	}
	_, err := ParseChallengeID(id.Value)
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

// AdmitNativeFactorChallenge validates the persisted MFA ceremony vocabulary.
// It does not construct a public login response or verify a factor proof.
func AdmitNativeFactorChallenge(challenge *auth.FactorChallenge, now time.Time) error {
	if !validFactorChallenge(challenge, now) {
		return ErrInvalidAuthContract
	}
	return nil
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

// Connect mapping is exhaustive over admitted auth categories. It does not
// serialize any credential or construct a public message from untrusted text.

// AdmitAuthenticationSatisfaction admits newly issued authentication methods.
// Legacy UNSPECIFIED remains an explicit metadata state, never new evidence.
func AdmitAuthenticationSatisfaction(value auth.AuthenticationSatisfaction) error {
	switch value {
	case auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY, auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2, auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE:
		return nil
	case auth.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED:
		return ErrInvalidAuthContract
	default:
		return ErrInvalidAuthContract
	}
}
func AdmitResetSecondFactor(request *auth.ResetSecondFactorRequest) error {
	if request == nil || !knownFieldsOnly(request.ProtoReflect()) || !validAccountID(request.AccountId) || len(request.AccountId.Value) > 128 {
		return ErrInvalidAuthContract
	}
	return nil
}
