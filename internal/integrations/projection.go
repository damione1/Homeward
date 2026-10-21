package integrations

import (
	"context"

	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

// ConfigurationIdentityProjection is the already committed Registry image
// during a closed publication cut. Only Run's calculated publishers use it;
// physical pilots keep the ordinary SQL identity admission.
type ConfigurationIdentityProjection interface {
	ResolveConfigurationIdentities(context.Context, domain.IntegrationInstanceID, []SourceKey) ([]ResolvedIdentity, error)
}

// RegistryRecord is a typed, immutable copy of an identity owned by the SQL
// registry. Historical aliases stay attached even after retirement.
type RegistryRecord struct {
	device   domain.DeviceID
	entity   domain.EntityID
	instance domain.IntegrationInstanceID
	key      SourceKey
	original domain.HAAlias
	current  domain.HAAlias
	aliases  []domain.HAAlias
	retired  bool
}

func NewRegistryRecord(device domain.DeviceID, entity domain.EntityID, instance domain.IntegrationInstanceID, key SourceKey, original, current domain.HAAlias, aliases []domain.HAAlias, retired bool) (RegistryRecord, error) {
	if _, err := device.Value(); err != nil {
		return RegistryRecord{}, ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return RegistryRecord{}, ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return RegistryRecord{}, ErrInvalidInput
	}
	if _, _, err := key.Components(); err != nil {
		return RegistryRecord{}, ErrInvalidInput
	}
	if _, err := original.Value(); err != nil {
		return RegistryRecord{}, ErrInvalidInput
	}
	if _, err := current.Value(); err != nil {
		return RegistryRecord{}, ErrInvalidInput
	}
	if len(aliases) == 0 || len(aliases) > 256 {
		return RegistryRecord{}, ErrInvalidInput
	}
	foundOriginal, foundCurrent := false, false
	seen := make(map[domain.HAAlias]bool, len(aliases))
	for _, alias := range aliases {
		if _, err := alias.Value(); err != nil || seen[alias] {
			return RegistryRecord{}, ErrInvalidInput
		}
		seen[alias] = true
		foundOriginal = foundOriginal || alias == original
		foundCurrent = foundCurrent || alias == current
	}
	if !foundOriginal || !foundCurrent {
		return RegistryRecord{}, ErrInvalidInput
	}
	return RegistryRecord{device, entity, instance, key, original, current, append([]domain.HAAlias(nil), aliases...), retired}, nil
}

func (r RegistryRecord) IDs() (domain.DeviceID, domain.EntityID, error) {
	if _, err := r.device.Value(); err != nil {
		return domain.DeviceID{}, domain.EntityID{}, err
	}
	if _, err := r.entity.Value(); err != nil {
		return domain.DeviceID{}, domain.EntityID{}, err
	}
	return r.device, r.entity, nil
}
func (r RegistryRecord) Source() (domain.IntegrationInstanceID, SourceKey, error) {
	if _, err := r.instance.Value(); err != nil {
		return domain.IntegrationInstanceID{}, SourceKey{}, err
	}
	if _, _, err := r.key.Components(); err != nil {
		return domain.IntegrationInstanceID{}, SourceKey{}, err
	}
	return r.instance, r.key, nil
}
func (r RegistryRecord) Aliases() (domain.HAAlias, domain.HAAlias, []domain.HAAlias, error) {
	if _, err := r.original.Value(); err != nil {
		return domain.HAAlias{}, domain.HAAlias{}, nil, err
	}
	if _, err := r.current.Value(); err != nil {
		return domain.HAAlias{}, domain.HAAlias{}, nil, err
	}
	return r.original, r.current, append([]domain.HAAlias(nil), r.aliases...), nil
}
func (r RegistryRecord) Retired() bool { return r.retired }

func (r RegistryRecord) Equal(other RegistryRecord) bool {
	if r.device != other.device || r.entity != other.entity || r.instance != other.instance || r.key != other.key || r.original != other.original || r.current != other.current || r.retired != other.retired || len(r.aliases) != len(other.aliases) {
		return false
	}
	for i, alias := range r.aliases {
		if alias != other.aliases[i] {
			return false
		}
	}
	return true
}

// DiscoveryDeclaration is constructed only by Host after identity resolution
// and capability validation. Its zero value is rejected by the sequencer.
type DiscoveryDeclaration struct {
	device       domain.DeviceID
	entity       domain.EntityID
	instance     domain.IntegrationInstanceID
	key          SourceKey
	descriptions []*wireregistry.CapabilityDescription
}

func newDiscoveryDeclaration(device domain.DeviceID, entity domain.EntityID, instance domain.IntegrationInstanceID, key SourceKey, descriptions []*wireregistry.CapabilityDescription) (DiscoveryDeclaration, error) {
	if _, err := device.Value(); err != nil {
		return DiscoveryDeclaration{}, ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return DiscoveryDeclaration{}, ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return DiscoveryDeclaration{}, ErrInvalidInput
	}
	if _, _, err := key.Components(); err != nil {
		return DiscoveryDeclaration{}, ErrInvalidInput
	}
	if len(descriptions) == 0 || len(descriptions) > 32 {
		return DiscoveryDeclaration{}, ErrInvalidInput
	}
	copyDescriptions := make([]*wireregistry.CapabilityDescription, len(descriptions))
	for i, description := range descriptions {
		if description == nil {
			return DiscoveryDeclaration{}, ErrInvalidInput
		}
		copyDescriptions[i] = proto.Clone(description).(*wireregistry.CapabilityDescription)
	}
	return DiscoveryDeclaration{device, entity, instance, key, copyDescriptions}, nil
}
func (d DiscoveryDeclaration) Identity() (domain.DeviceID, domain.EntityID, domain.IntegrationInstanceID, SourceKey, error) {
	if _, err := d.device.Value(); err != nil {
		return domain.DeviceID{}, domain.EntityID{}, domain.IntegrationInstanceID{}, SourceKey{}, err
	}
	if _, err := d.entity.Value(); err != nil {
		return domain.DeviceID{}, domain.EntityID{}, domain.IntegrationInstanceID{}, SourceKey{}, err
	}
	if _, err := d.instance.Value(); err != nil {
		return domain.DeviceID{}, domain.EntityID{}, domain.IntegrationInstanceID{}, SourceKey{}, err
	}
	if _, _, err := d.key.Components(); err != nil {
		return domain.DeviceID{}, domain.EntityID{}, domain.IntegrationInstanceID{}, SourceKey{}, err
	}
	return d.device, d.entity, d.instance, d.key, nil
}
func (d DiscoveryDeclaration) Descriptions() []*wireregistry.CapabilityDescription {
	out := make([]*wireregistry.CapabilityDescription, len(d.descriptions))
	for i, description := range d.descriptions {
		out[i] = proto.Clone(description).(*wireregistry.CapabilityDescription)
	}
	return out
}

// ProjectionSink is the in-memory admission path for registry and declarations.
// It is optional for legacy Sink fixtures; snapshot composition requires it.
type ProjectionSink interface {
	PublishRegistry(context.Context, []RegistryRecord) error
	AcceptDiscovery(context.Context, []DiscoveryDeclaration) error
	InvalidateRegistry()
}

// DiscoveryWithdrawalSink is the optional live-capability retirement boundary.
// The registry remains the durable identity authority.
type DiscoveryWithdrawalSink interface {
	WithdrawDiscovery(context.Context, domain.EntityID) error
}
