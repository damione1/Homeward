package server

import (
	"strconv"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func integrationCommittedOperation(home apitypes.HomeName, target apitypes.IntegrationInstanceName, attempt storage.ConfigActivationAttempt) (*longrunningpb.Operation, error) {
	nativeTarget, present := attempt.IntegrationTarget()
	if !present || !target.Valid() || target.Home() != home || !attempt.Target().Valid() || attempt.ManifestSHA256() == ([32]byte{}) {
		return nil, integrationResourceIntegrity()
	}
	kind, err := integrationMutationKind(nativeTarget.Method())
	if err != nil {
		return nil, err
	}
	name, err := configurationOperationName(home, attempt.ID())
	if err != nil {
		return nil, err
	}
	digest := attempt.ManifestSHA256()
	metadata := &api.IntegrationOperationMetadata{Kind: kind, Target: target.String(), CreateTime: timestamppb.New(attempt.CreateTime()), CommitTime: timestamppb.New(attempt.CommittedAt()), Phase: api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_COMMITTED, Generation: home.String() + "/configurationGenerations/g-" + strconv.FormatInt(attempt.Target().Number(), 10), GenerationRevision: attempt.Target().Number(), ManifestSha256: append([]byte(nil), digest[:]...)}
	if id, present := attempt.APIRequestID(); present {
		metadata.RequestId = id.String()
	}
	packed, err := anypb.New(metadata)
	if err != nil {
		return nil, err
	}
	operation := &longrunningpb.Operation{Name: name.String(), Metadata: packed}
	if contracts.AdmitIntegrationOperation(operation, name) != nil {
		return nil, integrationResourceIntegrity()
	}
	return operation, nil
}

func integrationPublishedOperation(home apitypes.HomeName, target apitypes.IntegrationInstanceName, attempt storage.ConfigActivationAttempt, proof configactivation.PublicationEvidence, definition *api.IntegrationInstance) (*longrunningpb.Operation, error) {
	if !proof.Valid() || proof.Attempt() != attempt.ID() || proof.Target() != attempt.Target() || proof.ManifestSHA256() != attempt.ManifestSHA256() {
		return nil, integrationResourceIntegrity()
	}
	operation, err := integrationCommittedOperation(home, target, attempt)
	if err != nil {
		return nil, err
	}
	nativeTarget, present := attempt.IntegrationTarget()
	key, keyErr := nativeTarget.Instance().Value()
	if !present || keyErr != nil {
		return nil, integrationResourceIntegrity()
	}
	var result proto.Message
	if nativeTarget.Method() == storage.ConfigurationDeleteIntegration {
		if definition != nil {
			return nil, integrationResourceIntegrity()
		}
		result = &emptypb.Empty{}
	} else {
		if contracts.AdmitIntegrationInstance(definition, home) != nil || definition.Name != target.String() || definition.InstanceKey != key || definition.ObservedHealth != nil || definition.ObservedSupervision != nil {
			return nil, integrationResourceIntegrity()
		}
		result = definition
	}
	metadata := new(api.IntegrationOperationMetadata)
	if anypb.UnmarshalTo(operation.Metadata, metadata, proto.UnmarshalOptions{}) != nil {
		return nil, integrationResourceIntegrity()
	}
	metadata.Phase = api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_PUBLISHED
	operation.Metadata, err = anypb.New(metadata)
	if err != nil {
		return nil, err
	}
	packed, err := anypb.New(result)
	if err != nil {
		return nil, err
	}
	operation.Done = true
	operation.Result = &longrunningpb.Operation_Response{Response: packed}
	name, err := configurationOperationName(home, attempt.ID())
	if err != nil || contracts.AdmitIntegrationOperation(operation, name) != nil {
		return nil, integrationResourceIntegrity()
	}
	return operation, nil
}

func admitIntegrationPublishedOperation(data []byte, home apitypes.HomeName, target apitypes.IntegrationInstanceName, attempt storage.ConfigActivationAttempt) (*longrunningpb.Operation, error) {
	if len(data) == 0 || len(data) > storage.ConfigOperationResultLimit {
		return nil, integrationResourceIntegrity()
	}
	expected, err := integrationCommittedOperation(home, target, attempt)
	if err != nil {
		return nil, err
	}
	name, err := configurationOperationName(home, attempt.ID())
	if err != nil {
		return nil, err
	}
	operation := new(longrunningpb.Operation)
	if proto.Unmarshal(data, operation) != nil || contracts.AdmitIntegrationOperation(operation, name) != nil || !operation.Done {
		return nil, integrationResourceIntegrity()
	}
	actualMetadata, expectedMetadata := new(api.IntegrationOperationMetadata), new(api.IntegrationOperationMetadata)
	if anypb.UnmarshalTo(operation.Metadata, actualMetadata, proto.UnmarshalOptions{}) != nil || anypb.UnmarshalTo(expected.Metadata, expectedMetadata, proto.UnmarshalOptions{}) != nil || actualMetadata.Phase != api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_PUBLISHED {
		return nil, integrationResourceIntegrity()
	}
	expectedMetadata.Phase = api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_PUBLISHED
	if !proto.Equal(actualMetadata, expectedMetadata) {
		return nil, integrationResourceIntegrity()
	}
	// The exact terminal bytes (including original etag/configuration) are
	// returned; only immutable attempt identity is checked, never current state.
	nativeTarget, _ := attempt.IntegrationTarget()
	if nativeTarget.Method() != storage.ConfigurationDeleteIntegration {
		definition := new(api.IntegrationInstance)
		key, err := nativeTarget.Instance().Value()
		if err != nil || anypb.UnmarshalTo(operation.GetResponse(), definition, proto.UnmarshalOptions{}) != nil || definition.InstanceKey != key {
			return nil, integrationResourceIntegrity()
		}
	}
	return operation, nil
}
