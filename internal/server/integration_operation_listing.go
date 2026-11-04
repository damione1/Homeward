package server

import (
	"context"
	"errors"
	"sort"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
)

func integrationFamilyUnavailable(err error) bool {
	var fault apierrors.Fault
	return errors.As(err, &fault) && (fault.Reason() == api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED || fault.Reason() == api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
}

func (h *integrationHandler) operationIDs(ctx context.Context, home apitypes.HomeName, requestID *apitypes.RequestID, after *apitypes.OperationName, pageSize uint32, cutoff time.Time) ([]apitypes.OperationName, *apitypes.OperationName, error) {
	permit, err := h.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, nil, err
	}
	defer permit.Release()
	if h.operations == nil || h.operations.reads == nil {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	select {
	case h.operations.reads <- struct{}{}:
		defer func() { <-h.operations.reads }()
	default:
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	if err := h.native.names.parent(permit.Context(), home.String()); err != nil {
		return nil, nil, integrationFault(err)
	}
	evidence, err := permit.IntegrationAdministrationEvidence()
	if err != nil {
		return nil, nil, integrationFault(err)
	}
	var cursor *storage.ActivationAttemptID
	if after != nil {
		if after.Home() != home {
			return nil, nil, integrationResourceIntegrity()
		}
		id, err := configurationOperationID(*after)
		if err != nil {
			return nil, nil, err
		}
		cursor = &id
	}
	var ids []storage.ActivationAttemptID
	more := false
	if requestID != nil {
		ids, err = h.native.documents.Store().FindIntegrationAPIRequests(permit.Context(), *requestID, cutoff, evidence)
	} else {
		ids, more, err = h.native.documents.Store().ListIntegrationOperationIDs(permit.Context(), cursor, pageSize, cutoff, evidence)
	}
	if err != nil {
		return nil, nil, integrationFault(err)
	}
	names := make([]apitypes.OperationName, 0, len(ids))
	for _, id := range ids {
		name, err := configurationOperationName(home, id)
		if err != nil {
			return nil, nil, err
		}
		names = append(names, name)
	}
	if !permit.Ready() {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	var next *apitypes.OperationName
	if more {
		if len(names) == 0 {
			return nil, nil, integrationResourceIntegrity()
		}
		last := names[len(names)-1]
		next = &last
	}
	return names, next, nil
}

func (p *configurationOperationProvider) FindOperationsByRequest(ctx context.Context, home apitypes.HomeName, requestID apitypes.RequestID, cutoff time.Time) ([]apitypes.OperationName, error) {
	if nilReactiveInterface(ctx) {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	if p == nil || p.integrations == nil {
		return p.findGlobalOperationsByRequest(ctx, home, requestID, cutoff)
	}
	ctx, cancel := context.WithTimeout(ctx, configurationOperationReadTimeout)
	defer cancel()
	global, globalErr := p.findGlobalOperationsByRequest(ctx, home, requestID, cutoff)
	if globalErr != nil && !integrationFamilyUnavailable(globalErr) {
		return nil, globalErr
	}
	integration, _, integrationErr := p.integrations.operationIDs(ctx, home, &requestID, nil, 0, cutoff)
	if integrationErr != nil && !integrationFamilyUnavailable(integrationErr) {
		return nil, integrationErr
	}
	if globalErr != nil && integrationErr != nil {
		return nil, globalErr
	}
	names := append(global, integration...)
	if len(names) > 100 {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	sort.Slice(names, func(i, j int) bool { return names[i].String() < names[j].String() })
	for i := 1; i < len(names); i++ {
		if names[i] == names[i-1] {
			return nil, integrationResourceIntegrity()
		}
	}
	return names, nil
}

func (p *configurationOperationProvider) ListOperationNames(ctx context.Context, home apitypes.HomeName, after *apitypes.OperationName, pageSize uint32, cutoff time.Time) ([]apitypes.OperationName, *apitypes.OperationName, error) {
	if nilReactiveInterface(ctx) {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	if p == nil || p.integrations == nil {
		return p.listGlobalOperationNames(ctx, home, after, pageSize, cutoff)
	}
	ctx, cancel := context.WithTimeout(ctx, configurationOperationReadTimeout)
	defer cancel()
	if pageSize < 1 || pageSize > 500 || !home.Valid() || cutoff.IsZero() {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	// At most 500 SQL candidates across both families. A prefetched candidate
	// beyond the covered prefix is not consumed and is safely read next time.
	batch := pageSize
	if batch > 250 {
		batch = 250
	}
	global, globalNext, globalErr := p.listGlobalOperationNames(ctx, home, after, batch, cutoff)
	if globalErr != nil && !integrationFamilyUnavailable(globalErr) {
		return nil, nil, globalErr
	}
	integration, integrationNext, integrationErr := p.integrations.operationIDs(ctx, home, nil, after, batch, cutoff)
	if integrationErr != nil && !integrationFamilyUnavailable(integrationErr) {
		return nil, nil, integrationErr
	}
	if globalErr != nil && integrationErr != nil {
		return nil, nil, globalErr
	}
	return mergeAdministrationOperationNames(global, globalNext, integration, integrationNext, pageSize)
}

func mergeAdministrationOperationNames(global []apitypes.OperationName, globalNext *apitypes.OperationName, integration []apitypes.OperationName, integrationNext *apitypes.OperationName, pageSize uint32) ([]apitypes.OperationName, *apitypes.OperationName, error) {
	names := append(global, integration...)
	sort.Slice(names, func(i, j int) bool { return names[i].String() < names[j].String() })
	frontier := globalNext
	if integrationNext != nil && (frontier == nil || integrationNext.String() < frontier.String()) {
		frontier = integrationNext
	}
	count := len(names)
	if frontier != nil {
		count = sort.Search(len(names), func(i int) bool { return names[i].String() > frontier.String() })
	}
	if count > int(pageSize) {
		count = int(pageSize)
	}
	for i := 1; i < count; i++ {
		if names[i] == names[i-1] {
			return nil, nil, integrationResourceIntegrity()
		}
	}
	var next *apitypes.OperationName
	if frontier != nil || count < len(names) {
		if count == 0 {
			return nil, nil, integrationResourceIntegrity()
		}
		last := names[count-1]
		next = &last
	}
	return names[:count], next, nil
}
