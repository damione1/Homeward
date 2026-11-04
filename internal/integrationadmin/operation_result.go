package integrationadmin

import (
	"context"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
)

// PublishedDefinition projects the original native generation after the Owner
// proves its exact publication. It does not read current integration state,
// compile or transform a source, resolve credentials, or contact a driver.
// A nil definition is valid only for a proven deletion; the source version is
// still returned to bind the response to the exact immutable configuration.
func (s *Service) PublishedDefinition(ctx context.Context, attempt storage.ConfigActivationAttempt, proof configactivation.PublicationEvidence) (*wire.IntegrationInstance, storage.ConfigVersionSnapshot, error) {
	target, present := attempt.IntegrationTarget()
	if !present || !proof.Valid() || proof.Attempt() != attempt.ID() || proof.Target() != attempt.Target() || proof.ManifestSHA256() != attempt.ManifestSHA256() {
		return nil, storage.ConfigVersionSnapshot{}, ErrInvalid
	}
	permit, account, err := s.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, storage.ConfigVersionSnapshot{}, err
	}
	defer permit.Release()
	actor, accountErr := account.Value()
	author, authored := attempt.Author().Value()
	if accountErr != nil || !authored || actor != author || !permit.Ready() {
		return nil, storage.ConfigVersionSnapshot{}, authorization.ErrDenied
	}
	version, source, err := s.documents.ReadHistoricalOperationalSource(permit.Context(), proof.Target(), proof.ManifestSHA256())
	if err != nil {
		return nil, storage.ConfigVersionSnapshot{}, err
	}
	root, err := activeDocument(source.Bytes())
	if err != nil {
		return nil, storage.ConfigVersionSnapshot{}, err
	}
	declared, err := entries(root)
	if err != nil {
		return nil, storage.ConfigVersionSnapshot{}, err
	}
	off, err := disabled(root)
	if err != nil {
		return nil, storage.ConfigVersionSnapshot{}, err
	}
	key, _ := target.Instance().Value()
	var definition *wire.IntegrationInstance
	for _, entry := range declared {
		if entry.id != key {
			continue
		}
		if definition != nil {
			return nil, storage.ConfigVersionSnapshot{}, ErrWorkspaceInvalid
		}
		definition, err = publicEntry(entry, off[key])
		if err != nil {
			return nil, storage.ConfigVersionSnapshot{}, err
		}
	}
	switch target.Method() {
	case storage.ConfigurationCreateIntegration, storage.ConfigurationUpdateIntegration:
		if definition == nil {
			return nil, storage.ConfigVersionSnapshot{}, ErrWorkspaceInvalid
		}
	case storage.ConfigurationDeleteIntegration:
		if definition != nil {
			return nil, storage.ConfigVersionSnapshot{}, ErrWorkspaceInvalid
		}
	default:
		return nil, storage.ConfigVersionSnapshot{}, ErrInvalid
	}
	if !permit.Ready() {
		return nil, storage.ConfigVersionSnapshot{}, authorization.ErrDenied
	}
	return definition, version, nil
}
