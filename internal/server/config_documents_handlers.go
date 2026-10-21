package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	wire "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type configDocumentsHandler struct {
	service   *configdocuments.Service
	authority *authorization.Authority
}

var _ configurationv1connect.ConfigurationServiceHandler = (*configDocumentsHandler)(nil)

type configDocumentsBinding struct {
	account contracts.AccountID
	digest  [32]byte
}

func configDocumentsResponse[T any](message *T) (*connect.Response[T], error) {
	wireMessage, ok := any(message).(proto.Message)
	if !ok || contracts.AdmitConfigurationResponse(wireMessage) != nil {
		return nil, configDocumentsRPCError(authorization.ErrUnavailable, nil)
	}
	return connect.NewResponse(message), nil
}
func configDocumentsSend(stream *connect.ServerStream[wire.WatchConfigurationResponse], message *wire.WatchConfigurationResponse) error {
	if contracts.AdmitConfigurationResponse(message) != nil {
		return configDocumentsRPCError(authorization.ErrUnavailable, nil)
	}
	return stream.Send(message)
}

func (h *configDocumentsHandler) admit(ctx context.Context, operation auth.ScopeOperation, expected *configDocumentsBinding) (configDocumentsBinding, error) {
	if h == nil || h.service == nil || h.authority == nil || nilReactiveInterface(ctx) {
		return configDocumentsBinding{}, authorization.ErrUnavailable
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || !principal.Native() {
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

func configDocumentsRPCError(err error, current *wire.DocumentVersion) error {
	code, status := wire.ErrorCode_ERROR_CODE_UNAVAILABLE, connect.CodeUnavailable
	var workspace *configdocuments.Error
	var store *storage.Error
	switch {
	case errors.Is(err, contracts.ErrInvalidConfigurationContract):
		code, status = wire.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, connect.CodeInvalidArgument
	case errors.Is(err, authorization.ErrDenied):
		code, status = wire.ErrorCode_ERROR_CODE_DENIED, connect.CodePermissionDenied
	case errors.Is(err, configactivation.ErrWorkspacePreconditionConflict), errors.Is(err, config.ErrStartupSourceChanged):
		code, status = wire.ErrorCode_ERROR_CODE_CONFLICT, connect.CodeAborted
	case errors.As(err, &workspace):
		switch workspace.Kind() {
		case configdocuments.FailureInvalid:
			code, status = wire.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, connect.CodeInvalidArgument
		case configdocuments.Conflict:
			code, status = wire.ErrorCode_ERROR_CODE_CONFLICT, connect.CodeAborted
		case configdocuments.ReconciliationRequired:
			code, status = wire.ErrorCode_ERROR_CODE_RECONCILIATION_REQUIRED, connect.CodeFailedPrecondition
		case configdocuments.IncompleteHistory:
			code, status = wire.ErrorCode_ERROR_CODE_INCOMPLETE_GENERATION, connect.CodeFailedPrecondition
		case configdocuments.Unavailable:
		}
	case errors.As(err, &store):
		switch store.Kind() {
		case storage.ConfigConflict:
			code, status = wire.ErrorCode_ERROR_CODE_CONFLICT, connect.CodeAborted
		case storage.ConfigInvalid:
			code, status = wire.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, connect.CodeInvalidArgument
		case storage.ConfigIncomplete:
			code, status = wire.ErrorCode_ERROR_CODE_INCOMPLETE_GENERATION, connect.CodeFailedPrecondition
		case storage.ConfigNotFound:
			code, status = wire.ErrorCode_ERROR_CODE_NOT_FOUND, connect.CodeNotFound
		}
	}
	result := connect.NewError(status, errors.New("configuration request rejected"))
	detail := &wire.ConfigurationError{Code: code}
	if code == wire.ErrorCode_ERROR_CODE_CONFLICT {
		detail.Current = current
	}
	if data, err := connect.NewErrorDetail(detail); err == nil {
		result.AddDetail(data)
	}
	return result
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
func (h *configDocumentsHandler) documentWire(document storage.ConfigDocumentMetadata, snapshot *configdocuments.Snapshot) *wire.Document {
	result := &wire.Document{Id: &wire.DocumentId{Value: document.Ref().Document().String()}, Kind: configDocumentKind(document.Kind()), SchemaVersion: document.SchemaVersion(), SyntaxProfile: document.SyntaxProfile(), Version: configVersion(document), Active: document.Active(), ProtectedSourceReference: h.service.ProtectedReference(document)}
	result.WorkspaceState = &wire.Document_WorkspaceUnobserved{WorkspaceUnobserved: &wire.WorkspaceUnobserved{Reason: wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_NO_PHYSICAL_LOCATOR}}
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
			result.WorkspaceState = &wire.Document_Workspace{Workspace: workspace}
		} else if snapshot.Dirty() == configdocuments.DirtyUnavailable {
			result.WorkspaceState = &wire.Document_WorkspaceUnobserved{WorkspaceUnobserved: &wire.WorkspaceUnobserved{Reason: wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_SOURCE_UNAVAILABLE}}
		}
	}
	return result
}
func (h *configDocumentsHandler) snapshotWire(snapshot configdocuments.Snapshot) *wire.ReadDocumentResponse {
	manifest := snapshot.ManifestSHA256()
	return &wire.ReadDocumentResponse{Document: h.documentWire(snapshot.Document(), &snapshot), ActiveGeneration: &wire.ActiveGeneration{Revision: snapshot.Generation().Number(), ManifestSha256: append([]byte(nil), manifest[:]...)}, ResumeToken: snapshot.ResumeToken()}
}

func (h *configDocumentsHandler) ReadDocument(ctx context.Context, r *connect.Request[wire.ReadDocumentRequest]) (*connect.Response[wire.ReadDocumentResponse], error) {
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(contracts.ErrInvalidConfigurationContract, nil)
	}
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	id, err := storage.NewConfigDocumentID(r.Msg.Id.Value)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	snapshot, err := h.service.Read(ctx, id)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	result := h.snapshotWire(snapshot)
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	return configDocumentsResponse(result)
}

func (h *configDocumentsHandler) pageCursor(page *wire.PageRequest, kind configdocuments.PageKind, id storage.ConfigDocumentID, binding configDocumentsBinding) (configdocuments.PageCursor, error) {
	if page.PageToken == nil {
		return configdocuments.PageCursor{}, nil
	}
	return h.service.DecodePage(*page.PageToken, kind, id, binding.digest)
}
func configComplete() *wire.PageContinuation {
	return &wire.PageContinuation{Result: &wire.PageContinuation_Complete{Complete: &emptypb.Empty{}}}
}
func (h *configDocumentsHandler) continuation(more bool, kind configdocuments.PageKind, id storage.ConfigDocumentID, after string, before int64, cut, binding [32]byte) (*wire.PageContinuation, error) {
	if !more {
		return configComplete(), nil
	}
	cursor, err := configdocuments.NewPageCursor(kind, id, after, before, cut, binding)
	if err != nil {
		return nil, err
	}
	token, err := h.service.EncodePage(cursor)
	if err != nil {
		return nil, err
	}
	return &wire.PageContinuation{Result: &wire.PageContinuation_NextPageToken{NextPageToken: token}}, nil
}

func (h *configDocumentsHandler) ListDocuments(ctx context.Context, r *connect.Request[wire.ListDocumentsRequest]) (*connect.Response[wire.ListDocumentsResponse], error) {
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(contracts.ErrInvalidConfigurationContract, nil)
	}
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	cursor, err := h.pageCursor(r.Msg.Page, configdocuments.PageDocuments, storage.ConfigDocumentID{}, binding)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	page, err := h.service.Store().ReadConfigDocumentPage(ctx, cursor.After(), int(r.Msg.Page.PageSize), cursor.Cut())
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	operational, _ := storage.NewConfigDocumentID("operational.main")
	snapshot, err := h.service.Read(ctx, operational)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	if snapshot.CatalogCut().SHA256() != page.Cut().SHA256() {
		return nil, configDocumentsRPCError(authorization.ErrUnavailable, nil)
	}
	result := &wire.ListDocumentsResponse{}
	after := ""
	for _, document := range page.Documents() {
		if document.Ref().Document() == operational && document.Ref() != snapshot.Document().Ref() {
			return nil, configDocumentsRPCError(authorization.ErrUnavailable, nil)
		}
		result.Documents = append(result.Documents, h.documentWire(document, &snapshot))
		after = document.Ref().Document().String()
	}
	result.Continuation, err = h.continuation(page.More(), configdocuments.PageDocuments, storage.ConfigDocumentID{}, after, 0, page.Cut().SHA256(), binding.digest)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	return configDocumentsResponse(result)
}
func (h *configDocumentsHandler) ListVersions(ctx context.Context, r *connect.Request[wire.ListVersionsRequest]) (*connect.Response[wire.ListVersionsResponse], error) {
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(contracts.ErrInvalidConfigurationContract, nil)
	}
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	id, _ := storage.NewConfigDocumentID(r.Msg.Id.Value)
	cursor, err := h.pageCursor(r.Msg.Page, configdocuments.PageVersions, id, binding)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	page, err := h.service.Store().ReadConfigVersionPage(ctx, id, cursor.Before(), int(r.Msg.Page.PageSize), cursor.Cut())
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	result := &wire.ListVersionsResponse{}
	var before int64
	for _, version := range page.Documents() {
		entry := &wire.HistoricalVersion{Id: &wire.DocumentId{Value: id.String()}, Version: configVersion(version), Availability: wire.VersionAvailability_VERSION_AVAILABILITY_PURGED}
		if version.Retained() {
			entry.Availability = wire.VersionAvailability_VERSION_AVAILABILITY_RETAINED
			updated, _ := version.UpdatedAt()
			entry.UpdatedAt = timestamppb.New(updated)
			entry.SchemaVersion = version.SchemaVersion()
		}
		result.Versions = append(result.Versions, entry)
		before = version.Ref().Revision().Number()
	}
	result.Continuation, err = h.continuation(page.More(), configdocuments.PageVersions, id, "", before, page.Cut().SHA256(), binding.digest)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	return configDocumentsResponse(result)
}
func (h *configDocumentsHandler) ListGenerations(ctx context.Context, r *connect.Request[wire.ListGenerationsRequest]) (*connect.Response[wire.ListGenerationsResponse], error) {
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(contracts.ErrInvalidConfigurationContract, nil)
	}
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	cursor, err := h.pageCursor(r.Msg.Page, configdocuments.PageGenerations, storage.ConfigDocumentID{}, binding)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	page, err := h.service.Store().ReadConfigGenerationPage(ctx, cursor.Before(), int(r.Msg.Page.PageSize), cursor.Cut())
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	result := &wire.ListGenerationsResponse{}
	var before int64
	for _, generation := range page.Generations() {
		manifest := generation.ManifestSHA256()
		availability := wire.GenerationAvailability_GENERATION_AVAILABILITY_COMPLETE
		if generation.Availability() == storage.ConfigGenerationIncomplete {
			availability = wire.GenerationAvailability_GENERATION_AVAILABILITY_PARTIALLY_PURGED
		}
		result.Generations = append(result.Generations, &wire.HistoricalGeneration{Generation: &wire.ActiveGeneration{Revision: generation.Revision().Number(), ManifestSha256: append([]byte(nil), manifest[:]...)}, Availability: availability, CreatedAt: timestamppb.New(generation.CreatedAt())})
		before = generation.Revision().Number()
	}
	result.Continuation, err = h.continuation(page.More(), configdocuments.PageGenerations, storage.ConfigDocumentID{}, "", before, page.Cut().SHA256(), binding.digest)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	return configDocumentsResponse(result)
}
func (h *configDocumentsHandler) ListSchemas(ctx context.Context, r *connect.Request[wire.ListSchemasRequest]) (*connect.Response[wire.ListSchemasResponse], error) {
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(contracts.ErrInvalidConfigurationContract, nil)
	}
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	cursor, err := h.pageCursor(r.Msg.Page, configdocuments.PageSchemas, storage.ConfigDocumentID{}, binding)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	schemas, err := h.service.Store().ReadConfigSchemas(ctx)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	hash := sha256.New()
	for _, schema := range schemas {
		_, _ = hash.Write([]byte(schema.SchemaVersion() + "\x00" + schema.SyntaxProfile() + "\x00"))
	}
	var cut [32]byte
	copy(cut[:], hash.Sum(nil))
	if cursor.Cut() != ([32]byte{}) && cursor.Cut() != cut || cursor.Before() > int64(len(schemas)) {
		return nil, configDocumentsRPCError(configactivation.ErrWorkspacePreconditionConflict, nil)
	}
	start := int(cursor.Before())
	through := start + int(r.Msg.Page.PageSize)
	if through > len(schemas) {
		through = len(schemas)
	}
	result := &wire.ListSchemasResponse{}
	for _, schema := range schemas[start:through] {
		result.Schemas = append(result.Schemas, &wire.Schema{Kind: configDocumentKind(schema.Kind()), SchemaVersion: schema.SchemaVersion(), SyntaxProfile: schema.SyntaxProfile(), EditableOperationalWorkspace: schema.Kind() == storage.ConfigDocumentKindOperationalYAML})
	}
	result.Continuation, err = h.continuation(through < len(schemas), configdocuments.PageSchemas, storage.ConfigDocumentID{}, "", int64(through), cut, binding.digest)
	if err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
		return nil, configDocumentsRPCError(err, nil)
	}
	return configDocumentsResponse(result)
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
		code := wire.DiagnosticCode_DIAGNOSTIC_CODE_ACTIVATION_UNAVAILABLE
		var failure *configdocuments.Error
		switch {
		case errors.Is(err, configactivation.ErrWorkspacePreconditionConflict), errors.Is(err, config.ErrStartupSourceChanged):
			code = wire.DiagnosticCode_DIAGNOSTIC_CODE_WORKSPACE_CHANGED
		case errors.As(err, &failure) && failure.Kind() == configdocuments.ReconciliationRequired:
			code = wire.DiagnosticCode_DIAGNOSTIC_CODE_RECONCILIATION_REQUIRED
		case result.Outcome() == configactivation.RejectedBeforeCommit:
			code = wire.DiagnosticCode_DIAGNOSTIC_CODE_CANDIDATE_REJECTED
		}
		response.Diagnostics = []wire.DiagnosticCode{code}
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
func (h *configDocumentsHandler) mutate(ctx context.Context, id *wire.DocumentId, version *wire.DocumentVersion, workspace, raw []byte, historical *wire.ActiveGeneration) (*wire.ActivationResult, *wire.ReadDocumentResponse, error) {
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil)
	if err != nil {
		return nil, nil, err
	}
	expected, err := configExpected(id, version, workspace)
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
	permit, err := h.mutationPermit(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer permit.Release()
	if !permit.Ready() {
		return nil, nil, authorization.ErrDenied
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
		return nil, nil, admissionErr
	}
	operational, _ := storage.NewConfigDocumentID("operational.main")
	current, readErr := h.service.Read(ctx, operational)
	var currentWire *wire.ReadDocumentResponse
	if readErr == nil {
		currentWire = h.snapshotWire(current)
	}
	if result.AttemptOutcome() == storage.TransactionNotCommitted && (errors.Is(err, configactivation.ErrWorkspacePreconditionConflict) || errors.Is(err, config.ErrStartupSourceChanged)) {
		var latest *wire.DocumentVersion
		if currentWire != nil {
			latest = currentWire.Document.Version
		}
		return nil, nil, configDocumentsRPCError(err, latest)
	}
	if result.Outcome() == configactivation.OutcomeInvalid {
		var latest *wire.DocumentVersion
		if currentWire != nil {
			latest = currentWire.Document.Version
		}
		return nil, nil, configDocumentsRPCError(err, latest)
	}
	activation := configActivationResult(result, err)
	if activation == nil {
		return nil, nil, authorization.ErrUnavailable
	}
	return activation, currentWire, nil
}
func (h *configDocumentsHandler) WriteDocument(ctx context.Context, r *connect.Request[wire.WriteDocumentRequest]) (*connect.Response[wire.WriteDocumentResponse], error) {
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(contracts.ErrInvalidConfigurationContract, nil)
	}
	activation, current, err := h.mutate(ctx, r.Msg.Id, r.Msg.Expected, r.Msg.ExpectedWorkspaceSha256, r.Msg.ExactYamlSource, nil)
	if err != nil {
		var rpc *connect.Error
		if errors.As(err, &rpc) {
			return nil, rpc
		}
		return nil, configDocumentsRPCError(err, nil)
	}
	return configDocumentsResponse(&wire.WriteDocumentResponse{Activation: activation, Current: current})
}
func (h *configDocumentsHandler) RestoreDocument(ctx context.Context, r *connect.Request[wire.RestoreDocumentRequest]) (*connect.Response[wire.RestoreDocumentResponse], error) {
	if r == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return nil, configDocumentsRPCError(contracts.ErrInvalidConfigurationContract, nil)
	}
	activation, current, err := h.mutate(ctx, r.Msg.Id, r.Msg.Expected, r.Msg.ExpectedWorkspaceSha256, nil, r.Msg.Historical)
	if err != nil {
		var rpc *connect.Error
		if errors.As(err, &rpc) {
			return nil, rpc
		}
		return nil, configDocumentsRPCError(err, nil)
	}
	return configDocumentsResponse(&wire.RestoreDocumentResponse{Activation: activation, Current: current})
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
	if r == nil || stream == nil || contracts.AdmitConfigurationRequest(r.Msg) != nil {
		return configDocumentsRPCError(contracts.ErrInvalidConfigurationContract, nil)
	}
	binding, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, nil)
	if err != nil {
		return configDocumentsRPCError(err, nil)
	}
	sendResync := func(err error) error {
		var resync *configdocuments.Resync
		if !errors.As(err, &resync) {
			return configDocumentsRPCError(err, nil)
		}
		if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
			return configDocumentsRPCError(err, nil)
		}
		if err := configDocumentsSend(stream, &wire.WatchConfigurationResponse{Result: &wire.WatchConfigurationResponse_ResyncRequired{ResyncRequired: &wire.ResyncRequired{Reason: configResync(resync.Reason())}}}); err != nil {
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
		return configDocumentsRPCError(err, nil)
	}
	if err := configDocumentsSend(stream, &wire.WatchConfigurationResponse{Result: &wire.WatchConfigurationResponse_Ready{Ready: &wire.WatchReady{ResumeToken: r.Msg.ResumeToken}}}); err != nil {
		return err
	}
	for {
		events, token, err := watch.Next(ctx)
		if err != nil {
			return sendResync(err)
		}
		batch := &wire.ChangeBatch{ResumeToken: token}
		for _, event := range events {
			for _, change := range event.Changes() {
				hash := change.SourceSHA256()
				entry := &wire.DocumentChange{Id: &wire.DocumentId{Value: change.Document().String()}, Version: &wire.DocumentVersion{Revision: change.Revision().Number(), SourceSha256: append([]byte(nil), hash[:]...)}, Dirty: configDirty(change.Dirty())}
				if observed := change.WorkspaceSHA256(); observed != ([32]byte{}) {
					entry.ObservedWorkspaceSha256 = append([]byte(nil), observed[:]...)
				}
				batch.Changes = append(batch.Changes, entry)
			}
		}
		if _, err := h.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, &binding); err != nil {
			return configDocumentsRPCError(err, nil)
		}
		if err := configDocumentsSend(stream, &wire.WatchConfigurationResponse{Result: &wire.WatchConfigurationResponse_Changes{Changes: batch}}); err != nil {
			return err
		}
	}
}
