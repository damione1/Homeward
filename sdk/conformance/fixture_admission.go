package conformance

import (
	"context"
	"sync"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
)

// SupplementalBinding is a validated extra source for a pilot whose inventory
// exceeds the two primary switch/light probes. It cannot replace either probe.
type SupplementalBinding struct {
	key   integrations.SourceKey
	alias domain.HAAlias
}

func NewSupplementalBinding(device, entity, alias string) (SupplementalBinding, error) {
	key, err := integrations.NewSourceKey(device, entity)
	if err != nil {
		return SupplementalBinding{}, err
	}
	label, err := domain.ParseHAAlias(alias)
	if err != nil {
		return SupplementalBinding{}, err
	}
	return SupplementalBinding{key, label}, nil
}

// FixtureAdmission is live only during PrepareFixture, before Supervisor.Start.
// Retained copies all share the same closed gate after that callback returns.
type FixtureAdmission struct{ state *fixtureAdmissionState }

type fixtureAdmissionState struct {
	mu              sync.Mutex
	open            bool
	secretsAttached bool
	bindings        int
	seen            map[integrations.SourceKey]bool
	aliases         map[domain.HAAlias]bool
	host            *integrations.Manager
	fixture         *fake.Fixture
	instance        domain.IntegrationInstanceID
	records         []integrations.RegistryRecord
}

type FixturePreparer interface {
	PrepareFixture(context.Context, FixtureAdmission) error
}

func (p FixtureAdmission) AddSources(ctx context.Context, bindings []SupplementalBinding) error {
	if p.state == nil || nilInterface(ctx) || len(bindings) == 0 {
		return integrations.ErrInvalidInput
	}
	s := p.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open || s.bindings+len(bindings) > 64 {
		return integrations.ErrInvalidInput
	}
	localKeys := make(map[integrations.SourceKey]bool, len(bindings))
	localAliases := make(map[domain.HAAlias]bool, len(bindings))
	for _, binding := range bindings {
		if _, _, err := binding.key.Components(); err != nil {
			return integrations.ErrInvalidInput
		}
		if _, err := binding.alias.Value(); err != nil || s.seen[binding.key] || s.aliases[binding.alias] || localKeys[binding.key] || localAliases[binding.alias] {
			return integrations.ErrInvalidInput
		}
		localKeys[binding.key], localAliases[binding.alias] = true, true
	}
	records := make([]integrations.RegistryRecord, 0, len(bindings))
	for _, binding := range bindings {
		resolved, err := s.fixture.Resolve(ctx, s.instance, []integrations.SourceKey{binding.key})
		if err != nil || len(resolved) != 1 {
			return integrations.ErrInvalidInput
		}
		record, err := integrations.NewRegistryRecord(resolved[0].Device, resolved[0].Entity,
			s.instance, binding.key, binding.alias, binding.alias, []domain.HAAlias{binding.alias}, false)
		if err != nil {
			return err
		}
		records = append(records, record)
	}
	s.records = append(s.records, records...)
	for key := range localKeys {
		s.seen[key] = true
	}
	for alias := range localAliases {
		s.aliases[alias] = true
	}
	s.bindings += len(bindings)
	return nil
}

func (p FixtureAdmission) AttachSecrets(repository *storage.SecretRepository) error {
	if p.state == nil || repository == nil {
		return integrations.ErrInvalidInput
	}
	s := p.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open || s.secretsAttached {
		return integrations.ErrInvalidInput
	}
	if err := s.host.AttachSecrets(repository); err != nil {
		return err
	}
	s.secretsAttached = true
	return nil
}
