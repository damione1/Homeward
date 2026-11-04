package server

import (
	"context"
	"errors"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

func (h *integrationHandler) operationTarget(ctx context.Context, home apitypes.HomeName, native storage.ConfigurationIntegrationTarget, allowMissingCreate bool) (apitypes.IntegrationInstanceName, error) {
	key, err := native.Instance().Value()
	if err != nil || !native.Valid() {
		return apitypes.IntegrationInstanceName{}, integrationResourceIntegrity()
	}
	name, err := h.native.names.name(ctx, storage.APIIntegrationInstances, key)
	if err != nil {
		if allowMissingCreate && native.Method() == storage.ConfigurationCreateIntegration && errors.Is(err, storage.ErrAPIResourceNotFound) {
			return apitypes.IntegrationInstanceName{}, nil
		}
		return apitypes.IntegrationInstanceName{}, err
	}
	target, err := apitypes.ParseIntegrationInstanceName(name)
	if err != nil || target.Home() != home {
		return apitypes.IntegrationInstanceName{}, integrationResourceIntegrity()
	}
	return target, nil
}

// readOperation observes the single native journal. Every read and terminal
// append rechecks the exact original session/key and current integration ceiling.
func (h *integrationHandler) readOperation(ctx context.Context, name apitypes.OperationName, activation *configactivation.Result) (*longrunningpb.Operation, error) {
	if nilReactiveInterface(ctx) {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	ctx, cancel := context.WithTimeout(ctx, configurationOperationReadTimeout)
	defer cancel()
	permit, err := h.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	if h.operations == nil || h.operations.owner == nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	select {
	case h.operations.reads <- struct{}{}:
		defer func() { <-h.operations.reads }()
	default:
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	if err := h.native.names.parent(permit.Context(), name.Home().String()); err != nil {
		return nil, integrationFault(err)
	}
	id, err := configurationOperationID(name)
	if err != nil {
		return nil, err
	}
	evidence, err := permit.IntegrationAdministrationEvidence()
	if err != nil {
		return nil, integrationFault(err)
	}
	store := h.native.documents.Store()
	record, reserved, err := store.ReadIntegrationAPIRequestByAttempt(permit.Context(), id, evidence)
	if err != nil {
		return nil, integrationFault(err)
	}
	attempt, committed, err := store.ReadActivationAttempt(permit.Context(), id)
	if err != nil {
		return nil, integrationFault(err)
	}
	if !committed {
		if !reserved {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
		}
		native, present := record.IntegrationTarget()
		if !present {
			return nil, integrationResourceIntegrity()
		}
		target, err := h.operationTarget(permit.Context(), name.Home(), native, true)
		if err != nil {
			return nil, integrationFault(err)
		}
		operation, err := integrationRequestRead(name.Home(), record, target)
		if err != nil {
			return nil, err
		}
		if !permit.Ready() {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
		}
		return operation, nil
	}
	// This SQL read fences origin credential even if no terminal bytes exist.
	// It precedes all target mapping and historical definition diagnostics.
	stored, done, err := store.ReadIntegrationOperationResult(permit.Context(), id, evidence)
	if err != nil {
		return nil, integrationFault(err)
	}
	native, present := attempt.IntegrationTarget()
	if !present {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	}
	target, err := h.operationTarget(permit.Context(), name.Home(), native, false)
	if err != nil {
		return nil, integrationFault(err)
	}
	var operation *longrunningpb.Operation
	if done {
		operation, err = admitIntegrationPublishedOperation(stored.Bytes(), name.Home(), target, attempt)
	} else {
		var proof configactivation.PublicationEvidence
		var published bool
		if activation != nil {
			proof, published = activation.PublishedEvidence(attempt)
		}
		if !published {
			proof, published, err = h.operations.owner.ObservePublishedAttempt(permit.Context(), attempt)
			if err != nil {
				return nil, integrationFault(err)
			}
		}
		if !published {
			operation, err = integrationCommittedOperation(name.Home(), target, attempt)
		} else {
			definition, version, readErr := h.native.service.PublishedDefinition(permit.Context(), attempt, proof)
			if readErr != nil {
				return nil, integrationFault(readErr)
			}
			var resource *api.IntegrationInstance
			if definition != nil {
				resource, err = h.native.integrationResource(permit.Context(), name.Home(), definition, integrationDefinitionVersion{ref: version.Ref(), hash: version.SHA256(), generation: proof.Target().Number(), manifest: proof.ManifestSHA256(), workspace: version.SHA256(), observed: true}, false)
				if err != nil {
					return nil, integrationFault(err)
				}
			}
			operation, err = integrationPublishedOperation(name.Home(), target, attempt, proof, resource)
			if err != nil {
				return nil, err
			}
			data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(operation)
			if err != nil {
				return nil, integrationResourceIntegrity()
			}
			recorded, outcome, err := store.RecordIntegrationOperationResult(permit.Context(), id, proof.Target(), proof.ManifestSHA256(), evidence, data)
			if outcome == storage.TransactionIndeterminate {
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
			}
			if err != nil {
				return nil, integrationFault(err)
			}
			operation, err = admitIntegrationPublishedOperation(recorded.Bytes(), name.Home(), target, attempt)
		}
	}
	if err != nil {
		return nil, err
	}
	if !permit.Ready() {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	return operation, nil
}
