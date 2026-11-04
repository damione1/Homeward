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
)

var ErrInvalidConfigurationOperation = errors.New("invalid configuration operation")

func configurationOperationPayload(message proto.Message, raw *anypb.Any) bool {
	return raw != nil && raw.TypeUrl == "type.googleapis.com/"+string(message.ProtoReflect().Descriptor().FullName()) &&
		len(raw.Value) > 0 && len(raw.Value) <= 64<<10 && anypb.UnmarshalTo(raw, message, proto.UnmarshalOptions{}) == nil && configurationKnown(message.ProtoReflect())
}

// AdmitConfigurationOperation adds the configuration owner's exact Any types
// and publication invariants to the shared standard Operations envelope. A SQL
// commit stays pending until native publication is established. Configuration
// operations retain accepted reservations and confirmed precommit failures.
// An uncertain commit never becomes a terminal failure.
func AdmitConfigurationOperation(raw *longrunningpb.Operation, expected apitypes.OperationName) error {
	if common.AdmitOperationEnvelope(raw, expected) != nil {
		return ErrInvalidConfigurationOperation
	}
	attempt, prefixed := strings.CutPrefix(expected.ID(), "config-")
	decoded, err := hex.DecodeString(attempt)
	if !prefixed || len(attempt) != 32 || err != nil || hex.EncodeToString(decoded) != attempt || bytes.Equal(decoded, make([]byte, 16)) {
		return ErrInvalidConfigurationOperation
	}
	metadata := new(api.ConfigurationOperationMetadata)
	if !configurationOperationPayload(metadata, raw.Metadata) || metadata.CreateTime == nil || metadata.CreateTime.CheckValid() != nil {
		return ErrInvalidConfigurationOperation
	}
	if metadata.RequestId != "" {
		if _, err := apitypes.ParseRequestID(metadata.RequestId); err != nil {
			return ErrInvalidConfigurationOperation
		}
	}
	switch metadata.Phase {
	case api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_ACCEPTED, api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_FAILED:
		if metadata.RequestId == "" || metadata.CommitTime != nil || metadata.Generation != "" || metadata.GenerationRevision != 0 || len(metadata.ManifestSha256) != 0 {
			return ErrInvalidConfigurationOperation
		}
		if metadata.Phase == api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_ACCEPTED {
			if raw.Done {
				return ErrInvalidConfigurationOperation
			}
			return nil
		}
		failure, ok := raw.Result.(*longrunningpb.Operation_Error)
		if !raw.Done || !ok || failure == nil {
			return ErrInvalidConfigurationOperation
		}
		admitted, err := apierrors.AdmitStatus(failure.Error)
		if err != nil || admitted.Reason() == api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN || admitted.Reason() == api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN {
			return ErrInvalidConfigurationOperation
		}
		return nil
	case api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_COMMITTED, api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_PUBLISHED:
		if metadata.CommitTime == nil || metadata.CommitTime.CheckValid() != nil || metadata.CommitTime.AsTime().Before(metadata.CreateTime.AsTime()) || metadata.GenerationRevision <= 0 || !configurationHash(metadata.ManifestSha256) || metadata.Generation != expected.Home().String()+"/configurationGenerations/g-"+strconv.FormatInt(metadata.GenerationRevision, 10) {
			return ErrInvalidConfigurationOperation
		}
	default:
		return ErrInvalidConfigurationOperation
	}
	if !raw.Done {
		if metadata.Phase != api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_COMMITTED {
			return ErrInvalidConfigurationOperation
		}
		return nil
	}
	if metadata.Phase != api.ConfigurationOperationPhase_CONFIGURATION_OPERATION_PHASE_PUBLISHED {
		return ErrInvalidConfigurationOperation
	}
	response, ok := raw.Result.(*longrunningpb.Operation_Response)
	result := new(api.ConfigurationActivationResult)
	if !ok || response == nil || !configurationOperationPayload(result, response.Response) || result.Generation != metadata.Generation ||
		result.GenerationRevision != metadata.GenerationRevision || !bytes.Equal(result.ManifestSha256, metadata.ManifestSha256) {
		return ErrInvalidConfigurationOperation
	}
	return nil
}
