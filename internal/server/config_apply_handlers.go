package server

import (
	"context"
	"errors"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	configuration "github.com/damione1/Homeward/gen/home/api/v2"
	planwire "github.com/damione1/Homeward/gen/home/api/v2"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	configurationapplyv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/configplans"
	"github.com/damione1/Homeward/internal/storage"
)

type configApplyHandler struct {
	service    *configplans.Service
	documents  *configDocumentsHandler
	catalog    *configurationPlanCatalog
	operations *configurationOperationProvider
}

var _ configurationapplyv1connect.ConfigurationApplyServiceHandler = (*configApplyHandler)(nil)

func configApplyRPCError(ctx context.Context, err error, current *configuration.DocumentVersion) error {
	switch {
	case errors.Is(err, configplans.ErrStale), errors.Is(err, authorization.ErrConflict):
		err = configactivation.ErrWorkspacePreconditionConflict
	case errors.Is(err, configplans.ErrAuthorMismatch):
		err = authorization.ErrDenied
	case errors.Is(err, configplans.ErrInvalid), errors.Is(err, contracts.ErrInvalidConfigurationApplyContract):
		err = contracts.ErrInvalidConfigurationContract
	}
	return configDocumentsRPCError(ctx, err, current)
}

func configApplySourceExpected(values []*wire.SourcePrecondition, family planwire.ConfigurationPlanSourceFamily) ([]configdocuments.ExpectedSource, error) {
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
		if family == planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OWNERSHIP && strings.HasSuffix(value.LogicalPath, ".json") {
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

func (h *configApplyHandler) mutate(ctx context.Context, id *configuration.DocumentId, expected *configuration.DocumentVersion, workspace []byte, candidate configplans.Candidate, application configurationPlanApplication, sources []configdocuments.ExpectedSource, historical *configuration.ActiveGeneration, historicalIdentity string, historicalPolicyVersion *configuration.DocumentVersion, historicalFamily configplans.Family) (*longrunningpb.Operation, error) {
	if h == nil || h.service == nil || h.documents == nil || h.operations == nil {
		return nil, authorization.ErrUnavailable
	}
	binding, err := h.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil)
	if err != nil {
		return nil, err
	}
	base, err := configExpected(id, expected, workspace)
	if err != nil {
		return nil, err
	}
	account, err := binding.account.Value()
	if err != nil {
		return nil, err
	}
	author, err := storage.NewConfigAuthor(account)
	if err != nil {
		return nil, err
	}
	permit, err := h.documents.mutationPermit(ctx)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	if !permit.Ready() {
		return nil, authorization.ErrDenied
	}
	var result configactivation.Result
	if historicalPolicyVersion != nil {
		if historical != nil || historicalIdentity == "" || historicalFamily != configplans.Authorization {
			return nil, configplans.ErrInvalid
		}
		var hash [32]byte
		copy(hash[:], historicalPolicyVersion.SourceSha256)
		version, versionErr := authorization.NewVersion(uint64(historicalPolicyVersion.Revision), hash)
		if versionErr != nil {
			return nil, configplans.ErrInvalid
		}
		result, err = h.service.RollbackAuthorizationVersion(permit.Context(), historicalIdentity, base, sources, version, author)
	} else if historical == nil {
		receipt, receiptErr := application.nativeReceipt(candidate, binding)
		if receiptErr != nil {
			return nil, receiptErr
		}
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
			return nil, configplans.ErrUnavailable
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
			return nil, configplans.ErrUnavailable
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
			if result.Outcome() == configactivation.CommitUnknown || result.AttemptOutcome() == storage.TransactionIndeterminate {
				return nil, configactivation.ErrCommitIndeterminate
			}
			return h.operations.fromAdmittedPending(ctx, result, binding)
		}
	}
	expectedAdmission := &binding
	if selfPolicy && result.AttemptOutcome() == storage.TransactionCommitted && result.Outcome() == configactivation.Published {
		// A committed edit of this admin's policy intentionally changes the
		// rights binding. Recheck the same principal and current permission,
		// rather than requiring the policy digest we just replaced.
		if !permit.Ready() {
			return nil, authorization.ErrDenied
		}
		expectedAdmission = nil
	}
	currentBinding, admissionErr := h.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, expectedAdmission)
	if admissionErr != nil {
		return nil, admissionErr
	}
	if currentBinding.account != binding.account {
		return nil, authorization.ErrDenied
	}
	return h.operations.FromActivation(ctx, result, err)
}

func configApplyOperationalID() storage.ConfigDocumentID {
	id, _ := storage.NewConfigDocumentID("operational.main")
	return id
}

func (h *configApplyHandler) ApplyConfigurationPlan(ctx context.Context, request *connect.Request[wire.ApplyConfigurationPlanRequest]) (*connect.Response[longrunningpb.Operation], error) {
	if h == nil || h.documents == nil {
		return nil, configApplyRPCError(ctx, authorization.ErrUnavailable, nil)
	}
	binding, err := h.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil)
	if err != nil {
		return nil, configApplyRPCError(ctx, err, nil)
	}
	if request == nil || contracts.AdmitConfigurationApplyRequest(request.Msg) != nil {
		return nil, configApplyRPCError(ctx, contracts.ErrInvalidConfigurationApplyContract, nil)
	}
	ctx, replay, err := h.documents.prepareConfigurationRequest(ctx, storage.ConfigurationApplyPlan, request.Msg.RequestId, request.Msg)
	if err != nil {
		return nil, configApplyRPCError(ctx, err, nil)
	}
	if replay != nil {
		return connect.NewResponse(replay), nil
	}
	name, err := apitypes.ParseConfigurationPlanName(request.Msg.Name)
	if err != nil {
		return nil, configApplyRPCError(ctx, configplans.ErrInvalid, nil)
	}
	if err := h.documents.names.parent(ctx, name.Home().String()); err != nil {
		return nil, configApplyRPCError(ctx, err, nil)
	}
	application, err := h.catalog.forApplication(name, binding, request.Msg.Etag)
	if err != nil {
		return nil, configApplyRPCError(ctx, err, nil)
	}
	candidate, err := configPlanCandidate(request.Msg.Candidate)
	if err != nil {
		return nil, configApplyRPCError(ctx, err, nil)
	}
	sources, err := configApplySourceExpected(request.Msg.Sources, request.Msg.Candidate.Family)
	if err != nil {
		return nil, configApplyRPCError(ctx, err, nil)
	}
	operation, err := h.mutate(ctx, &configuration.DocumentId{Value: "operational.main"}, request.Msg.Expected, request.Msg.ExpectedWorkspaceSha256, candidate, application, sources, nil, "", nil, candidate.Family())
	if err != nil {
		var rpc *connect.Error
		if errors.As(err, &rpc) {
			return nil, rpc
		}
		return nil, configApplyRPCError(ctx, err, nil)
	}
	return connect.NewResponse(operation), nil
}

func (h *configApplyHandler) RollbackConfiguration(ctx context.Context, request *connect.Request[wire.RollbackConfigurationRequest]) (*connect.Response[longrunningpb.Operation], error) {
	if h == nil || h.documents == nil {
		return nil, configApplyRPCError(ctx, authorization.ErrUnavailable, nil)
	}
	if _, err := h.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil); err != nil {
		return nil, configApplyRPCError(ctx, err, nil)
	}
	if request == nil || contracts.AdmitConfigurationApplyRequest(request.Msg) != nil {
		return nil, configApplyRPCError(ctx, contracts.ErrInvalidConfigurationApplyContract, nil)
	}
	ctx, replay, err := h.documents.prepareConfigurationRequest(ctx, storage.ConfigurationRollback, request.Msg.RequestId, request.Msg)
	if err != nil {
		return nil, configApplyRPCError(ctx, err, nil)
	}
	if replay != nil {
		return connect.NewResponse(replay), nil
	}
	historicalFamily := configplans.Operational
	if request.Msg.Family != planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_UNSPECIFIED {
		var err error
		historicalFamily, err = configPlanFamily(request.Msg.Family)
		if err != nil {
			return nil, configApplyRPCError(ctx, err, nil)
		}
	}
	sources, err := configApplySourceExpected(request.Msg.Sources, request.Msg.Family)
	if err != nil {
		return nil, configApplyRPCError(ctx, err, nil)
	}
	nativeKey, err := h.documents.names.resolve(ctx, storage.APIConfigurationDocuments, request.Msg.Name)
	if err != nil {
		return nil, configApplyRPCError(ctx, err, nil)
	}
	operation, err := h.mutate(ctx, &configuration.DocumentId{Value: nativeKey}, request.Msg.Expected, request.Msg.ExpectedWorkspaceSha256, configplans.Candidate{}, configurationPlanApplication{}, sources, request.Msg.Historical, request.Msg.Identity, request.Msg.HistoricalPolicyVersion, historicalFamily)
	if err != nil {
		var rpc *connect.Error
		if errors.As(err, &rpc) {
			return nil, rpc
		}
		return nil, configApplyRPCError(ctx, err, nil)
	}
	return connect.NewResponse(operation), nil
}
