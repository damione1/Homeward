package server

import (
	"context"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	configurationplanv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configplans"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

type configPlansHandler struct {
	service   *configplans.Service
	documents *configDocumentsHandler
	catalog   *configurationPlanCatalog
}

var _ configurationplanv1connect.ConfigurationPlanServiceHandler = (*configPlansHandler)(nil)

func configPlanError(ctx context.Context, err error) error {
	return apiRPCError(ctx, administrationFault(err))
}
func configPlanFamily(value wire.ConfigurationPlanSourceFamily) (configplans.Family, error) {
	var family configplans.Family
	switch value {
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL:
		family = configplans.Operational
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V1:
		family = configplans.Declarative1
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V2:
		family = configplans.Declarative2
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION:
		family = configplans.Authorization
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OWNERSHIP:
		family = configplans.Ownership
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD:
		family = configplans.Dashboard
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTOMATION:
		family = configplans.Automation
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCRIPT:
		family = configplans.Script
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCENE:
		family = configplans.Scene
	case wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_UNSPECIFIED:
		return family, configplans.ErrInvalid
	default:
		return family, configplans.ErrInvalid
	}
	return family, nil
}

func configPlanCandidate(value *wire.ConfigurationPlanCandidate) (configplans.Candidate, error) {
	if value == nil {
		return configplans.Candidate{}, configplans.ErrInvalid
	}
	family, err := configPlanFamily(value.Family)
	if err != nil {
		return configplans.Candidate{}, err
	}
	files := make([]parser.SourceFile, 0, len(value.Files))
	for _, file := range value.Files {
		if file == nil {
			return configplans.Candidate{}, configplans.ErrInvalid
		}
		admitted, e := configPlanSourceFile(family, file.LogicalPath, file.ExactSource)
		if e != nil {
			return configplans.Candidate{}, configplans.ErrInvalid
		}
		files = append(files, admitted)
	}
	return configplans.NewCandidate(family, value.Identity, value.Entry, files)
}
func configPlanWire(value configplans.Plan, author apitypes.AccountName) *wire.ConfigurationPlan {
	source, semantic, manifest, workspace := value.SourceSHA256(), value.SemanticSHA256(), value.ManifestSHA256(), value.WorkspaceSHA256()
	base := value.Base()
	out := &wire.ConfigurationPlan{State: planStateWire(value.State()), Author: author.String(), Family: planFamilyWire(value.Family()), Identity: value.Identity(), SchemaVersion: value.Schema(), SourceSha256: append([]byte(nil), source[:]...), Base: &wire.ConfigurationPlanBase{Kind: planBaseWire(base.Kind())}, GenerationRevision: value.Generation(), ManifestSha256: append([]byte(nil), manifest[:]...), ObservedWorkspaceSha256: append([]byte(nil), workspace[:]...), Effect: planEffectWire(value.Effect()), Receipt: value.Receipt(), ApplicationAvailable: value.ApplicationAvailable()}
	if semantic != ([32]byte{}) {
		out.SemanticSha256 = append([]byte(nil), semantic[:]...)
	}
	if base.Kind() == configplans.ActiveSQL {
		revision := base.Revision()
		out.Base.Revision = &revision
	}
	if hash := base.SourceSHA256(); hash != ([32]byte{}) {
		out.Base.SourceSha256 = append([]byte(nil), hash[:]...)
	}
	for _, change := range value.Changes() {
		if change.ReferencesAdded() > 4096 || change.ReferencesRemoved() > 4096 {
			return nil
		}
		out.Changes = append(out.Changes, &wire.ConfigurationPlanChange{Effect: planEffectWire(change.Effect()), Operation: planOperationWire(change.Operation()), Field: change.Field(), ReferencesAdded: int32(change.ReferencesAdded()), ReferencesRemoved: int32(change.ReferencesRemoved())})
	}
	for _, d := range value.Diagnostics() {
		out.Diagnostics = append(out.Diagnostics, planDiagnosticWire(d))
	}
	return out
}
func configPlanResponse[T any](ctx context.Context, message *T) (*connect.Response[T], error) {
	wireMessage, ok := any(message).(proto.Message)
	if !ok || contracts.AdmitConfigurationPlanResponse(wireMessage) != nil {
		return nil, configPlanError(ctx, configplans.ErrUnavailable)
	}
	return connect.NewResponse(message), nil
}
func (h *configPlansHandler) admit(ctx context.Context, expected *configDocumentsBinding) (configDocumentsBinding, error) {
	if h == nil || h.service == nil || h.documents == nil || h.catalog == nil || !h.catalog.valid {
		return configDocumentsBinding{}, authorization.ErrUnavailable
	}
	return h.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, expected)
}
func (h *configPlansHandler) CreateConfigurationPlan(ctx context.Context, request *connect.Request[wire.CreateConfigurationPlanRequest]) (*connect.Response[wire.ConfigurationPlan], error) {
	binding, err := h.admit(ctx, nil)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	if request == nil || contracts.AdmitConfigurationPlanRequest(request.Msg) != nil {
		return nil, configPlanError(ctx, configplans.ErrInvalid)
	}
	if err := h.documents.names.parent(ctx, request.Msg.Parent); err != nil {
		return nil, configPlanError(ctx, err)
	}
	home, err := apitypes.ParseHomeName(request.Msg.Parent)
	if err != nil {
		return nil, configPlanError(ctx, configplans.ErrInvalid)
	}
	var requestedID *storage.APIRequestedResourceID
	if request.Msg.ConfigurationPlanId != "" {
		id, err := storage.ParseAPIRequestedResourceID(request.Msg.ConfigurationPlanId)
		if err != nil {
			return nil, configPlanError(ctx, configplans.ErrInvalid)
		}
		requestedID = &id
	}
	ctx, err = configurationPlanMutationContext(ctx, configurationplanv1connect.ConfigurationPlanServiceCreateConfigurationPlanProcedure, request.Msg.RequestId, request.Msg)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	evidence, err := h.planMutationEvidence(ctx)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	if replay, present, err := h.probePlanMutation(ctx, evidence, binding); err != nil {
		return nil, configPlanError(ctx, err)
	} else if present {
		if _, err := h.admit(ctx, &binding); err != nil {
			return nil, configPlanError(ctx, err)
		}
		return configPlanResponse(ctx, replay)
	}
	resource, err := func() (*wire.ConfigurationPlan, error) {
		candidate, err := configPlanCandidate(request.Msg.ConfigurationPlan.Candidate)
		if err != nil {
			return nil, err
		}
		account, err := binding.account.Value()
		if err != nil {
			return nil, configplans.ErrInvalid
		}
		author, err := storage.NewConfigAuthor(account)
		if err != nil {
			return nil, err
		}
		// Build captures native SQL and Owner state. No ledger, catalogue or
		// current-rights SQL locks surround diagnostic compilation.
		plan, err := h.service.Build(ctx, candidate, author)
		if err != nil {
			return nil, err
		}
		if plan.State() == configplans.Stale {
			return nil, authorization.ErrUnavailable
		}
		expiry, err := h.service.ReceiptExpiration(plan.Receipt(), author, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		publicAuthor, err := (authResourceNames{h.documents.names}).account(ctx, binding.account)
		if err != nil {
			return nil, err
		}
		metadata := configPlanWire(plan, publicAuthor)
		if metadata == nil {
			return nil, storage.ErrAPIResourceIntegrity
		}
		return h.persistPlanMutation(ctx, evidence, binding, func() (configurationPlanRecord, error) {
			return h.catalog.prepareAddLocked(home, binding, metadata, expiry, requestedID)
		})
	}()
	if err != nil {
		resource, err = h.planMutationPreparationError(ctx, evidence, binding, err)
		if err != nil {
			return nil, configPlanError(ctx, err)
		}
	}
	if _, err := h.admit(ctx, &binding); err != nil {
		return nil, configPlanError(ctx, err)
	}
	return configPlanResponse(ctx, resource)
}

func (h *configPlansHandler) RevalidateConfigurationPlan(ctx context.Context, request *connect.Request[wire.RevalidateConfigurationPlanRequest]) (*connect.Response[wire.ConfigurationPlan], error) {
	binding, err := h.admit(ctx, nil)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	if request == nil || contracts.AdmitConfigurationPlanRequest(request.Msg) != nil {
		return nil, configPlanError(ctx, configplans.ErrInvalid)
	}
	name, err := apitypes.ParseConfigurationPlanName(request.Msg.Name)
	if err != nil {
		return nil, configPlanError(ctx, configplans.ErrInvalid)
	}
	if err := h.documents.names.parent(ctx, name.Home().String()); err != nil {
		return nil, configPlanError(ctx, err)
	}
	ctx, err = configurationPlanMutationContext(ctx, configurationplanv1connect.ConfigurationPlanServiceRevalidateConfigurationPlanProcedure, request.Msg.RequestId, request.Msg)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	evidence, err := h.planMutationEvidence(ctx)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	if replay, present, err := h.probePlanMutation(ctx, evidence, binding); err != nil {
		return nil, configPlanError(ctx, err)
	} else if present {
		if _, err := h.admit(ctx, &binding); err != nil {
			return nil, configPlanError(ctx, err)
		}
		return configPlanResponse(ctx, replay)
	}
	resource, err := func() (*wire.ConfigurationPlan, error) {
		current, err := h.catalog.get(name, binding)
		if err != nil {
			return nil, err
		}
		if configurationPlanResource(current).Etag != request.Msg.Etag {
			return nil, apierrors.New(wire.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH)
		}
		candidate, err := configPlanCandidate(request.Msg.Candidate)
		if err != nil {
			return nil, err
		}
		plan, err := h.service.Revalidate(ctx, candidate, current.metadata.Receipt)
		if err != nil {
			return nil, err
		}
		account, err := binding.account.Value()
		if err != nil || plan.Author() != account {
			return nil, authorization.ErrDenied
		}
		var expiry time.Time
		if plan.Receipt() != "" {
			author, err := storage.NewConfigAuthor(account)
			if err != nil {
				return nil, err
			}
			expiry, err = h.service.ReceiptExpiration(plan.Receipt(), author, time.Now().UTC())
			if err != nil {
				return nil, err
			}
		}
		publicAuthor, err := (authResourceNames{h.documents.names}).account(ctx, binding.account)
		if err != nil {
			return nil, err
		}
		metadata := configPlanWire(plan, publicAuthor)
		if metadata == nil {
			return nil, storage.ErrAPIResourceIntegrity
		}
		return h.persistPlanMutation(ctx, evidence, binding, func() (configurationPlanRecord, error) {
			return h.catalog.prepareReplaceLocked(name, binding, current.version, metadata, expiry)
		})
	}()
	if err != nil {
		resource, err = h.planMutationPreparationError(ctx, evidence, binding, err)
		if err != nil {
			return nil, configPlanError(ctx, err)
		}
	}
	if _, err := h.admit(ctx, &binding); err != nil {
		return nil, configPlanError(ctx, err)
	}
	return configPlanResponse(ctx, resource)
}
