package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	boundary "github.com/damione1/Homeward/contracts/apiv2"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

func registerCapabilityRoutes(plan *PublicPlan, handler *coreRPCHandler) error {
	if plan == nil || handler == nil || handler.home == nil || handler.commands == nil {
		return ErrInvalidPublicPlan
	}
	path := apiv2connect.CapabilityServiceExecuteCapabilityProcedure
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewCapabilityServiceHandler(handler, options...)
	}, ConnectProcedure{Path: path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired}); err != nil {
		return err
	}
	permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_WRITE)
	return plan.BindPermission(RouteKindConnect, RouteMethodPost, path, permission)
}

// Public UUIDs never replace native command IDs. A supplied key maps to one
// native occurrence within its actor; an absent key gets a fresh occurrence.
func publicCommandID(actor domain.ActorID, key *apitypes.RequestID) (domain.CommandID, error) {
	var id [16]byte
	if key == nil {
		if _, err := rand.Read(id[:]); err != nil {
			return domain.CommandID{}, err
		}
	} else {
		owner, err := actor.Value()
		if err != nil || !key.Valid() {
			return domain.CommandID{}, authorization.ErrDenied
		}
		digest := sha256.Sum256([]byte("homeward/api-command/1\x00" + owner + "\x00" + key.String()))
		copy(id[:], digest[:16])
	}
	return domain.ParseCommandID(hex.EncodeToString(id[:]))
}

func (h *coreRPCHandler) ExecuteCapability(ctx context.Context, request *connect.Request[api.ExecuteCapabilityRequest]) (*connect.Response[longrunningpb.Operation], error) {
	// Detach before any diagnostics, cloning or durable fingerprint construction.
	var code []byte
	if request != nil && request.Msg != nil {
		code = request.Msg.EphemeralAlarmCode
		request.Msg.EphemeralAlarmCode = nil
		defer clear(code)
	}
	if h == nil || h.home == nil || h.commands == nil {
		return nil, apiRPCError(ctx, authorization.ErrUnavailable)
	}
	permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_WRITE)
	_, current, err := h.home.current(ctx, &permission)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, apiRPCError(ctx, homeValidation(&api.ExecuteCapabilityRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	// Resolve and authorize the target before admitting the command's business
	// payload. Snapshot projection already excludes unreadable native slots.
	name, err := apitypes.ParseCapabilityName(request.Msg.Name)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(request.Msg, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	page, err := h.home.entityPage(ctx, name.Entity(), current)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, err
	}
	defer h.views.ReleaseView(context.WithoutCancel(ctx), actor, page.SnapshotToken)
	capabilities, err := preparedCapabilities(page)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	var selected apiresources.Capability
	found := false
	for _, capability := range capabilities {
		if capability.Resource().Name == name.String() {
			selected, found = capability, true
			break
		}
	}
	if !found {
		return nil, apiRPCError(ctx, authorization.ErrDenied)
	}
	entity, err := h.home.owner.ResolveEntityName(ctx, name.Entity())
	if err != nil {
		return nil, apiRPCError(ctx, authorization.ErrDenied)
	}
	object, err := authorization.EntityObject(entity, selected.Source())
	if err != nil || current.Authorize(permission, object) != nil {
		return nil, apiRPCError(ctx, authorization.ErrDenied)
	}
	readPermission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if current.Authorize(readPermission, object) != nil {
		return nil, apiRPCError(ctx, authorization.ErrDenied)
	}
	if len(code) > 32 || proto.Size(request.Msg) > 64*1024 {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_REQUEST_TOO_LARGE))
	}
	command, err := boundary.ParseCommand(request.Msg)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	var key *apitypes.RequestID
	if value, present := command.RequestID(); present {
		key = &value
	}
	id, err := publicCommandID(actor, key)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	idText, _ := id.Value()
	operationName, err := apitypes.ParseOperationName(name.Entity().Home().String() + "/operations/cmd-" + idText)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	canonical := proto.Clone(request.Msg).(*api.ExecuteCapabilityRequest)
	if key != nil {
		canonical.RequestId = key.String()
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(canonical)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	metadata, err := storage.NewCommandAPIMetadata(name, key, sha256.Sum256(encoded))
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if key != nil {
		found, err := h.commands.RecordedAPICommand(ctx, id, metadata)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		if found {
			operation, err := h.GetOperation(ctx, operationName)
			if err != nil {
				return nil, apiRPCError(ctx, err)
			}
			return connect.NewResponse(operation), nil
		}
	}
	if command.DefinitionETag() != "" && command.DefinitionETag() != selected.Resource().Etag {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH))
	}
	args, err := apiresources.NativeArguments(selected, command.Intent())
	if err != nil {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CAPABILITY_UNAVAILABLE))
	}
	requiresCode := false
	if definition := selected.Resource().GetAlarm().GetDefinition(); definition != nil && request.Msg.GetAlarm() != nil {
		for _, operation := range definition.CodeRequiredOperations {
			if operation == request.Msg.GetAlarm().Operation {
				requiresCode = true
			}
		}
	}
	if requiresCode != (len(code) != 0) {
		return nil, apiRPCError(ctx, homeValidation(request.Msg, "ephemeral_alarm_code", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	for _, digit := range code {
		if digit < '0' || digit > '9' {
			return nil, apiRPCError(ctx, homeValidation(request.Msg, "ephemeral_alarm_code", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
		}
	}
	now := time.Now()
	if !command.ExpireTime().After(now) || command.ExpireTime().After(now.Add(maximumOperationLease)) {
		return nil, apiRPCError(ctx, homeValidation(request.Msg, "expire_time", api.ValidationReason_VALIDATION_REASON_INVALID_DEADLINE))
	}
	continuation, release, err := retainOperationLease(ctx, command.ExpireTime())
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	epoch, err := h.epoch.CurrentAdmissionEpoch()
	if err != nil {
		release()
		return nil, apiRPCError(ctx, err)
	}
	_, stamp, _, _, err := page.Cursor.Components()
	if err != nil {
		release()
		return nil, apiRPCError(ctx, err)
	}
	revision, _, err := stamp.Components()
	if err != nil {
		release()
		return nil, apiRPCError(ctx, err)
	}
	entityText, _ := entity.Value()
	schema, endpoint, err := selected.Source().Values()
	if err != nil {
		release()
		return nil, apiRPCError(ctx, err)
	}
	native := &actions.CommandRequest{CommandId: idText, AdmissionEpoch: epoch, ConfigRevision: revision, Target: &registrywire.EntityRef{Reference: &registrywire.EntityRef_EntityId{EntityId: entityText}}, CapabilitySchemaId: schema, Endpoint: endpoint, Arguments: args, ExpiresAt: request.Msg.ExpireTime}
	if requiresCode {
		var dispose func()
		continuation, dispose, err = integrations.WithEphemeralAlarmCode(continuation, id, actor, entity, args.GetAlarm().GetOperation(), code)
		if err != nil {
			release()
			return nil, apiRPCError(ctx, homeValidation(request.Msg, "ephemeral_alarm_code", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
		}
		releaseLease := release
		release = func() { dispose(); releaseLease() }
	}
	response, err := h.commands.Admit(ctx, continuation, actor, native, metadata, release)
	if err != nil {
		fault := apiFault(err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Admission and HTTP cancellation can race. This response cannot establish
			// non-admission; reconciliation by the known handle remains read-only.
			fault = apierrors.New(api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN)
		}
		return nil, apiRPCError(ctx, fault.WithOperation(operationName))
	}
	if response == nil {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE))
	}
	if response.Error != nil {
		return nil, apiRPCError(ctx, nativeContractFault(response.Error))
	}
	operation, err := h.GetOperation(ctx, operationName)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(operation), nil
}

func nativeContractFault(value *actions.ContractError) error {
	if value == nil {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	switch value.Code {
	case actions.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return authorization.ErrDenied
	case actions.ErrorCode_ERROR_CODE_STALE_CONFIGURATION, actions.ErrorCode_ERROR_CODE_STALE_EPOCH:
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CONFIG_CHANGED)
	case actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY:
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CAPABILITY_UNAVAILABLE)
	case actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STATE_PRECONDITION_FAILED)
	default:
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
}
