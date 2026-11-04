package server

import (
	"context"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (n authResourceNames) accountResource(ctx context.Context, record storage.AccountRecord) (*api.Account, error) {
	id, err := record.ID()
	if err != nil {
		return nil, err
	}
	name, err := n.account(ctx, id)
	if err != nil {
		return nil, err
	}
	login, err := record.Login()
	if err != nil {
		return nil, err
	}
	loginText, _, err := login.Values()
	if err != nil {
		return nil, err
	}
	display, err := record.Display()
	if err != nil {
		return nil, err
	}
	displayText, err := display.Value()
	if err != nil {
		return nil, err
	}
	role, err := record.Role()
	if err != nil {
		return nil, err
	}
	publicRole, err := publicAccountRole(role)
	if err != nil {
		return nil, err
	}
	status, err := record.Status()
	if err != nil {
		return nil, err
	}
	var state api.AccountAccess
	switch status {
	case native.AccountStatus_ACCOUNT_STATUS_ACTIVE:
		state = api.AccountAccess_ACCOUNT_ACCESS_ENABLED
	case native.AccountStatus_ACCOUNT_STATUS_DISABLED:
		state = api.AccountAccess_ACCOUNT_ACCESS_DISABLED
	default:
		return nil, storage.ErrAPIResourceIntegrity
	}
	created, err := record.CreatedAt()
	if err != nil {
		return nil, err
	}
	updated, err := record.UpdatedAt()
	if err != nil {
		return nil, err
	}
	factor, err := record.SecondFactorEnabled()
	if err != nil {
		return nil, err
	}
	version, err := record.Version()
	if err != nil {
		return nil, err
	}
	etag, err := version.Etag()
	if err != nil {
		return nil, err
	}
	return &api.Account{Name: name.String(), LoginName: loginText, DisplayName: displayText, Role: publicRole, Access: state, SecondFactorEnabled: factor, CreateTime: timestamppb.New(created), UpdateTime: timestamppb.New(updated), Etag: etag}, nil
}

func (n authResourceNames) invitationResource(ctx context.Context, record storage.InvitationRecord) (*api.Invitation, error) {
	id, err := record.ID()
	if err != nil {
		return nil, err
	}
	name, err := n.invitation(ctx, id)
	if err != nil {
		return nil, err
	}
	login, err := record.Login()
	if err != nil {
		return nil, err
	}
	text, _, err := login.Values()
	if err != nil {
		return nil, err
	}
	role, err := record.Role()
	if err != nil {
		return nil, err
	}
	publicRole, err := publicAccountRole(role)
	if err != nil {
		return nil, err
	}
	expires, err := record.ExpiresAt()
	if err != nil {
		return nil, err
	}
	created, err := record.CreatedAt()
	if err != nil {
		return nil, err
	}
	accepted, err := record.AcceptedAt()
	if err != nil {
		return nil, err
	}
	revoked, err := record.RevokedAt()
	if err != nil {
		return nil, err
	}
	result := &api.Invitation{Name: name.String(), LoginName: text, Role: publicRole, ExpireTime: timestamppb.New(expires), CreateTime: timestamppb.New(created)}
	if accepted != nil {
		result.AcceptTime = timestamppb.New(*accepted)
	}
	if revoked != nil {
		result.RevokeTime = timestamppb.New(*revoked)
	}
	return result, nil
}

func (n authResourceNames) sessionResource(ctx context.Context, record storage.SessionRecord) (*api.Session, error) {
	id, err := record.ID()
	if err != nil {
		return nil, err
	}
	account, err := record.AccountID()
	if err != nil {
		return nil, err
	}
	name, err := n.session(ctx, storage.SessionOwner{Account: account, Session: id})
	if err != nil {
		return nil, err
	}
	satisfaction, authenticated, err := record.Authentication()
	if err != nil {
		return nil, err
	}
	evidence, err := publicAuthenticationEvidence(satisfaction, authenticated)
	if err != nil {
		return nil, err
	}
	result := &api.Session{Name: name.String(), CreateTime: timestamppb.New(record.CreatedAt()), ExpireTime: timestamppb.New(record.ExpiresAt()), AuthenticationSatisfaction: evidence, AuthenticationTime: timestamppb.New(authenticated)}
	if authenticated.IsZero() {
		result.AuthenticationTime = nil
	}
	if revoked := record.RevokedAt(); revoked != nil {
		result.RevokeTime = timestamppb.New(*revoked)
	}
	if used := record.LastUsedAt(); used != nil {
		result.LastUseTime = timestamppb.New(*used)
	}
	return result, nil
}
func (n authResourceNames) sessionTokens(ctx context.Context, record storage.SessionRecord, expiry time.Time, credentials issuedCredentials) (*api.SessionTokens, error) {
	session, err := n.sessionResource(ctx, record)
	if err != nil {
		return nil, err
	}
	access, err := credentials.access.Text()
	if err != nil {
		return nil, err
	}
	refresh, err := credentials.refresh.Text()
	if err != nil {
		return nil, err
	}
	return &api.SessionTokens{Session: session, Audience: api.CredentialAudience_CREDENTIAL_AUDIENCE_INTERACTIVE, AccessToken: access, RefreshToken: refresh, AccessExpireTime: timestamppb.New(expiry)}, nil
}

func (n authResourceNames) factorResource(ctx context.Context, record storage.MFAFactorMetadata) (*api.FactorCredential, error) {
	account, factor, created, used, err := record.Values()
	if err != nil {
		return nil, err
	}
	name, err := n.factor(ctx, account, factor)
	if err != nil {
		return nil, err
	}
	result := &api.FactorCredential{Name: name.String(), Kind: api.FactorKind_FACTOR_KIND_FIDO2, CreateTime: timestamppb.New(created)}
	if used != nil {
		result.LastUseTime = timestamppb.New(*used)
	}
	return result, nil
}

func publicAuthenticationEvidence(satisfaction native.AuthenticationSatisfaction, authenticated time.Time) (api.AuthenticationSatisfaction, error) {
	var evidence api.AuthenticationSatisfaction
	switch satisfaction {
	case native.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED:
		if !authenticated.IsZero() {
			return api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED, storage.ErrAPIResourceIntegrity
		}
		evidence = api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_NOT_RECORDED
	case native.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY:
		evidence = api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY
	case native.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2:
		evidence = api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2
	case native.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE:
		evidence = api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE
	default:
		return api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED, storage.ErrAPIResourceIntegrity
	}
	if satisfaction != native.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED && authenticated.IsZero() {
		return api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED, storage.ErrAPIResourceIntegrity
	}
	return evidence, nil
}
