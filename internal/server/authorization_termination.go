package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
)

type authorizationTerminationCause struct {
	reason authv1.AuthorizationTerminationReason
}

func (authorizationTerminationCause) Error() string {
	return "authorization ended; discard cached view"
}

func authorizationTerminationError(ctx context.Context) *connect.Error {
	if ctx == nil {
		return nil
	}
	var cause authorizationTerminationCause
	if !errors.As(context.Cause(ctx), &cause) {
		return nil
	}
	var reason api.ApiErrorReason
	switch cause.reason {
	case authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_REVOKED, authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_EXPIRED:
		reason = api.ApiErrorReason_API_ERROR_REASON_AUTHENTICATION_REQUIRED
	case authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_ACCOUNT_DISABLED, authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_RIGHTS_CHANGED:
		reason = api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED
	case authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_AUTHORITY_UNAVAILABLE:
		reason = api.ApiErrorReason_API_ERROR_REASON_AUTHORITY_UNAVAILABLE
	case authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_UNSPECIFIED:
		return nil
	default:
		return nil
	}
	return apierrors.New(reason).Connect()
}

// The generated handlers retain one common terminal rule even after a stream
// wrote its first frame. Ordinary client cancellation/shutdown has no such
// cause and keeps its original transport error.
type authorizationTerminationInterceptor struct{}

var _ connect.Interceptor = authorizationTerminationInterceptor{}

func (authorizationTerminationInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
		response, err := next(ctx, request)
		if terminal := authorizationTerminationError(ctx); terminal != nil {
			return nil, terminal
		}
		return response, err
	}
}
func (authorizationTerminationInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (authorizationTerminationInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		if terminal := authorizationTerminationError(ctx); terminal != nil {
			return terminal
		}
		return err
	}
}
