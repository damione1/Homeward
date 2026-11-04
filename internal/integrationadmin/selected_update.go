package integrationadmin

import (
	"context"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

// UpdateSelection is the finite writable field-mask vocabulary. Empty selection
// is explicit and valid; the zero value is invalid, never "all fields".
type UpdateSelection uint8

const (
	UpdateNothing UpdateSelection = iota + 1
	UpdateConfiguration
	UpdateDesiredState
	UpdateConfigurationAndDesiredState
)

func (s UpdateSelection) Valid() bool {
	return s >= UpdateNothing && s <= UpdateConfigurationAndDesiredState
}
func (s UpdateSelection) configuration() bool {
	return s == UpdateConfiguration || s == UpdateConfigurationAndDesiredState
}
func (s UpdateSelection) desired() bool {
	return s == UpdateDesiredState || s == UpdateConfigurationAndDesiredState
}

// PreparedUpdate is an exact source-bound, redacted definition capture. It is
// not permission to mutate; ApplyPreparedUpdate reacquires current native rights
// and the existing document/Owner CAS before either publication or NoChange.
type PreparedUpdate struct {
	owner         *Service
	instance      domain.IntegrationInstanceID
	snapshot      configdocuments.Snapshot
	definition    *wire.IntegrationInstance
	selection     UpdateSelection
	configuration *wire.PublicConfiguration
	desired       wire.DesiredState
	changed       bool
}

func (p PreparedUpdate) Definition() *wire.IntegrationInstance {
	if p.definition == nil {
		return nil
	}
	return proto.Clone(p.definition).(*wire.IntegrationInstance)
}
func (p PreparedUpdate) Snapshot() configdocuments.Snapshot { return p.snapshot }
func (p PreparedUpdate) Changed() bool                      { return p.changed }
func (p PreparedUpdate) valid() bool {
	_, err := p.instance.Value()
	return p.owner != nil && err == nil && p.definition != nil && p.selection.Valid()
}

// PrepareUpdate reads the native admitted source under ADMINISTER, so an MCP
// mutation ceiling is not accidentally widened to require an extra READ scope.
// It does not inspect health, stage files, compile or contact an integration.
func (s *Service) PrepareUpdate(ctx context.Context, id domain.IntegrationInstanceID, selection UpdateSelection, configuration *wire.PublicConfiguration, desired wire.DesiredState) (PreparedUpdate, error) {
	permit, _, err := s.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return PreparedUpdate{}, err
	}
	defer permit.Release()
	key, err := id.Value()
	if err != nil || !selection.Valid() {
		return PreparedUpdate{}, ErrInvalid
	}
	snapshot, source, err := s.documents.ReadOperationalSourceForMutation(permit.Context())
	if err != nil {
		return PreparedUpdate{}, err
	}
	root, err := activeDocument(source.Bytes())
	if err != nil {
		return PreparedUpdate{}, err
	}
	declared, err := entries(root)
	if err != nil {
		return PreparedUpdate{}, err
	}
	off, err := disabled(root)
	if err != nil {
		return PreparedUpdate{}, err
	}
	var definition *wire.IntegrationInstance
	for _, entry := range declared {
		if entry.id == key {
			if definition != nil {
				return PreparedUpdate{}, ErrWorkspaceInvalid
			}
			definition, err = publicEntry(entry, off[key])
			if err != nil {
				return PreparedUpdate{}, err
			}
		}
	}
	if !permit.Ready() {
		return PreparedUpdate{}, authorization.ErrDenied
	}
	if definition == nil {
		return PreparedUpdate{}, ErrNotFound
	}
	prepared := PreparedUpdate{owner: s, instance: id, snapshot: snapshot, definition: definition, selection: selection, desired: desired}
	if selection.configuration() {
		editable := definition.GetEditable()
		if editable == nil || configuration == nil {
			return PreparedUpdate{}, ErrInvalid
		}
		descriptor, err := findDescriptor(definition.TypeId)
		if err != nil {
			return PreparedUpdate{}, err
		}
		prepared.configuration = proto.Clone(configuration).(*wire.PublicConfiguration)
		if _, err := privateFragment(descriptor, key, prepared.configuration); err != nil {
			return PreparedUpdate{}, err
		}
		prepared.changed = !proto.Equal(editable, prepared.configuration)
	}
	if selection.desired() {
		if desired != wire.DesiredState_DESIRED_STATE_ENABLED && desired != wire.DesiredState_DESIRED_STATE_DISABLED {
			return PreparedUpdate{}, ErrInvalid
		}
		prepared.changed = prepared.changed || desired != definition.DesiredState
	}
	if !permit.Ready() {
		return PreparedUpdate{}, authorization.ErrDenied
	}
	return prepared, nil
}

// encodeNoChange must use the caller's already prepared names and redacted API
// projection. It executes under the native SQL fence and cannot perform lookups.
func (s *Service) ApplyPreparedUpdate(ctx context.Context, prepared PreparedUpdate, expected configdocuments.ExpectedDocument, encodeNoChange func(storage.ConfigurationAPIRequestRecord, time.Time) ([]byte, error)) (MutationResult, error) {
	permit, account, err := s.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return MutationResult{}, err
	}
	defer permit.Release()
	if !prepared.valid() || prepared.owner != s || encodeNoChange == nil {
		return MutationResult{}, ErrInvalid
	}
	if !expected.ConfirmsDefinition(prepared.snapshot) {
		return MutationResult{}, configactivation.ErrWorkspacePreconditionConflict
	}
	key, _ := prepared.instance.Value()
	if prepared.changed {
		return s.change(permit.Context(), mutation{kind: mutationPatch, typ: prepared.definition.TypeId, id: key, configuration: prepared.configuration, desired: prepared.desired, selection: prepared.selection}, expected)
	}
	actor, err := account.Value()
	if err != nil {
		return MutationResult{}, authorization.ErrDenied
	}
	author, err := storage.NewConfigAuthor(actor)
	if err != nil {
		return MutationResult{}, err
	}
	result, err := s.documents.ConfirmOperationalNoChange(permit.Context(), expected, author, func(current configdocuments.Snapshot, record storage.ConfigurationAPIRequestRecord, at time.Time) ([]byte, error) {
		if current.Generation() != prepared.snapshot.Generation() || current.ManifestSHA256() != prepared.snapshot.ManifestSHA256() || current.Document().Ref() != prepared.snapshot.Document().Ref() || current.Document().SourceSHA256() != prepared.snapshot.Document().SourceSHA256() {
			return nil, configactivation.ErrWorkspacePreconditionConflict
		}
		return encodeNoChange(record, at)
	})
	if !permit.Ready() {
		return MutationResult{Activation: result}, authorization.ErrDenied
	}
	return MutationResult{Activation: result, Snapshot: prepared.snapshot, HasSnapshot: true}, err
}
