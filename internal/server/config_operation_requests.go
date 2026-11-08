package server

import (
	"context"
	"sort"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The accepted state proves only a durable reservation. It is deliberately
// silent about compilation, filesystem staging and the eventual SQL outcome.
func configurationRequestOperation(name apitypes.OperationName, record storage.ConfigurationAPIRequestRecord, failure *apierrors.Fault) (*longrunningpb.Operation, error) {
	if !record.Valid() {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	expected, err := configurationOperationName(name.Home(), record.ID())
	if err != nil || expected != name {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	metadata := &api.ConfigurationOperationMetadata{
		CreateTime: timestamppb.New(record.AcceptedAt()), RequestId: record.RequestID().String(),
		Phase: api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_ACCEPTED,
	}
	if failure != nil {
		metadata.Phase = api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_FAILED
	}
	packed, err := anypb.New(metadata)
	if err != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	operation := &longrunningpb.Operation{Name: name.String(), Metadata: packed}
	if failure != nil {
		operation.Done = true
		operation.Result = &longrunningpb.Operation_Error{Error: failure.Status()}
	}
	if contracts.AdmitConfigurationOperation(operation, name) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	return operation, nil
}

func admitConfigurationRequestOperation(data []byte, name apitypes.OperationName, record storage.ConfigurationAPIRequestRecord) (*longrunningpb.Operation, error) {
	invalid := func() (*longrunningpb.Operation, error) {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	if !record.Valid() || len(data) < 1 || len(data) > storage.ConfigurationAPIFailureBytes {
		return invalid()
	}
	operation := new(longrunningpb.Operation)
	if proto.Unmarshal(data, operation) != nil || contracts.AdmitConfigurationOperation(operation, name) != nil || !operation.Done {
		return invalid()
	}
	metadata := new(api.ConfigurationOperationMetadata)
	if anypb.UnmarshalTo(operation.Metadata, metadata, proto.UnmarshalOptions{}) != nil || metadata.Phase != api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_FAILED ||
		metadata.RequestId != record.RequestID().String() || !metadata.CreateTime.AsTime().Equal(record.AcceptedAt()) {
		return invalid()
	}
	expected, err := configurationOperationName(name.Home(), record.ID())
	if err != nil || expected != name {
		return invalid()
	}
	return operation, nil
}

func configurationRequestRead(name apitypes.OperationName, record storage.ConfigurationAPIRequestRecord) (*longrunningpb.Operation, error) {
	if data, failed := record.Failure(); failed {
		return admitConfigurationRequestOperation(data, name, record)
	}
	return configurationRequestOperation(name, record, nil)
}

// This callback performs bounded encoding only. The native Owner chooses when
// a definitely uncommitted outcome may be appended, using the same request-row
// lock as its activation transaction. It retains no candidate or HTTP context.
func configurationOutcomeEncoder(home apitypes.HomeName) func(configactivation.Result, error) ([]byte, error) {
	return func(result configactivation.Result, cause error) ([]byte, error) {
		record, present := result.APIRequest()
		if !present {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
		}
		fault := administrationFault(cause)
		if result.Outcome() == configactivation.RestartRequired {
			fault = apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CONFIG_RESTART_REQUIRED)
		}
		// A later winner may have been published while this caller's own
		// transaction definitely lost its CAS. That is not its success.
		if cause == nil && result.AttemptOutcome() == storage.TransactionNotCommitted {
			fault = apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH)
		}
		name, err := configurationOperationName(home, record.ID())
		if err != nil {
			return nil, err
		}
		operation, err := configurationRequestOperation(name, record, &fault)
		if err != nil {
			return nil, err
		}
		return (proto.MarshalOptions{Deterministic: true}).Marshal(operation)
	}
}

func (p *configurationOperationProvider) replayRequest(ctx context.Context, record storage.ConfigurationAPIRequestRecord) (*longrunningpb.Operation, error) {
	home, err := p.documents.names.home.HomeName(ctx)
	if err != nil {
		return nil, administrationFault(err)
	}
	name, err := configurationOperationName(home, record.ID())
	if err != nil {
		return nil, err
	}
	return p.GetOperation(ctx, name)
}

// FindOperationsByRequest supplies actual owner identities to the common
// Operations listing. The common service owns pagination and multi-owner
// merging; this method neither truncates nor creates a second catalogue.
func (p *configurationOperationProvider) findGlobalOperationsByRequest(ctx context.Context, home apitypes.HomeName, requestID apitypes.RequestID, cutoff time.Time) ([]apitypes.OperationName, error) {
	if p == nil || p.documents == nil || p.reads == nil || ctx == nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	ctx, cancel := context.WithTimeout(ctx, configurationOperationReadTimeout)
	defer cancel()
	binding, err := p.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil)
	if err != nil {
		return nil, administrationFault(err)
	}
	if !home.Valid() || !requestID.Valid() || cutoff.IsZero() {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	if err := p.documents.names.parent(ctx, home.String()); err != nil {
		return nil, administrationFault(err)
	}
	select {
	case p.reads <- struct{}{}:
		defer func() { <-p.reads }()
	default:
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	permit, err := p.documents.mutationPermit(ctx)
	if err != nil {
		return nil, administrationFault(err)
	}
	defer permit.Release()
	evidence, err := permit.EntityMetadataEvidence()
	if err != nil || !permit.Ready() {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	ids, err := p.documents.service.Store().FindConfigurationAPIRequests(permit.Context(), requestID, cutoff, evidence)
	if err != nil {
		return nil, administrationFault(err)
	}
	names := make([]apitypes.OperationName, 0, len(ids))
	for _, id := range ids {
		name, err := configurationOperationName(home, id)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if _, err := p.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, &binding); err != nil {
		return nil, administrationFault(err)
	}
	return names, nil
}

// ListOperationNames is the bounded native provider port for common Operations
// pagination. The common service owns authenticated tokens and provider merging.
// A nonnil continuation is the last scanned candidate; nil means end.
func (p *configurationOperationProvider) listGlobalOperationNames(ctx context.Context, home apitypes.HomeName, after *apitypes.OperationName, pageSize uint32, cutoff time.Time) ([]apitypes.OperationName, *apitypes.OperationName, error) {
	if p == nil || p.documents == nil || p.reads == nil || ctx == nil {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	ctx, cancel := context.WithTimeout(ctx, configurationOperationReadTimeout)
	defer cancel()
	binding, err := p.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil)
	if err != nil {
		return nil, nil, administrationFault(err)
	}
	if !home.Valid() || pageSize < 1 || pageSize > 500 || cutoff.IsZero() {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	if err := p.documents.names.parent(ctx, home.String()); err != nil {
		return nil, nil, administrationFault(err)
	}
	var cursor *storage.ActivationAttemptID
	if after != nil {
		if !after.Valid() || after.Home() != home {
			return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
		}
		id, err := configurationOperationID(*after)
		if err != nil {
			return nil, nil, err
		}
		cursor = &id
	}
	select {
	case p.reads <- struct{}{}:
		defer func() { <-p.reads }()
	default:
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	permit, err := p.documents.mutationPermit(ctx)
	if err != nil {
		return nil, nil, administrationFault(err)
	}
	defer permit.Release()
	evidence, err := permit.EntityMetadataEvidence()
	if err != nil || !permit.Ready() {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	ids, more, err := p.documents.service.Store().ListConfigurationOperationIDs(permit.Context(), cursor, pageSize, cutoff, evidence)
	if err != nil {
		return nil, nil, administrationFault(err)
	}
	names := make([]apitypes.OperationName, 0, len(ids))
	for _, id := range ids {
		name, err := configurationOperationName(home, id)
		if err != nil {
			return nil, nil, err
		}
		names = append(names, name)
	}
	if _, err := p.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, &binding); err != nil {
		return nil, nil, administrationFault(err)
	}
	var next *apitypes.OperationName
	if more {
		if len(names) == 0 {
			return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
		}
		last := names[len(names)-1]
		next = &last
	}
	return names, next, nil
}

func normalizedConfigurationRequest(raw proto.Message) ([]byte, error) {
	copy := proto.Clone(raw)
	if copy == nil {
		return nil, contracts.ErrInvalidConfigurationContract
	}
	field := copy.ProtoReflect().Descriptor().Fields().ByName("request_id")
	if field == nil {
		return nil, contracts.ErrInvalidConfigurationContract
	}
	copy.ProtoReflect().Clear(field)
	sortSources := func(sources []*api.SourcePrecondition) {
		sort.Slice(sources, func(i, j int) bool { return sources[i].LogicalPath < sources[j].LogicalPath })
	}
	switch value := copy.(type) {
	case *api.ApplyConfigurationPlanRequest:
		sortSources(value.Sources)
		if value.Candidate != nil {
			sort.Slice(value.Candidate.Files, func(i, j int) bool {
				return value.Candidate.Files[i].LogicalPath < value.Candidate.Files[j].LogicalPath
			})
		}
	case *api.RollbackConfigurationRequest:
		sortSources(value.Sources)
		if value.Family == api.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_UNSPECIFIED {
			value.Family = api.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL
			value.Identity = "operational.main"
		}
	case *api.PublishConfigurationDocumentRequest, *api.RollbackConfigurationDocumentRequest:
	default:
		return nil, contracts.ErrInvalidConfigurationContract
	}
	return (proto.MarshalOptions{Deterministic: true}).Marshal(copy)
}

// Call after object authorization and wire admission, before the ephemeral
// plan lookup. A duplicate surviving plan expiry or restart reads only its
// original operation; it cannot recreate the candidate or extend its receipt.
func (h *configDocumentsHandler) prepareConfigurationRequest(ctx context.Context, method storage.ConfigurationAPIMethod, requestID string, raw proto.Message) (context.Context, *longrunningpb.Operation, error) {
	if requestID == "" {
		prepared, err := storage.WithConfigurationOperationMethod(ctx, method)
		return prepared, nil, err
	}
	id, err := apitypes.ParseRequestID(requestID)
	if err != nil {
		violation, buildErr := apierrors.NewViolation(raw.ProtoReflect().Descriptor(), "request_id", api.ValidationReason_VALIDATION_REASON_INVALID_REQUEST_ID)
		if buildErr != nil {
			return ctx, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
		}
		return ctx, nil, apierrors.Validation(violation)
	}
	normalized, err := normalizedConfigurationRequest(raw)
	if err != nil {
		return ctx, nil, err
	}
	permit, err := h.mutationPermit(ctx)
	if err != nil {
		return ctx, nil, err
	}
	defer permit.Release()
	if !permit.Ready() {
		return ctx, nil, authorization.ErrDenied
	}
	evidence, err := permit.EntityMetadataEvidence()
	if err != nil {
		return ctx, nil, err
	}
	request, err := storage.NewConfigurationAPIRequest(method, id, normalized, evidence)
	if err != nil {
		return ctx, nil, err
	}
	record, present, err := h.service.Store().ProbeConfigurationAPIRequest(permit.Context(), request)
	if err != nil {
		return ctx, nil, err
	}
	// Release the current admission before reentering the read provider. Its
	// own fresh permit and original-author fence remain mandatory on replay.
	permit.Release()
	if present {
		operation, err := h.operations.replayRequest(ctx, record)
		return ctx, operation, err
	}
	home, err := h.names.home.HomeName(ctx)
	if err != nil {
		return ctx, nil, err
	}
	prepared, err := storage.WithConfigurationAPIRequest(ctx, request)
	if err != nil {
		return ctx, nil, err
	}
	bound, err := configactivation.WithAPIOutcomeEncoder(prepared, configurationOutcomeEncoder(home))
	if err != nil {
		return ctx, nil, err
	}
	return bound, nil, nil
}
