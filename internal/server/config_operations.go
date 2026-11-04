package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// This is a read provider for the shared standard Operations service. It is
// never a second compiler, activator, request dispatcher or physical authority.
type configurationOperationProvider struct {
	documents    *configDocumentsHandler
	owner        *configactivation.Owner
	reads        chan struct{}
	integrations *integrationHandler
}

const configurationOperationReadTimeout = 2 * time.Second

func prepareConfigurationOperations(documents *configdocuments.Service, owner *configactivation.Owner, sessions *sessionCoordinator, names administrationNames) (*configurationOperationProvider, error) {
	if documents == nil {
		return nil, nil
	}
	if owner == nil || sessions == nil || sessions.authority == nil || names.store == nil || nilReactiveInterface(names.home) {
		return nil, ErrInvalidPublicPlan
	}
	return &configurationOperationProvider{documents: &configDocumentsHandler{service: documents, authority: sessions.authority, names: names}, owner: owner, reads: make(chan struct{}, 8)}, nil
}
func configurationOperationName(home apitypes.HomeName, id storage.ActivationAttemptID) (apitypes.OperationName, error) {
	if !home.Valid() || !id.Valid() {
		return apitypes.OperationName{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	name, err := apitypes.ParseOperationName(home.String() + "/operations/config-" + id.String())
	if err != nil {
		return apitypes.OperationName{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	return name, nil
}
func configurationOperationID(name apitypes.OperationName) (storage.ActivationAttemptID, error) {
	invalid := func() (storage.ActivationAttemptID, error) {
		violation, err := apierrors.NewViolation((&longrunningpb.GetOperationRequest{}).ProtoReflect().Descriptor(), "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
		if err != nil {
			return storage.ActivationAttemptID{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
		}
		return storage.ActivationAttemptID{}, apierrors.Validation(violation)
	}
	suffix, present := strings.CutPrefix(name.ID(), "config-")
	if !name.Valid() || !present || len(suffix) != 32 {
		return invalid()
	}
	raw, err := hex.DecodeString(suffix)
	if err != nil || hex.EncodeToString(raw) != suffix {
		return invalid()
	}
	var bytes [16]byte
	copy(bytes[:], raw)
	id, err := storage.NewActivationAttemptID(bytes)
	if err != nil {
		return invalid()
	}
	return id, nil
}
func configurationGenerationName(home apitypes.HomeName, attempt storage.ConfigActivationAttempt) string {
	return home.String() + "/configurationGenerations/g-" + strconv.FormatInt(attempt.Target().Number(), 10)
}
func configurationOperation(name apitypes.OperationName, attempt storage.ConfigActivationAttempt, published bool) (*longrunningpb.Operation, error) {
	if !name.Valid() || !attempt.ID().Valid() || !attempt.Target().Valid() || attempt.ManifestSHA256() == ([32]byte{}) || attempt.CommittedAt().IsZero() {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	created := timestamppb.New(attempt.CreateTime())
	if created.CheckValid() != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	digest := attempt.ManifestSHA256()
	generation := configurationGenerationName(name.Home(), attempt)
	metadata := &api.ConfigurationOperationMetadata{CreateTime: created, Generation: generation, GenerationRevision: attempt.Target().Number(), ManifestSha256: append([]byte(nil), digest[:]...), Phase: api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_COMMITTED, CommitTime: timestamppb.New(attempt.CommittedAt())}
	if requestID, present := attempt.APIRequestID(); present {
		metadata.RequestId = requestID.String()
	}
	if published {
		metadata.Phase = api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_PUBLISHED
	}
	packed, err := anypb.New(metadata)
	if err != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	operation := &longrunningpb.Operation{Name: name.String(), Metadata: packed, Done: published}
	if published {
		result, err := anypb.New(configurationActivationEnvelope(attempt, &api.ConfigurationActivationResult{Generation: generation, GenerationRevision: attempt.Target().Number(), ManifestSha256: append([]byte(nil), digest[:]...)}))
		if err != nil {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
		}
		operation.Result = &longrunningpb.Operation_Response{Response: result}
	}
	if contracts.AdmitConfigurationOperation(operation, name) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	return operation, nil
}
func configurationActivationEnvelope(attempt storage.ConfigActivationAttempt, result *api.ConfigurationActivationResult) proto.Message {
	method, _ := attempt.APIMethod()
	switch method {
	case storage.ConfigurationPublishDocument:
		return &api.PublishConfigurationDocumentResponse{Result: result}
	case storage.ConfigurationRollbackDocument:
		return &api.RollbackConfigurationDocumentResponse{Result: result}
	case storage.ConfigurationApplyPlan:
		return &api.ApplyConfigurationPlanResponse{Result: result}
	case storage.ConfigurationRollback:
		return &api.RollbackConfigurationResponse{Result: result}
	default:
		return nil
	}
}

func configurationOperationType(message proto.Message) string {
	if message == nil || !message.ProtoReflect().IsValid() {
		return ""
	}
	return "type.googleapis.com/" + string(message.ProtoReflect().Descriptor().FullName())
}

// Stored terminal bytes have one exact current result vocabulary. Unknown or
// malformed known fields cannot turn a durable commit into a published result.
func admitConfigurationOperationTerminal(data []byte, name apitypes.OperationName, attempt storage.ConfigActivationAttempt) (*longrunningpb.Operation, error) {
	invalid := func() (*longrunningpb.Operation, error) {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	if len(data) < 1 || len(data) > storage.ConfigOperationResultLimit {
		return invalid()
	}
	operation := new(longrunningpb.Operation)
	if proto.Unmarshal(data, operation) != nil || contracts.AdmitConfigurationOperation(operation, name) != nil || !operation.Done {
		return invalid()
	}
	metadata := new(api.ConfigurationOperationMetadata)
	if anypb.UnmarshalTo(operation.Metadata, metadata, proto.UnmarshalOptions{}) != nil {
		return invalid()
	}
	digest := attempt.ManifestSHA256()
	generation := configurationGenerationName(name.Home(), attempt)
	requestID := ""
	if id, present := attempt.APIRequestID(); present {
		requestID = id.String()
	}
	if metadata.RequestId != requestID || metadata.CreateTime == nil || metadata.CreateTime.CheckValid() != nil || !metadata.CreateTime.AsTime().Equal(attempt.CreateTime()) || metadata.CommitTime == nil || !metadata.CommitTime.AsTime().Equal(attempt.CommittedAt()) || metadata.Generation != generation || metadata.GenerationRevision != attempt.Target().Number() || !bytes.Equal(metadata.ManifestSha256, digest[:]) || metadata.Phase != api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_PUBLISHED {
		return invalid()
	}
	response, ok := operation.Result.(*longrunningpb.Operation_Response)
	if !ok || response == nil || response.Response == nil || response.Response.TypeUrl != configurationOperationType(configurationActivationEnvelope(attempt, &api.ConfigurationActivationResult{})) {
		return invalid()
	}
	result, err := contracts.ParseConfigurationActivationResult(response.Response)
	if err != nil || result.Generation != generation || result.GenerationRevision != attempt.Target().Number() || !bytes.Equal(result.ManifestSha256, digest[:]) {
		return invalid()
	}
	return operation, nil
}

func (p *configurationOperationProvider) GetOperation(ctx context.Context, name apitypes.OperationName) (*longrunningpb.Operation, error) {
	operation, err := p.readOperation(ctx, name, nil)
	if err != nil && p != nil && p.integrations != nil && integrationFamilyUnavailable(err) {
		return p.integrations.readOperation(ctx, name, nil)
	}
	return operation, err
}

// FromActivation reports only this caller's durable native attempt. It records
// a Published proof before returning a successful terminal response, even if a
// later activation has already become current. It never retries activation.
func (p *configurationOperationProvider) FromActivation(ctx context.Context, result configactivation.Result, nativeErr error) (*longrunningpb.Operation, error) {
	if nilReactiveInterface(ctx) {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	ctx, cancel := context.WithTimeout(ctx, configurationOperationReadTimeout)
	defer cancel()
	if p == nil || p.documents == nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	if _, err := p.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil); err != nil {
		return nil, administrationFault(err)
	}
	var replayed *storage.ConfigurationRequestError
	if errors.As(nativeErr, &replayed) {
		if record, present := replayed.Existing(); present {
			return p.replayRequest(ctx, record)
		}
	}
	if _, attached := result.APIRequest(); !attached && result.AttemptOutcome() != storage.TransactionCommitted {
		record, present, err := p.documents.service.Store().ProbeConfigurationAPIContext(ctx)
		if err != nil {
			return nil, administrationFault(err)
		}
		if present {
			return p.replayRequest(ctx, record)
		}
	}
	if result.Outcome() == configactivation.CommitUnknown || result.AttemptOutcome() == storage.TransactionIndeterminate {
		fault := apierrors.New(api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN)
		id := result.Attempt()
		if record, present := result.APIRequest(); present {
			id = record.ID()
		}
		// This original caller has just passed current administrator admission.
		// The native attempt/reservation supplies only a read handle, never a
		// replacement execution capability or permission to retry the mutation.
		if id.Valid() {
			if home, err := p.documents.names.home.HomeName(ctx); err == nil {
				if name, err := configurationOperationName(home, id); err == nil {
					fault = fault.WithOperation(name)
				}
			}
		}
		return nil, fault
	}
	if record, present := result.APIRequest(); present && result.AttemptOutcome() != storage.TransactionCommitted {
		return p.replayRequest(ctx, record)
	}
	if result.Outcome() == configactivation.RestartRequired && result.AttemptOutcome() != storage.TransactionCommitted {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CONFIG_RESTART_REQUIRED)
	}
	if result.AttemptOutcome() != storage.TransactionCommitted || !result.Attempt().Valid() {
		if nativeErr != nil {
			return nil, administrationFault(nativeErr)
		}
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	home, err := p.documents.names.home.HomeName(ctx)
	if err != nil {
		return nil, administrationFault(err)
	}
	name, err := configurationOperationName(home, result.Attempt())
	if err != nil {
		return nil, err
	}
	return p.readOperation(ctx, name, &result)
}

// A self-policy activation can retain the native authority barrier while its
// known COMMIT is reconciling. Re-entering that barrier would wait on itself.
// Only this original admitted mutation may return its author-bound pending
// metadata here; this path cannot expose a new snapshot or terminal success.
func (p *configurationOperationProvider) fromAdmittedPending(ctx context.Context, result configactivation.Result, binding configDocumentsBinding) (*longrunningpb.Operation, error) {
	if nilReactiveInterface(ctx) {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	ctx, cancel := context.WithTimeout(ctx, configurationOperationReadTimeout)
	defer cancel()
	if p == nil || p.documents == nil || !binding.valid() || result.AttemptOutcome() != storage.TransactionCommitted || !result.Attempt().Valid() ||
		(result.Outcome() != configactivation.CommittedReconciling && result.Outcome() != configactivation.RestartRequired) {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	attempt, present, err := p.documents.service.Store().ReadActivationAttempt(ctx, result.Attempt())
	if err != nil {
		return nil, administrationFault(err)
	}
	actor, actorErr := binding.account.Value()
	author, valid := attempt.Author().Value()
	if !present || !valid || actorErr != nil || actor != author {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	home, err := p.documents.names.home.HomeName(ctx)
	if err != nil {
		return nil, administrationFault(err)
	}
	name, err := configurationOperationName(home, attempt.ID())
	if err != nil {
		return nil, err
	}
	return configurationOperation(name, attempt, false)
}

func (p *configurationOperationProvider) readOperation(ctx context.Context, name apitypes.OperationName, activation *configactivation.Result) (*longrunningpb.Operation, error) {
	if nilReactiveInterface(ctx) {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	ctx, cancel := context.WithTimeout(ctx, configurationOperationReadTimeout)
	defer cancel()
	if p == nil || p.documents == nil || p.owner == nil || p.reads == nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	binding, err := p.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, administrationFault(err)
	}
	if !name.Valid() {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	if err := p.documents.names.parent(ctx, name.Home().String()); err != nil {
		return nil, administrationFault(err)
	}
	id, err := configurationOperationID(name)
	if err != nil {
		return nil, err
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
	if !permit.Ready() {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	evidence, err := permit.EntityMetadataEvidence()
	if err != nil {
		return nil, administrationFault(err)
	}
	attempt, present, err := p.documents.service.Store().ReadActivationAttempt(permit.Context(), id)
	if err != nil {
		return nil, administrationFault(err)
	}
	if !present {
		record, reserved, readErr := p.documents.service.Store().ReadConfigurationAPIRequestByAttempt(permit.Context(), id, evidence)
		if readErr != nil {
			return nil, administrationFault(readErr)
		}
		if reserved {
			operation, readErr := configurationRequestRead(name, record)
			if readErr != nil {
				return nil, readErr
			}
			if _, err := p.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
				return nil, administrationFault(err)
			}
			return operation, nil
		}
		if activation != nil {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
		}
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	}
	actor, err := binding.account.Value()
	author, valid := attempt.Author().Value()
	if err != nil || !valid || actor != author {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	stored, done, err := p.documents.service.Store().ReadConfigurationOperationResult(permit.Context(), id, evidence)
	if err != nil {
		return nil, administrationFault(err)
	}
	var operation *longrunningpb.Operation
	if done {
		operation, err = admitConfigurationOperationTerminal(stored.Bytes(), name, attempt)
		if err != nil {
			return nil, err
		}
	} else {
		var proof configactivation.PublicationEvidence
		var published bool
		if activation != nil {
			proof, published = activation.PublishedEvidence(attempt)
		}
		if !published {
			proof, published, err = p.owner.ObservePublishedAttempt(permit.Context(), attempt)
			if err != nil {
				return nil, administrationFault(err)
			}
		}
		operation, err = configurationOperation(name, attempt, published)
		if err != nil {
			return nil, err
		}
		if published {
			if !proof.Valid() || proof.Attempt() != id {
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
			}
			data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(operation)
			if err != nil {
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
			}
			if _, err := admitConfigurationOperationTerminal(data, name, attempt); err != nil {
				return nil, err
			}
			recorded, outcome, err := p.documents.service.Store().RecordConfigurationOperationResult(permit.Context(), id, proof.Target(), proof.ManifestSHA256(), evidence, data)
			if outcome == storage.TransactionIndeterminate {
				// Only the terminal metadata append is uncertain here. The
				// original configuration COMMIT is already independently proven.
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
			}
			if err != nil {
				return nil, administrationFault(err)
			}
			operation, err = admitConfigurationOperationTerminal(recorded.Bytes(), name, attempt)
			if err != nil {
				return nil, err
			}
		}
	}
	if _, err := p.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return nil, administrationFault(err)
	}
	return operation, nil
}
