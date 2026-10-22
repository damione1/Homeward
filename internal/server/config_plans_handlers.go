package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/gen/home/configurationplan/v1/configurationplanv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configplans"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

type configPlansHandler struct {
	service   *configplans.Service
	documents *configDocumentsHandler
}

var _ configurationplanv1connect.ConfigurationPlanServiceHandler = (*configPlansHandler)(nil)

func configPlanError(err error) error {
	status := connect.CodeUnavailable
	switch {
	case errors.Is(err, configplans.ErrInvalid), errors.Is(err, contracts.ErrInvalidConfigurationPlanContract):
		status = connect.CodeInvalidArgument
	case errors.Is(err, configplans.ErrStale):
		status = connect.CodeAborted
	case errors.Is(err, authorization.ErrDenied):
		status = connect.CodePermissionDenied
	case errors.Is(err, context.Canceled):
		status = connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		status = connect.CodeDeadlineExceeded
	}
	return connect.NewError(status, errors.New("configuration plan request rejected"))
}
func configPlanFamily(value wire.SourceFamily) (configplans.Family, error) {
	var family configplans.Family
	switch value {
	case wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL:
		family = configplans.Operational
	case wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1:
		family = configplans.Declarative1
	case wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2:
		family = configplans.Declarative2
	case wire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION:
		family = configplans.Authorization
	case wire.SourceFamily_SOURCE_FAMILY_OWNERSHIP:
		family = configplans.Ownership
	case wire.SourceFamily_SOURCE_FAMILY_DASHBOARD:
		family = configplans.Dashboard
	case wire.SourceFamily_SOURCE_FAMILY_AUTOMATION:
		family = configplans.Automation
	case wire.SourceFamily_SOURCE_FAMILY_SCRIPT:
		family = configplans.Script
	case wire.SourceFamily_SOURCE_FAMILY_SCENE:
		family = configplans.Scene
	case wire.SourceFamily_SOURCE_FAMILY_UNSPECIFIED:
		return family, configplans.ErrInvalid
	default:
		return family, configplans.ErrInvalid
	}
	return family, nil
}

func configPlanCandidate(value *wire.Candidate) (configplans.Candidate, error) {
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
func configPlanWire(value configplans.Plan) *wire.Plan {
	source, semantic, manifest, workspace := value.SourceSHA256(), value.SemanticSHA256(), value.ManifestSHA256(), value.WorkspaceSHA256()
	base := value.Base()
	out := &wire.Plan{State: planStateWire(value.State()), Author: value.Author(), Family: planFamilyWire(value.Family()), Identity: value.Identity(), SchemaVersion: value.Schema(), SourceSha256: append([]byte(nil), source[:]...), Base: &wire.Base{Kind: planBaseWire(base.Kind())}, GenerationRevision: value.Generation(), ManifestSha256: append([]byte(nil), manifest[:]...), ObservedWorkspaceSha256: append([]byte(nil), workspace[:]...), Effect: planEffectWire(value.Effect()), Receipt: value.Receipt(), ApplicationAvailable: value.ApplicationAvailable()}
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
		out.Changes = append(out.Changes, &wire.Change{Effect: planEffectWire(change.Effect()), Operation: planOperationWire(change.Operation()), Field: change.Field(), ReferencesAdded: change.ReferencesAdded(), ReferencesRemoved: change.ReferencesRemoved()})
	}
	for _, d := range value.Diagnostics() {
		out.Diagnostics = append(out.Diagnostics, planDiagnosticWire(d))
	}
	return out
}
func configPlanResponse[T any](message *T) (*connect.Response[T], error) {
	wireMessage, ok := any(message).(proto.Message)
	if !ok || contracts.AdmitConfigurationPlanResponse(wireMessage) != nil {
		return nil, configPlanError(configplans.ErrUnavailable)
	}
	return connect.NewResponse(message), nil
}
func (h *configPlansHandler) admit(ctx context.Context, expected *configDocumentsBinding) (configDocumentsBinding, error) {
	if h == nil || h.service == nil || h.documents == nil {
		return configDocumentsBinding{}, authorization.ErrUnavailable
	}
	return h.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, expected)
}
func (h *configPlansHandler) BuildPlan(ctx context.Context, request *connect.Request[wire.BuildPlanRequest]) (*connect.Response[wire.BuildPlanResponse], error) {
	if request == nil || contracts.AdmitConfigurationPlanRequest(request.Msg) != nil {
		return nil, configPlanError(configplans.ErrInvalid)
	}
	binding, e := h.admit(ctx, nil)
	if e != nil {
		return nil, configPlanError(e)
	}
	candidate, e := configPlanCandidate(request.Msg.Candidate)
	if e != nil {
		return nil, configPlanError(e)
	}
	name, e := binding.account.Value()
	if e != nil {
		return nil, configPlanError(configplans.ErrInvalid)
	}
	author, e := storage.NewConfigAuthor(name)
	if e != nil {
		return nil, configPlanError(e)
	}
	plan, e := h.service.Build(ctx, candidate, author)
	if e != nil {
		return nil, configPlanError(e)
	}
	if _, e := h.admit(ctx, &binding); e != nil {
		return nil, configPlanError(e)
	}
	return configPlanResponse(&wire.BuildPlanResponse{Plan: configPlanWire(plan)})
}
func (h *configPlansHandler) RevalidatePlan(ctx context.Context, request *connect.Request[wire.RevalidatePlanRequest]) (*connect.Response[wire.RevalidatePlanResponse], error) {
	if request == nil || contracts.AdmitConfigurationPlanRequest(request.Msg) != nil {
		return nil, configPlanError(configplans.ErrInvalid)
	}
	binding, e := h.admit(ctx, nil)
	if e != nil {
		return nil, configPlanError(e)
	}
	candidate, e := configPlanCandidate(request.Msg.Candidate)
	if e != nil {
		return nil, configPlanError(e)
	}
	plan, e := h.service.Revalidate(ctx, candidate, request.Msg.Receipt)
	if e != nil {
		return nil, configPlanError(e)
	}
	if _, e := h.admit(ctx, &binding); e != nil {
		return nil, configPlanError(e)
	}
	return configPlanResponse(&wire.RevalidatePlanResponse{Plan: configPlanWire(plan)})
}
