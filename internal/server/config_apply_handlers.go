package server

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	"github.com/damione1/Homeward/gen/home/configurationapply/v1/configurationapplyv1connect"
	planwire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/configplans"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

type configApplyHandler struct {
	service   *configplans.Service
	documents *configDocumentsHandler
}

var _ configurationapplyv1connect.ConfigurationApplyServiceHandler = (*configApplyHandler)(nil)

func configApplyRPCError(err error, current *configuration.DocumentVersion) error {
	switch {
	case errors.Is(err, configplans.ErrStale), errors.Is(err, authorization.ErrConflict):
		err = configactivation.ErrWorkspacePreconditionConflict
	case errors.Is(err, configplans.ErrAuthorMismatch):
		err = authorization.ErrDenied
	case errors.Is(err, configplans.ErrInvalid), errors.Is(err, contracts.ErrInvalidConfigurationApplyContract):
		err = contracts.ErrInvalidConfigurationContract
	}
	return configDocumentsRPCError(err, current)
}

func configApplyResponse[T any](value *T) (*connect.Response[T], error) {
	message, ok := any(value).(proto.Message)
	if !ok || contracts.AdmitConfigurationApplyResponse(message) != nil {
		return nil, configApplyRPCError(configplans.ErrUnavailable, nil)
	}
	return connect.NewResponse(value), nil
}

func configApplySourceExpected(values []*wire.SourcePrecondition, family planwire.SourceFamily) ([]configdocuments.ExpectedSource, error) {
	result := make([]configdocuments.ExpectedSource, 0, len(values))
	for _, value := range values {
		if value == nil || value.Id == nil {
			return nil, configplans.ErrInvalid
		}
		id, err := storage.NewConfigDocumentID(value.Id.Value)
		if err != nil {
			return nil, configplans.ErrInvalid
		}
		var binding storage.ConfigDocumentBinding
		if family == planwire.SourceFamily_SOURCE_FAMILY_OWNERSHIP && strings.HasSuffix(value.LogicalPath, ".json") {
			binding, err = storage.NewJSONConfigDocumentBinding(id, value.LogicalPath)
		} else {
			binding, err = storage.NewConfigDocumentBinding(id, value.LogicalPath)
		}
		if err != nil {
			return nil, configplans.ErrInvalid
		}
		workspace := configdocuments.ExpectAbsentMember()
		if hash := value.GetWorkspaceSha256(); hash != nil {
			var observed [32]byte
			copy(observed[:], hash)
			workspace, err = configdocuments.ExpectExistingMember(observed)
			if err != nil {
				return nil, configplans.ErrInvalid
			}
		}
		var revision storage.ConfigVersionRevision
		var source [32]byte
		absent := value.GetSqlAbsent() != nil
		if existing := value.GetExisting(); existing != nil {
			revision, err = storage.NewConfigVersionRevision(existing.Revision)
			if err != nil {
				return nil, configplans.ErrInvalid
			}
			copy(source[:], existing.SourceSha256)
		}
		expected, err := configdocuments.NewExpectedSource(binding, revision, source, absent, workspace)
		if err != nil {
			return nil, configplans.ErrInvalid
		}
		result = append(result, expected)
	}
	return result, nil
}

func (h *configApplyHandler) mutate(ctx context.Context, id *configuration.DocumentId, expected *configuration.DocumentVersion, workspace []byte, candidate configplans.Candidate, receipt string, sources []configdocuments.ExpectedSource, historical *configuration.ActiveGeneration, historicalIdentity string, historicalPolicyVersion *configuration.DocumentVersion, historicalFamily configplans.Family) (*configuration.ActivationResult, *configuration.ReadDocumentResponse, error) {
	if h == nil || h.service == nil || h.documents == nil {
		return nil, nil, authorization.ErrUnavailable
	}
	binding, err := h.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil)
	if err != nil {
		return nil, nil, err
	}
	base, err := configExpected(id, expected, workspace)
	if err != nil {
		return nil, nil, err
	}
	account, err := binding.account.Value()
	if err != nil {
		return nil, nil, err
	}
	author, err := storage.NewConfigAuthor(account)
	if err != nil {
		return nil, nil, err
	}
	permit, err := h.documents.mutationPermit(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer permit.Release()
	if !permit.Ready() {
		return nil, nil, authorization.ErrDenied
	}
	var result configactivation.Result
	if historicalPolicyVersion != nil {
		if historical != nil || historicalIdentity == "" || historicalFamily != configplans.Authorization {
			return nil, nil, configplans.ErrInvalid
		}
		var hash [32]byte
		copy(hash[:], historicalPolicyVersion.SourceSha256)
		version, versionErr := authorization.NewVersion(uint64(historicalPolicyVersion.Revision), hash)
		if versionErr != nil {
			return nil, nil, configplans.ErrInvalid
		}
		result, err = h.service.RollbackAuthorizationVersion(permit.Context(), historicalIdentity, base, sources, version, author)
	} else if historical == nil {
		switch candidate.Family() {
		case configplans.Operational:
			result, err = h.service.Apply(permit.Context(), candidate, receipt, base, author)
		case configplans.Authorization:
			result, err = h.service.ApplyAuthorization(permit.Context(), candidate, receipt, base, sources, author)
		case configplans.Declarative1, configplans.Declarative2:
			result, err = h.service.ApplyDefinitions(permit.Context(), candidate, receipt, base, sources, author)
		case configplans.Dashboard, configplans.Automation, configplans.Script, configplans.Scene, configplans.Ownership:
			result, err = h.service.ApplySources(permit.Context(), candidate, receipt, base, sources, author)
		default:
			return nil, nil, configplans.ErrUnavailable
		}
	} else {
		generation, _ := storage.NewConfigGenerationRevision(historical.Revision)
		var manifest [32]byte
		copy(manifest[:], historical.ManifestSha256)
		switch historicalFamily {
		case configplans.Authorization:
			result, err = h.service.RollbackAuthorization(permit.Context(), historicalIdentity, base, sources, generation, manifest, author)
		case configplans.Dashboard, configplans.Automation, configplans.Script, configplans.Scene, configplans.Ownership:
			result, err = h.service.RollbackSources(permit.Context(), historicalFamily, historicalIdentity, base, sources, generation, manifest, author)
		case configplans.Declarative1, configplans.Declarative2:
			result, err = h.service.RollbackDefinitions(permit.Context(), historicalFamily, historicalIdentity, base, sources, generation, manifest, author)
		case configplans.Operational:
			result, err = h.service.Rollback(permit.Context(), base, generation, manifest, author)
		default:
			return nil, nil, configplans.ErrUnavailable
		}
	}
	selfPolicy := candidate.Family() == configplans.Authorization && candidate.Identity() == account
	if historical != nil || historicalPolicyVersion != nil {
		selfPolicy = historicalFamily == configplans.Authorization && historicalIdentity == account
	}
	if selfPolicy {
		switch result.Outcome() {
		case configactivation.CommitUnknown, configactivation.CommittedReconciling, configactivation.RestartRequired:
			// The owner retains this account's policy barrier while reconciling.
			// Re-entering authorization here would wait on our own reservation.
			// Return only this admitted attempt's outcome, without a new snapshot.
			activation := configActivationResult(result, err)
			if activation == nil {
				return nil, nil, authorization.ErrUnavailable
			}
			return activation, nil, nil
		}
	}
	expectedAdmission := &binding
	if selfPolicy && result.AttemptOutcome() == storage.TransactionCommitted && result.Outcome() == configactivation.Published {
		// A committed edit of this admin's policy intentionally changes the
		// rights binding. Recheck the same principal and current permission,
		// rather than requiring the policy digest we just replaced.
		if !permit.Ready() {
			return nil, nil, authorization.ErrDenied
		}
		expectedAdmission = nil
	}
	currentBinding, admissionErr := h.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, expectedAdmission)
	if admissionErr != nil {
		return nil, nil, admissionErr
	}
	if currentBinding.account != binding.account {
		return nil, nil, authorization.ErrDenied
	}
	current, readErr := h.documents.service.Read(ctx, configApplyOperationalID())
	var currentWire *configuration.ReadDocumentResponse
	var version *configuration.DocumentVersion
	if readErr == nil {
		currentWire = h.documents.snapshotWire(current)
		version = currentWire.Document.Version
	}
	var workspaceError *configdocuments.Error
	if result.Outcome() == configactivation.OutcomeInvalid || result.AttemptOutcome() == storage.TransactionNotCommitted && (errors.Is(err, configactivation.ErrWorkspacePreconditionConflict) || errors.Is(err, authorization.ErrConflict) || errors.Is(err, config.ErrStartupSourceChanged) || errors.As(err, &workspaceError)) {
		return nil, nil, configApplyRPCError(err, version)
	}
	activation := configActivationResult(result, err)
	if activation == nil {
		return nil, nil, authorization.ErrUnavailable
	}
	return activation, currentWire, nil
}

func configApplyOperationalID() storage.ConfigDocumentID {
	id, _ := storage.NewConfigDocumentID("operational.main")
	return id
}

func (h *configApplyHandler) Apply(ctx context.Context, request *connect.Request[wire.ApplyRequest]) (*connect.Response[wire.ApplyResponse], error) {
	if request == nil || contracts.AdmitConfigurationApplyRequest(request.Msg) != nil {
		return nil, configApplyRPCError(contracts.ErrInvalidConfigurationApplyContract, nil)
	}
	candidate, err := configPlanCandidate(request.Msg.Candidate)
	if err != nil {
		return nil, configApplyRPCError(err, nil)
	}
	sources, err := configApplySourceExpected(request.Msg.Sources, request.Msg.Candidate.Family)
	if err != nil {
		return nil, configApplyRPCError(err, nil)
	}
	activation, current, err := h.mutate(ctx, &configuration.DocumentId{Value: "operational.main"}, request.Msg.Expected, request.Msg.ExpectedWorkspaceSha256, candidate, request.Msg.GetReceipt(), sources, nil, "", nil, candidate.Family())
	if err != nil {
		var rpc *connect.Error
		if errors.As(err, &rpc) {
			return nil, rpc
		}
		return nil, configApplyRPCError(err, nil)
	}
	return configApplyResponse(&wire.ApplyResponse{Activation: activation, Current: current})
}

func (h *configApplyHandler) Rollback(ctx context.Context, request *connect.Request[wire.RollbackRequest]) (*connect.Response[wire.RollbackResponse], error) {
	if request == nil || contracts.AdmitConfigurationApplyRequest(request.Msg) != nil {
		return nil, configApplyRPCError(contracts.ErrInvalidConfigurationApplyContract, nil)
	}
	historicalFamily := configplans.Operational
	if request.Msg.Family != planwire.SourceFamily_SOURCE_FAMILY_UNSPECIFIED {
		var err error
		historicalFamily, err = configPlanFamily(request.Msg.Family)
		if err != nil {
			return nil, configApplyRPCError(err, nil)
		}
	}
	sources, err := configApplySourceExpected(request.Msg.Sources, request.Msg.Family)
	if err != nil {
		return nil, configApplyRPCError(err, nil)
	}
	activation, current, err := h.mutate(ctx, request.Msg.Id, request.Msg.Expected, request.Msg.ExpectedWorkspaceSha256, configplans.Candidate{}, "", sources, request.Msg.Historical, request.Msg.Identity, request.Msg.HistoricalPolicyVersion, historicalFamily)
	if err != nil {
		var rpc *connect.Error
		if errors.As(err, &rpc) {
			return nil, rpc
		}
		return nil, configApplyRPCError(err, nil)
	}
	return configApplyResponse(&wire.RollbackResponse{Activation: activation, Current: current})
}
