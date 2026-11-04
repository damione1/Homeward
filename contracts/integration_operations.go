package contracts

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	common "github.com/damione1/Homeward/contracts/apiv2"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
)

var ErrInvalidIntegrationOperation = errors.New("invalid integration operation")

// AdmitIntegrationOperation bounds the entire Operation before decoding its
// finite metadata/result vocabulary. It never relaxes configuration or physical
// operation admissions, and NoChange never claims a configuration commit.
func AdmitIntegrationOperation(raw *longrunningpb.Operation, expected apitypes.OperationName) error {
	invalid := ErrInvalidIntegrationOperation
	if common.AdmitOperationEnvelope(raw, expected) != nil {
		return invalid
	}
	text, prefixed := strings.CutPrefix(expected.ID(), "config-")
	decoded, err := hex.DecodeString(text)
	if !prefixed || len(text) != 32 || err != nil || hex.EncodeToString(decoded) != text || bytes.Equal(decoded, make([]byte, 16)) {
		return invalid
	}
	metadata := new(api.IntegrationOperationMetadata)
	if !configurationOperationPayload(metadata, raw.Metadata) || metadata.CreateTime == nil || metadata.CreateTime.CheckValid() != nil {
		return invalid
	}
	if metadata.RequestId != "" {
		if _, err := apitypes.ParseRequestID(metadata.RequestId); err != nil {
			return invalid
		}
	}
	switch metadata.Kind {
	case api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_CREATE, api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_UPDATE, api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_DELETE:
	default:
		return invalid
	}
	if metadata.Target != "" {
		target, err := apitypes.ParseIntegrationInstanceName(metadata.Target)
		if err != nil || target.Home() != expected.Home() {
			return invalid
		}
	} else if metadata.Kind != api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_CREATE {
		return invalid
	}
	noCommit := metadata.CommitTime == nil && metadata.Generation == "" && metadata.GenerationRevision == 0 && len(metadata.ManifestSha256) == 0
	switch metadata.Phase {
	case api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_ACCEPTED:
		if raw.Done || !noCommit || metadata.ValidationTime != nil {
			return invalid
		}
		return nil
	case api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_FAILED:
		if !raw.Done || !noCommit || metadata.ValidationTime != nil {
			return invalid
		}
		failure, ok := raw.Result.(*longrunningpb.Operation_Error)
		if !ok || failure == nil {
			return invalid
		}
		admitted, err := apierrors.AdmitStatus(failure.Error)
		if err != nil || admitted.Reason() == api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN || admitted.Reason() == api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN {
			return invalid
		}
		return nil
	case api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_NO_CHANGE:
		if !raw.Done || !noCommit || metadata.Kind != api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_UPDATE || metadata.ValidationTime == nil || metadata.ValidationTime.CheckValid() != nil || metadata.ValidationTime.AsTime().Before(metadata.CreateTime.AsTime()) {
			return invalid
		}
	case api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_COMMITTED, api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_PUBLISHED:
		if metadata.Target == "" || metadata.ValidationTime != nil || metadata.CommitTime == nil || metadata.CommitTime.CheckValid() != nil || metadata.CommitTime.AsTime().Before(metadata.CreateTime.AsTime()) || metadata.GenerationRevision < 1 || !configurationHash(metadata.ManifestSha256) || metadata.Generation != expected.Home().String()+"/configurationGenerations/g-"+strconv.FormatInt(metadata.GenerationRevision, 10) {
			return invalid
		}
		if raw.Done != (metadata.Phase == api.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_PUBLISHED) {
			return invalid
		}
		if !raw.Done {
			return nil
		}
	default:
		return invalid
	}
	response, ok := raw.Result.(*longrunningpb.Operation_Response)
	if !ok || response == nil || response.Response == nil {
		return invalid
	}
	if metadata.Kind == api.IntegrationMutationKind_INTEGRATION_MUTATION_KIND_DELETE {
		empty := new(emptypb.Empty)
		if response.Response.TypeUrl != "type.googleapis.com/google.protobuf.Empty" || anypb.UnmarshalTo(response.Response, empty, proto.UnmarshalOptions{}) != nil || !configurationKnown(empty.ProtoReflect()) {
			return invalid
		}
		return nil
	}
	result := new(api.IntegrationInstance)
	if response.Response.TypeUrl != "type.googleapis.com/home.api.v2.IntegrationInstance" || len(response.Response.Value) > 128<<10 || anypb.UnmarshalTo(response.Response, result, proto.UnmarshalOptions{}) != nil || AdmitIntegrationInstance(result, expected.Home()) != nil || result.Name != metadata.Target || result.ObservedHealth != nil || result.ObservedSupervision != nil {
		return invalid
	}
	return nil
}
