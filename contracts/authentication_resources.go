package contracts

import (
	"errors"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

var ErrInvalidAuthenticationResource = errors.New("invalid authentication resource contract")

// AdmitAuthenticationRequestShape rejects unknown executable fields recursively
// before any effect. It does not establish credentials, permissions or names.
// Policy sources alone may reach 1 MiB; all other requests are bounded to 64 KiB.
func AdmitAuthenticationRequestShape(value proto.Message) error {
	if value == nil || !value.ProtoReflect().IsValid() || !configurationKnown(value.ProtoReflect()) {
		return ErrInvalidAuthenticationResource
	}
	limit := 64 << 10
	switch value.(type) {
	case *api.ReplaceAuthorizationPolicyRequest:
		limit = (1 << 20) + 4096
	case *api.CreateAccountRequest, *api.GetAccountRequest, *api.ListAccountsRequest,
		*api.UpdateAccountRequest, *api.DeleteAccountRequest, *api.IssueInvitationRequest,
		*api.AcceptInvitationRequest, *api.ListInvitationsRequest, *api.RevokeInvitationRequest,
		*api.GetSecondFactorPolicyRequest, *api.ChangeSecondFactorPolicyRequest,
		*api.StartLoginRequest, *api.CompleteLoginRequest, *api.RefreshSessionRequest,
		*api.ListSessionsRequest, *api.RevokeSessionRequest, *api.BeginFactorEnrollmentRequest,
		*api.CompleteFactorEnrollmentRequest, *api.ListFactorCredentialsRequest,
		*api.RemoveFactorCredentialRequest, *api.ReplaceRecoveryCodesRequest,
		*api.IssueMcpKeyRequest, *api.ListMcpKeysRequest, *api.RevokeMcpKeyRequest,
		*api.GetAuthorizationPolicyRequest:
	default:
		return ErrInvalidAuthenticationResource
	}
	if proto.Size(value) > limit {
		return ErrInvalidAuthenticationResource
	}
	return nil
}

func AdmitAPIScope(value *api.Scope) error {
	if value == nil || !configurationKnown(value.ProtoReflect()) {
		return ErrInvalidAuthenticationResource
	}
	switch value.Family {
	case api.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, api.ScopeFamily_SCOPE_FAMILY_HISTORY, api.ScopeFamily_SCOPE_FAMILY_MEDIA,
		api.ScopeFamily_SCOPE_FAMILY_LOCATION, api.ScopeFamily_SCOPE_FAMILY_ACTIONS, api.ScopeFamily_SCOPE_FAMILY_DASHBOARDS,
		api.ScopeFamily_SCOPE_FAMILY_CALENDARS, api.ScopeFamily_SCOPE_FAMILY_LISTS, api.ScopeFamily_SCOPE_FAMILY_LOGIC,
		api.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, api.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, api.ScopeFamily_SCOPE_FAMILY_ACCOUNTS, api.ScopeFamily_SCOPE_FAMILY_BACKUPS:
	default:
		return ErrInvalidAuthenticationResource
	}
	switch value.Operation {
	case api.ScopeOperation_SCOPE_OPERATION_READ, api.ScopeOperation_SCOPE_OPERATION_WRITE, api.ScopeOperation_SCOPE_OPERATION_ADMINISTER:
		return nil
	default:
		return ErrInvalidAuthenticationResource
	}
}
func AdmitAPIMcpKeyIssue(value *api.IssueMcpKeyRequest) error {
	if value == nil || AdmitAuthenticationRequestShape(value) != nil || !validText(value.ClientName) || len(value.ClientName) > 256 || len(value.Scopes) > 39 {
		return ErrInvalidAuthenticationResource
	}
	if _, err := apitypes.ParseAccountName(value.Parent); err != nil {
		return ErrInvalidAuthenticationResource
	}
	if _, err := AdmitMcpKeyLifetime(value.Lifetime); err != nil {
		return ErrInvalidAuthenticationResource
	}
	seen := map[[2]int32]bool{}
	for _, scope := range value.Scopes {
		if AdmitAPIScope(scope) != nil {
			return ErrInvalidAuthenticationResource
		}
		key := [2]int32{int32(scope.Family), int32(scope.Operation)}
		if seen[key] {
			return ErrInvalidAuthenticationResource
		}
		seen[key] = true
	}
	return nil
}
func AdmitAPIMcpKey(value *api.McpKey, now time.Time) error {
	if value == nil || !configurationKnown(value.ProtoReflect()) || value.Audience != api.CredentialAudience_CREDENTIAL_AUDIENCE_MCP || !validText(value.ClientName) || len(value.ClientName) > 256 || len(value.Scopes) > 39 || !validTimestamp(value.CreateTime) || !validFuture(value.ExpireTime, now) || value.RevokeTime != nil {
		return ErrInvalidAuthenticationResource
	}
	if _, err := apitypes.ParseMcpKeyName(value.Name); err != nil {
		return ErrInvalidAuthenticationResource
	}
	lifetime := value.ExpireTime.AsTime().Sub(value.CreateTime.AsTime())
	if lifetime <= 0 || lifetime > 365*24*time.Hour || value.LastUseTime != nil && !validTimestamp(value.LastUseTime) {
		return ErrInvalidAuthenticationResource
	}
	seen := map[[2]int32]bool{}
	for _, scope := range value.Scopes {
		if AdmitAPIScope(scope) != nil {
			return ErrInvalidAuthenticationResource
		}
		key := [2]int32{int32(scope.Family), int32(scope.Operation)}
		if seen[key] {
			return ErrInvalidAuthenticationResource
		}
		seen[key] = true
	}
	return nil
}

// AdmitAPISessionTokens validates received metadata without verifying bearers.
// A bounded clock interval matches the interactive Rust owner: admission can
// retain access that needs immediate renewal, never a dead session family.
func AdmitAPISessionTokens(value *api.SessionTokens, now time.Time) error {
	return admitAPISessionTokens(value, now, 30*time.Second)
}

func admitAPISessionTokens(value *api.SessionTokens, now time.Time, uncertainty time.Duration) error {
	if value == nil || !configurationKnown(value.ProtoReflect()) || proto.Size(value) > 64<<10 || value.Audience != api.CredentialAudience_CREDENTIAL_AUDIENCE_INTERACTIVE || !validText(value.AccessToken) || !validText(value.RefreshToken) || len(value.AccessToken) > 4096 || len(value.RefreshToken) > 4096 || !validTimestamp(value.AccessExpireTime) || value.Session == nil {
		return ErrInvalidAuthenticationResource
	}
	session := value.Session
	if _, err := apitypes.ParseSessionName(session.Name); err != nil {
		return ErrInvalidAuthenticationResource
	}
	if !validTimestamp(session.CreateTime) || !validTimestamp(session.ExpireTime) || session.RevokeTime != nil {
		return ErrInvalidAuthenticationResource
	}
	created, expires := session.CreateTime.AsTime(), session.ExpireTime.AsTime()
	latest, earliest := now.Add(uncertainty), now.Add(-uncertainty)
	access := value.AccessExpireTime.AsTime()
	if created.After(latest) || !expires.After(created) || !expires.After(latest) || !access.After(earliest) || access.After(latest.Add(15*time.Minute)) || access.After(expires) {
		return ErrInvalidAuthenticationResource
	}
	if session.LastUseTime != nil && (!validTimestamp(session.LastUseTime) || session.LastUseTime.AsTime().Before(created) || session.LastUseTime.AsTime().After(latest)) {
		return ErrInvalidAuthenticationResource
	}
	switch session.AuthenticationSatisfaction {
	case api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_NOT_RECORDED:
		if session.AuthenticationTime != nil {
			return ErrInvalidAuthenticationResource
		}
	case api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY, api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2, api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE:
		if !validTimestamp(session.AuthenticationTime) || !session.AuthenticationTime.AsTime().Equal(created) || session.AuthenticationTime.AsTime().After(latest) {
			return ErrInvalidAuthenticationResource
		}
	default:
		return ErrInvalidAuthenticationResource
	}
	return nil
}
