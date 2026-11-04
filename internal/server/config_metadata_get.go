package server

import (
	"context"
	"strconv"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func configurationRevisionWire(parent string, version storage.ConfigDocumentMetadata) *api.ConfigurationRevision {
	entry := &api.ConfigurationRevision{Name: parent + "/configurationRevisions/r-" + strconv.FormatInt(version.Ref().Revision().Number(), 10), Id: &api.DocumentId{Value: version.Ref().Document().String()}, Version: configVersion(version), Availability: api.VersionAvailability_VERSION_AVAILABILITY_PURGED}
	if version.Retained() {
		entry.Availability = api.VersionAvailability_VERSION_AVAILABILITY_RETAINED
		updated, _ := version.UpdatedAt()
		entry.UpdateTime = timestamppb.New(updated)
		entry.SchemaVersion = version.SchemaVersion()
	}
	return entry
}
func configurationGenerationWire(parent string, generation storage.ConfigGenerationMetadata) *api.ConfigurationGeneration {
	digest := generation.ManifestSHA256()
	availability := api.GenerationAvailability_GENERATION_AVAILABILITY_COMPLETE
	if generation.Availability() == storage.ConfigGenerationIncomplete {
		availability = api.GenerationAvailability_GENERATION_AVAILABILITY_PARTIALLY_PURGED
	}
	return &api.ConfigurationGeneration{Name: parent + "/configurationGenerations/g-" + strconv.FormatInt(generation.Revision().Number(), 10), Generation: &api.ActiveGeneration{Revision: generation.Revision().Number(), ManifestSha256: append([]byte(nil), digest[:]...)}, Availability: availability, CreateTime: timestamppb.New(generation.CreatedAt())}
}
func configurationSchemaWire(parent string, schema storage.ConfigSchemaMetadata) (*api.ConfigurationSchema, error) {
	name, err := configurationSchemaResourceName(parent, schema)
	if err != nil {
		return nil, err
	}
	return &api.ConfigurationSchema{Name: name, Kind: configDocumentKind(schema.Kind()), SchemaVersion: schema.SchemaVersion(), SyntaxProfile: schema.SyntaxProfile(), EditableOperationalWorkspace: schema.Kind() == storage.ConfigDocumentKindOperationalYAML}, nil
}

type configurationMetadataRead struct {
	handler  *configDocumentsHandler
	binding  configDocumentsBinding
	permit   *authorization.Permit
	retained bool
}

func (h *configDocumentsHandler) beginMetadataRead(ctx context.Context) (*configurationMetadataRead, error) {
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, err
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, err
	}
	object, err := authorization.AdministrativeObject(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION)
	if err != nil {
		return nil, err
	}
	permit, err := h.authority.Permit(ctx, principal, permission, object)
	if err != nil {
		return nil, err
	}
	return &configurationMetadataRead{handler: h, binding: binding, permit: permit}, nil
}
func (r *configurationMetadataRead) close() {
	if !r.retained {
		r.permit.Release()
	}
}
func (r *configurationMetadataRead) finish(ctx context.Context, message proto.Message) error {
	if contracts.AdmitConfigurationResponse(message) != nil {
		return authorization.ErrUnavailable
	}
	if _, err := r.handler.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &r.binding); err != nil {
		return err
	}
	if !r.permit.Ready() {
		return authorization.ErrDenied
	}
	retained, err := retainPublicResponse(ctx, r.permit.Context(), r.permit.Ready, r.permit.Release)
	if err == nil {
		r.retained = retained
	}
	return err
}

func (h *configDocumentsHandler) GetConfigurationRevision(ctx context.Context, request *connect.Request[api.GetConfigurationRevisionRequest]) (*connect.Response[api.ConfigurationRevision], error) {
	read, err := h.beginMetadataRead(ctx)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	defer read.close()
	ctx = read.permit.Context()
	if request == nil || contracts.AdmitConfigurationRequest(request.Msg) != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	name, err := apitypes.ParseConfigurationRevisionName(request.Msg.Name)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	native, err := h.names.resolve(ctx, storage.APIConfigurationDocuments, name.Parent().String())
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	id, err := storage.NewConfigDocumentID(native)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	revision, err := storage.NewConfigVersionRevision(name.Revision())
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	ref, err := storage.NewConfigVersionRef(id, revision)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	version, err := h.service.Store().ReadConfigVersionMetadata(ctx, ref)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	result := configurationRevisionWire(name.Parent().String(), version)
	if err := read.finish(ctx, result); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	return connect.NewResponse(result), nil
}
func (h *configDocumentsHandler) GetConfigurationGeneration(ctx context.Context, request *connect.Request[api.GetConfigurationGenerationRequest]) (*connect.Response[api.ConfigurationGeneration], error) {
	read, err := h.beginMetadataRead(ctx)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	defer read.close()
	ctx = read.permit.Context()
	if request == nil || contracts.AdmitConfigurationRequest(request.Msg) != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	name, err := apitypes.ParseConfigurationGenerationName(request.Msg.Name)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	if err := h.names.parent(ctx, name.Parent().String()); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	revision, err := storage.NewConfigGenerationRevision(name.Revision())
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	generation, err := h.service.Store().ReadConfigGenerationMetadata(ctx, revision)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	result := configurationGenerationWire(name.Parent().String(), generation)
	if err := read.finish(ctx, result); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	return connect.NewResponse(result), nil
}
func (h *configDocumentsHandler) GetConfigurationSchema(ctx context.Context, request *connect.Request[api.GetConfigurationSchemaRequest]) (*connect.Response[api.ConfigurationSchema], error) {
	read, err := h.beginMetadataRead(ctx)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	defer read.close()
	ctx = read.permit.Context()
	if request == nil || contracts.AdmitConfigurationRequest(request.Msg) != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	name, err := apitypes.ParseConfigurationSchemaName(request.Msg.Name)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	if err := h.names.parent(ctx, name.Parent().String()); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	// This native SQL enum catalogue is hard-bounded to sixteen entries; it is
	// not a public paginated list or an alternate resource catalogue.
	schemas, err := h.service.Store().ReadConfigSchemas(ctx)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	for _, schema := range schemas {
		result, err := configurationSchemaWire(name.Parent().String(), schema)
		if err != nil {
			return nil, configDocumentsRPCError(ctx, err, nil)
		}
		if result.Name != name.String() {
			continue
		}
		if err := read.finish(ctx, result); err != nil {
			return nil, configDocumentsRPCError(ctx, err, nil)
		}
		return connect.NewResponse(result), nil
	}
	return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND))
}
