package server

import (
	"context"
	"crypto/cipher"
	"errors"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type authResourceHandler struct {
	*authHandler
	names authResourceNames
	pages cipher.AEAD
}

func (h *authResourceHandler) deniedLogin(ctx context.Context, limited bool) error {
	if err := h.store.RecordLoginDenial(ctx, limited); err != nil {
		return authCredentialError(ctx, err)
	}
	if limited {
		return authCredentialError(ctx, storage.ErrLoginRateLimited)
	}
	return authCredentialError(ctx, errSessionUnauthenticated)
}

func (h *authResourceHandler) createSession(ctx context.Context, account contracts.AccountID, revision storage.RevocationRevision, challenge *contracts.ChallengeID) (*api.SessionTokens, error) {
	id, err := accounts.NewSessionID()
	if err != nil {
		return nil, err
	}
	credentials, err := newIssuedCredentials()
	if err != nil {
		return nil, err
	}
	ticket, err := h.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, err
	}
	defer ticket.release()
	guard, err := h.coordinator.enter(ctx, account)
	if err != nil {
		return nil, err
	}
	defer guard.release()
	created, err := h.store.CreateSession(ctx, storage.SessionCreation{ID: id, Account: account, Revision: revision,
		ChallengeID: challenge, AccessDigest: credentials.accessDigest, RefreshDigest: credentials.refreshDigest, Now: h.coordinator.clock.Now()})
	if errors.Is(err, storage.ErrSessionCommitUnknown) {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, err
	}
	if created.Evicted != nil {
		guard.cancelLeases(ctx, created.Evicted)
	}
	tokens, err := h.names.sessionTokens(ctx, created.Session, created.AccessExpiresAt, credentials)
	if err != nil {
		return nil, err
	}
	if err := guard.retainIssuedSession(ctx, created.Session, created.AccessExpiresAt); err != nil {
		return nil, err
	}
	return tokens, nil
}

func (h *authResourceHandler) StartLogin(ctx context.Context, request *connect.Request[api.StartLoginRequest]) (*connect.Response[api.StartLoginResponse], error) {
	if request == nil || request.Msg == nil || contracts.AdmitAuthenticationRequestShape(request.Msg) != nil {
		return nil, authCredentialError(ctx, errSessionUnauthenticated)
	}
	now := h.coordinator.clock.Now()
	if !h.bucket.take(now) {
		return nil, h.deniedLogin(ctx, true)
	}
	login, err := accounts.ParseLoginName(request.Msg.LoginName)
	if err != nil {
		return nil, h.deniedLogin(ctx, false)
	}
	if err := h.store.ReserveLoginAttempt(ctx, login, now); err != nil {
		if errors.Is(err, storage.ErrLoginRateLimited) {
			return nil, h.deniedLogin(ctx, true)
		}
		return nil, authCredentialError(ctx, err)
	}
	record, lookupErr := h.store.CredentialByLogin(ctx, login)
	if lookupErr != nil && !errors.Is(lookupErr, storage.ErrAccountNotFound) {
		return nil, authCredentialError(ctx, lookupErr)
	}
	if errors.Is(lookupErr, storage.ErrAccountNotFound) {
		if err := accounts.VerifyUnknownPassword(ctx, request.Msg.Password); err != nil && !errors.Is(err, accounts.ErrInvalidPassword) {
			return nil, authCredentialError(ctx, err)
		}
		return nil, h.deniedLogin(ctx, false)
	}
	verifier, err := record.Verifier()
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	ok, err := accounts.VerifyPassword(ctx, request.Msg.Password, verifier)
	if errors.Is(err, accounts.ErrInvalidPassword) {
		return nil, h.deniedLogin(ctx, false)
	}
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	status, err := record.Status()
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	if !ok || status != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		return nil, h.deniedLogin(ctx, false)
	}
	account, err := record.ID()
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	revision, err := record.Revision()
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	factorEnabled, err := record.SecondFactorEnabled()
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	preparation := factorAuthenticated()
	challengeID, err := accounts.NewChallengeID()
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	if h.provider == nil {
		if factorEnabled {
			return nil, h.deniedLogin(ctx, false)
		}
	} else {
		if h.coordinator.mfa == nil {
			return nil, authCredentialError(ctx, authmfa.NewError(authmfa.ErrorUnavailable))
		}
		permit, permitErr := h.coordinator.mfa.crypto.Acquire(ctx)
		if permitErr != nil {
			return nil, authCredentialError(ctx, permitErr)
		}
		defer permit.Release()
		preparation, err = h.provider.Prepare(ctx, account, challengeID)
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		if preparation.revision != revision {
			return nil, authCredentialError(ctx, authmfa.NewError(authmfa.ErrorConflict))
		}
	}
	switch preparation.kind {
	case factorPreparationAuthenticated:
		if factorEnabled || len(preparation.options) != 0 {
			return nil, h.deniedLogin(ctx, false)
		}
		tokens, err := h.createSession(ctx, account, revision, nil)
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		return connect.NewResponse(&api.StartLoginResponse{Decision: &api.LoginDecision{Outcome: &api.LoginDecision_Authenticated{Authenticated: tokens}}}), nil
	case factorPreparationChallenge:
		copied := factorChallenge(preparation.options)
		if copied.kind != factorPreparationChallenge || len(copied.options) == 0 {
			return nil, authCredentialError(ctx, storage.ErrSessionUnavailable)
		}
		expiry, expiryErr := preparation.ceremony.ExpiresAt()
		if expiryErr != nil || preparation.started.IsZero() {
			return nil, authCredentialError(ctx, authmfa.NewError(authmfa.ErrorIntegrity))
		}
		issueNow := h.coordinator.clock.Now()
		idText, _ := challengeID.Value()
		candidate := &authv1.FactorChallenge{Id: &authv1.ChallengeId{Value: idText}, ExpiresAt: timestamppb.New(expiry), Options: copied.options}
		if err := contracts.AdmitNativeFactorChallenge(candidate, issueNow); err != nil {
			return nil, authCredentialError(ctx, storage.ErrSessionUnavailable)
		}
		payload, err := proto.Marshal(candidate)
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		admitted, err := h.coordinator.mfa.repository.PrepareLoginChallenge(storage.ChallengeCreation{ID: challengeID, Account: account, Revision: revision, Payload: payload, Now: preparation.started}, preparation.policy, preparation.ceremony)
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		ticket, err := h.coordinator.enterMFAPolicy(ctx)
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		defer ticket.release()
		guard, err := h.coordinator.enter(ctx, account)
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		stored, issueErr := h.coordinator.mfa.repository.IssueLoginChallenge(ctx, admitted)
		if authmfa.KindOf(issueErr) == authmfa.ErrorCommitUnknown {
			guard.quarantine()
			guard.cancelLeases(ctx, nil)
		}
		guard.release()
		if issueErr != nil {
			return nil, authCredentialError(ctx, issueErr)
		}
		candidate.ExpiresAt = timestamppb.New(stored.ExpiresAt)
		publicChallenge, err := publicFactorChallenge(candidate)
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		return connect.NewResponse(&api.StartLoginResponse{Decision: &api.LoginDecision{Outcome: &api.LoginDecision_Challenge{Challenge: publicChallenge}}}), nil
	case factorPreparationEnrollment:
		if factorEnabled || len(preparation.options) != 0 {
			return nil, h.deniedLogin(ctx, false)
		}
		grant, err := accounts.NewEnrollmentSecret()
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		digest, err := grant.Digest()
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		ticket, err := h.coordinator.enterMFAPolicy(ctx)
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		defer ticket.release()
		guard, err := h.coordinator.enter(ctx, account)
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		expires, issueErr := h.store.IssueEnrollmentGrant(ctx, account, revision, digest, h.coordinator.clock.Now())
		if errors.Is(issueErr, storage.ErrSessionCommitUnknown) {
			guard.quarantine()
			guard.cancelLeases(ctx, nil)
		}
		guard.release()
		if issueErr != nil {
			return nil, authCredentialError(ctx, issueErr)
		}
		grantText, err := grant.Text()
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		publicAccount, err := h.names.account(ctx, account)
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		decision := &api.LoginDecision{Outcome: &api.LoginDecision_EnrollmentRequired{EnrollmentRequired: &api.EnrollmentRequired{Account: publicAccount.String(), EnrollmentToken: grantText, ExpireTime: timestamppb.New(expires)}}}
		if _, err := contracts.AdmitAPILoginDecision(decision, h.coordinator.clock.Now()); err != nil {
			return nil, authCredentialError(ctx, storage.ErrSessionUnavailable)
		}
		return connect.NewResponse(&api.StartLoginResponse{Decision: decision}), nil
	default:
		return nil, authCredentialError(ctx, storage.ErrSessionUnavailable)
	}
}

func (h *authResourceHandler) CompleteLogin(ctx context.Context, request *connect.Request[api.CompleteLoginRequest]) (*connect.Response[api.CompleteLoginResponse], error) {
	if request == nil || request.Msg == nil || request.Msg.ChallengeId == "" || contracts.AdmitAuthenticationRequestShape(request.Msg) != nil || proto.Size(request.Msg) > authmfa.MaxRequestBytes {
		return nil, authCredentialError(ctx, errSessionUnauthenticated)
	}
	id, err := contracts.ParseChallengeID(request.Msg.ChallengeId)
	if err != nil {
		return nil, authCredentialError(ctx, errSessionUnauthenticated)
	}
	runtime := h.coordinator.mfa
	if runtime == nil || h.provider == nil {
		return nil, authCredentialError(ctx, authmfa.NewError(authmfa.ErrorUnavailable))
	}
	permit, err := runtime.crypto.Acquire(ctx)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	defer permit.Release()
	now := h.coordinator.clock.Now()
	challenge, err := h.store.LookupChallenge(ctx, id, now)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	snapshot, err := runtime.repository.LoginSnapshot(ctx, challenge, now)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	// Reserve both persistent budgets before offered-proof validation or crypto.
	if err := runtime.repository.ReserveProof(ctx, snapshot, now); err != nil {
		return nil, authCredentialError(ctx, err)
	}
	offered, err := snapshot.Offered()
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	nativeProof, err := nativeFactorProof(request.Msg.Proof)
	if err != nil || offered.GetId().GetValue() != request.Msg.ChallengeId || contracts.AdmitFactorProofForChallenge(nativeProof, offered, now) != nil {
		return nil, h.deniedLogin(ctx, false)
	}
	proof, err := h.provider.Verify(ctx, snapshot, nativeProof)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	finalization, err := runtime.repository.PrepareFinalization(proof, snapshot)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	sessionID, err := accounts.NewSessionID()
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	credentials, err := newIssuedCredentials()
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	ticket, err := h.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	defer ticket.release()
	guard, err := h.coordinator.enter(ctx, challenge.Account)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	defer guard.release()
	created, err := runtime.repository.FinalizeLogin(ctx, storage.SessionCreation{ID: sessionID, Account: challenge.Account, Revision: challenge.Revision, ChallengeID: &id, AccessDigest: credentials.accessDigest, RefreshDigest: credentials.refreshDigest, Now: h.coordinator.clock.Now()}, finalization)
	if authmfa.KindOf(err) == authmfa.ErrorCommitUnknown {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	if created.Evicted != nil {
		guard.cancelLeases(ctx, created.Evicted)
	}
	tokens, err := h.names.sessionTokens(ctx, created.Session, created.AccessExpiresAt, credentials)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	if err := guard.retainIssuedSession(ctx, created.Session, created.AccessExpiresAt); err != nil {
		return nil, authCredentialError(ctx, err)
	}
	return connect.NewResponse(&api.CompleteLoginResponse{Decision: &api.LoginDecision{Outcome: &api.LoginDecision_Authenticated{Authenticated: tokens}}}), nil
}

func (h *authResourceHandler) RefreshSession(ctx context.Context, request *connect.Request[api.RefreshSessionRequest]) (*connect.Response[api.RefreshSessionResponse], error) {
	if request == nil || request.Msg == nil || contracts.AdmitAuthenticationRequestShape(request.Msg) != nil {
		return nil, authCredentialError(ctx, errSessionUnauthenticated)
	}
	secret, err := accounts.ParseRefreshSecret(request.Msg.RefreshToken)
	if err != nil {
		return nil, authCredentialError(ctx, errSessionUnauthenticated)
	}
	digest, err := secret.Digest()
	if err != nil {
		return nil, authCredentialError(ctx, errSessionUnauthenticated)
	}
	key, err := accounts.ParseRenewalKey(request.Msg.RenewalKey)
	if err != nil {
		return nil, authCredentialError(ctx, errSessionUnauthenticated)
	}
	owner, err := h.store.LookupRefreshOwner(ctx, digest)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	credentials, err := newIssuedCredentials()
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	{
		credentials.refresh, err = secret.Successor(key)
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
		credentials.refreshDigest, err = credentials.refresh.Digest()
		if err != nil {
			return nil, authCredentialError(ctx, err)
		}
	}
	ticket, err := h.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	defer ticket.release()
	guard, err := h.coordinator.enter(ctx, owner.Account)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	defer guard.release()
	rotated, err := h.store.RotateRefresh(ctx, storage.RefreshRotation{Owner: owner, OldDigest: digest,
		NewDigest: credentials.refreshDigest, NewAccessDigest: credentials.accessDigest, Now: h.coordinator.clock.Now(), Recoverable: true})
	if errors.Is(err, storage.ErrSessionCommitUnknown) {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	if rotated.ReplayRevoked() {
		guard.cancelLeases(ctx, &owner.Session)
		return nil, authCredentialError(ctx, storage.ErrSessionRevoked)
	}
	current, accessExpiry, ok := rotated.Rotated()
	if !ok {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
		return nil, authCredentialError(ctx, storage.ErrSessionUnavailable)
	}
	guard.cancelLeases(ctx, &owner.Session) // old access is invalid after the confirmed commit
	tokens, err := h.names.sessionTokens(ctx, current, accessExpiry, credentials)
	if err != nil {
		return nil, authCredentialError(ctx, err)
	}
	if err := guard.retainIssuedSession(ctx, current, accessExpiry); err != nil {
		return nil, authCredentialError(ctx, err)
	}
	return connect.NewResponse(&api.RefreshSessionResponse{Tokens: tokens}), nil
}
