package server

import (
	"context"
	"crypto/cipher"
	"net/http"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	boundary "github.com/damione1/Homeward/contracts/apiv2"
	"github.com/damione1/Homeward/gen/google/longrunning/longrunningpbconnect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"google.golang.org/protobuf/types/known/emptypb"
)

// The native owners provide reads. This interface has no execution method and
// cannot turn polling, cancellation or a lost connection into a redispatch.
type operationReadProvider interface {
	GetOperation(context.Context, apitypes.OperationName) (*longrunningpb.Operation, error)
}
type operationWaitProvider interface {
	WaitOperation(context.Context, *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error)
}
type operationCancelProvider interface {
	CancelOperation(context.Context, apitypes.OperationName) error
}
type operationDeleteProvider interface {
	DeleteOperation(context.Context, apitypes.OperationName) error
}
type operationRPCHandler struct {
	physical, configuration operationReadProvider
	authority               *authorization.Authority
	pages                   cipher.AEAD
}

func registerOperationRoutes(plan *PublicPlan, physical, configuration operationReadProvider, authority *authorization.Authority) error {
	if plan == nil || nilReactiveInterface(physical) && nilReactiveInterface(configuration) {
		return ErrInvalidPublicPlan
	}
	handler, err := newOperationRPCHandler(physical, configuration, authority)
	if err != nil {
		return err
	}
	paths := []string{
		longrunningpbconnect.OperationsGetOperationProcedure,
		longrunningpbconnect.OperationsListOperationsProcedure,
		longrunningpbconnect.OperationsWaitOperationProcedure,
		longrunningpbconnect.OperationsCancelOperationProcedure,
		longrunningpbconnect.OperationsDeleteOperationProcedure,
	}
	procedures := make([]ConnectProcedure, 0, len(paths))
	for _, path := range paths {
		procedures = append(procedures, ConnectProcedure{Path: path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	return plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return longrunningpbconnect.NewOperationsHandler(handler, append(options, connect.WithSendMaxBytes(maxOperationListBytes))...)
	}, procedures...)
}

func (h *operationRPCHandler) provider(name apitypes.OperationName) (operationReadProvider, error) {
	if h == nil {
		return nil, authorization.ErrUnavailable
	}
	var provider operationReadProvider
	switch {
	case strings.HasPrefix(name.ID(), "cmd-"):
		provider = h.physical
	case strings.HasPrefix(name.ID(), "config-"):
		provider = h.configuration
	default:
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	}
	if nilReactiveInterface(provider) {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_METHOD_NOT_SUPPORTED)
	}
	return provider, nil
}

func operationName(ctx context.Context, text string) (apitypes.OperationName, error) {
	if _, err := coreRPCActor(ctx); err != nil {
		return apitypes.OperationName{}, err
	}
	if _, err := authorization.PrincipalFromContext(ctx); err != nil {
		return apitypes.OperationName{}, authorization.ErrDenied
	}
	name, err := apitypes.ParseOperationName(text)
	if err != nil {
		return name, homeValidation(&longrunningpb.GetOperationRequest{}, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	return name, nil
}
func (h *operationRPCHandler) read(ctx context.Context, name apitypes.OperationName) (*longrunningpb.Operation, error) {
	provider, err := h.provider(name)
	if err != nil {
		return nil, err
	}
	value, err := provider.GetOperation(ctx, name)
	if err != nil {
		return nil, err
	}
	if boundary.AdmitOperationEnvelope(value, name) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	return value, nil
}
func (h *operationRPCHandler) GetOperation(ctx context.Context, request *connect.Request[longrunningpb.GetOperationRequest]) (*connect.Response[longrunningpb.Operation], error) {
	text := ""
	if request != nil && request.Msg != nil {
		text = request.Msg.Name
	}
	name, err := operationName(ctx, text)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	value, err := h.read(ctx, name)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(value), nil
}
func (h *operationRPCHandler) WaitOperation(ctx context.Context, request *connect.Request[longrunningpb.WaitOperationRequest]) (*connect.Response[longrunningpb.Operation], error) {
	text := ""
	if request != nil && request.Msg != nil {
		text = request.Msg.Name
	}
	name, err := operationName(ctx, text)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	// Object authorization precedes timeout diagnostics. A coarse MCP ACTIONS
	// ceiling cannot disclose validation for an unreadable historical slot.
	value, err := h.read(ctx, name)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request.Msg.Timeout != nil && (request.Msg.Timeout.CheckValid() != nil || request.Msg.Timeout.AsDuration() < 0) {
		return nil, apiRPCError(ctx, homeValidation(request.Msg, "timeout", api.ValidationReason_VALIDATION_REASON_OUT_OF_RANGE))
	}
	provider, err := h.provider(name)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if waiter, ok := provider.(operationWaitProvider); ok {
		value, err = waiter.WaitOperation(ctx, request.Msg)
	}
	// Standard WaitOperation is best effort; without a native waiter, the
	// authorized preflight read above is the current result, without redispatch.
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if boundary.AdmitOperationEnvelope(value, name) != nil {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY))
	}
	return connect.NewResponse(value), nil
}
func (h *operationRPCHandler) CancelOperation(ctx context.Context, request *connect.Request[longrunningpb.CancelOperationRequest]) (*connect.Response[emptypb.Empty], error) {
	text := ""
	if request != nil && request.Msg != nil {
		text = request.Msg.Name
	}
	name, err := operationName(ctx, text)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if _, err := h.read(ctx, name); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	provider, _ := h.provider(name)
	canceller, ok := provider.(operationCancelProvider)
	if !ok {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_METHOD_NOT_SUPPORTED))
	}
	if err := canceller.CancelOperation(ctx, name); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}
func (h *operationRPCHandler) DeleteOperation(ctx context.Context, request *connect.Request[longrunningpb.DeleteOperationRequest]) (*connect.Response[emptypb.Empty], error) {
	text := ""
	if request != nil && request.Msg != nil {
		text = request.Msg.Name
	}
	name, err := operationName(ctx, text)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if _, err := h.read(ctx, name); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	provider, _ := h.provider(name)
	deleter, ok := provider.(operationDeleteProvider)
	if !ok {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_METHOD_NOT_SUPPORTED))
	}
	if err := deleter.DeleteOperation(ctx, name); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}
