package server

import (
	"connectrpc.com/connect"
	"context"
	"errors"
	"github.com/damione1/Homeward/contracts"
	cfg "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/gen/home/integrationadmin/v1/integrationadminv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/integrationadmin"
	"google.golang.org/protobuf/proto"
)

type integrationAdminHandler struct {
	service   *integrationadmin.Service
	documents *configdocuments.Service
}

var _ integrationadminv1connect.IntegrationServiceHandler = (*integrationAdminHandler)(nil)

func integrationAdminRPCError(err error, current *cfg.DocumentVersion) error {
	code, status := wire.ErrorCode_ERROR_CODE_UNAVAILABLE, connect.CodeUnavailable
	var document *configdocuments.Error
	switch {
	case errors.Is(err, contracts.ErrInvalidIntegrationAdminContract), errors.Is(err, integrationadmin.ErrInvalid):
		code, status = wire.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, connect.CodeInvalidArgument
	case errors.Is(err, authorization.ErrDenied):
		code, status = wire.ErrorCode_ERROR_CODE_DENIED, connect.CodePermissionDenied
	case errors.Is(err, integrationadmin.ErrUnknownType):
		code, status = wire.ErrorCode_ERROR_CODE_UNKNOWN_TYPE, connect.CodeNotFound
	case errors.Is(err, integrationadmin.ErrNotFound):
		code, status = wire.ErrorCode_ERROR_CODE_NOT_FOUND, connect.CodeNotFound
	case errors.Is(err, integrationadmin.ErrAlreadyExists):
		code, status = wire.ErrorCode_ERROR_CODE_ALREADY_EXISTS, connect.CodeAlreadyExists
	case errors.Is(err, integrationadmin.ErrWorkspaceInvalid):
		code, status = wire.ErrorCode_ERROR_CODE_WORKSPACE_INVALID, connect.CodeFailedPrecondition
	case errors.Is(err, configactivation.ErrWorkspacePreconditionConflict), errors.Is(err, config.ErrStartupSourceChanged):
		code, status = wire.ErrorCode_ERROR_CODE_CONFLICT, connect.CodeAborted
	case errors.As(err, &document):
		switch document.Kind() {
		case configdocuments.Conflict:
			code, status = wire.ErrorCode_ERROR_CODE_CONFLICT, connect.CodeAborted
		case configdocuments.FailureInvalid:
			code, status = wire.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, connect.CodeInvalidArgument
		case configdocuments.ReconciliationRequired, configdocuments.IncompleteHistory:
			code, status = wire.ErrorCode_ERROR_CODE_WORKSPACE_INVALID, connect.CodeFailedPrecondition
		case configdocuments.Unavailable:
		}
	}
	result := connect.NewError(status, errors.New("integration administration request rejected"))
	detail := &wire.IntegrationError{Code: code}
	if code == wire.ErrorCode_ERROR_CODE_CONFLICT {
		detail.Current = current
	}
	if data, detailErr := connect.NewErrorDetail(detail); detailErr == nil {
		result.AddDetail(data)
	}
	return result
}
func integrationAdminResponse[T any](message *T) (*connect.Response[T], error) {
	value, ok := any(message).(proto.Message)
	if !ok || contracts.AdmitIntegrationAdminResponse(value) != nil {
		return nil, integrationAdminRPCError(integrationadmin.ErrUnavailable, nil)
	}
	return connect.NewResponse(message), nil
}
func (h *integrationAdminHandler) admit(ctx context.Context, message proto.Message) error {
	if h == nil || h.service == nil || h.documents == nil {
		return integrationAdminRPCError(integrationadmin.ErrUnavailable, nil)
	}
	if contracts.AdmitIntegrationAdminRequest(message) != nil {
		return integrationAdminRPCError(contracts.ErrInvalidIntegrationAdminContract, nil)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return integrationAdminRPCError(authorization.ErrDenied, nil)
	}
	actor, present := ActorFromContext(ctx)
	admitted, err := principal.Actor()
	if err != nil || !present || actor != admitted {
		return integrationAdminRPCError(authorization.ErrDenied, nil)
	}
	return nil
}
func (h *integrationAdminHandler) snapshot(snapshot configdocuments.Snapshot) *cfg.ReadDocumentResponse {
	return (&configDocumentsHandler{service: h.documents}).snapshotWire(snapshot)
}
func (h *integrationAdminHandler) mutation(result integrationadmin.MutationResult, err error) (*cfg.ActivationResult, *cfg.ReadDocumentResponse, error) {
	var current *cfg.ReadDocumentResponse
	var version *cfg.DocumentVersion
	if result.HasSnapshot {
		current = h.snapshot(result.Snapshot)
		version = current.Document.Version
	}
	if errors.Is(err, authorization.ErrDenied) || result.Activation.Outcome() == configactivation.OutcomeInvalid {
		if err == nil {
			err = integrationadmin.ErrUnavailable
		}
		return nil, nil, integrationAdminRPCError(err, version)
	}
	activation := configActivationResult(result.Activation, err)
	if activation == nil {
		return nil, nil, integrationAdminRPCError(integrationadmin.ErrUnavailable, nil)
	}
	return activation, current, nil
}
func integrationAdminExpected(p *wire.MutationPrecondition) (configdocuments.ExpectedDocument, error) {
	return configExpected(&cfg.DocumentId{Value: "operational.main"}, p.Expected, p.ExpectedWorkspaceSha256)
}
func (h *integrationAdminHandler) ListIntegrationTypes(ctx context.Context, r *connect.Request[wire.ListIntegrationTypesRequest]) (*connect.Response[wire.ListIntegrationTypesResponse], error) {
	if r == nil {
		return nil, integrationAdminRPCError(integrationadmin.ErrInvalid, nil)
	}
	if err := h.admit(ctx, r.Msg); err != nil {
		return nil, err
	}
	types, err := h.service.Types(ctx)
	if err != nil {
		return nil, integrationAdminRPCError(err, nil)
	}
	return integrationAdminResponse(&wire.ListIntegrationTypesResponse{Types: types})
}
func (h *integrationAdminHandler) GetIntegrationType(ctx context.Context, r *connect.Request[wire.GetIntegrationTypeRequest]) (*connect.Response[wire.GetIntegrationTypeResponse], error) {
	if r == nil {
		return nil, integrationAdminRPCError(integrationadmin.ErrInvalid, nil)
	}
	if err := h.admit(ctx, r.Msg); err != nil {
		return nil, err
	}
	typ, err := h.service.Type(ctx, r.Msg.TypeId)
	if err != nil {
		return nil, integrationAdminRPCError(err, nil)
	}
	return integrationAdminResponse(&wire.GetIntegrationTypeResponse{Type: typ})
}
func (h *integrationAdminHandler) ListInstances(ctx context.Context, r *connect.Request[wire.ListInstancesRequest]) (*connect.Response[wire.ListInstancesResponse], error) {
	if r == nil {
		return nil, integrationAdminRPCError(integrationadmin.ErrInvalid, nil)
	}
	if err := h.admit(ctx, r.Msg); err != nil {
		return nil, err
	}
	instances, snapshot, err := h.service.Instances(ctx)
	if err != nil {
		return nil, integrationAdminRPCError(err, nil)
	}
	return integrationAdminResponse(&wire.ListInstancesResponse{Instances: instances, Document: h.snapshot(snapshot)})
}
func (h *integrationAdminHandler) GetInstance(ctx context.Context, r *connect.Request[wire.GetInstanceRequest]) (*connect.Response[wire.GetInstanceResponse], error) {
	if r == nil {
		return nil, integrationAdminRPCError(integrationadmin.ErrInvalid, nil)
	}
	if err := h.admit(ctx, r.Msg); err != nil {
		return nil, err
	}
	instance, snapshot, err := h.service.Instance(ctx, r.Msg.IntegrationInstanceId)
	if err != nil {
		return nil, integrationAdminRPCError(err, nil)
	}
	return integrationAdminResponse(&wire.GetInstanceResponse{Instance: instance, Document: h.snapshot(snapshot)})
}
func (h *integrationAdminHandler) CreateInstance(ctx context.Context, r *connect.Request[wire.CreateInstanceRequest]) (*connect.Response[wire.CreateInstanceResponse], error) {
	if r == nil {
		return nil, integrationAdminRPCError(integrationadmin.ErrInvalid, nil)
	}
	if err := h.admit(ctx, r.Msg); err != nil {
		return nil, err
	}
	expected, err := integrationAdminExpected(r.Msg.Precondition)
	if err != nil {
		return nil, integrationAdminRPCError(err, nil)
	}
	result, callErr := h.service.Create(ctx, r.Msg.TypeId, r.Msg.IntegrationInstanceId, r.Msg.Configuration, r.Msg.DesiredState, expected)
	activation, current, err := h.mutation(result, callErr)
	if err != nil {
		return nil, err
	}
	return integrationAdminResponse(&wire.CreateInstanceResponse{Activation: activation, Document: current})
}
func (h *integrationAdminHandler) UpdateInstance(ctx context.Context, r *connect.Request[wire.UpdateInstanceRequest]) (*connect.Response[wire.UpdateInstanceResponse], error) {
	if r == nil {
		return nil, integrationAdminRPCError(integrationadmin.ErrInvalid, nil)
	}
	if err := h.admit(ctx, r.Msg); err != nil {
		return nil, err
	}
	expected, err := integrationAdminExpected(r.Msg.Precondition)
	if err != nil {
		return nil, integrationAdminRPCError(err, nil)
	}
	result, callErr := h.service.Update(ctx, r.Msg.TypeId, r.Msg.IntegrationInstanceId, r.Msg.Configuration, expected)
	activation, current, err := h.mutation(result, callErr)
	if err != nil {
		return nil, err
	}
	return integrationAdminResponse(&wire.UpdateInstanceResponse{Activation: activation, Document: current})
}
func (h *integrationAdminHandler) DeleteInstance(ctx context.Context, r *connect.Request[wire.DeleteInstanceRequest]) (*connect.Response[wire.DeleteInstanceResponse], error) {
	if r == nil {
		return nil, integrationAdminRPCError(integrationadmin.ErrInvalid, nil)
	}
	if err := h.admit(ctx, r.Msg); err != nil {
		return nil, err
	}
	expected, err := integrationAdminExpected(r.Msg.Precondition)
	if err != nil {
		return nil, integrationAdminRPCError(err, nil)
	}
	result, callErr := h.service.Delete(ctx, r.Msg.IntegrationInstanceId, expected)
	activation, current, err := h.mutation(result, callErr)
	if err != nil {
		return nil, err
	}
	return integrationAdminResponse(&wire.DeleteInstanceResponse{Activation: activation, Document: current})
}
func (h *integrationAdminHandler) SetInstanceEnabled(ctx context.Context, r *connect.Request[wire.SetInstanceEnabledRequest]) (*connect.Response[wire.SetInstanceEnabledResponse], error) {
	if r == nil {
		return nil, integrationAdminRPCError(integrationadmin.ErrInvalid, nil)
	}
	if err := h.admit(ctx, r.Msg); err != nil {
		return nil, err
	}
	expected, err := integrationAdminExpected(r.Msg.Precondition)
	if err != nil {
		return nil, integrationAdminRPCError(err, nil)
	}
	result, callErr := h.service.SetEnabled(ctx, r.Msg.IntegrationInstanceId, r.Msg.DesiredState, expected)
	activation, current, err := h.mutation(result, callErr)
	if err != nil {
		return nil, err
	}
	return integrationAdminResponse(&wire.SetInstanceEnabledResponse{Activation: activation, Document: current})
}
