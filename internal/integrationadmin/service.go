package integrationadmin

import (
	"bytes"
	"context"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

// HealthReader borrows the composed056 health/supervision owners. Reports and
// phases remain independent facts: Running must never synthesize Ready.
type HealthReader func(context.Context, domain.IntegrationInstanceID) (*obs.IntegrationReportPresence, *obs.IntegrationSupervision, error)
type Service struct {
	documents *configdocuments.Service
	authority *authorization.Authority
	health    HealthReader
}

func New(documents *configdocuments.Service, authority *authorization.Authority, health HealthReader) (*Service, error) {
	if documents == nil || authority == nil || health == nil {
		return nil, ErrInvalid
	}
	return &Service{documents, authority, health}, nil
}
func (s *Service) permit(ctx context.Context, op auth.ScopeOperation) (*authorization.Permit, contracts.AccountID, error) {
	if s == nil || s.documents == nil || s.authority == nil || s.health == nil {
		return nil, contracts.AccountID{}, ErrUnavailable
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, contracts.AccountID{}, authorization.ErrDenied
	}
	account, err := principal.Account()
	if err != nil {
		return nil, contracts.AccountID{}, authorization.ErrDenied
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, op)
	if err != nil {
		return nil, contracts.AccountID{}, err
	}
	object, err := authorization.AdministrativeObject(auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS)
	if err != nil {
		return nil, contracts.AccountID{}, err
	}
	permit, err := s.authority.Permit(ctx, principal, permission, object)
	return permit, account, err
}
func (s *Service) Types(ctx context.Context) ([]*wire.IntegrationType, error) {
	permit, _, err := s.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	if !permit.Ready() {
		return nil, authorization.ErrDenied
	}
	result := make([]*wire.IntegrationType, 0, len(descriptors))
	for _, d := range descriptors {
		value := descriptorWire(d)
		if value == nil {
			return nil, ErrUnavailable
		}
		result = append(result, value)
	}
	if !permit.Ready() {
		return nil, authorization.ErrDenied
	}
	return result, nil
}
func (s *Service) Type(ctx context.Context, id string) (*wire.IntegrationType, error) {
	types, err := s.Types(ctx)
	if err != nil {
		return nil, err
	}
	for _, typ := range types {
		if typ.TypeId == id {
			return typ, nil
		}
	}
	return nil, ErrUnknownType
}
func (s *Service) Instances(ctx context.Context) ([]*wire.IntegrationInstance, configdocuments.Snapshot, error) {
	permit, _, err := s.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, configdocuments.Snapshot{}, err
	}
	defer permit.Release()
	if !permit.Ready() {
		return nil, configdocuments.Snapshot{}, authorization.ErrDenied
	}
	snapshot, source, err := s.documents.ReadOperationalSource(permit.Context())
	if err != nil {
		return nil, configdocuments.Snapshot{}, err
	}
	root, err := activeDocument(source.Bytes())
	if err != nil {
		return nil, configdocuments.Snapshot{}, err
	}
	declared, err := entries(root)
	if err != nil {
		return nil, configdocuments.Snapshot{}, err
	}
	off, err := disabled(root)
	if err != nil {
		return nil, configdocuments.Snapshot{}, err
	}
	result := make([]*wire.IntegrationInstance, 0, len(declared))
	for _, e := range declared {
		item, err := publicEntry(e, off[e.id])
		if err != nil {
			return nil, configdocuments.Snapshot{}, err
		}
		id, err := domain.ParseIntegrationInstanceID(e.id)
		if err != nil {
			return nil, configdocuments.Snapshot{}, ErrInvalid
		}
		item.Report, item.Supervision, err = s.health(permit.Context(), id)
		if err != nil {
			return nil, configdocuments.Snapshot{}, err
		}
		result = append(result, item)
	}
	id, _ := storage.NewConfigDocumentID("operational.main")
	after, err := s.documents.Read(permit.Context(), id)
	if err != nil {
		return nil, configdocuments.Snapshot{}, err
	}
	if after.Generation() != snapshot.Generation() || after.ManifestSHA256() != snapshot.ManifestSHA256() {
		return nil, configdocuments.Snapshot{}, ErrUnavailable
	}
	if !permit.Ready() {
		return nil, configdocuments.Snapshot{}, authorization.ErrDenied
	}
	return result, after, nil
}
func (s *Service) Instance(ctx context.Context, id string) (*wire.IntegrationInstance, configdocuments.Snapshot, error) {
	if !contracts.IntegrationAdminID(id) {
		return nil, configdocuments.Snapshot{}, ErrInvalid
	}
	instances, snapshot, err := s.Instances(ctx)
	if err != nil {
		return nil, configdocuments.Snapshot{}, err
	}
	for _, instance := range instances {
		if instance.IntegrationInstanceId == id {
			return instance, snapshot, nil
		}
	}
	return nil, snapshot, ErrNotFound
}

type mutationKind uint8

const (
	mutationInvalid mutationKind = iota
	mutationCreate
	mutationUpdate
	mutationDelete
	mutationEnable
)

type mutation struct {
	kind          mutationKind
	typ, id       string
	configuration *wire.PublicConfiguration
	desired       wire.DesiredState
}
type MutationResult struct {
	Activation  configactivation.Result
	Snapshot    configdocuments.Snapshot
	HasSnapshot bool
}

func (s *Service) Create(ctx context.Context, typ, id string, c *wire.PublicConfiguration, desired wire.DesiredState, expected configdocuments.ExpectedDocument) (MutationResult, error) {
	return s.change(ctx, mutation{kind: mutationCreate, typ: typ, id: id, configuration: c, desired: desired}, expected)
}
func (s *Service) Update(ctx context.Context, typ, id string, c *wire.PublicConfiguration, expected configdocuments.ExpectedDocument) (MutationResult, error) {
	return s.change(ctx, mutation{kind: mutationUpdate, typ: typ, id: id, configuration: c}, expected)
}
func (s *Service) Delete(ctx context.Context, id string, expected configdocuments.ExpectedDocument) (MutationResult, error) {
	return s.change(ctx, mutation{kind: mutationDelete, id: id}, expected)
}
func (s *Service) SetEnabled(ctx context.Context, id string, desired wire.DesiredState, expected configdocuments.ExpectedDocument) (MutationResult, error) {
	return s.change(ctx, mutation{kind: mutationEnable, id: id, desired: desired}, expected)
}
func (s *Service) change(ctx context.Context, m mutation, expected configdocuments.ExpectedDocument) (MutationResult, error) {
	if !contracts.IntegrationAdminID(m.id) {
		return MutationResult{}, ErrInvalid
	}
	if (m.kind == mutationCreate || m.kind == mutationEnable) && m.desired != wire.DesiredState_DESIRED_STATE_ENABLED && m.desired != wire.DesiredState_DESIRED_STATE_DISABLED {
		return MutationResult{}, ErrInvalid
	}
	if m.configuration != nil {
		m.configuration = proto.Clone(m.configuration).(*wire.PublicConfiguration)
	}
	permit, account, err := s.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return MutationResult{}, err
	}
	defer permit.Release()
	if !permit.Ready() {
		return MutationResult{}, authorization.ErrDenied
	}
	name, err := account.Value()
	if err != nil {
		return MutationResult{}, ErrInvalid
	}
	author, err := storage.NewConfigAuthor(name)
	if err != nil {
		return MutationResult{}, err
	}
	prepare := func(source configdocuments.ExactSource) ([]byte, error) {
		if !permit.Ready() {
			return nil, authorization.ErrDenied
		}
		root, err := admittedDocument(source.Bytes())
		if err != nil {
			return nil, err
		}
		declared, err := entries(root)
		if err != nil {
			return nil, err
		}
		off, err := disabled(root)
		if err != nil {
			return nil, err
		}
		var found *entry
		for i := range declared {
			if declared[i].id == m.id {
				found = &declared[i]
				break
			}
		}
		switch m.kind {
		case mutationCreate, mutationUpdate:
			d, err := findDescriptor(m.typ)
			if err != nil {
				return nil, err
			}
			if m.kind == mutationCreate {
				occupied := found != nil
				for _, existing := range declared {
					if existing.key == d.id {
						occupied = true
					}
				}
				if occupied {
					return nil, ErrAlreadyExists
				}
			}
			if m.kind == mutationUpdate && found == nil {
				return nil, ErrNotFound
			}
			if found != nil && found.typ != m.typ {
				return nil, ErrInvalid
			}
			node, err := privateFragment(d, m.id, m.configuration)
			if err != nil {
				return nil, err
			}
			setField(root, d.id, node)
			if m.kind == mutationCreate {
				off[m.id] = m.desired == wire.DesiredState_DESIRED_STATE_DISABLED
			}
		case mutationDelete:
			if found == nil {
				return nil, ErrNotFound
			}
			if found.workerIndex >= 0 {
				workers := field(root, "external_workers")
				workers.Content = append(workers.Content[:found.workerIndex], workers.Content[found.workerIndex+1:]...)
				if len(workers.Content) == 0 {
					setField(root, "external_workers", nil)
				}
			} else {
				setField(root, found.key, nil)
			}
			delete(off, m.id)
		case mutationEnable:
			if found == nil {
				return nil, ErrNotFound
			}
			off[m.id] = m.desired == wire.DesiredState_DESIRED_STATE_DISABLED
		default:
			return nil, ErrInvalid
		}
		setDisabled(root, off)
		raw, err := encodeNode(root)
		if err != nil {
			return nil, err
		}
		if _, err := config.ParseOperationalWithEnv(raw, func(string) (string, bool) { return "", false }); err != nil {
			return nil, ErrInvalid
		}
		return raw, nil
	}
	result, err := func() (configactivation.Result, error) {
		source, err := s.documents.ReadOperationalWorkspace(permit.Context(), expected)
		if err != nil {
			return configactivation.Result{}, err
		}
		candidate, err := prepare(source)
		if err != nil {
			return configactivation.Result{}, err
		}
		return s.documents.TransformOperational(permit.Context(), expected, author, func(current configdocuments.ExactSource) ([]byte, error) {
			if !permit.Ready() {
				return nil, authorization.ErrDenied
			}
			if current.SHA256() != source.SHA256() {
				return nil, ErrWorkspaceInvalid
			}
			return bytes.Clone(candidate), nil
		})
	}()
	out := MutationResult{Activation: result}
	// A cancellation/revocation cannot be disguised as a successful response.
	if !permit.Ready() {
		return out, authorization.ErrDenied
	}
	id, _ := storage.NewConfigDocumentID("operational.main")
	if current, readErr := s.documents.Read(permit.Context(), id); readErr == nil {
		out.Snapshot = current
		out.HasSnapshot = true
	}
	return out, err
}
