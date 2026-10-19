package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nativeTLSContextKey struct{}
type nativeTLSAdmission struct{}

func mfaRPCError(err error) error {
	if err == nil {
		return nil
	}
	code := connect.CodeUnavailable
	switch authmfa.KindOf(err) {
	case authmfa.ErrorInvalidInput:
		code = connect.CodeInvalidArgument
	case authmfa.ErrorInvalidProof, authmfa.ErrorExpired, authmfa.ErrorConsumed, authmfa.ErrorConflict:
		code = connect.CodeUnauthenticated
	case authmfa.ErrorRateLimited, authmfa.ErrorBusy:
		code = connect.CodeResourceExhausted
	case authmfa.ErrorIntegrity, authmfa.ErrorUnavailable, authmfa.ErrorCommitUnknown:
	case authmfa.ErrorUnspecified:
		return sessionRPCError(err)
	default:
	}
	return connect.NewError(code, errors.New("second factor operation refused"))
}
func mfaPolicyRPCError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sessionRPCError(err)
	}
	detail := &authv1.SecondFactorPolicyError{Code: authv1.SecondFactorPolicyErrorCode_SECOND_FACTOR_POLICY_ERROR_CODE_AUTHORITY_UNAVAILABLE}
	code := connect.CodeUnavailable
	switch authmfa.KindOf(err) {
	case authmfa.ErrorInvalidInput:
		code = connect.CodeInvalidArgument
		detail.Code = authv1.SecondFactorPolicyErrorCode_SECOND_FACTOR_POLICY_ERROR_CODE_INVALID_VERSION
		detail.Field = "expected"
	case authmfa.ErrorConflict:
		code = connect.CodeAborted
		detail.Code = authv1.SecondFactorPolicyErrorCode_SECOND_FACTOR_POLICY_ERROR_CODE_VERSION_CONFLICT
		detail.Field = "expected"
	case authmfa.ErrorInvalidProof:
		code = connect.CodePermissionDenied
	case authmfa.ErrorBusy, authmfa.ErrorRateLimited:
		code = connect.CodeResourceExhausted
	case authmfa.ErrorUnspecified, authmfa.ErrorExpired, authmfa.ErrorConsumed, authmfa.ErrorIntegrity, authmfa.ErrorUnavailable, authmfa.ErrorCommitUnknown:
	default:
	}
	result := connect.NewError(code, errors.New("second factor policy unavailable or precondition refused"))
	if _, err := contracts.AdmitSecondFactorPolicyError(detail); err == nil {
		if wire, e := connect.NewErrorDetail(detail); e == nil {
			result.AddDetail(wire)
		}
	}
	return result
}

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
	ctx       context.Context
	release   func()
	account   contracts.AccountID
	revision  storage.RevocationRevision
	authority storage.MFAEnrollmentAuthority
}

func (h *authHandler) enrollmentAdmission(ctx context.Context, headers http.Header, grant string) (mfaEnrollmentAdmission, error) {
	result := mfaEnrollmentAdmission{ctx: ctx, release: func() {}}
	if !validMFAPolicyContext(ctx) {
		return result, authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	if _, ok := ctx.Value(nativeTLSContextKey{}).(nativeTLSAdmission); !ok {
		return result, authmfa.NewError(authmfa.ErrorInvalidProof)
	}
	runtime, err := h.mfaRuntime()
	if err != nil {
		return result, err
	}
	if grant != "" {
		if len(headers.Values("Authorization")) != 0 {
			return result, authmfa.NewError(authmfa.ErrorInvalidInput)
		}
		secret, err := accounts.ParseEnrollmentSecret(grant)
		if err != nil {
			return result, authmfa.NewError(authmfa.ErrorInvalidProof)
		}
		digest, err := secret.Digest()
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
func factorMetadataWire(value storage.MFAFactorMetadata) (*authv1.FactorCredential, error) {
	account, factor, created, used, err := value.Values()
	if err != nil {
		return nil, err
	}
	accountID, _ := account.Value()
	factorID, _ := factor.Value()
	result := &authv1.FactorCredential{Id: &authv1.FactorCredentialId{Value: factorID}, AccountId: &authv1.AccountId{Value: accountID}, Kind: authv1.FactorKind_FACTOR_KIND_FIDO2, CreatedAt: timestamppb.New(created)}
	if used != nil {
		result.LastUsedAt = timestamppb.New(*used)
	}
	return result, nil
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
func selfMFAAccount(wire *authv1.AccountId, owner storage.SessionOwner) error {
	if wire == nil || len(wire.ProtoReflect().GetUnknown()) != 0 {
		return authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	id, err := contracts.ParseAccountID(wire.Value)
	if err != nil {
		return authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	if id != owner.Account {
		return authmfa.NewError(authmfa.ErrorInvalidProof)
	}
	return nil
}

func (h *authHandler) BeginFactorEnrollment(ctx context.Context, request *connect.Request[authv1.BeginFactorEnrollmentRequest]) (*connect.Response[authv1.BeginFactorEnrollmentResponse], error) {
	if request == nil || request.Msg == nil || proto.Size(request.Msg) > authmfa.MaxRequestBytes {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorInvalidInput))
	}
	runtime, err := h.mfaRuntime()
	if err != nil {
		return nil, mfaRPCError(err)
	}
	permit, err := runtime.crypto.Acquire(ctx)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	defer permit.Release()
	admitted, err := h.enrollmentAdmission(ctx, request.Header(), request.Msg.EnrollmentToken)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	defer admitted.release()
	ctx = admitted.ctx
	state, err := runtime.repository.AccountState(ctx, admitted.account)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	_, revision, policy, err := state.Requirement()
	if err != nil || revision != admitted.revision {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorConflict))
	}
	handle, err := authmfa.NewUserHandle()
	if err != nil {
		return nil, mfaRPCError(err)
	}
	ticket, err := h.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	guard, err := h.coordinator.enter(ctx, admitted.account)
	if err != nil {
		ticket.release()
		return nil, mfaRPCError(err)
	}
	err = runtime.repository.ReserveBegin(ctx, admitted.authority, admitted.account, revision, policy, h.coordinator.clock.Now())
	if err == nil && contracts.AdmitBeginFactorEnrollment(request.Msg) != nil {
		err = authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	if err == nil {
		_, err = runtime.repository.EnsureUserHandle(ctx, admitted.account, revision, runtime.party, handle)
	}
	err = releaseMFAPreparation(guard, ctx, err)
	ticket.release()
	if err != nil {
		return nil, mfaRPCError(err)
	}
	user, err := runtime.repository.UserSnapshot(ctx, admitted.account, runtime.party)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	id, err := accounts.NewChallengeID()
	if err != nil {
		return nil, mfaRPCError(err)
	}
	instance, _, _, _ := runtime.party.Values()
	binding, err := authmfa.NewBinding(admitted.account, id, instance)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	now := h.coordinator.clock.Now()
	ceremony, err := runtime.verifier.BeginEnrollment(ctx, binding, user, now)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	prepared, err := runtime.repository.PrepareEnrollmentBegin(admitted.account, revision, policy, ceremony, user, admitted.authority, now)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	ticket, err = h.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	guard, err = h.coordinator.enter(ctx, admitted.account)
	if err != nil {
		ticket.release()
		return nil, mfaRPCError(err)
	}
	_, err = runtime.repository.BeginEnrollment(ctx, prepared)
	err = releaseMFAPreparation(guard, ctx, err)
	ticket.release()
	if err != nil {
		return nil, mfaRPCError(err)
	}
	options, err := ceremony.EnrollmentOptions(user, now)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	expiry, _ := ceremony.ExpiresAt()
	idText, _ := id.Value()
	return connect.NewResponse(&authv1.BeginFactorEnrollmentResponse{Challenge: &authv1.EnrollmentChallenge{Id: &authv1.ChallengeId{Value: idText}, ExpiresAt: timestamppb.New(expiry), Fido2: options}}), nil
}

func (h *authHandler) CompleteFactorEnrollment(ctx context.Context, request *connect.Request[authv1.CompleteFactorEnrollmentRequest]) (*connect.Response[authv1.CompleteFactorEnrollmentResponse], error) {
	if request == nil || request.Msg == nil || request.Msg.ChallengeId == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 || len(request.Msg.ChallengeId.ProtoReflect().GetUnknown()) != 0 || proto.Size(request.Msg) > authmfa.MaxRequestBytes {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorInvalidInput))
	}
	id, err := contracts.ParseChallengeID(request.Msg.ChallengeId.Value)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	runtime, err := h.mfaRuntime()
	if err != nil {
		return nil, mfaRPCError(err)
	}
	permit, err := runtime.crypto.Acquire(ctx)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	defer permit.Release()
	admitted, err := h.enrollmentAdmission(ctx, request.Header(), request.Msg.EnrollmentToken)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	defer admitted.release()
	ctx = admitted.ctx
	snapshot, err := runtime.repository.EnrollmentSnapshot(ctx, id, admitted.authority, h.coordinator.clock.Now())
	if err != nil {
		return nil, mfaRPCError(err)
	}
	if err := runtime.repository.ReserveEnrollmentProof(ctx, snapshot, h.coordinator.clock.Now()); err != nil {
		return nil, mfaRPCError(err)
	}
	user, ceremony, err := snapshot.Verification()
	if err != nil {
		return nil, mfaRPCError(err)
	}
	factor, err := newFactorCredentialID()
	if err != nil {
		return nil, mfaRPCError(err)
	}
	proof, err := runtime.verifier.VerifyEnrollment(ctx, user, ceremony, factor, request.Msg.Attestation, h.coordinator.clock.Now())
	if err != nil {
		return nil, mfaRPCError(err)
	}
	batch, candidates, codes, err := authmfa.GenerateRecoveryBatch(admitted.account)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	values, err := recoveryCodesWire(codes)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	persisted, err := storage.NewMFARecoveryBatch(admitted.account, batch, candidates)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	prepared, err := runtime.repository.PrepareEnrollmentCommit(proof, snapshot, persisted)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	accountsHandler := &accountHandler{store: h.store, coordinator: h.coordinator}
	guard, err := accountsHandler.accountMutationGuard(ctx, admitted.account)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	activation, err := runtime.repository.CompleteEnrollment(ctx, prepared, h.coordinator.clock.Now())
	err = finishMFAMutation(guard, ctx, err)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	metadata, err := activation.Metadata()
	if err != nil {
		return nil, mfaRPCError(err)
	}
	wire, err := factorMetadataWire(metadata)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	return connect.NewResponse(&authv1.CompleteFactorEnrollmentResponse{Factor: wire, RecoveryCodes: values}), nil
}

func (h *authHandler) ListFactors(ctx context.Context, request *connect.Request[authv1.ListFactorsRequest]) (*connect.Response[authv1.ListFactorsResponse], error) {
	if request == nil || request.Msg == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorInvalidInput))
	}
	owner, err := h.selfMFASession(ctx)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	if err := selfMFAAccount(request.Msg.AccountId, owner); err != nil {
		return nil, mfaRPCError(err)
	}
	if h.coordinator.mfa == nil {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorUnavailable))
	}
	values, err := h.coordinator.mfa.repository.ListFactors(ctx, owner, h.coordinator.clock.Now())
	if err != nil {
		return nil, mfaRPCError(err)
	}
	result := &authv1.ListFactorsResponse{}
	for _, value := range values {
		wire, err := factorMetadataWire(value)
		if err != nil {
			return nil, mfaRPCError(err)
		}
		result.Factors = append(result.Factors, wire)
	}
	return connect.NewResponse(result), nil
}
func (h *authHandler) RemoveFactor(ctx context.Context, request *connect.Request[authv1.RemoveFactorRequest]) (*connect.Response[authv1.RemoveFactorResponse], error) {
	if request == nil || request.Msg == nil || request.Msg.FactorId == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 || len(request.Msg.FactorId.ProtoReflect().GetUnknown()) != 0 {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorInvalidInput))
	}
	owner, err := h.selfMFASession(ctx)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	factor, err := contracts.ParseFactorCredentialID(request.Msg.FactorId.Value)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	if h.coordinator.mfa == nil {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorUnavailable))
	}
	account := &accountHandler{store: h.store, coordinator: h.coordinator}
	guard, err := account.accountMutationGuard(ctx, owner.Account)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	_, err = h.coordinator.mfa.repository.RemoveFactor(ctx, owner, factor, h.coordinator.clock.Now())
	if err := finishMFAMutation(guard, ctx, err); err != nil {
		return nil, mfaRPCError(err)
	}
	return connect.NewResponse(&authv1.RemoveFactorResponse{}), nil
}
func (h *authHandler) ReplaceRecoveryCodes(ctx context.Context, request *connect.Request[authv1.ReplaceRecoveryCodesRequest]) (*connect.Response[authv1.ReplaceRecoveryCodesResponse], error) {
	if request == nil || request.Msg == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorInvalidInput))
	}
	owner, err := h.selfMFASession(ctx)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	if err := selfMFAAccount(request.Msg.AccountId, owner); err != nil {
		return nil, mfaRPCError(err)
	}
	if h.coordinator.mfa == nil {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorUnavailable))
	}
	permit, err := h.coordinator.mfa.crypto.Acquire(ctx)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	defer permit.Release()
	batch, candidates, codes, err := authmfa.GenerateRecoveryBatch(owner.Account)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	values, err := recoveryCodesWire(codes)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	persisted, err := storage.NewMFARecoveryBatch(owner.Account, batch, candidates)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	account := &accountHandler{store: h.store, coordinator: h.coordinator}
	guard, err := account.accountMutationGuard(ctx, owner.Account)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	_, err = h.coordinator.mfa.repository.ReplaceRecoveryCodes(ctx, owner, persisted, h.coordinator.clock.Now())
	if err := finishMFAMutation(guard, ctx, err); err != nil {
		return nil, mfaRPCError(err)
	}
	return connect.NewResponse(&authv1.ReplaceRecoveryCodesResponse{RecoveryCodes: values}), nil
}

func policyWire(value storage.MFAPolicySnapshot) (*authv1.SecondFactorPolicy, error) {
	source, err := value.Source()
	if err != nil {
		return nil, err
	}
	required, _ := source.Required()
	version, err := value.Version()
	if err != nil {
		return nil, err
	}
	revision, hash, _ := version.Values()
	when, _ := value.UpdatedAt()
	return &authv1.SecondFactorPolicy{RequireAdminFactor: required, UpdatedAt: timestamppb.New(when), Version: &authv1.PolicyVersion{Revision: revision, SourceSha256: append([]byte(nil), hash[:]...)}}, nil
}

func (h *accountHandler) GetSecondFactorPolicy(ctx context.Context, request *connect.Request[authv1.GetSecondFactorPolicyRequest]) (*connect.Response[authv1.GetSecondFactorPolicyResponse], error) {
	if request == nil || request.Msg == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, mfaPolicyRPCError(authmfa.NewError(authmfa.ErrorInvalidInput))
	}
	if _, err := admittedActor(ctx); err != nil {
		return nil, accountRPCError(err)
	}
	value, err := h.store.MFAPolicy(ctx)
	if err != nil {
		return nil, mfaPolicyRPCError(err)
	}
	wire, err := policyWire(value)
	if err != nil {
		return nil, mfaPolicyRPCError(err)
	}
	return connect.NewResponse(&authv1.GetSecondFactorPolicyResponse{Policy: wire}), nil
}
func (h *accountHandler) SetSecondFactorPolicy(ctx context.Context, request *connect.Request[authv1.SetSecondFactorPolicyRequest]) (*connect.Response[authv1.SetSecondFactorPolicyResponse], error) {
	if request == nil {
		return nil, mfaPolicyRPCError(authmfa.NewError(authmfa.ErrorInvalidInput))
	}
	admitted, err := contracts.AdmitSetSecondFactorPolicy(request.Msg)
	if err != nil {
		return nil, mfaPolicyRPCError(authmfa.NewError(authmfa.ErrorInvalidInput))
	}
	input, err := admitted.Snapshot()
	if err != nil {
		return nil, mfaPolicyRPCError(err)
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return nil, accountRPCError(err)
	}
	if h.coordinator == nil || h.coordinator.mfa == nil {
		return nil, mfaPolicyRPCError(authmfa.NewError(authmfa.ErrorUnavailable))
	}
	if input.RequireAdminFactor {
		if err := h.coordinator.mfa.requireReady(); err != nil {
			return nil, mfaPolicyRPCError(err)
		}
	}
	if err := h.coordinator.reconcileMFAPolicy(ctx); err != nil {
		return nil, mfaPolicyRPCError(err)
	}
	expected, err := authmfa.NewExpectedPolicyVersion(input.Expected.Revision, input.Expected.SourceSha256)
	if err != nil {
		return nil, mfaPolicyRPCError(err)
	}
	owner, err := h.coordinator.policy.beginReplace(ctx, expected)
	if err != nil {
		return nil, mfaPolicyRPCError(err)
	}
	defer owner.abandon()
	result, err := h.coordinator.mfa.repository.SetMFAPolicy(owner.ctx, actor, expected, authmfa.NewPolicySource(input.RequireAdminFactor), h.coordinator.clock.Now())
	if err != nil {
		if authmfa.KindOf(err) == authmfa.ErrorCommitUnknown {
			known := result.AttemptedAccounts()
			_ = owner.setUnknown(known)
			h.coordinator.quarantineMFAPolicyAccounts(known)
		} else {
			_ = owner.rollback()
		}
		return nil, mfaPolicyRPCError(err)
	}
	known := result.AttemptedAccounts()
	policy, returned, _, err := result.Result()
	if err == nil {
		known = returned
	}
	if err == nil {
		err = h.coordinator.publishMFAPolicyAccounts(owner.ctx, known)
	}
	if err != nil {
		_ = owner.setUnknown(known)
		h.coordinator.quarantineMFAPolicyAccounts(known)
		return nil, mfaPolicyRPCError(err)
	}
	version, err := policy.Version()
	if err == nil {
		err = owner.publish(version)
	}
	if err != nil {
		_ = owner.setUnknown(known)
		h.coordinator.quarantineMFAPolicyAccounts(known)
		return nil, mfaPolicyRPCError(err)
	}
	wire, err := policyWire(policy)
	if err != nil {
		return nil, mfaPolicyRPCError(err)
	}
	continuation := authv1.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_CONTINUES
	for _, id := range known {
		if id == actor {
			continuation = authv1.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_REAUTHENTICATION_REQUIRED
			break
		}
	}
	response := &authv1.SetSecondFactorPolicyResponse{Policy: wire, Continuation: continuation}
	if _, err := contracts.AdmitSetSecondFactorPolicyResponse(response); err != nil {
		return nil, mfaPolicyRPCError(err)
	}
	return connect.NewResponse(response), nil
}

// Only OperatorAccountService on the owned Unix socket registers this method.
// RecoverAccount continues to change only the password.
func (h *accountHandler) ResetSecondFactor(ctx context.Context, request *connect.Request[authv1.ResetSecondFactorRequest]) (*connect.Response[authv1.ResetSecondFactorResponse], error) {
	if request == nil || contracts.AdmitResetSecondFactor(request.Msg) != nil {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorInvalidInput))
	}
	id, err := contracts.ParseAccountID(request.Msg.AccountId.Value)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	if h.coordinator == nil || h.coordinator.mfa == nil {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorUnavailable))
	}
	guard, err := h.accountMutationGuard(ctx, id)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	result, err := h.coordinator.mfa.repository.ResetSecondFactor(ctx, id, h.coordinator.clock.Now())
	if err := finishMFAMutation(guard, ctx, err); err != nil {
		return nil, mfaRPCError(err)
	}
	record, err := result.Record()
	if err != nil {
		return nil, mfaRPCError(err)
	}
	account, err := accountWire(record)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	return connect.NewResponse(&authv1.ResetSecondFactorResponse{Account: account}), nil
}
