package authorization

import "context"

type principalContextKey struct{}

// WithPrincipal is a trusted authentication/composition boundary. It admits no
// header or wire role; transports may only call it after authenticating the
// corresponding credential. Its private context key cannot be sent over RPC.
func WithPrincipal(ctx context.Context, principal Principal) (context.Context, error) {
	if nilContext(ctx) || !principal.valid() {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, principalContextKey{}, principal), nil
}

func PrincipalFromContext(ctx context.Context) (Principal, error) {
	if nilContext(ctx) || ctx.Err() != nil {
		return Principal{}, ErrDenied
	}
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	if !ok || !principal.valid() {
		return Principal{}, ErrDenied
	}
	return principal, nil
}
