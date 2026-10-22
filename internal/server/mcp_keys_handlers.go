package server

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/mcpkeys"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type mcpKeyHandler struct {
	store       *storage.Store
	coordinator *sessionCoordinator
}

var _ authv1connect.McpKeyServiceHandler = (*mcpKeyHandler)(nil)

func mcpInvalid(field authv1.AuthField) error {
	result := connect.NewError(connect.CodeInvalidArgument, errors.New("invalid MCP key request"))
	detail := &authv1.AuthError{Code: authv1.AuthErrorCode_AUTH_ERROR_CODE_INVALID_ARGUMENT, Detail: &authv1.AuthError_InvalidField{InvalidField: field}}
	if wire, err := connect.NewErrorDetail(detail); err == nil {
		result.AddDetail(wire)
	}
	return result
}
func mcpKeyRPCError(err error) error {
	switch {
	case errors.Is(err, authorization.ErrDenied), errors.Is(err, storage.ErrSessionPermission):
		result := connect.NewError(connect.CodePermissionDenied, errors.New("MCP key permission denied"))
		detail := &authv1.AuthError{Code: authv1.AuthErrorCode_AUTH_ERROR_CODE_PERMISSION_DENIED, Detail: &authv1.AuthError_Permission{Permission: authv1.PermissionFailure_PERMISSION_FAILURE_SCOPE_DENIED}}
		if wire, e := connect.NewErrorDetail(detail); e == nil {
			result.AddDetail(wire)
		}
		return result
	case errors.Is(err, mcpkeys.ErrInvalid):
		return mcpInvalid(authv1.AuthField_AUTH_FIELD_MCP_KEY_ID)
	case errors.Is(err, storage.ErrMCPKeyNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("MCP key unavailable"))
	case errors.Is(err, authorization.ErrCapacity):
		return connect.NewError(connect.CodeResourceExhausted, errors.New("MCP key capacity exhausted"))
	default:
		return sessionRPCError(err)
	}
}
func (h *mcpKeyHandler) actor(ctx context.Context) (contracts.AccountID, error) {
	if h == nil || h.store == nil || h.coordinator == nil {
		return contracts.AccountID{}, authorization.ErrUnavailable
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || !principal.Native() {
		return contracts.AccountID{}, errSessionUnauthenticated
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return contracts.AccountID{}, errSessionUnauthenticated
	}
	account, err := principal.Account()
	if err != nil || account != actor {
		return contracts.AccountID{}, errSessionUnauthenticated
	}
	return actor, nil
}
func mcpKeyWire(r storage.MCPKeyRecord) (*authv1.McpKey, error) {
	id, e := r.Owner.Key.Value()
	if e != nil {
		return nil, e
	}
	account, e := r.Owner.Account.Value()
	if e != nil {
		return nil, e
	}
	client, e := r.Client.Value()
	if e != nil {
		return nil, e
	}
	result := &authv1.McpKey{Id: &authv1.McpKeyId{Value: id}, AccountId: &authv1.AccountId{Value: account}, ClientName: client, Audience: authv1.CredentialAudience_CREDENTIAL_AUDIENCE_MCP, CreatedAt: timestamppb.New(r.CreatedAt), ExpiresAt: timestamppb.New(r.ExpiresAt), Scopes: make([]*authv1.Scope, 0, len(r.Scopes))}
	for _, permission := range r.Scopes {
		family, operation, err := permission.Values()
		if err != nil {
			return nil, err
		}
		result.Scopes = append(result.Scopes, &authv1.Scope{Family: family, Operation: operation})
	}
	if r.RevokedAt != nil {
		result.RevokedAt = timestamppb.New(*r.RevokedAt)
	}
	if r.LastUsedAt != nil {
		result.LastUsedAt = timestamppb.New(*r.LastUsedAt)
	}
	return result, nil
}
func (h *mcpKeyHandler) CreateMcpKey(ctx context.Context, request *connect.Request[authv1.CreateMcpKeyRequest]) (*connect.Response[authv1.CreateMcpKeyResponse], error) {
	actor, err := h.actor(ctx)
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	if request == nil || request.Msg == nil {
		return nil, mcpInvalid(authv1.AuthField_AUTH_FIELD_CLIENT_NAME)
	}
	if _, err := contracts.AdmitMcpKeyLifetime(request.Msg.Lifetime); err != nil {
		return nil, mcpInvalid(authv1.AuthField_AUTH_FIELD_LIFETIME)
	}
	if err := contracts.AdmitMcpKeyCreateRequest(request.Msg); err != nil {
		return nil, mcpInvalid(authv1.AuthField_AUTH_FIELD_SCOPES)
	}
	client, err := mcpkeys.ParseClientName(request.Msg.ClientName)
	if err != nil {
		return nil, mcpInvalid(authv1.AuthField_AUTH_FIELD_CLIENT_NAME)
	}
	lifetime, _ := contracts.AdmitMcpKeyLifetime(request.Msg.Lifetime)
	permissions := make([]authorization.Permission, 0, len(request.Msg.Scopes))
	for _, scope := range request.Msg.Scopes {
		permission, err := authorization.NewPermission(scope.Family, scope.Operation)
		if err != nil {
			return nil, mcpInvalid(authv1.AuthField_AUTH_FIELD_SCOPES)
		}
		permissions = append(permissions, permission)
	}
	id, err := mcpkeys.NewID()
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	secret, err := mcpkeys.NewSecret()
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	digest, err := secret.Digest()
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	guard, err := h.coordinator.enter(ctx, actor)
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	defer guard.release()
	record, err := h.store.CreateMCPKey(ctx, storage.MCPKeyCreation{ID: id, Account: actor, Client: client, Digest: digest, Scopes: permissions, Lifetime: lifetime, Now: h.coordinator.clock.Now()})
	if errors.Is(err, storage.ErrSessionCommitUnknown) {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	wire, err := mcpKeyWire(record)
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	text, err := secret.Text()
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	return connect.NewResponse(&authv1.CreateMcpKeyResponse{Key: wire, Secret: text}), nil
}
func (h *mcpKeyHandler) ListMcpKeys(ctx context.Context, request *connect.Request[authv1.ListMcpKeysRequest]) (*connect.Response[authv1.ListMcpKeysResponse], error) {
	actor, err := h.actor(ctx)
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	if request == nil || request.Msg == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, mcpInvalid(authv1.AuthField_AUTH_FIELD_ACCOUNT_ID)
	}
	target := actor
	if request.Msg.AccountId != nil {
		if len(request.Msg.AccountId.ProtoReflect().GetUnknown()) != 0 {
			return nil, mcpInvalid(authv1.AuthField_AUTH_FIELD_ACCOUNT_ID)
		}
		target, err = contracts.ParseAccountID(request.Msg.AccountId.Value)
		if err != nil {
			return nil, mcpInvalid(authv1.AuthField_AUTH_FIELD_ACCOUNT_ID)
		}
	}
	records, err := h.store.ListMCPKeys(ctx, actor, target)
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	result := &authv1.ListMcpKeysResponse{Keys: make([]*authv1.McpKey, 0, len(records))}
	for _, record := range records {
		wire, err := mcpKeyWire(record)
		if err != nil {
			return nil, mcpKeyRPCError(err)
		}
		result.Keys = append(result.Keys, wire)
	}
	return connect.NewResponse(result), nil
}
func (h *mcpKeyHandler) RevokeMcpKey(ctx context.Context, request *connect.Request[authv1.RevokeMcpKeyRequest]) (*connect.Response[authv1.RevokeMcpKeyResponse], error) {
	actor, err := h.actor(ctx)
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	if request == nil || request.Msg == nil || request.Msg.KeyId == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 || len(request.Msg.KeyId.ProtoReflect().GetUnknown()) != 0 {
		return nil, mcpInvalid(authv1.AuthField_AUTH_FIELD_MCP_KEY_ID)
	}
	id, err := contracts.ParseMcpKeyID(request.Msg.KeyId.Value)
	if err != nil {
		return nil, mcpInvalid(authv1.AuthField_AUTH_FIELD_MCP_KEY_ID)
	}
	owner, err := h.store.MCPKeyOwner(ctx, id)
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	guard, err := h.coordinator.enter(ctx, owner.Account)
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	defer guard.release()
	err = h.store.RevokeMCPKey(ctx, actor, owner, h.coordinator.clock.Now())
	if errors.Is(err, storage.ErrSessionCommitUnknown) {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, mcpKeyRPCError(err)
	}
	guard.cancelMCPLeases(id)
	return connect.NewResponse(&authv1.RevokeMcpKeyResponse{}), nil
}
func registerMCPKeyRoutes(plan *PublicPlan, store *storage.Store, coordinator *sessionCoordinator) error {
	handler := &mcpKeyHandler{store: store, coordinator: coordinator}
	return plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return authv1connect.NewMcpKeyServiceHandler(handler, options...)
	}, ConnectProcedure{Path: authv1connect.McpKeyServiceCreateMcpKeyProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired}, ConnectProcedure{Path: authv1connect.McpKeyServiceListMcpKeysProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired}, ConnectProcedure{Path: authv1connect.McpKeyServiceRevokeMcpKeyProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
}
