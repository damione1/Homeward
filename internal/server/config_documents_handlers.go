package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	configurationv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type configDocumentsHandler struct {
	service    *configdocuments.Service
	authority  *authorization.Authority
	names      administrationNames
	operations *configurationOperationProvider
}

var _ configurationv1connect.ConfigurationServiceHandler = (*configDocumentsHandler)(nil)

type configDocumentsBinding struct {
	account contracts.AccountID
	digest  [32]byte
}

func configDocumentsResponse[T any](ctx context.Context, message *T) (*connect.Response[T], error) {
	wireMessage, ok := any(message).(proto.Message)
	if !ok || contracts.AdmitConfigurationResponse(wireMessage) != nil {
		return nil, configDocumentsRPCError(ctx, authorization.ErrUnavailable, nil)
	}
	return connect.NewResponse(message), nil
}
func configDocumentsSend(ctx context.Context, stream *connect.ServerStream[wire.WatchConfigurationResponse], message *wire.WatchConfigurationResponse) error {
	if contracts.AdmitConfigurationResponse(message) != nil {
		return configDocumentsRPCError(ctx, authorization.ErrUnavailable, nil)
	}
	return stream.Send(message)
}

func (h *configDocumentsHandler) admit(ctx context.Context, operation auth.ScopeOperation, expected *configDocumentsBinding) (configDocumentsBinding, error) {
	if h == nil || h.service == nil || h.authority == nil || nilReactiveInterface(ctx) {
		return configDocumentsBinding{}, authorization.ErrUnavailable
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || !principal.Native() && (operation != auth.ScopeOperation_SCOPE_OPERATION_READ || !mcpConfigurationReadPrincipal(ctx)) {
		return configDocumentsBinding{}, authorization.ErrDenied
	}
	actor, present := ActorFromContext(ctx)
	admitted, err := principal.Actor()
	if err != nil || !present || admitted != actor {
		return configDocumentsBinding{}, authorization.ErrDenied
	}
	account, err := principal.Account()
	if err != nil {
		return configDocumentsBinding{}, err
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, operation)
	if err != nil {
		return configDocumentsBinding{}, err
	}
	object, err := authorization.AdministrativeObject(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION)
	if err != nil {
		return configDocumentsBinding{}, err
	}
	var binding configDocumentsBinding
	err = h.authority.WithCurrent(ctx, principal, func(current authorization.Current) error {
		if err := current.Authorize(permission, object); err != nil {
			return err
		}
		revision, digest, err := current.Binding()
		if err != nil {
			return err
		}
		id, err := account.Value()
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, _ = hash.Write([]byte(id))
		_, _ = hash.Write(digest[:])
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], revision)
		_, _ = hash.Write(n[:])
		binding.account = account
		copy(binding.digest[:], hash.Sum(nil))
		if expected != nil && binding != *expected {
			return authorization.ErrDenied
		}
		return nil
	})
	return binding, err
}

func configDocumentsRPCError(ctx context.Context, err error, _ *wire.DocumentVersion) error {
	return apiRPCError(ctx, administrationFault(err))
}
func configDocumentKind(kind storage.ConfigDocumentKind) wire.DocumentKind {
	switch kind {
	case storage.ConfigDocumentKindYAMLSource:
		return wire.DocumentKind_DOCUMENT_KIND_YAML_SOURCE
	case storage.ConfigDocumentKindAuthorizationPolicy:
		return wire.DocumentKind_DOCUMENT_KIND_AUTHORIZATION_POLICY
	case storage.ConfigDocumentKindMFAPolicy:
		return wire.DocumentKind_DOCUMENT_KIND_MFA_POLICY
	case storage.ConfigDocumentKindOperationalYAML:
		return wire.DocumentKind_DOCUMENT_KIND_OPERATIONAL_YAML
	case storage.ConfigDocumentKindBootstrapLaunch:
		return wire.DocumentKind_DOCUMENT_KIND_BOOTSTRAP_LAUNCH
	case storage.ConfigDocumentKindDashboardSource:
		return wire.DocumentKind_DOCUMENT_KIND_DASHBOARD_SOURCE
	case storage.ConfigDocumentKindAutomationSource:
		return wire.DocumentKind_DOCUMENT_KIND_AUTOMATION_SOURCE
	case storage.ConfigDocumentKindScriptSource:
		return wire.DocumentKind_DOCUMENT_KIND_SCRIPT_SOURCE
	case storage.ConfigDocumentKindSceneSource:
		return wire.DocumentKind_DOCUMENT_KIND_SCENE_SOURCE
	case storage.ConfigDocumentKindOwnershipSource:
		return wire.DocumentKind_DOCUMENT_KIND_OWNERSHIP_SOURCE
	case storage.ConfigDocumentKindInvalid:
		return wire.DocumentKind_DOCUMENT_KIND_UNSPECIFIED
	default:
		return wire.DocumentKind_DOCUMENT_KIND_UNSPECIFIED
	}
}
func configDirty(state configdocuments.DirtyState) wire.DirtyState {
	switch state {
	case configdocuments.Clean:
		return wire.DirtyState_DIRTY_STATE_CLEAN
	case configdocuments.DirtyUnvalidated:
		return wire.DirtyState_DIRTY_STATE_UNVALIDATED
	case configdocuments.DirtyRejected:
		return wire.DirtyState_DIRTY_STATE_REJECTED
	case configdocuments.DirtyReconciling:
		return wire.DirtyState_DIRTY_STATE_RECONCILING
	case configdocuments.DirtyUnavailable:
		return wire.DirtyState_DIRTY_STATE_UNAVAILABLE
	case configdocuments.DirtyUnknown:
		return wire.DirtyState_DIRTY_STATE_UNSPECIFIED
	default:
		return wire.DirtyState_DIRTY_STATE_UNSPECIFIED
	}
}
func configVersion(document storage.ConfigDocumentMetadata) *wire.DocumentVersion {
	hash := document.SourceSHA256()
	return &wire.DocumentVersion{Revision: document.Ref().Revision().Number(), SourceSha256: append([]byte(nil), hash[:]...)}
}
func (h *configDocumentsHandler) documentWire(ctx context.Context, document storage.ConfigDocumentMetadata, snapshot *configdocuments.Snapshot) *wire.ConfigurationDocument {
	name, err := h.names.name(ctx, storage.APIConfigurationDocuments, document.Ref().Document().String())
	if err != nil {
		return nil
	}
	return h.documentWireNamed(document, snapshot, name)
}
func (h *configDocumentsHandler) documentWireNamed(document storage.ConfigDocumentMetadata, snapshot *configdocuments.Snapshot, name string) *wire.ConfigurationDocument {
	result := &wire.ConfigurationDocument{Id: &wire.DocumentId{Value: document.Ref().Document().String()}, Kind: configDocumentKind(document.Kind()), SchemaVersion: document.SchemaVersion(), SyntaxProfile: document.SyntaxProfile(), Version: configVersion(document), Active: document.Active(), ProtectedSourceReference: h.service.ProtectedReference(document)}
	result.WorkspaceState = &wire.ConfigurationDocument_WorkspaceUnobserved{WorkspaceUnobserved: &wire.WorkspaceUnobserved{Reason: wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_NO_PHYSICAL_LOCATOR}}
	if updated, present := document.UpdatedAt(); present {
		result.UpdatedAt = timestamppb.New(updated)
	}
	if snapshot != nil && snapshot.Document().Ref() == document.Ref() {
		observation := snapshot.Workspace()
		hash, present := observation.SHA256()
		if present {
			updated, _ := observation.UpdatedAt()
			workspace := &wire.WorkspaceObservation{SourceSha256: append([]byte(nil), hash[:]...), UpdatedAt: timestamppb.New(updated), Dirty: configDirty(snapshot.Dirty())}
			if diagnostic, ok := snapshot.Diagnostic(); ok {
				workspace.Diagnostic = configWorkspaceDiagnostic(diagnostic)
			}
			result.WorkspaceState = &wire.ConfigurationDocument_Workspace{Workspace: workspace}
		} else if snapshot.Dirty() == configdocuments.DirtyUnavailable {
			result.WorkspaceState = &wire.ConfigurationDocument_WorkspaceUnobserved{WorkspaceUnobserved: &wire.WorkspaceUnobserved{Reason: wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_SOURCE_UNAVAILABLE}}
		}
	}
	result.Name = name
	result.Etag = configurationDocumentEtag(result.Version)
	return result
}
func (h *configDocumentsHandler) snapshotWire(ctx context.Context, snapshot configdocuments.Snapshot) *wire.ConfigurationDocument {
	manifest := snapshot.ManifestSHA256()
	document := h.documentWire(ctx, snapshot.Document(), &snapshot)
	if document == nil {
		return nil
	}
	document.ActiveGeneration = &wire.ActiveGeneration{Revision: snapshot.Generation().Number(), ManifestSha256: append([]byte(nil), manifest[:]...)}
	document.ResumeToken = snapshot.ResumeToken()
	return document
}

func (h *configDocumentsHandler) GetConfigurationDocument(ctx context.Context, r *connect.Request[wire.GetConfigurationDocumentRequest]) (*connect.Response[wire.ConfigurationDocument], error) {
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	var input *wire.GetConfigurationDocumentRequest
	if r != nil {
		input = r.Msg
	}
	admitted, err := contracts.ParseGetConfigurationDocument(input)
	if err != nil {
		return nil, configurationReadError(ctx, err)
	}
	nativeKey, err := h.names.configurationDocument(ctx, admitted.Name())
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	id, err := storage.NewConfigDocumentID(nativeKey)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	snapshot, err := h.service.Read(ctx, id)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	result := h.snapshotWire(ctx, snapshot)
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	return configDocumentsResponse(ctx, result)
}

func (h *configDocumentsHandler) pageCursor(token string, kind configdocuments.PageKind, id storage.ConfigDocumentID, binding configDocumentsBinding) (configdocuments.PageCursor, error) {
	if token == "" {
		return configdocuments.PageCursor{}, nil
	}
	return h.service.DecodePage(token, kind, id, binding.digest)
}
func configPageSize(size int32) int {
	if size == 0 {
		return 100
	}
	if size > 100 {
		return 100
	}
	return int(size)
}
func (h *configDocumentsHandler) continuation(more bool, kind configdocuments.PageKind, id storage.ConfigDocumentID, after string, before int64, cut, binding [32]byte) (string, error) {
	if !more {
		return "", nil
	}
	cursor, err := configdocuments.NewPageCursor(kind, id, after, before, cut, binding)
	if err != nil {
		return "", err
	}
	return h.service.EncodePage(cursor)
}
func (h *configDocumentsHandler) ListConfigurationDocuments(ctx context.Context, r *connect.Request[wire.ListConfigurationDocumentsRequest]) (*connect.Response[wire.ListConfigurationDocumentsResponse], error) {
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	if err := h.names.parent(ctx, r.Msg.Parent); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	cursor, err := h.pageCursor(r.Msg.PageToken, configdocuments.PageDocuments, storage.ConfigDocumentID{}, binding)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	page, err := h.service.Store().ReadConfigDocumentPage(ctx, cursor.After(), configPageSize(r.Msg.PageSize), cursor.Cut())
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	operational, _ := storage.NewConfigDocumentID("operational.main")
	snapshot, err := h.service.Read(ctx, operational)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if snapshot.CatalogCut().SHA256() != page.Cut().SHA256() {
		return nil, configDocumentsRPCError(ctx, authorization.ErrUnavailable, nil)
	}
	result := &wire.ListConfigurationDocumentsResponse{}
	keys := make([]string, 0, len(page.Documents()))
	for _, document := range page.Documents() {
		keys = append(keys, document.Ref().Document().String())
	}
	names, err := h.names.many(ctx, storage.APIConfigurationDocuments, keys)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	after := ""
	for _, document := range page.Documents() {
		if document.Ref().Document() == operational && document.Ref() != snapshot.Document().Ref() {
			return nil, configDocumentsRPCError(ctx, authorization.ErrUnavailable, nil)
		}
		result.ConfigurationDocuments = append(result.ConfigurationDocuments, h.documentWireNamed(document, &snapshot, names[document.Ref().Document().String()]))
		after = document.Ref().Document().String()
	}
	result.NextPageToken, err = h.continuation(page.More(), configdocuments.PageDocuments, storage.ConfigDocumentID{}, after, 0, page.Cut().SHA256(), binding.digest)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	return configDocumentsResponse(ctx, result)
}
func (h *configDocumentsHandler) ListConfigurationRevisions(ctx context.Context, r *connect.Request[wire.ListConfigurationRevisionsRequest]) (*connect.Response[wire.ListConfigurationRevisionsResponse], error) {
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	nativeKey, err := h.names.resolve(ctx, storage.APIConfigurationDocuments, r.Msg.Parent)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	id, _ := storage.NewConfigDocumentID(nativeKey)
	cursor, err := h.pageCursor(r.Msg.PageToken, configdocuments.PageVersions, id, binding)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	page, err := h.service.Store().ReadConfigVersionPage(ctx, id, cursor.Before(), configPageSize(r.Msg.PageSize), cursor.Cut())
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	result := &wire.ListConfigurationRevisionsResponse{}
	var before int64
	for _, version := range page.Documents() {
		entry := configurationRevisionWire(r.Msg.Parent, version)
		result.ConfigurationRevisions = append(result.ConfigurationRevisions, entry)
		before = version.Ref().Revision().Number()
	}
	result.NextPageToken, err = h.continuation(page.More(), configdocuments.PageVersions, id, "", before, page.Cut().SHA256(), binding.digest)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	return configDocumentsResponse(ctx, result)
}
func (h *configDocumentsHandler) ListConfigurationGenerations(ctx context.Context, r *connect.Request[wire.ListConfigurationGenerationsRequest]) (*connect.Response[wire.ListConfigurationGenerationsResponse], error) {
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	if err := h.names.parent(ctx, r.Msg.Parent); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	cursor, err := h.pageCursor(r.Msg.PageToken, configdocuments.PageGenerations, storage.ConfigDocumentID{}, binding)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	page, err := h.service.Store().ReadConfigGenerationPage(ctx, cursor.Before(), configPageSize(r.Msg.PageSize), cursor.Cut())
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	result := &wire.ListConfigurationGenerationsResponse{}
	var before int64
	for _, generation := range page.Generations() {
		result.ConfigurationGenerations = append(result.ConfigurationGenerations, configurationGenerationWire(r.Msg.Parent, generation))
		before = generation.Revision().Number()
	}
	result.NextPageToken, err = h.continuation(page.More(), configdocuments.PageGenerations, storage.ConfigDocumentID{}, "", before, page.Cut().SHA256(), binding.digest)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	return configDocumentsResponse(ctx, result)
}
func (h *configDocumentsHandler) ListConfigurationSchemas(ctx context.Context, r *connect.Request[wire.ListConfigurationSchemasRequest]) (*connect.Response[wire.ListConfigurationSchemasResponse], error) {
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	if err := h.names.parent(ctx, r.Msg.Parent); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	cursor, err := h.pageCursor(r.Msg.PageToken, configdocuments.PageSchemas, storage.ConfigDocumentID{}, binding)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	schemas, err := h.service.Store().ReadConfigSchemas(ctx)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	hash := sha256.New()
	for _, schema := range schemas {
		_, _ = hash.Write([]byte(schema.Kind().String() + "\x00" + schema.SchemaVersion() + "\x00" + schema.SyntaxProfile() + "\x00"))
	}
	var cut [32]byte
	copy(cut[:], hash.Sum(nil))
	if cursor.Cut() != ([32]byte{}) && cursor.Cut() != cut || cursor.Before() > int64(len(schemas)) {
		return nil, configDocumentsRPCError(ctx, configactivation.ErrWorkspacePreconditionConflict, nil)
	}
	start := int(cursor.Before())
	through := start + configPageSize(r.Msg.PageSize)
	if through > len(schemas) {
		through = len(schemas)
	}
	result := &wire.ListConfigurationSchemasResponse{}
	for _, schema := range schemas[start:through] {
		entry, err := configurationSchemaWire(r.Msg.Parent, schema)
		if err != nil {
			return nil, configDocumentsRPCError(ctx, err, nil)
		}
		result.ConfigurationSchemas = append(result.ConfigurationSchemas, entry)
	}
	result.NextPageToken, err = h.continuation(through < len(schemas), configdocuments.PageSchemas, storage.ConfigDocumentID{}, "", int64(through), cut, binding.digest)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	return configDocumentsResponse(ctx, result)
}

func configActivationResult(result configactivation.Result, err error) *wire.ActivationResult {
	response := &wire.ActivationResult{}
	switch result.Outcome() {
	case configactivation.RejectedBeforeCommit:
		response.Outcome = wire.ActivationOutcome_ACTIVATION_OUTCOME_REJECTED_BEFORE_COMMIT
	case configactivation.NotCommitted:
		response.Outcome = wire.ActivationOutcome_ACTIVATION_OUTCOME_NOT_COMMITTED
	case configactivation.CommitUnknown:
		response.Outcome = wire.ActivationOutcome_ACTIVATION_OUTCOME_COMMIT_UNKNOWN
	case configactivation.CommittedReconciling:
		response.Outcome = wire.ActivationOutcome_ACTIVATION_OUTCOME_COMMITTED_RECONCILING
	case configactivation.Published:
		response.Outcome = wire.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED
	case configactivation.RestartRequired:
		response.Outcome = wire.ActivationOutcome_ACTIVATION_OUTCOME_RESTART_REQUIRED
	case configactivation.OutcomeInvalid:
		return nil
	default:
		return nil
	}
	switch result.AttemptOutcome() {
	case storage.TransactionOutcomeUnspecified:
		response.Transaction = wire.TransactionOutcome_TRANSACTION_OUTCOME_NO_TRANSACTION
	case storage.TransactionNotCommitted:
		response.Transaction = wire.TransactionOutcome_TRANSACTION_OUTCOME_NOT_COMMITTED
	case storage.TransactionCommitted:
		response.Transaction = wire.TransactionOutcome_TRANSACTION_OUTCOME_COMMITTED
	case storage.TransactionIndeterminate:
		response.Transaction = wire.TransactionOutcome_TRANSACTION_OUTCOME_COMMIT_UNKNOWN
	default:
		return nil
	}
	if result.Revision().Valid() {
		revision := result.Revision().Number()
		response.GenerationRevision = &revision
	}
	if err != nil {
		code := wire.ConfigurationDiagnosticCode_CONFIGURATION_DIAGNOSTIC_CODE_ACTIVATION_UNAVAILABLE
		var failure *configdocuments.Error
		switch {
		case errors.Is(err, configactivation.ErrWorkspacePreconditionConflict), errors.Is(err, config.ErrStartupSourceChanged):
			code = wire.ConfigurationDiagnosticCode_CONFIGURATION_DIAGNOSTIC_CODE_WORKSPACE_CHANGED
		case errors.As(err, &failure) && failure.Kind() == configdocuments.ReconciliationRequired:
			code = wire.ConfigurationDiagnosticCode_CONFIGURATION_DIAGNOSTIC_CODE_RECONCILIATION_REQUIRED
		case result.Outcome() == configactivation.RejectedBeforeCommit:
			code = wire.ConfigurationDiagnosticCode_CONFIGURATION_DIAGNOSTIC_CODE_CANDIDATE_REJECTED
		}
		response.Diagnostics = []wire.ConfigurationDiagnosticCode{code}
	}
	return response
}

func (h *configDocumentsHandler) mutationPermit(ctx context.Context) (*authorization.Permit, error) {
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || !principal.Native() {
		return nil, authorization.ErrDenied
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, err
	}
	object, err := authorization.AdministrativeObject(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION)
	if err != nil {
		return nil, err
	}
	return h.authority.Permit(ctx, principal, permission, object)
}
func configExpected(id *wire.DocumentId, version *wire.DocumentVersion, workspace []byte) (configdocuments.ExpectedDocument, error) {
	document, err := storage.NewConfigDocumentID(id.Value)
	if err != nil {
		return configdocuments.ExpectedDocument{}, err
	}
	revision, err := storage.NewConfigVersionRevision(version.Revision)
	if err != nil {
		return configdocuments.ExpectedDocument{}, err
	}
	var source, observed [32]byte
	copy(source[:], version.SourceSha256)
	copy(observed[:], workspace)
	return configdocuments.NewExpectedDocument(document, revision, source, observed)
}
func (h *configDocumentsHandler) mutate(ctx context.Context, id *wire.DocumentId, version *wire.DocumentVersion, workspace, raw []byte, historical *wire.ActiveGeneration) (*longrunningpb.Operation, error) {
	if h == nil || h.operations == nil {
		return nil, authorization.ErrUnavailable
	}
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil)
	if err != nil {
		return nil, err
	}
	expected, err := configExpected(id, version, workspace)
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
	permit, err := h.mutationPermit(ctx)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	if !permit.Ready() {
		return nil, authorization.ErrDenied
	}
	var result configactivation.Result
	if historical == nil {
		result, err = h.service.Write(permit.Context(), expected, raw, author)
	} else {
		generation, _ := storage.NewConfigGenerationRevision(historical.Revision)
		var manifest [32]byte
		copy(manifest[:], historical.ManifestSha256)
		result, err = h.service.RestoreDocument(permit.Context(), expected, generation, manifest, author)
	}
	if _, admissionErr := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, &binding); admissionErr != nil {
		return nil, admissionErr
	}
	return h.operations.FromActivation(ctx, result, err)
}
func (h *configDocumentsHandler) PublishConfigurationDocument(ctx context.Context, r *connect.Request[wire.PublishConfigurationDocumentRequest]) (*connect.Response[longrunningpb.Operation], error) {
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	ctx, replay, err := h.prepareConfigurationRequest(ctx, storage.ConfigurationPublishDocument, r.Msg.RequestId, r.Msg)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if replay != nil {
		return connect.NewResponse(replay), nil
	}
	nativeKey, err := h.names.resolve(ctx, storage.APIConfigurationDocuments, r.Msg.Name)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	operation, err := h.mutate(ctx, &wire.DocumentId{Value: nativeKey}, r.Msg.Expected, r.Msg.ExpectedWorkspaceSha256, r.Msg.ExactYamlSource, nil)
	if err != nil {
		var rpc *connect.Error
		if errors.As(err, &rpc) {
			return nil, rpc
		}
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	return connect.NewResponse(operation), nil
}
func (h *configDocumentsHandler) RollbackConfigurationDocument(ctx context.Context, r *connect.Request[wire.RollbackConfigurationDocumentRequest]) (*connect.Response[longrunningpb.Operation], error) {
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil); err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	ctx, replay, err := h.prepareConfigurationRequest(ctx, storage.ConfigurationRollbackDocument, r.Msg.RequestId, r.Msg)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	if replay != nil {
		return connect.NewResponse(replay), nil
	}
	nativeKey, err := h.names.resolve(ctx, storage.APIConfigurationDocuments, r.Msg.Name)
	if err != nil {
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	operation, err := h.mutate(ctx, &wire.DocumentId{Value: nativeKey}, r.Msg.Expected, r.Msg.ExpectedWorkspaceSha256, nil, r.Msg.Historical)
	if err != nil {
		var rpc *connect.Error
		if errors.As(err, &rpc) {
			return nil, rpc
		}
		return nil, configDocumentsRPCError(ctx, err, nil)
	}
	return connect.NewResponse(operation), nil
}
func configResync(reason configdocuments.ResyncReason) wire.ResyncReason {
	switch reason {
	case configdocuments.ResyncEpochChanged:
		return wire.ResyncReason_RESYNC_REASON_EPOCH_CHANGED
	case configdocuments.ResyncTokenInvalid:
		return wire.ResyncReason_RESYNC_REASON_TOKEN_INVALID
	case configdocuments.ResyncHistoryLost:
		return wire.ResyncReason_RESYNC_REASON_HISTORY_LOST
	case configdocuments.ResyncAuthorityChanged:
		return wire.ResyncReason_RESYNC_REASON_AUTHORITY_CHANGED
	case configdocuments.ResyncActivationSignal:
		return wire.ResyncReason_RESYNC_REASON_ACTIVATION_SIGNAL
	case configdocuments.ResyncClosed:
		return wire.ResyncReason_RESYNC_REASON_CLOSED
	case configdocuments.ResyncUnknown:
		return wire.ResyncReason_RESYNC_REASON_UNSPECIFIED
	default:
		return wire.ResyncReason_RESYNC_REASON_UNSPECIFIED
	}
}
func (h *configDocumentsHandler) WatchConfiguration(ctx context.Context, r *connect.Request[wire.WatchConfigurationRequest], stream *connect.ServerStream[wire.WatchConfigurationResponse]) error {
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return configDocumentsRPCError(ctx, err, nil)
	}
	if r == nil || stream == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return configDocumentsRPCError(ctx, contracts.ErrInvalidConfigurationContract, nil)
	}
	if err := h.names.parent(ctx, r.Msg.Parent); err != nil {
		return configDocumentsRPCError(ctx, err, nil)
	}
	sendResync := func(err error) error {
		var resync *configdocuments.Resync
		if !errors.As(err, &resync) {
			return configDocumentsRPCError(ctx, err, nil)
		}
		if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
			return configDocumentsRPCError(ctx, err, nil)
		}
		if err := configDocumentsSend(ctx, stream, &wire.WatchConfigurationResponse{Result: &wire.WatchConfigurationResponse_ResyncRequired{ResyncRequired: &wire.ResyncRequired{Reason: configResync(resync.Reason())}}}); err != nil {
			return err
		}
		return nil
	}
	watch, err := h.service.Subscribe(ctx, r.Msg.ResumeToken)
	if err != nil {
		return sendResync(err)
	}
	defer watch.Close()
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return configDocumentsRPCError(ctx, err, nil)
	}
	if err := configDocumentsSend(ctx, stream, &wire.WatchConfigurationResponse{Result: &wire.WatchConfigurationResponse_Ready{Ready: &wire.WatchReady{ResumeToken: r.Msg.ResumeToken}}}); err != nil {
		return err
	}
	for {
		events, token, err := watch.Next(ctx)
		if err != nil {
			return sendResync(err)
		}
		batch := &wire.ChangeBatch{ResumeToken: token}
		keys := make([]string, 0)
		for _, event := range events {
			for _, change := range event.Changes() {
				keys = append(keys, change.Document().String())
			}
		}
		names, err := h.names.many(ctx, storage.APIConfigurationDocuments, keys)
		if err != nil {
			return configDocumentsRPCError(ctx, err, nil)
		}
		for _, event := range events {
			for _, change := range event.Changes() {
				hash := change.SourceSHA256()
				entry := &wire.DocumentChange{Name: names[change.Document().String()], Version: &wire.DocumentVersion{Revision: change.Revision().Number(), SourceSha256: append([]byte(nil), hash[:]...)}, Dirty: configDirty(change.Dirty())}
				if observed := change.WorkspaceSHA256(); observed != ([32]byte{}) {
					entry.ObservedWorkspaceSha256 = append([]byte(nil), observed[:]...)
				}
				batch.Changes = append(batch.Changes, entry)
			}
		}
		if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
			return configDocumentsRPCError(ctx, err, nil)
		}
		if err := configDocumentsSend(ctx, stream, &wire.WatchConfigurationResponse{Result: &wire.WatchConfigurationResponse_Changes{Changes: batch}}); err != nil {
			return err
		}
	}
}
