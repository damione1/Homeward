package backups

import (
	"context"
	"io"
	"sync"
)

type Registry struct {
	mu        sync.RWMutex
	instances map[InstanceID]ManagedProvider
	order     []InstanceID
	closed    bool
}

func NewRegistry(ctx context.Context, configuration Configuration, factories []Factory) (*Registry, error) {
	if IsNil(ctx) || !configuration.Valid() {
		return nil, NewError(InvalidInput)
	}
	if ctx.Err() != nil {
		return nil, NewError(Cancelled)
	}
	byKind := make(map[ProviderKind]Factory, len(factories))
	for _, factory := range factories {
		if IsNil(factory) {
			return nil, NewError(InvalidInput)
		}
		kind := factory.Kind()
		if !kind.Valid() {
			return nil, NewError(Unsupported)
		}
		if _, found := byKind[kind]; found {
			return nil, NewError(Conflict)
		}
		byKind[kind] = factory
	}
	// Validate the entire plan before the first factory can perform IO.
	for _, settings := range configuration.instances {
		if settings.Enabled() && IsNil(byKind[settings.Kind()]) {
			return nil, NewError(Unsupported)
		}
	}
	registry := &Registry{instances: make(map[InstanceID]ManagedProvider)}
	for _, settings := range configuration.instances {
		if !settings.Enabled() {
			continue
		}
		if ctx.Err() != nil {
			_ = registry.Close()
			return nil, NewError(Cancelled)
		}
		provider, err := byKind[settings.Kind()].Open(ctx, settings)
		if err != nil || IsNil(provider) {
			if !IsNil(provider) {
				_ = provider.Close()
			}
			_ = registry.Close()
			return nil, closedError(err)
		}
		if !provider.Capabilities().Valid() {
			_ = provider.Close()
			_ = registry.Close()
			return nil, NewError(InvalidInput)
		}
		registry.instances[settings.ID()] = provider
		registry.order = append(registry.order, settings.ID())
	}
	return registry, nil
}
func closedError(err error) error {
	if admitted, ok := err.(*Error); ok && admitted != nil {
		return NewError(admitted.Kind())
	}
	return NewError(Unavailable)
}
func (r *Registry) Instance(id InstanceID) (Provider, error) {
	if r == nil || !id.Valid() {
		return nil, NewError(InvalidInput)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, NewError(Closed)
	}
	p, found := r.instances[id]
	if !found {
		return nil, NewError(NotFound)
	}
	// Hide Close even from a runtime type assertion by a borrower.
	return borrowed{provider: p}, nil
}
func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var result error
	for i := len(r.order) - 1; i >= 0; i-- {
		if err := r.instances[r.order[i]].Close(); err != nil {
			result = closedError(err)
		}
	}
	return result
}

type borrowed struct{ provider Provider }

func (p borrowed) Capabilities() Capabilities { return p.provider.Capabilities() }
func (p borrowed) Write(ctx context.Context, intent WriteIntent, reader io.Reader) (WriteResult, error) {
	return p.provider.Write(ctx, intent, reader)
}
func (p borrowed) List(ctx context.Context, request ListRequest) (Page, error) {
	return p.provider.List(ctx, request)
}
func (p borrowed) Open(ctx context.Context, id ArtifactID) (ArtifactReader, error) {
	return p.provider.Open(ctx, id)
}
func (p borrowed) Delete(ctx context.Context, id ArtifactID) (DeleteResult, error) {
	return p.provider.Delete(ctx, id)
}
func (p borrowed) Verify(ctx context.Context, id ArtifactID) (VerifiedArtifact, error) {
	return p.provider.Verify(ctx, id)
}
