package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type authorizationPolicyHandler struct {
	store       *storage.Store
	coordinator *sessionCoordinator
}

var _ authv1connect.AuthorizationPolicyServiceHandler = (*authorizationPolicyHandler)(nil)

func policyRPCError(code authv1.AuthorizationPolicyErrorCode, path string) error {
	detail := &authv1.AuthorizationPolicyError{Code: code, FieldPath: path}
	connectCode, err := contracts.AuthorizationPolicyErrorConnectCode(detail)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("authorization service unavailable"))
	}
	result := connect.NewError(connectCode, errors.New("authorization policy request rejected"))
	if wire, err := connect.NewErrorDetail(detail); err == nil {
		result.AddDetail(wire)
	}
	return result
}
func policyStorageError(err error) error {
	switch {
	case errors.Is(err, authorization.ErrConflict):
		return policyRPCError(authv1.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_VERSION_CONFLICT, "")
	case errors.Is(err, storage.ErrAccountPermission), errors.Is(err, authorization.ErrDenied):
		return policyRPCError(authv1.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_SCOPE_DENIED, "")
	case errors.Is(err, storage.ErrAccountNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("account not found"))
	default:
		return policyRPCError(authv1.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_AUTHORITY_UNAVAILABLE, "")
	}
}
func (h *authorizationPolicyHandler) admit(ctx context.Context, operation authv1.ScopeOperation) (contracts.AccountID, error) {
	if h == nil || h.store == nil || h.coordinator == nil {
		return contracts.AccountID{}, authorization.ErrUnavailable
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return contracts.AccountID{}, authorization.ErrDenied
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return contracts.AccountID{}, authorization.ErrDenied
	}
	account, err := principal.Account()
	if err != nil || account != actor {
		return contracts.AccountID{}, authorization.ErrDenied
	}
	permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS, operation)
	if err != nil {
		return contracts.AccountID{}, err
	}
	object, err := authorization.AdministrativeObject(authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS)
	if err != nil {
		return contracts.AccountID{}, err
	}
	current, err := h.coordinator.authority.Current(principal)
	if err != nil {
		return contracts.AccountID{}, err
	}
	if err := current.Authorize(permission, object); err != nil {
		return contracts.AccountID{}, err
	}
	return account, nil
}
func authorizationPolicyWire(snapshot storage.AuthorizationPolicySnapshot) (*authv1.PolicySnapshot, error) {
	source, err := snapshot.Source()
	if err != nil {
		return nil, err
	}
	account, err := source.Account()
	if err != nil {
		return nil, err
	}
	id, err := account.Value()
	if err != nil {
		return nil, err
	}
	data, err := source.Bytes()
	if err != nil {
		return nil, err
	}
	version, err := snapshot.Version()
	if err != nil {
		return nil, err
	}
	revision, hash, err := version.Values()
	if err != nil {
		return nil, err
	}
	updated, err := snapshot.UpdatedAt()
	if err != nil {
		return nil, err
	}
	return &authv1.PolicySnapshot{AccountId: &authv1.AccountId{Value: id}, YamlSource: data, Schema: authorization.Schema, SyntaxProfile: authorization.SyntaxProfile, Version: &authv1.PolicyVersion{Revision: revision, SourceSha256: append([]byte(nil), hash[:]...)}, UpdatedAt: timestamppb.New(updated)}, nil
}
func (h *authorizationPolicyHandler) GetPolicy(ctx context.Context, request *connect.Request[authv1.GetPolicyRequest]) (*connect.Response[authv1.GetPolicyResponse], error) {
	actor, err := h.admit(ctx, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, policyStorageError(err)
	}
	if request == nil || contracts.AdmitGetPolicyRequest(request.Msg) != nil {
		return nil, policyRPCError(authv1.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_VERSION, "account_id")
	}
	target, err := admittedAccountID(request.Msg.AccountId)
	if err != nil {
		return nil, policyRPCError(authv1.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_VERSION, "account_id")
	}
	snapshot, err := h.store.GetAuthorizationPolicy(ctx, actor, target)
	if errors.Is(err, storage.ErrPolicyNotConfigured) {
		return connect.NewResponse(&authv1.GetPolicyResponse{Result: &authv1.GetPolicyResponse_Absent{Absent: &emptypb.Empty{}}}), nil
	}
	if err != nil {
		return nil, policyStorageError(err)
	}
	wire, err := authorizationPolicyWire(snapshot)
	if err != nil {
		return nil, policyStorageError(err)
	}
	return connect.NewResponse(&authv1.GetPolicyResponse{Result: &authv1.GetPolicyResponse_Present{Present: wire}}), nil
}
func (h *authorizationPolicyHandler) ReplacePolicy(ctx context.Context, request *connect.Request[authv1.ReplacePolicyRequest]) (*connect.Response[authv1.ReplacePolicyResponse], error) {
	actor, err := h.admit(ctx, authv1.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, policyStorageError(err)
	}
	if request == nil || request.Msg == nil {
		return nil, policyRPCError(authv1.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_VERSION, "precondition")
	}
	target, err := admittedAccountID(request.Msg.AccountId)
	if err != nil {
		return nil, policyRPCError(authv1.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_VERSION, "account_id")
	}
	source, err := authorization.AdmitPolicySource(target, request.Msg.YamlSource)
	if err != nil {
		path := "yaml_source"
		var diagnostic *authorization.Diagnostic
		if errors.As(err, &diagnostic) && diagnostic.Path() != "" && diagnostic.Path() != "source" {
			path = diagnostic.Path()
		}
		return nil, policyRPCError(authv1.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_SOURCE, path)
	}
	// Source admission owns UTF-8, size and YAML diagnostics. The remaining
	// wire/CAS structure is checked separately before any mutation admission.
	if contracts.AdmitReplacePolicyRequest(request.Msg) != nil {
		return nil, policyRPCError(authv1.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_VERSION, "precondition")
	}
	expected := authorization.ExpectAbsent()
	if v := request.Msg.GetExpected(); v != nil {
		var hash [sha256.Size]byte
		copy(hash[:], v.SourceSha256)
		version, err := authorization.NewVersion(v.Revision, hash)
		if err != nil {
			return nil, policyRPCError(authv1.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_VERSION, "precondition.expected")
		}
		expected, err = authorization.ExpectVersion(version)
		if err != nil {
			return nil, policyRPCError(authv1.AuthorizationPolicyErrorCode_AUTHORIZATION_POLICY_ERROR_CODE_INVALID_VERSION, "precondition.expected")
		}
	}
	guard, err := (&accountHandler{store: h.store, coordinator: h.coordinator}).accountMutationGuard(ctx, target)
	if err != nil {
		return nil, policyStorageError(err)
	}
	if err := guard.mutation.ReservePolicy(source); err != nil {
		guard.release()
		return nil, policyStorageError(err)
	}
	outcome, err := h.store.ReplaceAuthorizationPolicy(ctx, actor, target, source, expected)
	var snapshot storage.AuthorizationPolicySnapshot
	var changed bool
	if err == nil {
		snapshot, changed, err = outcome.Result()
		if err != nil {
			guard.quarantine()
			guard.cancelLeases(ctx, nil)
			guard.release()
			return nil, policyStorageError(err)
		}
	}
	// A policy change invalidates grants and affected permits, while the native
	// session itself remains usable under the account's new current policy.
	err = finishAccountMutation(guard, ctx, err, false)
	if err != nil {
		return nil, policyStorageError(err)
	}
	wire, err := authorizationPolicyWire(snapshot)
	if err != nil {
		return nil, policyStorageError(err)
	}
	rights := authv1.PolicyRightsChange_POLICY_RIGHTS_CHANGE_UNCHANGED
	if changed {
		rights = authv1.PolicyRightsChange_POLICY_RIGHTS_CHANGE_CHANGED
	}
	return connect.NewResponse(&authv1.ReplacePolicyResponse{Policy: wire, RightsChange: rights}), nil
}
func registerAuthorizationPolicyRoutes(plan *PublicPlan, store *storage.Store, coordinator *sessionCoordinator) error {
	if plan == nil || store == nil || coordinator == nil {
		return ErrInvalidPublicPlan
	}
	h := &authorizationPolicyHandler{store: store, coordinator: coordinator}
	return plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return authv1connect.NewAuthorizationPolicyServiceHandler(h, options...)
	},
		ConnectProcedure{Path: authv1connect.AuthorizationPolicyServiceGetPolicyProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: authv1connect.AuthorizationPolicyServiceReplacePolicyProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
}
