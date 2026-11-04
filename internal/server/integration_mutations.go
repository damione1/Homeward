package server

import (
	"context"
	"encoding/base32"
	"sort"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	native "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrationadmin"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func integrationTypeKey(name apitypes.IntegrationTypeName) (string, error) {
	encoded, present := strings.CutPrefix(name.ResourceID(), "t-")
	if !present {
		return "", integrationResourceIntegrity()
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(encoded))
	if err != nil {
		return "", integrationResourceIntegrity()
	}
	expected, err := integrationTypeName(name.Home(), string(decoded))
	if err != nil || expected != name {
		return "", integrationResourceIntegrity()
	}
	return string(decoded), nil
}

func integrationUpdateIntent(input *api.UpdateIntegrationInstanceRequest) (*api.UpdateIntegrationInstanceRequest, error) {
	value := input.IntegrationInstance
	selection, err := integrationUpdateSelection(value, value, input.UpdateMask)
	if err != nil {
		return nil, err
	}
	intent := &api.UpdateIntegrationInstanceRequest{RequestId: input.RequestId, IntegrationInstance: &api.IntegrationInstance{Name: value.Name, Etag: value.Etag}, UpdateMask: &fieldmaskpb.FieldMask{}}
	if selection == integrationadmin.UpdateConfiguration || selection == integrationadmin.UpdateConfigurationAndDesiredState {
		intent.UpdateMask.Paths = append(intent.UpdateMask.Paths, "editable")
		if configuration := value.GetEditable(); configuration != nil {
			intent.IntegrationInstance.Configuration = &api.IntegrationInstance_Editable{Editable: proto.Clone(configuration).(*api.IntegrationConfiguration)}
		}
	}
	if selection == integrationadmin.UpdateDesiredState || selection == integrationadmin.UpdateConfigurationAndDesiredState {
		intent.UpdateMask.Paths = append(intent.UpdateMask.Paths, "desired_state")
		intent.IntegrationInstance.DesiredState = value.DesiredState
	}
	immutable := func(field string, populated bool) bool {
		if input.UpdateMask == nil {
			return populated
		}
		for _, path := range input.UpdateMask.Paths {
			if path == field || path == "*" && populated {
				return true
			}
		}
		return false
	}
	if immutable("instance_key", value.InstanceKey != "") {
		intent.UpdateMask.Paths = append(intent.UpdateMask.Paths, "instance_key")
		intent.IntegrationInstance.InstanceKey = value.InstanceKey
	}
	if immutable("integration_type", value.IntegrationType != "") {
		intent.UpdateMask.Paths = append(intent.UpdateMask.Paths, "integration_type")
		intent.IntegrationInstance.IntegrationType = value.IntegrationType
	}
	sort.Strings(intent.UpdateMask.Paths)
	return intent, nil
}

func (h *integrationHandler) UpdateIntegrationInstance(ctx context.Context, request *connect.Request[api.UpdateIntegrationInstanceRequest]) (response *connect.Response[longrunningpb.Operation], callErr error) {
	defer func() {
		if callErr != nil {
			callErr = apiRPCError(ctx, integrationFault(callErr))
		}
	}()
	permit, err := h.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	invalid := func(field string, reason api.ValidationReason) error {
		return integrationInvalidField(&api.UpdateIntegrationInstanceRequest{}, field, reason)
	}
	if request == nil || request.Msg == nil || request.Msg.IntegrationInstance == nil {
		return nil, invalid("integration_instance", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	if contracts.AdmitIntegrationRequestShape(request.Msg) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	input, value := request.Msg, request.Msg.IntegrationInstance
	name, err := apitypes.ParseIntegrationInstanceName(value.Name)
	if err != nil {
		return nil, invalid("integration_instance.name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	key, err := h.native.names.resolve(permit.Context(), storage.APIIntegrationInstances, name.String())
	if err != nil {
		return nil, integrationFault(err)
	}
	id, err := domain.ParseIntegrationInstanceID(key)
	if err != nil {
		return nil, integrationResourceIntegrity()
	}
	target, err := storage.NewConfigurationIntegrationTarget(storage.ConfigurationUpdateIntegration, id)
	if err != nil {
		return nil, integrationFault(err)
	}
	intent, err := integrationUpdateIntent(input)
	if err != nil {
		return nil, invalid("update_mask", api.ValidationReason_VALIDATION_REASON_INVALID_MASK)
	}
	mutationCtx, replay, err := h.prepareMutation(permit.Context(), permit, name.Home(), target, name, input.RequestId, intent)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return connect.NewResponse(replay), nil
	}
	if value.Etag == "" {
		return nil, invalid("integration_instance.etag", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	prepared, err := h.native.service.PrepareUpdate(mutationCtx, id, integrationadmin.UpdateNothing, nil, native.DesiredState_DESIRED_STATE_UNSPECIFIED)
	if err != nil {
		return nil, integrationFault(err)
	}
	original, err := h.native.integrationResource(mutationCtx, name.Home(), prepared.Definition(), integrationSnapshotVersion(prepared.Snapshot()), false)
	if err != nil {
		return nil, integrationFault(err)
	}
	if value.Etag != original.Etag {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH)
	}
	selection, err := integrationUpdateSelection(value, original, input.UpdateMask)
	if err != nil {
		return nil, invalid("update_mask", api.ValidationReason_VALIDATION_REASON_INVALID_MASK)
	}
	if selection == integrationadmin.UpdateConfiguration || selection == integrationadmin.UpdateConfigurationAndDesiredState {
		if contracts.AdmitIntegrationConfiguration(value.GetEditable(), key) != nil {
			return nil, invalid("integration_instance.editable", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
		}
	}
	if selection != integrationadmin.UpdateNothing {
		prepared, err = h.native.service.PrepareUpdate(mutationCtx, id, selection, integrationNativeConfiguration(value.GetEditable()), integrationNativeDesired(value.DesiredState))
		if err != nil {
			return nil, integrationFault(err)
		}
		original, err = h.native.integrationResource(mutationCtx, name.Home(), prepared.Definition(), integrationSnapshotVersion(prepared.Snapshot()), false)
		if err != nil {
			return nil, integrationFault(err)
		}
		if value.Etag != original.Etag {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH)
		}
	}
	expected, err := integrationExpected(prepared.Snapshot(), prepared.Changed())
	if err != nil {
		return nil, err
	}
	encoder, err := integrationNoChangeEncoder(name.Home(), name, original)
	if err != nil {
		return nil, err
	}
	result, nativeErr := h.native.service.ApplyPreparedUpdate(mutationCtx, prepared, expected, encoder)
	operation, err := h.fromActivation(mutationCtx, name.Home(), result.Activation, nativeErr)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(operation), nil
}

func integrationNativeConfiguration(value *api.IntegrationConfiguration) *native.PublicConfiguration {
	if value == nil {
		return nil
	}
	result := &native.PublicConfiguration{SchemaVersion: value.SchemaVersion, YamlSource: append([]byte(nil), value.YamlSource...)}
	for _, secret := range value.Secrets {
		if secret == nil {
			result.Secrets = append(result.Secrets, nil)
			continue
		}
		result.Secrets = append(result.Secrets, &native.SecretReference{Field: secret.Field, IntegrationInstanceId: secret.InstanceKey, Name: secret.SecretKey})
	}
	return result
}
func integrationNativeDesired(value api.IntegrationDesiredState) native.DesiredState {
	switch value {
	case api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED:
		return native.DesiredState_DESIRED_STATE_ENABLED
	case api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED:
		return native.DesiredState_DESIRED_STATE_DISABLED
	default:
		return native.DesiredState_DESIRED_STATE_UNSPECIFIED
	}
}

func (h *integrationHandler) CreateIntegrationInstance(ctx context.Context, request *connect.Request[api.CreateIntegrationInstanceRequest]) (response *connect.Response[longrunningpb.Operation], callErr error) {
	defer func() {
		if callErr != nil {
			callErr = apiRPCError(ctx, integrationFault(callErr))
		}
	}()
	permit, err := h.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	invalid := func(field string, reason api.ValidationReason) error {
		return integrationInvalidField(&api.CreateIntegrationInstanceRequest{}, field, reason)
	}
	if request == nil || request.Msg == nil {
		return nil, invalid("integration_instance", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	if contracts.AdmitIntegrationRequestShape(request.Msg) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	input := request.Msg
	home, err := apitypes.ParseHomeName(input.Parent)
	if err != nil {
		return nil, invalid("parent", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	if err := h.native.names.parent(permit.Context(), home.String()); err != nil {
		return nil, integrationFault(err)
	}
	value := input.IntegrationInstance
	if value == nil {
		return nil, invalid("integration_instance", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	id, err := domain.ParseIntegrationInstanceID(value.InstanceKey)
	if err != nil {
		return nil, invalid("integration_instance.instance_key", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
	}
	typeName, err := apitypes.ParseIntegrationTypeName(value.IntegrationType)
	if err != nil || typeName.Home() != home {
		return nil, invalid("integration_instance.integration_type", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	typeKey, err := integrationTypeKey(typeName)
	if err != nil {
		return nil, invalid("integration_instance.integration_type", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	target, err := storage.NewConfigurationIntegrationTarget(storage.ConfigurationCreateIntegration, id)
	if err != nil {
		return nil, integrationFault(err)
	}
	// Copy selected creation intent, excluding all observation/output fields.
	intent := &api.CreateIntegrationInstanceRequest{Parent: home.String(), RequestId: input.RequestId, IntegrationInstance: &api.IntegrationInstance{InstanceKey: value.InstanceKey, IntegrationType: value.IntegrationType, DesiredState: value.DesiredState}}
	if editable := value.GetEditable(); editable != nil {
		intent.IntegrationInstance.Configuration = &api.IntegrationInstance_Editable{Editable: proto.Clone(editable).(*api.IntegrationConfiguration)}
	}
	mutationCtx, replay, err := h.prepareMutation(permit.Context(), permit, home, target, apitypes.IntegrationInstanceName{}, input.RequestId, intent)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return connect.NewResponse(replay), nil
	}
	if value.GetEditable() == nil {
		return nil, invalid("integration_instance.editable", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	if contracts.AdmitIntegrationConfiguration(value.GetEditable(), value.InstanceKey) != nil {
		return nil, invalid("integration_instance.editable", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
	}
	if integrationNativeDesired(value.DesiredState) == native.DesiredState_DESIRED_STATE_UNSPECIFIED {
		return nil, invalid("integration_instance.desired_state", api.ValidationReason_VALIDATION_REASON_UNKNOWN_VALUE)
	}
	snapshot, _, err := h.native.documents.ReadOperationalSourceForMutation(mutationCtx)
	if err != nil {
		return nil, integrationFault(err)
	}
	expected, err := integrationExpected(snapshot, true)
	if err != nil {
		return nil, err
	}
	result, nativeErr := h.native.service.Create(mutationCtx, typeKey, value.InstanceKey, integrationNativeConfiguration(value.GetEditable()), integrationNativeDesired(value.DesiredState), expected)
	operation, err := h.fromActivation(mutationCtx, home, result.Activation, nativeErr)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(operation), nil
}

func (h *integrationHandler) DeleteIntegrationInstance(ctx context.Context, request *connect.Request[api.DeleteIntegrationInstanceRequest]) (response *connect.Response[longrunningpb.Operation], callErr error) {
	defer func() {
		if callErr != nil {
			callErr = apiRPCError(ctx, integrationFault(callErr))
		}
	}()
	permit, err := h.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	invalid := func(field string, reason api.ValidationReason) error {
		return integrationInvalidField(&api.DeleteIntegrationInstanceRequest{}, field, reason)
	}
	if request == nil || request.Msg == nil {
		return nil, invalid("name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	if contracts.AdmitIntegrationRequestShape(request.Msg) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	input := request.Msg
	name, err := apitypes.ParseIntegrationInstanceName(input.Name)
	if err != nil {
		return nil, invalid("name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	key, err := h.native.names.resolve(permit.Context(), storage.APIIntegrationInstances, name.String())
	if err != nil {
		return nil, integrationFault(err)
	}
	id, err := domain.ParseIntegrationInstanceID(key)
	if err != nil {
		return nil, integrationResourceIntegrity()
	}
	target, err := storage.NewConfigurationIntegrationTarget(storage.ConfigurationDeleteIntegration, id)
	if err != nil {
		return nil, integrationFault(err)
	}
	intent := &api.DeleteIntegrationInstanceRequest{Name: name.String(), Etag: input.Etag, RequestId: input.RequestId}
	mutationCtx, replay, err := h.prepareMutation(permit.Context(), permit, name.Home(), target, name, input.RequestId, intent)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return connect.NewResponse(replay), nil
	}
	if input.Etag == "" {
		return nil, invalid("etag", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	prepared, err := h.native.service.PrepareUpdate(mutationCtx, id, integrationadmin.UpdateNothing, nil, native.DesiredState_DESIRED_STATE_UNSPECIFIED)
	if err != nil {
		return nil, integrationFault(err)
	}
	original, err := h.native.integrationResource(mutationCtx, name.Home(), prepared.Definition(), integrationSnapshotVersion(prepared.Snapshot()), false)
	if err != nil {
		return nil, integrationFault(err)
	}
	if input.Etag != original.Etag {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH)
	}
	expected, err := integrationExpected(prepared.Snapshot(), true)
	if err != nil {
		return nil, err
	}
	result, nativeErr := h.native.service.Delete(mutationCtx, key, expected)
	operation, err := h.fromActivation(mutationCtx, name.Home(), result.Activation, nativeErr)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(operation), nil
}
