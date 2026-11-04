package server

import (
	"connectrpc.com/connect"
	"context"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (h *authResourceHandler) BeginFactorEnrollment(ctx context.Context, request *connect.Request[api.BeginFactorEnrollmentRequest]) (*connect.Response[api.BeginFactorEnrollmentResponse], error) {
	if request == nil || request.Msg == nil || proto.Size(request.Msg) > authmfa.MaxRequestBytes {
		return nil, authAPIError(ctx, errSessionUnauthenticated)
	}
	admitted, err := h.enrollmentAdmission(ctx, request.Header(), request.Msg.EnrollmentToken)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer func() { admitted.release() }()
	ctx = admitted.ctx
	runtime, err := h.mfaRuntime()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	permit, err := runtime.crypto.Acquire(ctx)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer permit.Release()
	parent, err := h.names.account(ctx, admitted.account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if parent.String() != request.Msg.Parent {
		return nil, authAPIError(ctx, authorization.ErrDenied)
	}
	if err := authShape(request.Msg); err != nil {
		return nil, authAPIError(ctx, err)
	}
	state, err := runtime.repository.AccountState(ctx, admitted.account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	_, revision, policy, err := state.Requirement()
	if err != nil || revision != admitted.revision {
		return nil, authAPIError(ctx, authmfa.NewError(authmfa.ErrorConflict))
	}
	handle, err := authmfa.NewUserHandle()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	ticket, err := h.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard, err := h.coordinator.enter(ctx, admitted.account)
	if err != nil {
		ticket.release()
		return nil, authAPIError(ctx, err)
	}
	err = runtime.repository.ReserveBegin(ctx, admitted.authority, admitted.account, revision, policy, h.coordinator.clock.Now())
	if err == nil && request.Msg.Kind != api.FactorKind_FACTOR_KIND_FIDO2 {
		err = authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	if err == nil {
		_, err = runtime.repository.EnsureUserHandle(ctx, admitted.account, revision, runtime.party, handle)
	}
	err = releaseMFAPreparation(guard, ctx, err)
	ticket.release()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	user, err := runtime.repository.UserSnapshot(ctx, admitted.account, runtime.party)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	id, err := accounts.NewChallengeID()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	instance, _, _, _ := runtime.party.Values()
	binding, err := authmfa.NewBinding(admitted.account, id, instance)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	now := h.coordinator.clock.Now()
	ceremony, err := runtime.verifier.BeginEnrollment(ctx, binding, user, now)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	prepared, err := runtime.repository.PrepareEnrollmentBegin(admitted.account, revision, policy, ceremony, user, admitted.authority, now)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	ticket, err = h.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard, err = h.coordinator.enter(ctx, admitted.account)
	if err != nil {
		ticket.release()
		return nil, authAPIError(ctx, err)
	}
	_, err = runtime.repository.BeginEnrollment(ctx, prepared)
	err = releaseMFAPreparation(guard, ctx, err)
	ticket.release()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	options, err := ceremony.EnrollmentOptions(user, now)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	expiry, _ := ceremony.ExpiresAt()
	idText, _ := id.Value()
	publicOptions, err := publicEnrollmentOptions(options)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := admitted.retainResponse(ctx); err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&api.BeginFactorEnrollmentResponse{Challenge: &api.EnrollmentChallenge{ChallengeId: idText, ExpireTime: timestamppb.New(expiry), Fido2: publicOptions}}), nil
}

func (h *authResourceHandler) CompleteFactorEnrollment(ctx context.Context, request *connect.Request[api.CompleteFactorEnrollmentRequest]) (*connect.Response[api.CompleteFactorEnrollmentResponse], error) {
	if request == nil || request.Msg == nil || proto.Size(request.Msg) > authmfa.MaxRequestBytes {
		return nil, authAPIError(ctx, errSessionUnauthenticated)
	}
	admitted, err := h.enrollmentAdmission(ctx, request.Header(), request.Msg.EnrollmentToken)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer func() { admitted.release() }()
	ctx = admitted.ctx
	runtime, err := h.mfaRuntime()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	permit, err := runtime.crypto.Acquire(ctx)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer permit.Release()
	parent, err := h.names.account(ctx, admitted.account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if parent.String() != request.Msg.Parent {
		return nil, authAPIError(ctx, authorization.ErrDenied)
	}
	if err := authShape(request.Msg); err != nil {
		return nil, authAPIError(ctx, err)
	}
	id, err := contracts.ParseChallengeID(request.Msg.ChallengeId)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(request.Msg, "challenge_id", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
	}
	snapshot, err := runtime.repository.EnrollmentSnapshot(ctx, id, admitted.authority, h.coordinator.clock.Now())
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := runtime.repository.ReserveEnrollmentProof(ctx, snapshot, h.coordinator.clock.Now()); err != nil {
		return nil, authAPIError(ctx, err)
	}
	user, ceremony, err := snapshot.Verification()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	factor, err := newFactorCredentialID()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	attestation, err := nativeFactorAttestation(request.Msg.Attestation)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	proof, err := runtime.verifier.VerifyEnrollment(ctx, user, ceremony, factor, attestation, h.coordinator.clock.Now())
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	batch, candidates, codes, err := authmfa.GenerateRecoveryBatch(admitted.account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	values, err := recoveryCodesWire(codes)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	persisted, err := storage.NewMFARecoveryBatch(admitted.account, batch, candidates)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	prepared, err := runtime.repository.PrepareEnrollmentCommit(proof, snapshot, persisted)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	accountsHandler := &accountHandler{store: h.store, coordinator: h.coordinator}
	guard, err := accountsHandler.accountMutationGuard(ctx, admitted.account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	activation, err := runtime.repository.CompleteEnrollment(ctx, prepared, h.coordinator.clock.Now())
	err = finishMFAMutation(guard, ctx, err)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	metadata, err := activation.Metadata()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	wire, err := h.names.factorResource(ctx, metadata)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := admitted.retainResponse(ctx); err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&api.CompleteFactorEnrollmentResponse{FactorCredential: wire, RecoveryCodes: values}), nil
}
