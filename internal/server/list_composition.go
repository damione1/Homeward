package server

import (
	"context"
	"errors"
	"sync"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/lists"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

var ErrListImportBinding = errors.New("server: legacy list binding conflicts with registry")

type listComposition struct {
	store    *storage.Store
	manifest registry.Manifest
	registry *registry.Repository
	service  *lists.Service
	handler  *listHandler
	once     sync.Once
	done     chan struct{}
	closeErr error
}

// Run must pass its one Store and the same session Authority used by native
// authentication. This composition has no pilot, background worker or device
// action, so it can exist even when the Host has no configured integration.
func newListComposition(store *storage.Store, authority *authorization.Authority) (*listComposition, error) {
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		return nil, err
	}
	return newListCompositionWithManifest(store, authority, manifest)
}

func newListCompositionWithManifest(store *storage.Store, authority *authorization.Authority, manifest registry.Manifest) (*listComposition, error) {
	if store == nil || authority == nil {
		return nil, ErrInvalidPublicPlan
	}
	repo, err := lists.NewRepository(store, manifest)
	if err != nil {
		return nil, err
	}
	service, err := lists.NewService(repo, authority)
	if err != nil {
		return nil, err
	}
	cursors, err := lists.NewCursorCodec()
	if err != nil {
		_ = service.Close()
		return nil, err
	}
	handler, err := newListHandler(service, cursors)
	if err != nil {
		_ = service.Close()
		return nil, err
	}
	registryRepo, err := registry.NewRepository(store, manifest)
	if err != nil {
		_ = service.Close()
		return nil, err
	}
	return &listComposition{store: store, manifest: manifest, registry: registryRepo, service: service, handler: handler, done: make(chan struct{})}, nil
}
func (c *listComposition) Register(plan *PublicPlan) error {
	if c == nil || c.handler == nil {
		return ErrInvalidPublicPlan
	}
	return registerListRoutes(plan, c.handler)
}
func (c *listComposition) Close() error {
	if c == nil {
		return ErrInvalidPublicPlan
	}
	c.once.Do(func() {
		c.closeErr = c.service.Close()
		close(c.done)
	})
	return c.closeErr
}
func (c *listComposition) Done() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.done
}

// An eventual trusted importer calls these methods, never an RPC. The frozen
// manifest and the mutable registry are compared before the lists transaction.
// There is intentionally no claim of cross-repository atomicity.
func (c *listComposition) ImportLegacyList(ctx context.Context, alias domain.HAAlias, payload lists.LegacyListPayload) (lists.MutationReceipt, error) {
	if err := c.checkImportBinding(ctx, alias); err != nil {
		return lists.MutationReceipt{}, err
	}
	return c.service.ImportLegacyList(ctx, alias, payload)
}
func (c *listComposition) ImportLegacyItem(ctx context.Context, id domain.ListID, alias domain.HAAlias, payload lists.LegacyItemPayload, expected lists.ListRevision) (lists.MutationReceipt, error) {
	if c == nil || c.service == nil || ctx == nil {
		return lists.MutationReceipt{}, ErrListImportBinding
	}
	if err := c.service.VerifyLegacyImportAlias(ctx, id, alias); err != nil {
		return lists.MutationReceipt{}, err
	}
	if err := c.checkImportBinding(ctx, alias); err != nil {
		return lists.MutationReceipt{}, err
	}
	return c.service.ImportLegacyItem(ctx, id, payload, expected)
}
func (c *listComposition) checkImportBinding(ctx context.Context, alias domain.HAAlias) error {
	if c == nil || c.registry == nil || ctx == nil {
		return ErrListImportBinding
	}
	source, known := c.manifest.SourceFor(alias)
	name, valid := source.Value()
	if !known || !valid || name != "local_todo" && name != "shopping_list" {
		return ErrListImportBinding
	}
	records, err := c.registry.List(ctx)
	if err != nil {
		if errors.Is(err, registry.ErrAliasConflict) {
			return errors.Join(ErrListImportBinding, err)
		}
		return err
	}
	seen := false
	for _, record := range records {
		original, _, aliases, err := record.Aliases()
		if err != nil {
			return ErrListImportBinding
		}
		for _, present := range aliases {
			if present != alias {
				continue
			}
			if seen || original != alias || record.Retired() {
				return ErrListImportBinding
			}
			seen = true
		}
	}
	return nil
}

// Compile-time check that the guard consumes the existing registry record
// contract rather than maintaining a shadow alias table.
var _ interface {
	List(context.Context) ([]integrations.RegistryRecord, error)
} = (*registry.Repository)(nil)
