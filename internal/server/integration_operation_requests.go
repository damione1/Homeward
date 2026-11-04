package server

import (
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func integrationMutationKind(method storage.ConfigurationAPIMethod) (api.IntegrationMutationKind, error) {
	switch method {
	case storage.ConfigurationCreateIntegration:
		return api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_CREATE, nil
	case storage.ConfigurationUpdateIntegration:
		return api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_UPDATE, nil
	case storage.ConfigurationDeleteIntegration:
		return api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_DELETE, nil
	default:
		return 0, integrationResourceIntegrity()
	}
}

// Names and the redacted definition must already have been resolved before a
// native SQL callback. These builders perform bounded memory-only work.
func integrationRequestMetadata(record storage.ConfigurationAPIRequestRecord, target apitypes.IntegrationInstanceName) (*api.IntegrationOperationMetadata, error) {
	if !record.Valid() {
		return nil, integrationResourceIntegrity()
	}
	kind, err := integrationMutationKind(record.Method())
	if err != nil {
		return nil, err
	}
	if _, present := record.IntegrationTarget(); !present {
		return nil, integrationResourceIntegrity()
	}
	return &api.IntegrationOperationMetadata{Kind: kind, Target: target.String(), CreateTime: timestamppb.New(record.AcceptedAt()), RequestId: record.RequestID().String(), Phase: api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_ACCEPTED}, nil
}

func integrationRequestOperation(home apitypes.HomeName, record storage.ConfigurationAPIRequestRecord, target apitypes.IntegrationInstanceName, failure *apierrors.Fault) (*longrunningpb.Operation, error) {
	name, err := configurationOperationName(home, record.ID())
	if err != nil {
		return nil, err
	}
	metadata, err := integrationRequestMetadata(record, target)
	if err != nil {
		return nil, err
	}
	if failure != nil {
		metadata.Phase = api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_FAILED
	}
	packed, err := anypb.New(metadata)
	if err != nil {
		return nil, err
	}
	operation := &longrunningpb.Operation{Name: name.String(), Metadata: packed}
	if failure != nil {
		operation.Done = true
		operation.Result = &longrunningpb.Operation_Error{Error: failure.Status()}
	}
	if contracts.AdmitIntegrationOperation(operation, name) != nil {
		return nil, integrationResourceIntegrity()
	}
	return operation, nil
}

func integrationNoChangeEncoder(home apitypes.HomeName, target apitypes.IntegrationInstanceName, definition *api.IntegrationInstance) (func(storage.ConfigurationAPIRequestRecord, time.Time) ([]byte, error), error) {
	if !target.Valid() || target.Home() != home || contracts.AdmitIntegrationInstance(definition, home) != nil || definition.Name != target.String() || definition.ObservedHealth != nil || definition.ObservedSupervision != nil {
		return nil, integrationResourceIntegrity()
	}
	// Freeze the prepared original projection; no caller can mutate it while the
	// native owner waits for the exact credential/source/workspace fence.
	captured := proto.Clone(definition).(*api.IntegrationInstance)
	packedResult, err := anypb.New(captured)
	if err != nil {
		return nil, err
	}
	return func(record storage.ConfigurationAPIRequestRecord, at time.Time) ([]byte, error) {
		nativeTarget, present := record.IntegrationTarget()
		if !present || nativeTarget.Method() != storage.ConfigurationUpdateIntegration {
			return nil, integrationResourceIntegrity()
		}
		key, err := nativeTarget.Instance().Value()
		if err != nil || key != captured.InstanceKey {
			return nil, integrationResourceIntegrity()
		}
		name, err := configurationOperationName(home, record.ID())
		if err != nil {
			return nil, err
		}
		metadata, err := integrationRequestMetadata(record, target)
		if err != nil {
			return nil, err
		}
		metadata.Phase = api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_NO_CHANGE
		metadata.ValidationTime = timestamppb.New(at)
		packedMetadata, err := anypb.New(metadata)
		if err != nil {
			return nil, err
		}
		operation := &longrunningpb.Operation{Name: name.String(), Metadata: packedMetadata, Done: true, Result: &longrunningpb.Operation_Response{Response: packedResult}}
		// Admit the entire encoded Operation, including both Any wrappers, before
		// the native transaction appends its immutable NoChange receipt.
		if contracts.AdmitIntegrationOperation(operation, name) != nil {
			return nil, integrationResourceIntegrity()
		}
		return (proto.MarshalOptions{Deterministic: true}).Marshal(operation)
	}, nil
}

// Stored receipts are checked against the durable original reservation, never
// reconstructed from today's configuration or observation projection.
func integrationRequestRead(home apitypes.HomeName, record storage.ConfigurationAPIRequestRecord, target apitypes.IntegrationInstanceName) (*longrunningpb.Operation, error) {
	name, err := configurationOperationName(home, record.ID())
	if err != nil {
		return nil, err
	}
	data, capturedAt, noChange := record.NoChange()
	if !noChange {
		var failed bool
		data, failed = record.Failure()
		if !failed {
			return integrationRequestOperation(home, record, target, nil)
		}
		if len(data) > storage.ConfigurationAPIFailureBytes {
			return nil, integrationResourceIntegrity()
		}
	}
	if len(data) == 0 || len(data) > storage.ConfigOperationResultLimit {
		return nil, integrationResourceIntegrity()
	}
	operation := new(longrunningpb.Operation)
	if proto.Unmarshal(data, operation) != nil || contracts.AdmitIntegrationOperation(operation, name) != nil || !operation.Done {
		return nil, integrationResourceIntegrity()
	}
	metadata := new(api.IntegrationOperationMetadata)
	kind, err := integrationMutationKind(record.Method())
	if err != nil {
		return nil, err
	}
	if anypb.UnmarshalTo(operation.Metadata, metadata, proto.UnmarshalOptions{}) != nil || metadata.Kind != kind || metadata.RequestId != record.RequestID().String() || !metadata.CreateTime.AsTime().Equal(record.AcceptedAt()) {
		return nil, integrationResourceIntegrity()
	}
	// Create may have failed before any resource mapping existed. If a mapping
	// appeared later, that does not rewrite the original failed operation.
	if metadata.Target != "" && metadata.Target != target.String() {
		return nil, integrationResourceIntegrity()
	}
	if noChange {
		if metadata.Phase != api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_NO_CHANGE || !metadata.ValidationTime.AsTime().Equal(capturedAt) {
			return nil, integrationResourceIntegrity()
		}
		result := new(api.IntegrationInstance)
		nativeTarget, present := record.IntegrationTarget()
		key, keyErr := nativeTarget.Instance().Value()
		if !present || keyErr != nil || anypb.UnmarshalTo(operation.GetResponse(), result, proto.UnmarshalOptions{}) != nil || result.InstanceKey != key {
			return nil, integrationResourceIntegrity()
		}
	} else if metadata.Phase != api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_FAILED {
		return nil, integrationResourceIntegrity()
	}
	return operation, nil
}
