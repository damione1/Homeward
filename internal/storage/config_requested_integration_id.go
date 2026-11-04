package storage

import "context"

type requestedIntegrationResourceIDKey struct{}
type requestedIntegrationResourceID struct {
	target ConfigurationIntegrationTarget
	id     APIRequestedResourceID
}

// WithRequestedIntegrationResourceID carries a typed public naming choice to
// the existing native configuration transaction. It cannot create authority or
// apply to another method/instance than the already admitted durable intent.
func WithRequestedIntegrationResourceID(ctx context.Context, id APIRequestedResourceID) (context.Context, error) {
	if ctx == nil || !id.valid() {
		return nil, ErrAPIResourceIdentity
	}
	target, ok := configurationIntegrationTargetFromContext(ctx)
	if !ok || target.method != ConfigurationCreateIntegration {
		return nil, ErrAPIResourceIdentity
	}
	request, ok := ctx.Value(configurationAPIRequestKey{}).(ConfigurationAPIRequest)
	if !ok || !request.Valid() || request.target != target {
		return nil, ErrAPIResourceIdentity
	}
	if _, exists := ctx.Value(requestedIntegrationResourceIDKey{}).(requestedIntegrationResourceID); exists {
		return nil, ErrAPIResourceIdentity
	}
	return context.WithValue(ctx, requestedIntegrationResourceIDKey{}, requestedIntegrationResourceID{target, id}), nil
}
