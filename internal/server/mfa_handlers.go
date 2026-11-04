package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
)

type nativeTLSContextKey struct{}
type nativeTLSAdmission struct{}

func (h *authHandler) mfaRuntime() (*mfaRuntime, error) {
	if h == nil || h.coordinator == nil || h.coordinator.mfa == nil {
		return nil, authmfa.NewError(authmfa.ErrorUnavailable)
	}
	runtime := h.coordinator.mfa
	if err := runtime.requireReady(); err != nil {
		return nil, err
	}
	return runtime, nil
}
func (h *authHandler) selfMFASession(ctx context.Context) (storage.SessionOwner, error) {
	if h == nil || h.coordinator == nil {
		return storage.SessionOwner{}, authmfa.NewError(authmfa.ErrorUnavailable)
	}
	if !validMFAPolicyContext(ctx) {
		return storage.SessionOwner{}, authmfa.NewError(authmfa.ErrorInvalidProof)
	}
	lease, ok := ctx.Value(sessionLeaseContextKey{}).(*sessionLease)
	principal, err := authorization.PrincipalFromContext(ctx)
	if !ok || lease == nil || lease.coordinator != h.coordinator || err != nil || !principal.Native() {
		return storage.SessionOwner{}, authmfa.NewError(authmfa.ErrorInvalidProof)
	}
	return lease.owner, nil
}

type mfaEnrollmentAdmission struct {
	session   *sessionLease
	ctx       context.Context
	release   func()
	account   contracts.AccountID
	revision  storage.RevocationRevision
	authority storage.MFAEnrollmentAuthority
}

// retainResponse transfers only the actual interactive enrollment lease. A grant
// has no interactive principal and cannot be upgraded into one by the transport.
func (a *mfaEnrollmentAdmission) retainResponse(ctx context.Context) error {
	if a.session == nil {
		return nil
	}
	retained, err := retainPublicResponse(ctx, a.ctx, func() bool {
		return a.ctx != nil && a.ctx.Err() == nil
	}, a.release)
	if retained {
		a.release = func() {}
	}
	return err
}

func (h *authHandler) enrollmentAdmission(ctx context.Context, headers http.Header, grant string) (mfaEnrollmentAdmission, error) {
	result := mfaEnrollmentAdmission{ctx: ctx, release: func() {}}
	if !validMFAPolicyContext(ctx) {
		return result, authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	if _, ok := ctx.Value(nativeTLSContextKey{}).(nativeTLSAdmission); !ok {
		return result, authmfa.NewError(authmfa.ErrorInvalidProof)
	}
	if grant != "" {
		if len(headers.Values("Authorization")) != 0 {
			return result, authmfa.NewError(authmfa.ErrorInvalidProof)
		}
		secret, err := accounts.ParseEnrollmentSecret(grant)
		if err != nil {
			return result, authmfa.NewError(authmfa.ErrorInvalidProof)
		}
		digest, err := secret.Digest()
		if err != nil {
			return result, err
		}
		runtime, err := h.mfaRuntime()
		if err != nil {
			return result, err
		}
		result.account, result.revision, err = runtime.repository.LookupEnrollmentGrant(ctx, digest, h.coordinator.clock.Now())
		if err == nil {
			result.authority, err = storage.MFAEnrollmentFromGrant(digest)
		}
		return result, err
	}
	auth := &sessionAuth{store: h.store, coordinator: h.coordinator}
	_, leased, release, err := auth.authenticateAccessHeaders(ctx, headers)
	if err != nil {
		return result, err
	}
	owner, err := h.selfMFASession(leased)
	if err != nil {
		release()
		return result, err
	}
	result.ctx, result.release, result.account = leased, release, owner.Account
	result.session, _ = leased.Value(sessionLeaseContextKey{}).(*sessionLease)
	runtime, err := h.mfaRuntime()
	if err != nil {
		release()
		result.release = func() {}
		return result, err
	}
	state, err := runtime.repository.AccountState(leased, owner.Account)
	if err == nil {
		_, result.revision, _, err = state.Requirement()
	}
	if err == nil {
		result.authority, err = storage.MFAEnrollmentFromSession(owner)
	}
	if err != nil {
		release()
		result.release = func() {}
	}
	return result, err
}

func finishMFAMutation(guard *sessionGuard, ctx context.Context, err error) error {
	if authmfa.KindOf(err) == authmfa.ErrorCommitUnknown {
		_ = finishAccountMutation(guard, ctx, storage.ErrAccountCommitUnknown, true)
		return err
	}
	if err != nil {
		_ = finishAccountMutation(guard, ctx, err, false)
		return err
	}
	if err := finishAccountMutation(guard, ctx, nil, true); err != nil {
		return authmfa.NewError(authmfa.ErrorUnavailable)
	}
	return nil
}
func releaseMFAPreparation(guard *sessionGuard, ctx context.Context, err error) error {
	if authmfa.KindOf(err) == authmfa.ErrorCommitUnknown {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	guard.release()
	return err
}
func recoveryCodesWire(codes []authmfa.RecoveryCode) ([]string, error) {
	if len(codes) != authmfa.RecoveryCodeCount {
		return nil, authmfa.NewError(authmfa.ErrorIntegrity)
	}
	values := make([]string, len(codes))
	for i, code := range codes {
		value, err := code.Text()
		if err != nil {
			return nil, err
		}
		values[i] = value
	}
	return values, nil
}
func newFactorCredentialID() (contracts.FactorCredentialID, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return contracts.FactorCredentialID{}, authmfa.NewError(authmfa.ErrorUnavailable)
	}
	return contracts.ParseFactorCredentialID(hex.EncodeToString(entropy[:]))
}

// Only OperatorAccountService on the owned Unix socket registers this method.
// RecoverAccount continues to change only the password.
func (h *accountHandler) ResetSecondFactor(ctx context.Context, request *connect.Request[authv1.ResetSecondFactorRequest]) (*connect.Response[authv1.ResetSecondFactorResponse], error) {
	if request == nil || contracts.AdmitResetSecondFactor(request.Msg) != nil {
		return nil, authAPIError(ctx, authmfa.NewError(authmfa.ErrorInvalidInput))
	}
	id, err := contracts.ParseAccountID(request.Msg.AccountId.Value)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if h.coordinator == nil || h.coordinator.mfa == nil {
		return nil, authAPIError(ctx, authmfa.NewError(authmfa.ErrorUnavailable))
	}
	guard, err := h.accountMutationGuard(ctx, id)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	result, err := h.coordinator.mfa.repository.ResetSecondFactor(ctx, id, h.coordinator.clock.Now())
	if err := finishMFAMutation(guard, ctx, err); err != nil {
		return nil, authAPIError(ctx, err)
	}
	record, err := result.Record()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	account, err := accountWire(record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&authv1.ResetSecondFactorResponse{Account: account}), nil
}
